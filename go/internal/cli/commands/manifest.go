package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"
)

const gcsBaseURL = "https://storage.googleapis.com/wendyos-images-public"

// mainManifest is the top-level manifest fetched from GCS master.json.
type mainManifest struct {
	Devices map[string]manifestDevice `json:"devices"`
}

// manifestDevice describes a single device entry in the main manifest.
type manifestDevice struct {
	Name         string `json:"name"`
	ManifestPath string `json:"manifest_path"`
	Architecture string `json:"architecture"`
}

// deviceManifest contains version info for a specific device.
type deviceManifest struct {
	Versions map[string]deviceVersion `json:"versions"`
}

// deviceVersion describes one OS image version.
type deviceVersion struct {
	ImagePath string `json:"image_path"`
	ImageSize int64  `json:"image_size"`
	Stable    bool   `json:"stable"`
}

// deviceInfo is the aggregated info shown in the picker for one device.
type deviceInfo struct {
	Key            string // manifest key, e.g. "raspberry-pi-5"
	Name           string // human-readable name
	Architecture   string
	LatestVersion  string // latest stable version tag
	NightlyVersion string // latest prerelease version tag
	ManifestPath   string
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
		dm, err := fetchDeviceManifest(dev.ManifestPath)
		if err != nil {
			// Skip devices whose manifest can't be fetched.
			continue
		}

		info := deviceInfo{
			Key:          key,
			Name:         dev.Name,
			Architecture: dev.Architecture,
			ManifestPath: dev.ManifestPath,
		}

		// Find latest stable and nightly versions.
		for ver, v := range dm.Versions {
			if v.Stable {
				if info.LatestVersion == "" || ver > info.LatestVersion {
					info.LatestVersion = ver
				}
			} else {
				if info.NightlyVersion == "" || ver > info.NightlyVersion {
					info.NightlyVersion = ver
				}
			}
		}

		devices = append(devices, info)
	}

	sort.Slice(devices, func(i, j int) bool {
		return devices[i].Name < devices[j].Name
	})

	return devices, nil
}

// getLatestImageInfo returns the download URL and metadata for the latest image.
func getLatestImageInfo(manifestPath string, nightly bool) (*imageInfo, error) {
	dm, err := fetchDeviceManifest(manifestPath)
	if err != nil {
		return nil, err
	}

	var bestVersion string
	var bestImg deviceVersion
	for ver, v := range dm.Versions {
		if nightly && !v.Stable {
			if bestVersion == "" || ver > bestVersion {
				bestVersion = ver
				bestImg = v
			}
		} else if !nightly && v.Stable {
			if bestVersion == "" || ver > bestVersion {
				bestVersion = ver
				bestImg = v
			}
		}
	}

	if bestVersion == "" {
		kind := "stable"
		if nightly {
			kind = "nightly"
		}
		return nil, fmt.Errorf("no %s version found", kind)
	}

	return &imageInfo{
		DownloadURL: gcsBaseURL + "/" + bestImg.ImagePath,
		ImageSize:   bestImg.ImageSize,
		Version:     bestVersion,
	}, nil
}
