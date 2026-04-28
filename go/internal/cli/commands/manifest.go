package commands

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// StorageType identifies the physical storage medium of a drive or image.
type StorageType string

const (
	StorageUnknown StorageType = ""
	StorageNVMe    StorageType = "nvme"
	StorageSD      StorageType = "sd"
	StorageEMMC    StorageType = "emmc"
)

const gcsBaseURL = "https://storage.googleapis.com/wendyos-images-public"

// mainManifest is the top-level manifest fetched from GCS master.json.
type mainManifest struct {
	Devices  map[string]manifestDevice `json:"devices"`
	Firmware map[string]manifestDevice `json:"firmware,omitempty"`
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
	Path               string `json:"path"`
	SizeBytes          int64  `json:"size_bytes"`
	Checksum           string `json:"checksum"`
	IsLatest           bool   `json:"is_latest"`
	IsNightly          bool   `json:"is_nightly"`
	OTAUpdatePath      string `json:"ota_update_path"`
	OTAUpdateChecksum  string `json:"ota_update_checksum"`
	OTAUpdateSizeBytes int64  `json:"ota_update_size_bytes"`
	// Storage-specific image paths for devices that produce both an NVMe and
	// an SD card image (e.g. jetson-orin-nano). Old CLI versions use Path,
	// which always points to the NVMe image for backwards compatibility.
	NVMEPath        string `json:"nvme_path,omitempty"`
	NVMESizeBytes   int64  `json:"nvme_size_bytes,omitempty"`
	NVMEChecksum    string `json:"nvme_checksum,omitempty"`
	SDCardPath      string `json:"sd_path,omitempty"`
	SDCardSizeBytes int64  `json:"sd_size_bytes,omitempty"`
	SDCardChecksum  string `json:"sd_checksum,omitempty"`
	EMMCPath          string `json:"emmc_path,omitempty"`
	EMMCSizeBytes     int64  `json:"emmc_size_bytes,omitempty"`
	EMMCChecksum      string `json:"emmc_checksum,omitempty"`
	RecoveryPath      string `json:"recovery_path,omitempty"`
	RecoveryChecksum  string `json:"recovery_checksum,omitempty"`
	RecoverySizeBytes int64  `json:"recovery_size_bytes,omitempty"`
}

// deviceInfo is the aggregated info shown in the picker for one device.
type deviceInfo struct {
	Key            string          // manifest key, e.g. "raspberry-pi-5"
	Name           string          // human-readable name
	LatestVersion  string          // latest stable version tag
	NightlyVersion string          // latest prerelease version tag
	Stability      string          // "stable" or "experimental"
	Manifest       *deviceManifest // cached manifest to avoid re-fetching
}

// imageInfo describes a downloadable OS image.
type imageInfo struct {
	DownloadURL string
	ImageSize   int64
	Version     string
	Storage     StorageType // set for multi-storage devices; empty for single-storage
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
			Stability:      dev.Stability,
			Manifest:       dm,
		}

		devices = append(devices, info)
	}

	// Stable devices first, then alphabetically within each group.
	sort.Slice(devices, func(i, j int) bool {
		si := devices[i].Stability == "stable"
		sj := devices[j].Stability == "stable"
		if si != sj {
			return si
		}
		return devices[i].Name < devices[j].Name
	})

	return devices, nil
}

// getImageInfo returns the download URL and metadata for a specific version
// from an already-fetched device manifest, using the legacy Path field.
// For devices with multiple storage types use getImageInfoForStorage instead.
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

// hasMultipleStorages reports whether the given version of dm has at least two
// distinct storage-specific images published (NVMe, SD card, and/or eMMC).
func hasMultipleStorages(dm *deviceManifest, ver string) bool {
	v, ok := dm.Versions[ver]
	if !ok {
		return false
	}
	count := 0
	if v.NVMEPath != "" {
		count++
	}
	if v.SDCardPath != "" {
		count++
	}
	if v.EMMCPath != "" {
		count++
	}
	return count >= 2
}

// getImageInfoForStorage returns the image info for a specific storage type.
// For single-storage devices (or when st is StorageUnknown) it falls back to
// the legacy Path field so old behaviour is preserved.
func getImageInfoForStorage(dm *deviceManifest, ver string, st StorageType) (*imageInfo, error) {
	v, ok := dm.Versions[ver]
	if !ok {
		return nil, fmt.Errorf("version %s not found in device manifest", ver)
	}

	switch st {
	case StorageNVMe:
		if v.NVMEPath != "" {
			return &imageInfo{
				DownloadURL: gcsBaseURL + "/" + v.NVMEPath,
				ImageSize:   v.NVMESizeBytes,
				Version:     ver,
				Storage:     StorageNVMe,
			}, nil
		}
	case StorageSD:
		if v.SDCardPath != "" {
			return &imageInfo{
				DownloadURL: gcsBaseURL + "/" + v.SDCardPath,
				ImageSize:   v.SDCardSizeBytes,
				Version:     ver,
				Storage:     StorageSD,
			}, nil
		}
	case StorageEMMC:
		if v.EMMCPath != "" {
			return &imageInfo{
				DownloadURL: gcsBaseURL + "/" + v.EMMCPath,
				ImageSize:   v.EMMCSizeBytes,
				Version:     ver,
				Storage:     StorageEMMC,
			}, nil
		}
	}

	// Fallback: use the legacy single-storage Path.
	return &imageInfo{
		DownloadURL: gcsBaseURL + "/" + v.Path,
		ImageSize:   v.SizeBytes,
		Version:     ver,
		Storage:     StorageUnknown,
	}, nil
}

// getOTAUpdateURL returns the Mender artifact URL for a specific version,
// or an error if the version has no OTA artifact.
func getOTAUpdateURL(dm *deviceManifest, ver string) (string, error) {
	v, ok := dm.Versions[ver]
	if !ok {
		return "", fmt.Errorf("version %s not found in device manifest", ver)
	}
	if v.OTAUpdatePath == "" {
		return "", fmt.Errorf("version %s has no OTA update artifact", ver)
	}
	return gcsBaseURL + "/" + v.OTAUpdatePath, nil
}

// getLatestOTAInfoForDeviceType fetches the manifest and returns the OTA artifact
// URL and version tag for the given device type. When nightly is true the latest
// nightly (prerelease) version is used instead of the latest stable version.
func getLatestOTAInfoForDeviceType(deviceType string, nightly bool) (artifactURL, latestVersion string, err error) {
	main, err := fetchMainManifest()
	if err != nil {
		return "", "", fmt.Errorf("fetching manifest: %w", err)
	}

	dev, ok := main.Devices[deviceType]
	if !ok {
		return "", "", fmt.Errorf("device type %q not found in manifest", deviceType)
	}
	if dev.ManifestPath == "" {
		return "", "", fmt.Errorf("no manifest path for device type %q", deviceType)
	}

	dm, err := fetchDeviceManifest(dev.ManifestPath)
	if err != nil {
		return "", "", fmt.Errorf("fetching device manifest: %w", err)
	}

	latest := dev.Latest
	if nightly && dev.LatestNightly != "" {
		latest = dev.LatestNightly
	}
	if latest == "" {
		return "", "", fmt.Errorf("no latest version for device type %q", deviceType)
	}

	u, err := getOTAUpdateURL(dm, latest)
	if err != nil {
		return "", "", err
	}
	return u, latest, nil
}

// firmwareManifest contains version info for a specific chip.
type firmwareManifest struct {
	ChipID   string                         `json:"chip_id"`
	Versions map[string]firmwareVersionInfo `json:"versions"`
}

// firmwareVersionInfo describes one firmware version.
type firmwareVersionInfo struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
	Checksum  string `json:"checksum"`
	IsLatest  bool   `json:"is_latest"`
	IsNightly bool   `json:"is_nightly"`
}

func fetchFirmwareManifest(path string) (*firmwareManifest, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	url := gcsBaseURL + "/" + path
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetching firmware manifest: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("firmware manifest returned status %d", resp.StatusCode)
	}

	var fm firmwareManifest
	if err := json.NewDecoder(resp.Body).Decode(&fm); err != nil {
		return nil, fmt.Errorf("decoding firmware manifest: %w", err)
	}
	return &fm, nil
}

// getFirmwareInfo returns the download URL and metadata for a specific firmware
// version from an already-fetched firmware manifest.
func getFirmwareInfo(fm *firmwareManifest, ver string) (*imageInfo, error) {
	v, ok := fm.Versions[ver]
	if !ok {
		return nil, fmt.Errorf("version %s not found in firmware manifest", ver)
	}

	return &imageInfo{
		DownloadURL: gcsBaseURL + "/" + v.Path,
		ImageSize:   v.SizeBytes,
		Version:     ver,
	}, nil
}

func getTegraflashInfo(dm *deviceManifest, ver, target string) (*imageInfo, error) {
	v, ok := dm.Versions[ver]
	if !ok {
		return nil, fmt.Errorf("version %s not found in device manifest", ver)
	}

	var path string
	var size int64
	switch target {
	case "emmc":
		path = v.EMMCPath
		size = v.EMMCSizeBytes
	case "recovery":
		path = v.RecoveryPath
		size = v.RecoverySizeBytes
	default:
		return nil, fmt.Errorf("unknown tegraflash target %q", target)
	}
	if path == "" {
		return nil, fmt.Errorf("version %s has no %s tegraflash artifact", ver, target)
	}

	return &imageInfo{
		DownloadURL: gcsBaseURL + "/" + path,
		ImageSize:   size,
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
