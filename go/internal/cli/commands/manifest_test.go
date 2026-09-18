package commands

import (
	"strings"
	"testing"
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
