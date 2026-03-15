package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

const gcsBaseURL = "https://storage.googleapis.com/wendyos-images-public"

// mainManifest is the top-level manifest fetched from GCS master.json.
type mainManifest struct {
	Devices map[string]manifestDevice `json:"devices"`
}

// manifestDevice describes a single device entry in the main manifest.
type manifestDevice struct {
	Latest        string `json:"latest"`
	LatestNightly string `json:"latest_nightly"`
	ManifestPath  string `json:"manifest_path"`
	Stability     string `json:"stability"`
}

// deviceManifest contains version info for a specific device.
type deviceManifest struct {
	DeviceID string                   `json:"device_id"`
	Versions map[string]deviceVersion `json:"versions"`
}

// deviceVersion describes one OS image version.
type deviceVersion struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	Checksum  string `json:"checksum"`
	IsLatest  bool   `json:"is_latest"`
	IsNightly bool   `json:"is_nightly"`
}

// deviceInfo is the aggregated info shown in the picker for one device.
type deviceInfo struct {
	Key            string          // manifest key, e.g. "raspberry-pi-5"
	Name           string          // human-readable name
	LatestVersion  string          // latest stable version tag
	NightlyVersion string          // latest prerelease version tag
	Manifest       *deviceManifest // cached manifest to avoid re-fetching
}

// imageInfo describes a downloadable OS image.
type imageInfo struct {
	DownloadURL string
	ImageSize   int64
	Version     string
}

func fetchMainManifest() (*mainManifest, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(gcsBaseURL + "/manifests/master.json")
	if err != nil {
		return nil, fmt.Errorf("fetching main manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest returned status %d", resp.StatusCode)
	}

	var m mainManifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("decoding main manifest: %w", err)
	}
	return &m, nil
}

func fetchDeviceManifest(path string) (*deviceManifest, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	url := gcsBaseURL + "/" + path
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetching device manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device manifest returned status %d", resp.StatusCode)
	}

	var dm deviceManifest
	if err := json.NewDecoder(resp.Body).Decode(&dm); err != nil {
		return nil, fmt.Errorf("decoding device manifest: %w", err)
	}
	return &dm, nil
}

// getAvailableDevices fetches the main manifest and each device's version info.
func getAvailableDevices() ([]deviceInfo, error) {
	main, err := fetchMainManifest()
	if err != nil {
		return nil, err
	}

	var devices []deviceInfo
	for key, dev := range main.Devices {
		if dev.ManifestPath == "" {
			continue
		}

		dm, err := fetchDeviceManifest(dev.ManifestPath)
		if err != nil {
			// Skip devices whose manifest can't be fetched.
			continue
		}

		info := deviceInfo{
			Key:            key,
			Name:           humanizeDeviceKey(key),
			LatestVersion:  dev.Latest,
			NightlyVersion: dev.LatestNightly,
			Manifest:       dm,
		}

		devices = append(devices, info)
	}

	sort.Slice(devices, func(i, j int) bool {
		return devices[i].Name < devices[j].Name
	})

	return devices, nil
}

// getImageInfo returns the download URL and metadata for a specific version
// from an already-fetched device manifest.
func getImageInfo(dm *deviceManifest, ver string) (*imageInfo, error) {
	v, ok := dm.Versions[ver]
	if !ok {
		return nil, fmt.Errorf("version %s not found in device manifest", ver)
	}

	return &imageInfo{
		DownloadURL: gcsBaseURL + "/" + v.Path,
		ImageSize:   v.SizeBytes,
		Version:     ver,
	}, nil
}

// humanizeDeviceKey converts a manifest key like "raspberry-pi-5" to "Raspberry Pi 5".
func humanizeDeviceKey(key string) string {
	words := strings.Split(key, "-")
	for i, w := range words {
		if len(w) > 0 {
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}
