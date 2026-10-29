package commands

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestManifest() *deviceManifest {
	return &deviceManifest{
		DeviceID: "jetson-orin-nano",
		Versions: map[string]deviceVersion{
			"1.0.0": {
				// legacy fields (clobbered in the wild) point at SD
				Path: "img/sd.img.zip", BmapPath: "img/sd.bmap",
				// storage-keyed triples
				NVMEPath: "img/nvme.img.zip", NVMEBmapPath: "img/nvme.bmap",
				NVMEZstPath: "img/nvme.img.zst", NVMEZstSizeBytes: 111,
				SDPath: "img/sd.img.zip", SDBmapPath: "img/sd.bmap",
				SDZstPath: "img/sd.img.zst", SDZstSizeBytes: 222,
			},
		},
	}
}

func TestGetImageInfoNVMePicksNVMeTriple(t *testing.T) {
	info, err := getImageInfo(newTestManifest(), "1.0.0", "nvme")
	if err != nil {
		t.Fatalf("getImageInfo: %v", err)
	}
	if info.DownloadURL != gcsBaseURL+"/img/nvme.img.zip" {
		t.Errorf("DownloadURL = %q", info.DownloadURL)
	}
	if info.BmapURL != gcsBaseURL+"/img/nvme.bmap" {
		t.Errorf("BmapURL = %q", info.BmapURL)
	}
	if info.ZstURL != gcsBaseURL+"/img/nvme.img.zst" {
		t.Errorf("ZstURL = %q", info.ZstURL)
	}
}

func TestGetImageInfoSDPicksSDTriple(t *testing.T) {
	info, err := getImageInfo(newTestManifest(), "1.0.0", "sd")
	if err != nil {
		t.Fatalf("getImageInfo: %v", err)
	}
	if info.ZstURL != gcsBaseURL+"/img/sd.img.zst" || info.BmapURL != gcsBaseURL+"/img/sd.bmap" {
		t.Errorf("SD triple not selected: %+v", info)
	}
}

// An artifact this build cannot write must be refused at resolution, which is
// the one point `os install`, `os download` and the VM path all share.
func TestGetImageInfoRejectsAnUnsupportedInstallMode(t *testing.T) {
	dm := &deviceManifest{Versions: map[string]deviceVersion{
		"1.0.0": {InstallMode: "edl", Path: "img/bundle.qcomflash.tar.gz"},
	}}
	_, err := getImageInfo(dm, "1.0.0", "sd")
	if err == nil {
		t.Fatal("an edl-mode version resolved to a writable image")
	}
	if !strings.Contains(err.Error(), "update wendy") {
		t.Errorf("error %q does not name the remedy", err)
	}
}

func TestGetImageInfoFallsBackToLegacy(t *testing.T) {
	dm := &deviceManifest{Versions: map[string]deviceVersion{
		"1.0.0": {Path: "img/legacy.img.zip", BmapPath: "img/legacy.bmap"},
	}}
	info, err := getImageInfo(dm, "1.0.0", "nvme")
	if err != nil {
		t.Fatalf("getImageInfo: %v", err)
	}
	if info.DownloadURL != gcsBaseURL+"/img/legacy.img.zip" || info.BmapURL != gcsBaseURL+"/img/legacy.bmap" {
		t.Errorf("legacy fallback not used: %+v", info)
	}
	if info.ZstURL != "" {
		t.Errorf("ZstURL should be empty when no zst published, got %q", info.ZstURL)
	}
}

func TestFetchDeviceInfosFetchesInParallel(t *testing.T) {
	main := &mainManifest{Devices: map[string]manifestDevice{
		"raspberry-pi-5":   {Latest: "1.0.0", ManifestPath: "manifests/raspberry-pi-5.json"},
		"jetson-orin-nano": {Latest: "1.0.0", ManifestPath: "manifests/jetson-orin-nano.json"},
		"vm-arm64":         {Latest: "1.0.0", ManifestPath: "manifests/vm-arm64.json"},
	}}
	// Every fetch waits until all of them have started, so fetches made one
	// after another time out and their devices drop out of the result.
	var mu sync.Mutex
	started := 0
	allStarted := make(chan struct{})
	fetch := func(path string) (*deviceManifest, error) {
		mu.Lock()
		started++
		if started == len(main.Devices) {
			close(allStarted)
		}
		mu.Unlock()
		select {
		case <-allStarted:
			return &deviceManifest{DeviceID: path}, nil
		case <-time.After(2 * time.Second):
			return nil, errors.New("fetches did not overlap")
		}
	}

	devices := fetchDeviceInfos(main, func(dev manifestDevice) string { return dev.Latest }, fetch)
	if len(devices) != len(main.Devices) {
		t.Fatalf("got %d devices, want %d: device manifests were not fetched in parallel", len(devices), len(main.Devices))
	}
}

func TestFetchDeviceInfosSkipsDevicesWithoutAManifest(t *testing.T) {
	main := &mainManifest{Devices: map[string]manifestDevice{
		"raspberry-pi-5":   {Latest: "1.0.0", LatestNightly: "1.1.0-nightly", Stability: "stable", ManifestPath: "manifests/raspberry-pi-5.json"},
		"jetson-orin-nano": {Latest: "1.0.0", ManifestPath: "manifests/jetson-orin-nano.json"},
		"vm-arm64":         {Latest: "1.0.0"},
	}}
	fetch := func(path string) (*deviceManifest, error) {
		if path == "manifests/jetson-orin-nano.json" {
			return nil, errors.New("unreachable")
		}
		return &deviceManifest{DeviceID: path}, nil
	}

	devices := fetchDeviceInfos(main, func(dev manifestDevice) string { return dev.LatestNightly }, fetch)
	if len(devices) != 1 {
		var keys []string
		for _, d := range devices {
			keys = append(keys, d.Key)
		}
		sort.Strings(keys)
		t.Fatalf("got devices %v, want only raspberry-pi-5", keys)
	}
	got := devices[0]
	if got.Key != "raspberry-pi-5" || got.Name != humanizeDeviceKey("raspberry-pi-5") {
		t.Errorf("device = %q (%q)", got.Key, got.Name)
	}
	if got.LatestVersion != "1.1.0-nightly" || got.NightlyVersion != "1.1.0-nightly" || got.Stability != "stable" {
		t.Errorf("versions = %q/%q, stability %q", got.LatestVersion, got.NightlyVersion, got.Stability)
	}
	if got.Manifest == nil || got.Manifest.DeviceID != "manifests/raspberry-pi-5.json" {
		t.Errorf("manifest = %+v", got.Manifest)
	}
}
