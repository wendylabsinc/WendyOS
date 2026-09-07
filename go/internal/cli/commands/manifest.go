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
	InstallMode            string `json:"install_mode"`
	Path                   string `json:"path"`
	SizeBytes              int64  `json:"size_bytes"`
	Checksum               string `json:"checksum"`
	IsLatest               bool   `json:"is_latest"`
	IsNightly              bool   `json:"is_nightly"`
	OTAUpdatePath          string `json:"ota_update_path"`
	OTAUpdateChecksum      string `json:"ota_update_checksum"`
	OTAUpdateSizeBytes     int64  `json:"ota_update_size_bytes"`
	NVMEOTAUpdatePath      string `json:"nvme_ota_update_path"`
	NVMEOTAUpdateChecksum  string `json:"nvme_ota_update_checksum"`
	NVMEOTAUpdateSizeBytes int64  `json:"nvme_ota_update_size_bytes"`
	BmapPath               string `json:"bmap_path"`

	ZstPath      string `json:"zst_path"`
	ZstChecksum  string `json:"zst_checksum"`
	ZstSizeBytes int64  `json:"zst_size_bytes"`

	// Storage-keyed image+bmap+zst triples (NVMe).
	NVMEPath         string `json:"nvme_path"`
	NVMEChecksum     string `json:"nvme_checksum"`
	NVMESizeBytes    int64  `json:"nvme_size_bytes"`
	NVMEBmapPath     string `json:"nvme_bmap_path"`
	NVMEZstPath      string `json:"nvme_zst_path"`
	NVMEZstChecksum  string `json:"nvme_zst_checksum"`
	NVMEZstSizeBytes int64  `json:"nvme_zst_size_bytes"`

	// Storage-keyed image+bmap+zst triples (SD / removable card; the default).
	SDPath         string `json:"sd_path"`
	SDChecksum     string `json:"sd_checksum"`
	SDSizeBytes    int64  `json:"sd_size_bytes"`
	SDBmapPath     string `json:"sd_bmap_path"`
	SDZstPath      string `json:"sd_zst_path"`
	SDZstChecksum  string `json:"sd_zst_checksum"`
	SDZstSizeBytes int64  `json:"sd_zst_size_bytes"`

	// Explicit raw-media artifacts retained for --rootfs-only on recovery-first
	// T234 releases. They deliberately do not fall back to legacy image fields.
	NVMERootfsOnlyPath         string `json:"nvme_rootfs_only_path"`
	NVMERootfsOnlyChecksum     string `json:"nvme_rootfs_only_checksum"`
	NVMERootfsOnlySizeBytes    int64  `json:"nvme_rootfs_only_size_bytes"`
	NVMERootfsOnlyBmapPath     string `json:"nvme_rootfs_only_bmap_path"`
	NVMERootfsOnlyZstPath      string `json:"nvme_rootfs_only_zst_path"`
	NVMERootfsOnlyZstChecksum  string `json:"nvme_rootfs_only_zst_checksum"`
	NVMERootfsOnlyZstSizeBytes int64  `json:"nvme_rootfs_only_zst_size_bytes"`
	SDRootfsOnlyPath           string `json:"sd_rootfs_only_path"`
	SDRootfsOnlyChecksum       string `json:"sd_rootfs_only_checksum"`
	SDRootfsOnlySizeBytes      int64  `json:"sd_rootfs_only_size_bytes"`
	SDRootfsOnlyBmapPath       string `json:"sd_rootfs_only_bmap_path"`
	SDRootfsOnlyZstPath        string `json:"sd_rootfs_only_zst_path"`
	SDRootfsOnlyZstChecksum    string `json:"sd_rootfs_only_zst_checksum"`
	SDRootfsOnlyZstSizeBytes   int64  `json:"sd_rootfs_only_zst_size_bytes"`

	// Thor flashpack: the USB-recovery flash artifact (a .tar.zst) wendy downloads,
	// extracts and flashes. Only present for jetson-agx-thor.
	FlashpackPath      string `json:"flashpack_path"`
	FlashpackChecksum  string `json:"flashpack_checksum"`
	FlashpackSizeBytes int64  `json:"flashpack_size_bytes"`

	NVMEFlashpackPath      string `json:"nvme_flashpack_path"`
	NVMEFlashpackChecksum  string `json:"nvme_flashpack_checksum"`
	NVMEFlashpackSizeBytes int64  `json:"nvme_flashpack_size_bytes"`
	EMMCFlashpackPath      string `json:"emmc_flashpack_path"`
	EMMCFlashpackChecksum  string `json:"emmc_flashpack_checksum"`
	EMMCFlashpackSizeBytes int64  `json:"emmc_flashpack_size_bytes"`

	// Driver add-ons (systemd-sysext .raw) published for this OS version, one per
	// (name, kernel). Written by the publisher's --extension-file; the CLI resolves
	// install-by-name against these. Mirrors the publisher's ExtensionMetadata.
	Extensions []extensionEntry `json:"extensions,omitempty"`
}

// extensionEntry is one driver add-on in a version's extensions[]. The fields
// mirror the publisher's ExtensionMetadata and map onto the agent's DriverSpec.
type extensionEntry struct {
	Name          string   `json:"name"`
	Version       string   `json:"version,omitempty"`
	KernelVersion string   `json:"kernel_version"`
	Path          string   `json:"path"`
	SHA256        string   `json:"sha256"`
	SizeBytes     int64    `json:"size_bytes"`
	Signature     string   `json:"signature,omitempty"`
	ModulesLoad   []string `json:"modules_load,omitempty"`
}

// driverExtensionsFor returns the add-ons the manifest publishes for a device.
// pr>0 resolves a per-PR manifest keyed by the PR's own version ("pr-<N>"), not a
// device VERSION_ID; the on-device extension-release check still rejects an add-on
// built for a different OS version.
func driverExtensionsFor(deviceType, osVersion string, pr int) ([]extensionEntry, error) {
	var main *mainManifest
	var err error
	if pr > 0 {
		main, err = fetchPRMainManifest(pr)
	} else {
		main, err = fetchMainManifest()
	}
	if err != nil {
		return nil, err
	}
	dev, ok := main.Devices[deviceType]
	if !ok || dev.ManifestPath == "" {
		return nil, fmt.Errorf("device type %q not found in the manifest", deviceType)
	}
	dm, err := fetchDeviceManifest(dev.ManifestPath)
	if err != nil {
		return nil, err
	}
	version := osVersion
	if pr > 0 {
		version = prDeviceVersion(dev)
	}
	v, ok := dm.Versions[version]
	if !ok {
		return nil, fmt.Errorf("no manifest entry for version %s on %s", version, deviceType)
	}
	return v.Extensions, nil
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
	Checksum    string
	ImageSize   int64
	Version     string
	BmapURL     string
	ZstURL      string
	ZstChecksum string
	// Storage is the resolved manifest variant ("sd"/"nvme"/""), used to keep
	// the on-disk cache keyed per variant so an SD download and an NVMe download
	// of the same device+version never collide on one cache file.
	Storage string
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

// prBasePath returns the GCS path prefix under which a PR build's manifests
// and images are published, e.g. "pr/123/".
func prBasePath(pr int) string {
	return fmt.Sprintf("pr/%d/", pr)
}

// fetchPRMainManifest fetches the per-PR master manifest written by the
// wendyos-builder publish-pr job.
func fetchPRMainManifest(pr int) (*mainManifest, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	url := gcsBaseURL + "/" + prBasePath(pr) + "manifests/master.json"
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetching PR %d manifest: %w", pr, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("no build found for PR %d — is the build still running or the PR closed?", pr)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("PR %d manifest returned status %d", pr, resp.StatusCode)
	}

	var m mainManifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return nil, fmt.Errorf("decoding PR %d manifest: %w", pr, err)
	}
	return &m, nil
}

// prDeviceVersion resolves the version tag to install/update for a PR device
// entry: the PR's Latest version, falling back to LatestNightly when the PR
// only published a nightly-style build. The publish-pr job always writes with
// --nightly, so Latest is empty and LatestNightly carries the "pr-N" tag —
// every PR device entry hits the fallback in practice. Shared by
// getAvailablePRDevices and getPROTAInfoForDeviceType so the two stay
// consistent.
func prDeviceVersion(dev manifestDevice) string {
	if dev.Latest != "" {
		return dev.Latest
	}
	return dev.LatestNightly
}

// getAvailablePRDevices mirrors getAvailableDevices for a per-PR manifest.
// The master entries' ManifestPath values are already "pr/<N>/manifests/<device>.json"
// (written by the publish-pr job), so fetchDeviceManifest works unchanged.
func getAvailablePRDevices(pr int) ([]deviceInfo, error) {
	main, err := fetchPRMainManifest(pr)
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
			LatestVersion:  prDeviceVersion(dev),
			NightlyVersion: dev.LatestNightly,
			Stability:      dev.Stability,
			Manifest:       dm,
		}

		devices = append(devices, info)
	}

	sort.Slice(devices, func(i, j int) bool { return devices[i].Name < devices[j].Name })

	return devices, nil
}

// imageTriple is the resolved (image, bmap, zst) path set for one storage.
type imageTriple struct {
	imagePath string
	imageSize int64
	bmapPath  string
	zstPath   string
}

// getImageInfo resolves the download URLs for ver on dm, preferring the triple
// matching storage ("nvme" or "sd") and falling back to the legacy fields when
// that storage has no dedicated artifacts. Keeping image+bmap+zst from one
// triple guarantees they describe the same image (no cross-storage mismatch).
func getImageInfo(dm *deviceManifest, ver, storage string) (*imageInfo, error) {
	v, ok := dm.Versions[ver]
	if !ok {
		return nil, fmt.Errorf("version %s not found in device manifest", ver)
	}
	t := resolveTriple(v, storage)
	if t.imagePath == "" && t.zstPath == "" {
		return nil, fmt.Errorf("version %s has no %s image artifact", ver, storage)
	}

	info := &imageInfo{
		DownloadURL: gcsBaseURL + "/" + t.imagePath,
		ImageSize:   t.imageSize,
		Version:     ver,
		Storage:     storage,
	}
	// Select checksums from the same artifact triple, including legacy fallback.
	info.Checksum, info.ZstChecksum = v.Checksum, v.ZstChecksum
	if storage == "nvme" && v.NVMEPath != "" {
		info.Checksum, info.ZstChecksum = v.NVMEChecksum, v.NVMEZstChecksum
	} else if storage == "sd" && v.SDPath != "" {
		info.Checksum, info.ZstChecksum = v.SDChecksum, v.SDZstChecksum
	}
	if t.bmapPath != "" {
		info.BmapURL = gcsBaseURL + "/" + t.bmapPath
	}
	if t.zstPath != "" {
		info.ZstURL = gcsBaseURL + "/" + t.zstPath
	}
	return info, nil
}

// getRootfsOnlyImageInfo resolves only the explicitly-named raw artifact on a
// recovery-first release. It never consults Path/NVMEPath/SDPath, preventing a
// new CLI from silently mixing a recovery-era rootfs with stale boot firmware.
func getRootfsOnlyImageInfo(dm *deviceManifest, ver, storage string) (*imageInfo, error) {
	v, ok := dm.Versions[ver]
	if !ok {
		return nil, fmt.Errorf("version %s not found in device manifest", ver)
	}
	var t imageTriple
	switch storage {
	case "nvme":
		t = imageTriple{v.NVMERootfsOnlyPath, v.NVMERootfsOnlySizeBytes, v.NVMERootfsOnlyBmapPath, v.NVMERootfsOnlyZstPath}
	case "sd":
		t = imageTriple{v.SDRootfsOnlyPath, v.SDRootfsOnlySizeBytes, v.SDRootfsOnlyBmapPath, v.SDRootfsOnlyZstPath}
	default:
		return nil, fmt.Errorf("rootfs-only imaging does not support storage %q", storage)
	}
	if t.imagePath == "" && t.zstPath == "" {
		return nil, fmt.Errorf("version %s has no %s rootfs-only artifact", ver, storage)
	}
	info := &imageInfo{Version: ver, ImageSize: t.imageSize, Storage: storage}
	if t.imagePath != "" {
		info.DownloadURL = gcsBaseURL + "/" + t.imagePath
	}
	if t.bmapPath != "" {
		info.BmapURL = gcsBaseURL + "/" + t.bmapPath
	}
	if t.zstPath != "" {
		info.ZstURL = gcsBaseURL + "/" + t.zstPath
	}
	return info, nil
}

func resolveTriple(v deviceVersion, storage string) imageTriple {
	switch storage {
	case "nvme":
		if v.NVMEPath != "" {
			return imageTriple{v.NVMEPath, v.NVMESizeBytes, v.NVMEBmapPath, v.NVMEZstPath}
		}
	case "sd":
		if v.SDPath != "" {
			return imageTriple{v.SDPath, v.SDSizeBytes, v.SDBmapPath, v.SDZstPath}
		}
	}
	// Legacy fallback.
	return imageTriple{v.Path, v.SizeBytes, v.BmapPath, v.ZstPath}
}

func getOTAUpdateURL(dm *deviceManifest, ver string, storageMedium string) (string, error) {
	v, ok := dm.Versions[ver]
	if !ok {
		return "", fmt.Errorf("version %s not found in device manifest", ver)
	}
	if storageMedium == "nvme" && v.NVMEOTAUpdatePath != "" {
		return gcsBaseURL + "/" + v.NVMEOTAUpdatePath, nil
	}
	if v.OTAUpdatePath == "" {
		return "", fmt.Errorf("version %s has no OTA update artifact", ver)
	}
	return gcsBaseURL + "/" + v.OTAUpdatePath, nil
}

// getLatestOTAInfoForDeviceType fetches the manifest and returns the OTA artifact
// URL and version tag for the given manifest device key. storageMedium (e.g. "nvme")
// selects the variant-specific artifact when the manifest provides one. nightly
// selects the latest prerelease version instead of the latest stable version.
func getLatestOTAInfoForDeviceType(deviceType string, storageMedium string, nightly bool) (artifactURL, latestVersion string, err error) {
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

	u, err := getOTAUpdateURL(dm, latest, storageMedium)
	if err != nil {
		return "", "", err
	}
	return u, latest, nil
}

// getPROTAInfoForDeviceType mirrors getLatestOTAInfoForDeviceType but resolves
// against a PR build's manifest (fetchPRMainManifest) instead of the master
// manifest. It selects the PR device's Latest version, falling back to
// LatestNightly when the PR only published a nightly-style build.
func getPROTAInfoForDeviceType(pr int, deviceType, storageMedium string) (artifactURL, version string, err error) {
	main, err := fetchPRMainManifest(pr)
	if err != nil {
		return "", "", err
	}
	dev, ok := main.Devices[deviceType]
	if !ok || dev.ManifestPath == "" {
		return "", "", fmt.Errorf("device type %q not built by PR %d", deviceType, pr)
	}
	dm, err := fetchDeviceManifest(dev.ManifestPath)
	if err != nil {
		return "", "", err
	}
	ver := prDeviceVersion(dev)
	if ver == "" {
		return "", "", fmt.Errorf("no version for %q in PR %d", deviceType, pr)
	}
	u, err := getOTAUpdateURL(dm, ver, storageMedium)
	if err != nil {
		return "", "", err
	}
	return u, ver, nil
}

// thorDeviceType is the manifest key / --device-type for the AGX Thor.
const thorDeviceType = "jetson-agx-thor"

// orinDeviceType is the manifest key / --device-type for the AGX Orin. Unlike
// Thor it supports two install modes: the NVMe disk image (regular drive
// flow) and the onboard-eMMC USB-recovery flash (--storage emmc).
const orinDeviceType = "jetson-agx-orin"

const orinNanoDeviceType = "jetson-orin-nano"

func isT234RecoveryDevice(deviceType string) bool {
	return deviceType == orinDeviceType || deviceType == orinNanoDeviceType
}

type recoveryFlashpackInfo struct {
	URL, Checksum string
	SizeBytes     int64
	Version       string
	Device        string
	Storage       string
}

func getRecoveryFlashpackInfo(dm *deviceManifest, device, version, storage string) (*recoveryFlashpackInfo, error) {
	v, ok := dm.Versions[version]
	if !ok {
		return nil, fmt.Errorf("version %s not found for %s", version, device)
	}
	var path, checksum string
	var size int64
	switch storage {
	case "nvme":
		path, checksum, size = v.NVMEFlashpackPath, v.NVMEFlashpackChecksum, v.NVMEFlashpackSizeBytes
	case "emmc":
		path, checksum, size = v.EMMCFlashpackPath, v.EMMCFlashpackChecksum, v.EMMCFlashpackSizeBytes
	default:
		return nil, fmt.Errorf("recovery flashpacks do not support storage %q", storage)
	}
	if path == "" || checksum == "" || size <= 0 {
		return nil, fmt.Errorf("version %s has no complete %s recovery flashpack", version, storage)
	}
	return &recoveryFlashpackInfo{URL: gcsBaseURL + "/" + path, Checksum: checksum, SizeBytes: size, Version: version, Device: device, Storage: storage}, nil
}

// thorFlashpackInfo is the resolved flashpack download for a Thor version.
type thorFlashpackInfo struct {
	URL       string
	Checksum  string
	SizeBytes int64
	Version   string
}

// getThorFlashpackInfo fetches the jetson-agx-thor manifest and returns the flashpack
// artifact for version (or the latest stable / nightly when version is ""). When
// pr > 0 it resolves against the per-PR manifest (pr/<N>/) written by the
// wendyos-builder publish-pr job instead of the released master manifest; the
// flashpack path there is already pr-prefixed, so the download URL is correct.
func getThorFlashpackInfo(version string, nightly bool, pr int) (*thorFlashpackInfo, error) {
	var main *mainManifest
	var err error
	if pr > 0 {
		main, err = fetchPRMainManifest(pr)
	} else {
		main, err = fetchMainManifest()
	}
	if err != nil {
		return nil, fmt.Errorf("fetching manifest: %w", err)
	}
	dev, ok := main.Devices[thorDeviceType]
	if !ok || dev.ManifestPath == "" {
		if pr > 0 {
			return nil, fmt.Errorf("%s not built by PR %d", thorDeviceType, pr)
		}
		return nil, fmt.Errorf("%s not found in manifest", thorDeviceType)
	}
	dm, err := fetchDeviceManifest(dev.ManifestPath)
	if err != nil {
		return nil, fmt.Errorf("fetching device manifest: %w", err)
	}
	if version == "" {
		if pr > 0 {
			version = prDeviceVersion(dev)
		} else {
			version = dev.Latest
			if nightly && dev.LatestNightly != "" {
				version = dev.LatestNightly
			}
		}
	}
	if version == "" {
		return nil, fmt.Errorf("no version available for %s", thorDeviceType)
	}
	v, ok := dm.Versions[version]
	if !ok {
		return nil, fmt.Errorf("version %s not found for %s", version, thorDeviceType)
	}
	if v.FlashpackPath == "" {
		return nil, fmt.Errorf("version %s has no flashpack artifact in the manifest", version)
	}
	return &thorFlashpackInfo{
		URL:       gcsBaseURL + "/" + v.FlashpackPath,
		Checksum:  v.FlashpackChecksum,
		SizeBytes: v.FlashpackSizeBytes,
		Version:   version,
	}, nil
}

// firmwareManifest contains version info for a specific chip.
type firmwareManifest struct {
	FirmwareID string                         `json:"firmware_id"`
	Versions   map[string]firmwareVersionInfo `json:"versions"`
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
