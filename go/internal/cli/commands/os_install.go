//go:build darwin || linux || windows

package commands

import (
	"archive/zip"
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/tegraflash"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/internal/shared/version"
	"github.com/wendylabsinc/wendy/internal/shared/wendyconf"
)

type preEnrollMode int

const (
	preEnrollAuto   preEnrollMode = iota // prompt if interactive terminal + auth session exists
	preEnrollForced                      // --pre-enroll explicitly set to true
	preEnrollSkip                        // --pre-enroll explicitly set to false
)

func newOSInstallCmd() *cobra.Command {
	var nightly bool
	var force bool
	var yesOverwriteInternal bool
	var preEnroll bool
	var deviceType string
	var versionFlag string
	var driveFlag string
	var storageFlag string
	var tegraflashXML string
	var tegraflashSkipLarger int64
	var wifiSSID string
	var wifiPassword string
	var wifiEntries []string
	var noWifi bool
	var deviceName string

	cmd := &cobra.Command{
		Use:   "install [image] [drive]",
		Short: "Install WendyOS or Wendy Lite firmware on a device",
		Long: `Interactively select a supported device, download the latest OS image or firmware, and write it to the target.

When called with positional arguments, skips interactive prompts:
  wendy os install <image-path> <drive-id> --force

When called with manifest-backed flags, installs a specific version:
  wendy os install --device-type raspberry-pi-5 --version 0.10.4 --drive /dev/disk4 --force

Pre-seed multiple WiFi networks (repeatable, highest-priority first):
  wendy os install --device-type raspberry-pi-5 --drive /dev/disk4 --force \
    --wifi "ssid=Home,password=hunter2,priority=100" \
    --wifi "ssid=Office,password=corp,priority=50" \
    --wifi "ssid=Cafe,hidden=true"

Flash onboard Jetson eMMC over USB recovery mode:
  wendy os install --device-type jetson-agx-orin --storage emmc --nightly --force

Flags can be provided progressively — omitted values trigger interactive pickers.`,
		Args: func(cmd *cobra.Command, args []string) error {
			switch len(args) {
			case 0, 2:
				return nil
			case 1:
				return fmt.Errorf("positional arguments must be provided as [image] [drive]; got 1 argument")
			default:
				return cobra.MaximumNArgs(2)(cmd, args)
			}
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// Positional direct-install mode is incompatible with manifest-backed flags.
			if len(args) > 0 && (deviceType != "" || versionFlag != "" || driveFlag != "" || storageFlag != "" || tegraflashXML != "" || cmd.Flags().Changed("tegraflash-skip-larger") || wifiSSID != "" || wifiPassword != "" || len(wifiEntries) > 0 || noWifi || deviceName != "") {
				return fmt.Errorf("positional [image] [drive] arguments cannot be combined with --device-type, --version, --drive, --storage, --tegraflash-xml, --tegraflash-skip-larger, --wifi-ssid, --wifi-password, --wifi, --no-wifi, or --device-name")
			}
			if nightly && versionFlag != "" {
				return fmt.Errorf("--nightly and --version are mutually exclusive")
			}

			opts := wifiCLIOptions{
				SSID:     wifiSSID,
				Password: wifiPassword,
				Entries:  wifiEntries,
				NoWifi:   noWifi,
			}

			if len(args) == 2 {
				return runOSInstallDirect(args[0], args[1], force, yesOverwriteInternal)
			}
			mode := preEnrollAuto
			if cmd.Flags().Changed("pre-enroll") {
				if preEnroll {
					mode = preEnrollForced
				} else {
					mode = preEnrollSkip
				}
			}
			return runOSInstall(cmd.Context(), nightly, deviceType, versionFlag, driveFlag, storageFlag, tegraflashXML, tegraflashSkipLarger, cmd.Flags().Changed("tegraflash-skip-larger"), force, yesOverwriteInternal, opts, deviceName, mode)
		},
	}

	cmd.Flags().BoolVar(&nightly, "nightly", false, "Use nightly/prerelease builds")
	cmd.Flags().BoolVar(&force, "force", false, "Skip confirmation prompt")
	cmd.Flags().BoolVar(&yesOverwriteInternal, "yes-overwrite-internal", false, "Required to wipe an internal (non-removable) drive in non-interactive mode")
	cmd.Flags().StringVar(&deviceType, "device-type", "", "Device type from manifest (e.g. raspberry-pi-5)")
	cmd.Flags().StringVar(&versionFlag, "version", "", "WendyOS version to install (interactive if omitted)")
	cmd.Flags().StringVar(&driveFlag, "drive", "", "Target drive path (e.g. /dev/disk4)")
	cmd.Flags().StringVar(&storageFlag, "storage", "", "Install target: auto, removable, emmc, or recovery")
	cmd.Flags().StringVar(&tegraflashXML, "tegraflash-xml", "", "Override tegraflash partition XML basename for Jetson USB recovery installs")
	cmd.Flags().Int64Var(&tegraflashSkipLarger, "tegraflash-skip-larger", tegraflash.DefaultSkipLarger, "Skip tegraflash partition files larger than this many bytes for recovery/QSPI installs (0 = write all)")
	cmd.Flags().StringVar(&wifiSSID, "wifi-ssid", "", "Pre-configure a single WiFi SSID on first boot (shortcut for --wifi)")
	cmd.Flags().StringVar(&wifiPassword, "wifi-password", "", "Password for --wifi-ssid")
	cmd.Flags().StringArrayVar(&wifiEntries, "wifi", nil, "Pre-configure a WiFi network. Repeatable. Format: ssid=X[,password=Y][,priority=N][,hidden=true][,security=wpa2]")
	cmd.Flags().BoolVar(&noWifi, "no-wifi", false, "Skip WiFi setup entirely (no interactive prompt, no pre-seeded networks)")
	cmd.Flags().StringVar(&deviceName, "device-name", "", "Set device name on first boot (e.g. brave-dolphin)")
	cmd.Flags().BoolVar(&preEnroll, "pre-enroll", false, "Pre-enroll this device with Wendy Cloud during imaging (requires 'wendy auth login')")

	return cmd
}

// runOSInstallDirect writes a local image file to the specified drive without interactive prompts.
func runOSInstallDirect(imagePath string, driveID string, force bool, yesOverwriteInternal bool) error {
	// Verify the image file exists.
	if _, err := os.Stat(imagePath); err != nil {
		return fmt.Errorf("image file: %w", err)
	}

	// Authenticate elevation before any disk-listing or write work. On
	// Windows this offers a UAC re-launch when the current process isn't
	// elevated; on Unix it pre-caches the sudo timestamp.
	if err := preAuthElevation(); err != nil {
		return err
	}

	// Find the target drive.
	drives, err := listAllDrives()
	if err != nil {
		return fmt.Errorf("listing drives: %w", err)
	}

	var targetDrive *drive
	for _, d := range drives {
		if d.DevicePath == driveID {
			targetDrive = &d
			break
		}
	}
	if targetDrive == nil {
		return fmt.Errorf("drive %s not found", driveID)
	}

	if err := confirmOverwriteInternalDrive(*targetDrive, force, yesOverwriteInternal); err != nil {
		return err
	}

	if !force {
		confirmed, err := tui.Confirm(fmt.Sprintf("Writing will ERASE ALL DATA on %s (%s). Continue?", targetDrive.Name, targetDrive.DevicePath))
		if err != nil {
			return err
		}
		if !confirmed {
			cliNotice("Cancelled.")
			return nil
		}
	}

	imgPath := imagePath
	if strings.HasSuffix(strings.ToLower(imagePath), ".zip") {
		extracted, err := extractImageFromZipWithProgress(imagePath)
		if err != nil {
			return fmt.Errorf("extracting image: %w", err)
		}
		defer os.Remove(extracted)
		imgPath = extracted
	}

	cliLogln("Writing image to %s...", targetDrive.DevicePath)
	cliNotice("%s", elevationHint())
	if err := writeImageToDisk(imgPath, *targetDrive, nil); err != nil {
		return fmt.Errorf("writing image: %w", err)
	}

	cliSuccess("\nSuccessfully installed image on %s.", targetDrive.Name)
	return nil
}

// pickerDevice is a unified entry for the device selection picker.
type pickerDevice struct {
	Name       string
	Version    string // display version (e.g. "0.10.5 (nightly)")
	RawVersion string // exact version key for manifest lookup
	Category   string // e.g. "Linux" or "Wendy Lite"
	IsESP32    bool
	ESP32Chip  string          // e.g. "esp32c6", "esp32c5"
	Manifest   *deviceManifest // cached manifest for Linux devices
}

// pickLinuxDevice fetches available Linux devices from the manifest and presents
// an interactive picker. Returns the selected device key and its deviceInfo.
func pickLinuxDevice() (string, deviceInfo, error) {
	cliLogln("Fetching available devices...")

	devices, err := getAvailableDevices()
	if err != nil {
		log.Printf("WARNING: could not fetch Linux device manifest: %v", err)
	}

	var items []tui.PickerItem
	deviceMap := make(map[string]deviceInfo)

	for _, dev := range devices {
		if dev.LatestVersion == "" {
			continue
		}
		deviceMap[dev.Key] = dev
		items = append(items, tui.PickerItem{
			Name:        dev.Name,
			Description: fmt.Sprintf("(latest: %s)", dev.LatestVersion),
			Value:       dev.Key,
		})
	}

	if len(items) == 0 {
		return "", deviceInfo{}, fmt.Errorf("no devices available")
	}

	fmt.Println()
	key, err := pickFromItems("Select a device", items)
	if err != nil {
		return "", deviceInfo{}, err
	}
	return key, deviceMap[key], nil
}

func runOSInstall(ctx context.Context, nightly bool, flagDeviceType, flagVersion, flagDrive, flagStorage, tegraflashXML string, tegraflashSkipLarger int64, tegraflashSkipLargerChanged bool, force bool, yesOverwriteInternal bool, wifi wifiCLIOptions, deviceName string, mode preEnrollMode) error {
	cliLogln("Fetching available devices...")

	// Fetch Linux devices from GCS manifest.
	linuxDevices, err := getAvailableDevices()
	if err != nil {
		log.Printf("WARNING: could not fetch Linux device manifest: %v", err)
	}

	// Build picker items.
	var items []tui.PickerItem
	deviceMap := make(map[string]pickerDevice)

	for _, dev := range linuxDevices {
		rawVersion := dev.LatestVersion
		displayVersion := "(" + rawVersion + ")"
		if nightly && dev.NightlyVersion != "" {
			rawVersion = dev.NightlyVersion
			displayVersion = "(" + rawVersion + ", nightly)"
		}
		if rawVersion == "" {
			continue // skip devices with no available version
		}

		pd := pickerDevice{
			Name:       dev.Name,
			Version:    displayVersion,
			RawVersion: rawVersion,
			Category:   "Linux",
			Manifest:   dev.Manifest,
		}
		deviceMap[dev.Key] = pd

		items = append(items, tui.PickerItem{
			Name:        dev.Name,
			Description: fmt.Sprintf("%s    %s", displayVersion, pd.Category),
			Value:       dev.Key,
		})
	}

	// When --device-type is not provided, also offer ESP32 entries in the picker.
	if flagDeviceType == "" {
		espVersion := "(latest)"
		for _, esp := range []struct {
			key, name, chip string
		}{
			{"esp32-c6", "ESP32-C6", "esp32c6"},
			{"esp32-c5", "ESP32-C5", "esp32c5"},
		} {
			deviceMap[esp.key] = pickerDevice{
				Name:      esp.name,
				Version:   espVersion,
				Category:  "Wendy Lite",
				IsESP32:   true,
				ESP32Chip: esp.chip,
			}
			items = append(items, tui.PickerItem{
				Name:        esp.name,
				Description: fmt.Sprintf("%s    %s", espVersion, "Wendy Lite"),
				Value:       esp.key,
			})
		}
	}

	// Resolve device — use flag or interactive picker.
	var selected string
	if flagDeviceType != "" {
		// --device-type is only supported for Linux devices, not ESP32/Wendy Lite.
		if flagDeviceType == "esp32-c6" || flagDeviceType == "esp32-c5" {
			return fmt.Errorf("--device-type does not support ESP32 targets; use the interactive picker for Wendy Lite devices")
		}
		if _, ok := deviceMap[flagDeviceType]; !ok {
			var available []string
			for k, d := range deviceMap {
				if !d.IsESP32 {
					available = append(available, k)
				}
			}
			sort.Strings(available)
			return fmt.Errorf("device type %q not found in manifest; available: %s", flagDeviceType, strings.Join(available, ", "))
		}
		selected = flagDeviceType
	} else {
		if len(items) == 0 {
			return fmt.Errorf("no devices available")
		}

		fmt.Println()
		selected, err = pickFromItems("Select a device", items)
		if err != nil {
			return err
		}
	}

	device := deviceMap[selected]

	if device.IsESP32 {
		return installESP32Firmware(ctx, nightly, device.ESP32Chip)
	}
	return installLinuxImage(ctx, selected, device, nightly, flagVersion, flagDrive, flagStorage, tegraflashXML, tegraflashSkipLarger, tegraflashSkipLargerChanged, force, yesOverwriteInternal, wifi, deviceName, mode)
}

// installLinuxImage handles the Linux device path: pick version → pick drive → download → write.
// nightly, flagVersion, flagDrive, and force allow skipping the corresponding interactive prompts.
func installLinuxImage(ctx context.Context, deviceKey string, device pickerDevice, nightly bool, flagVersion, flagDrive, flagStorage, tegraflashXML string, tegraflashSkipLarger int64, tegraflashSkipLargerChanged bool, force bool, yesOverwriteInternal bool, wifi wifiCLIOptions, deviceName string, mode preEnrollMode) error {
	// Step 1: Resolve version — use flag, nightly shortcut, or pick interactively.
	selectedVersion := device.RawVersion // default: latest (or nightly if --nightly)
	if flagVersion != "" {
		// Validate the requested version exists in the manifest (storage-agnostic check).
		if _, err := getImageInfo(device.Manifest, flagVersion); err != nil {
			return fmt.Errorf("version %q not found for %s", flagVersion, device.Name)
		}
		selectedVersion = flagVersion
	}

	storage, err := resolveInstallStorage(device.Manifest, selectedVersion, flagStorage, flagDrive)
	if err != nil {
		return err
	}
	if storage == installStorageEMMC || storage == installStorageRecovery {
		return installLinuxTegraflash(ctx, deviceKey, device, selectedVersion, storage, tegraflashXML, tegraflashSkipLarger, tegraflashSkipLargerChanged, force, wifi, deviceName, mode)
	}
	if tegraflashXML != "" || tegraflashSkipLargerChanged {
		return fmt.Errorf("--tegraflash-xml and --tegraflash-skip-larger require --storage emmc or --storage recovery")
	}

	// Authenticate elevation up front so we don't pay for the multi-hundred-MB
	// image download just to discover the user can't write to a raw disk. On
	// Windows this offers a UAC re-launch when not elevated; on Unix it
	// pre-caches the sudo timestamp.
	if err := preAuthElevation(); err != nil {
		return err
	}

	// Step 2: Resolve target drive — use flag or interactive picker.
	var targetDrive drive
	if flagDrive != "" {
		drives, err := listAllDrives()
		if err != nil {
			return fmt.Errorf("listing drives: %w", err)
		}
		var found bool
		for _, d := range drives {
			if d.DevicePath == flagDrive {
				targetDrive = d
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("drive %s not found", flagDrive)
		}
	} else {
		fmt.Println()
		selectedDrive, err := pickExternalDrive(ctx)
		if err != nil {
			return err
		}
		targetDrive = selectedDrive
	}

	// Step 3: Confirm destructive write (unless --force).
	if err := confirmOverwriteInternalDrive(targetDrive, force, yesOverwriteInternal); err != nil {
		return err
	}
	if !force {
		fmt.Println()
		confirmed, err := tui.Confirm(fmt.Sprintf("Writing will ERASE ALL DATA on %s (%s). Continue?", targetDrive.Name, targetDrive.DevicePath))
		if err != nil {
			return err
		}
		if !confirmed {
			cliNotice("Cancelled.")
			return nil
		}
	}

	provCreds, err := resolveWiFiCredentialsList(wifi)
	if err != nil {
		return err
	}

	provDeviceName, err := resolveDeviceName(deviceName)
	if err != nil {
		return err
	}

	// Step 4: Resolve image storage type.
	// For devices that publish both an NVMe and an SD card image, pick the one
	// that matches the selected drive's storage medium. When the medium cannot
	// be detected automatically, ask the user.
	selectedStorage := StorageUnknown
	if hasMultipleStorages(device.Manifest, selectedVersion) {
		selectedStorage = targetDrive.StorageType
		if selectedStorage == StorageUnknown {
			v := device.Manifest.Versions[selectedVersion]
			var storageItems []tui.PickerItem
			if v.NVMEPath != "" {
				storageItems = append(storageItems, tui.PickerItem{
					Name:        "NVMe",
					Description: "drive connected via USB enclosure",
					Value:       string(StorageNVMe),
				})
			}
			if v.SDCardPath != "" {
				storageItems = append(storageItems, tui.PickerItem{
					Name:        "SD card",
					Description: "internal slot or USB card reader",
					Value:       string(StorageSD),
				})
			}
			if v.EMMCPath != "" {
				storageItems = append(storageItems, tui.PickerItem{
					Name:        "eMMC",
					Description: "onboard flash via USB eMMC reader",
					Value:       string(StorageEMMC),
				})
			}
			fmt.Println()
			picked, pickErr := pickFromItems("Select target storage type", storageItems)
			if pickErr != nil {
				return pickErr
			}
			selectedStorage = StorageType(picked)
		}
	}

	// Resolve pre-enrollment — must happen before provisionConfigPartition because
	// the config partition is mounted and unmounted inside that call.
	var provisioningJSON []byte
	switch mode {
	case preEnrollForced:
		auth, authErr := pickAuthEntry("")
		if authErr != nil {
			return fmt.Errorf("--pre-enroll: %w", authErr)
		}
		cliLogln("Pre-enrolling device with Wendy Cloud (org: %d)...", auth.Certificates[0].OrganizationID)
		js, enrollErr := preEnrollDevice(ctx, auth, provDeviceName, nil)
		if enrollErr != nil {
			cliNotice("Warning: pre-enrollment failed: %v", enrollErr)
			cliNotice("The device will boot unenrolled. Run 'wendy device enroll' after first boot.")
		} else {
			provisioningJSON = js
			cliLogln("Device pre-enrolled. It will be secure from first boot.")
		}
	case preEnrollAuto:
		if isInteractiveTerminal() {
			cfg, loadErr := config.Load()
			if loadErr == nil && len(cfg.Auth) > 0 {
				ok, _ := tui.ConfirmDefaultYes("Pre-enroll this device with Wendy Cloud?")
				if ok {
					auth, authErr := pickAuthEntry("")
					if authErr != nil {
						cliNotice("Warning: could not resolve auth for pre-enrollment: %v", authErr)
					} else {
						cliLogln("Pre-enrolling device with Wendy Cloud (org: %d)...", auth.Certificates[0].OrganizationID)
						js, enrollErr := preEnrollDevice(ctx, auth, provDeviceName, nil)
						if enrollErr != nil {
							cliNotice("Warning: pre-enrollment failed: %v", enrollErr)
							cliNotice("The device will boot unenrolled. Run 'wendy device enroll' after first boot.")
						} else {
							provisioningJSON = js
							cliLogln("Device pre-enrolled. It will be secure from first boot.")
						}
					}
				}
			}
		}
	}

	cliLogln("\nPreparing %s %s image...", device.Name, selectedVersion)
	imgInfo, err := getImageInfoForStorage(device.Manifest, selectedVersion, selectedStorage)
	if err != nil {
		return fmt.Errorf("getting image info: %w", err)
	}

	imgPath, err := resolveOSImage(deviceKey, imgInfo)
	if err != nil {
		return fmt.Errorf("opening OS image: %w", err)
	}
	if strings.HasSuffix(strings.ToLower(imgPath), ".zip") {
		extracted, err := extractImageFromZipWithProgress(imgPath)
		if err != nil {
			return fmt.Errorf("extracting image: %w", err)
		}
		defer os.Remove(extracted)
		imgPath = extracted
	}
	var totalSize int64
	if info, err := os.Stat(imgPath); err == nil {
		totalSize = info.Size()
	}

	// Step 6: Write image to drive with progress bar.
	cliLogln("Writing image to %s...", targetDrive.DevicePath)
	writeProg := tui.NewProgress(fmt.Sprintf("Writing to %s...", targetDrive.DevicePath))
	wp := tea.NewProgram(writeProg)

	go func() {
		writeErr := writeImageToDisk(imgPath, targetDrive, func(written int64) {
			if totalSize > 0 {
				wp.Send(tui.ProgressUpdateMsg{
					Percent: float64(written) / float64(totalSize),
					Written: written,
					Total:   totalSize,
				})
			}
		})
		wp.Send(tui.ProgressDoneMsg{Err: writeErr})
	}()

	writeFinal, err := wp.Run()
	if err != nil {
		return fmt.Errorf("progress TUI: %w", err)
	}

	writeModel := writeFinal.(tui.ProgressModel)
	if writeModel.Err() != nil {
		return fmt.Errorf("writing image: %w", writeModel.Err())
	}

	hasProvisioningData := provisioningRequired(provCreds, provDeviceName, provisioningJSON)

	if !configPartitionSupported {
		// writeConfigPartition is not supported on this platform. Skip the
		// agent download — paying 5–30s of network for a guaranteed-skipped
		// step is the bug from WDY-1118.
		if hasProvisioningData {
			ejectDisk(targetDrive)
			return fmt.Errorf("the OS image was written to %s, but --wifi, --device-name, and --pre-enroll cannot be applied on this platform: writing to the device's config partition is not supported here. Re-run on a platform that supports config-partition provisioning to apply provisioning, or omit those flags to image without provisioning", targetDrive.Name)
		}
		cliLogln("\nNote: config-partition provisioning is not yet supported on this platform; skipping. The device will run the agent baked into the image and fetch updates after first boot.")
	} else {
		cliLogln("\nWriting provisioning data to config partition...")
		if err := provisionConfigPartition(targetDrive, provCreds, provDeviceName, provisioningJSON); err != nil {
			if hasProvisioningData {
				ejectDisk(targetDrive)
				return fmt.Errorf("could not write provisioning data to config partition (--wifi / --device-name / --pre-enroll were requested but not applied): %w", err)
			}
			cliNotice("Warning: could not write config partition: %v", err)
			cliNotice("Device will boot but agent auto-update will not be pre-configured.")
		}
	}

	ejectDisk(targetDrive)

	cliSuccess("\nSuccessfully installed %s %s on %s.", device.Name, imgInfo.Version, targetDrive.Name)
	cliSuccess("You can now insert the drive into your device and power it on.")
	return nil
}

// pickManifestVersion presents an interactive picker for available versions in a
// device manifest, sorted newest-first using semantic version comparison. It
// marks "latest" and "nightly" versions in the picker description.
// This is shared by both os install and os download flows.
func pickManifestVersion(title string, manifest *deviceManifest) (string, error) {
	if manifest == nil || len(manifest.Versions) == 0 {
		return "", fmt.Errorf("no versions available")
	}

	var versionKeys []string
	for v := range manifest.Versions {
		versionKeys = append(versionKeys, v)
	}
	sort.Slice(versionKeys, func(i, j int) bool {
		return version.CompareVersions(versionKeys[i], versionKeys[j]) > 0
	})

	var items []tui.PickerItem
	for _, v := range versionKeys {
		ver := manifest.Versions[v]
		desc := ""
		if ver.IsLatest {
			desc = "latest"
		} else if ver.IsNightly {
			desc = "nightly"
		}
		items = append(items, tui.PickerItem{
			Name:        v,
			Description: desc,
			Value:       v,
		})
	}

	fmt.Println()
	return pickFromItems(title, items)
}

const externalDrivePickerRefreshInterval = 2 * time.Second

func pickExternalDrive(ctx context.Context) (drive, error) {
	item, err := pickRefreshingItem(ctx, "Select target drive", externalDrivePickerRefreshInterval, func(context.Context) ([]tui.PickerItem, error) {
		drives, err := listExternalDrives()
		if err != nil {
			return nil, fmt.Errorf("listing drives: %w", err)
		}
		return externalDrivePickerItems(drives), nil
	})
	if err != nil {
		return drive{}, err
	}
	selected, ok := item.Value.(drive)
	if !ok {
		return drive{}, fmt.Errorf("invalid drive selection")
	}
	return selected, nil
}

func externalDrivePickerItems(drives []drive) []tui.PickerItem {
	items := make([]tui.PickerItem, 0, len(drives))
	for _, d := range drives {
		desc := d.DevicePath
		if d.Size != "" {
			desc += "  " + d.Size
		}
		items = append(items, tui.PickerItem{
			Name:        d.Name,
			Description: desc,
			DedupKey:    d.DevicePath,
			Value:       d,
		})
	}
	return items
}

// throttledProgress returns a sender that forwards ProgressUpdateMsg to p at
// most once per minInterval. Bubble Tea ingests every Send into a buffered
// channel and SetPercent kicks off a cascade of animation FrameMsgs, so a
// busy I/O loop posting updates per chunk can pile up enough work to slow
// the I/O loop itself. The terminal can't usefully render faster than the
// throttle rate anyway, and a trailing ProgressDoneMsg always renders 100%.
func throttledProgress(p *tea.Program, minInterval time.Duration) func(written, total int64) {
	var lastNanos atomic.Int64
	return func(written, total int64) {
		if total <= 0 {
			return
		}
		now := time.Now()
		prev := lastNanos.Load()
		if now.UnixNano()-prev < minInterval.Nanoseconds() {
			return
		}
		if !lastNanos.CompareAndSwap(prev, now.UnixNano()) {
			return
		}
		p.Send(tui.ProgressUpdateMsg{
			Percent: float64(written) / float64(total),
			Written: written,
			Total:   total,
		})
	}
}

const parallelDownloadWorkers = 8

// downloadChunk fetches the byte range [start, end] from url, writes it to dst
// at the correct offset via WriteAt, and atomically increments *downloaded.
func downloadChunk(client *http.Client, url string, start, end int64, dst *os.File, downloaded *int64, total int64, sendProgress func(int64, int64)) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("range request %d-%d: %w", start, end, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("range request %d-%d: expected 206, got %d", start, end, resp.StatusCode)
	}

	buf := make([]byte, 1*1024*1024)
	offset := start
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, writeErr := dst.WriteAt(buf[:n], offset); writeErr != nil {
				return fmt.Errorf("writing at offset %d: %w", offset, writeErr)
			}
			offset += int64(n)
			newTotal := atomic.AddInt64(downloaded, int64(n))
			sendProgress(newTotal, total)
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return fmt.Errorf("reading chunk %d-%d: %w", start, end, readErr)
		}
	}
}

// downloadParallel downloads url into dst using parallelDownloadWorkers concurrent
// range requests. dst must already be truncated to contentLength bytes.
func downloadParallel(client *http.Client, url string, contentLength int64, dst *os.File, sendProgress func(int64, int64)) error {
	chunkSize := (contentLength + parallelDownloadWorkers - 1) / parallelDownloadWorkers

	var wg sync.WaitGroup
	errCh := make(chan error, parallelDownloadWorkers)
	var downloaded int64

	for i := 0; i < parallelDownloadWorkers; i++ {
		start := int64(i) * chunkSize
		if start >= contentLength {
			break
		}
		end := start + chunkSize - 1
		if end >= contentLength {
			end = contentLength - 1
		}

		wg.Add(1)
		go func(start, end int64) {
			defer wg.Done()
			if err := downloadChunk(client, url, start, end, dst, &downloaded, contentLength, sendProgress); err != nil {
				errCh <- err
			}
		}(start, end)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		return err
	}
	return nil
}

// probeRangeSupport issues a HEAD request to check whether the server
// supports HTTP range requests. Returns the content length and true on
// success. Falls back to img.ImageSize if Content-Length is absent.
// Returns 0, false if ranges are unsupported or content length is unknown.
func probeRangeSupport(client *http.Client, img *imageInfo) (contentLength int64, ok bool) {
	resp, err := client.Head(img.DownloadURL)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, false
	}
	// Rejects both absent header and RFC 7233 "Accept-Ranges: none".
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		return 0, false
	}
	cl := resp.ContentLength
	if cl <= 0 && img.ImageSize > 0 {
		cl = img.ImageSize
	}
	if cl <= 0 {
		return 0, false
	}
	return cl, true
}

// downloadImage downloads an OS image to a temp file with a progress bar.
// If the server supports HTTP range requests, it downloads in parallel using
// parallelDownloadWorkers concurrent connections. Falls back to a single
// sequential stream otherwise.
func downloadImage(img *imageInfo) (string, error) {
	client := &http.Client{Timeout: 30 * time.Minute}

	// Write directly into the OS cache directory so we never land in /tmp
	// (which is often a size-limited tmpfs on Linux).
	cacheDir, err := osCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolving cache dir: %w", err)
	}
	tmpFile, err := os.CreateTemp(cacheDir, "wendyos-*.img")
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}

	prog := tui.NewProgress(fmt.Sprintf("Downloading %s...", img.Version))
	p := tea.NewProgram(prog)
	sendProgress := throttledProgress(p, 33*time.Millisecond)

	contentLength, supportsRanges := probeRangeSupport(client, img)

	if supportsRanges {
		if err := tmpFile.Truncate(contentLength); err != nil {
			tmpFile.Close()
			os.Remove(tmpFile.Name())
			return "", fmt.Errorf("pre-allocating: %w", err)
		}
		go func() {
			p.Send(tui.ProgressDoneMsg{Err: downloadParallel(client, img.DownloadURL, contentLength, tmpFile, sendProgress)})
		}()
	} else {
		go func() {
			resp, err := client.Get(img.DownloadURL)
			if err != nil {
				p.Send(tui.ProgressDoneMsg{Err: fmt.Errorf("downloading: %w", err)})
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				p.Send(tui.ProgressDoneMsg{Err: fmt.Errorf("download returned status %d", resp.StatusCode)})
				return
			}
			total := resp.ContentLength
			if img.ImageSize > 0 {
				total = img.ImageSize
			}
			buf := make([]byte, 1*1024*1024)
			var downloaded int64
			for {
				n, readErr := resp.Body.Read(buf)
				if n > 0 {
					if _, writeErr := tmpFile.Write(buf[:n]); writeErr != nil {
						p.Send(tui.ProgressDoneMsg{Err: writeErr})
						return
					}
					downloaded += int64(n)
					sendProgress(downloaded, total)
				}
				if readErr == io.EOF {
					p.Send(tui.ProgressDoneMsg{})
					return
				}
				if readErr != nil {
					p.Send(tui.ProgressDoneMsg{Err: readErr})
					return
				}
			}
		}()
	}

	finalModel, err := p.Run()
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("progress TUI: %w", err)
	}

	model := finalModel.(tui.ProgressModel)
	if model.Err() != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return "", model.Err()
	}

	tmpFile.Close()
	return tmpFile.Name(), nil
}

// extractImageFromZipWithProgress opens a zip archive and extracts the first OS
// image file (.img, .raw, or .wic) to a temp file, and displays a progress bar.
func extractImageFromZipWithProgress(zipPath string) (string, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("opening zip: %w", err)
	}
	defer r.Close()

	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(f.Name))
		if ext != ".img" && ext != ".raw" && ext != ".wic" {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			return "", fmt.Errorf("opening %s in zip: %w", f.Name, err)
		}
		defer rc.Close()

		// Write directly into the OS cache directory so we never land in
		// /tmp (which is often a size-limited tmpfs on Linux).
		cacheDir, err := osCacheDir()
		if err != nil {
			return "", fmt.Errorf("resolving cache dir: %w", err)
		}
		tmpFile, err := os.CreateTemp(cacheDir, "wendyos-*.img")
		if err != nil {
			return "", fmt.Errorf("creating temp file: %w", err)
		}

		totalSize := int64(f.UncompressedSize64)
		if totalSize == 0 {
			// Some zip writers don't populate UncompressedSize64;
			// fall back to FileInfo which may use the 32-bit field.
			totalSize = f.FileInfo().Size()
		}

		prog := tui.NewProgress("Extracting image...")
		p := tea.NewProgram(prog)

		sendProgress := throttledProgress(p, 33*time.Millisecond)
		go func() {
			// Brief pause so Bubble Tea can initialize the terminal
			// before we start sending updates. Without this, fast local
			// I/O can queue all messages before the TUI renders.
			time.Sleep(50 * time.Millisecond)
			buf := make([]byte, 1*1024*1024) // 1 MiB chunks for visible progress
			var extracted int64
			for {
				n, readErr := rc.Read(buf)
				if n > 0 {
					if _, writeErr := tmpFile.Write(buf[:n]); writeErr != nil {
						p.Send(tui.ProgressDoneMsg{Err: writeErr})
						return
					}
					extracted += int64(n)
					sendProgress(extracted, totalSize)
				}
				if readErr == io.EOF {
					p.Send(tui.ProgressDoneMsg{})
					return
				}
				if readErr != nil {
					p.Send(tui.ProgressDoneMsg{Err: readErr})
					return
				}
			}
		}()

		finalModel, err := p.Run()
		if err != nil {
			tmpFile.Close()
			os.Remove(tmpFile.Name())
			return "", fmt.Errorf("progress TUI: %w", err)
		}

		model := finalModel.(tui.ProgressModel)
		if model.Err() != nil {
			tmpFile.Close()
			os.Remove(tmpFile.Name())
			return "", model.Err()
		}

		tmpFile.Close()
		return tmpFile.Name(), nil
	}

	return "", fmt.Errorf("no .img, .raw, or .wic file found in zip archive")
}

// osCacheDir returns the OS image cache directory, e.g.
// ~/Library/Caches/wendy/os-images (macOS) or ~/.cache/wendy/os-images (Linux).
func osCacheDir() (string, error) {
	base, err := config.CacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "os-images")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating OS cache directory: %w", err)
	}
	return dir, nil
}

// osCachedImagePath returns the expected cache path for a device+version image.
type installStorage string

const (
	installStorageAuto      installStorage = "auto"
	installStorageRemovable installStorage = "removable"
	installStorageEMMC      installStorage = "emmc"
	installStorageRecovery  installStorage = "recovery"
)

func resolveInstallStorage(manifest *deviceManifest, version, flagStorage, flagDrive string) (installStorage, error) {
	storage, err := parseInstallStorage(flagStorage)
	if err != nil {
		return "", err
	}

	if flagDrive != "" {
		if storage == installStorageEMMC || storage == installStorageRecovery {
			return "", fmt.Errorf("--drive cannot be used with --storage %s; Jetson recovery flashing writes over USB", storage)
		}
		return installStorageRemovable, nil
	}

	if storage != installStorageAuto {
		return storage, nil
	}

	v, ok := manifest.Versions[version]
	if !ok {
		return "", fmt.Errorf("version %s not found in device manifest", version)
	}
	if !isInteractiveTerminal() || (v.EMMCPath == "" && v.RecoveryPath == "") {
		return installStorageRemovable, nil
	}

	var items []tui.PickerItem
	items = append(items, tui.PickerItem{
		Name:        "Removable drive",
		Description: "Write a raw image to an SD card, USB drive, or external NVMe",
		Value:       string(installStorageRemovable),
	})
	if v.EMMCPath != "" {
		items = append(items, tui.PickerItem{
			Name:        "Onboard eMMC",
			Description: "Flash over Jetson USB recovery mode",
			Value:       string(installStorageEMMC),
		})
	}
	if v.RecoveryPath != "" {
		items = append(items, tui.PickerItem{
			Name:        "Jetson recovery / QSPI",
			Description: "Flash boot firmware only over USB recovery mode",
			Value:       string(installStorageRecovery),
		})
	}

	fmt.Println()
	picked, err := pickFromItems("Select install target", items)
	if err != nil {
		return "", err
	}
	return installStorage(picked), nil
}

func parseInstallStorage(raw string) (installStorage, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "auto":
		return installStorageAuto, nil
	case "removable", "drive", "disk", "sd", "usb", "nvme", "external":
		return installStorageRemovable, nil
	case "emmc":
		return installStorageEMMC, nil
	case "recovery", "qspi", "tegraflash":
		return installStorageRecovery, nil
	default:
		return "", fmt.Errorf("invalid --storage %q (expected auto, removable, emmc, or recovery)", raw)
	}
}

func installLinuxTegraflash(ctx context.Context, deviceKey string, device pickerDevice, selectedVersion string, storage installStorage, tegraflashXML string, tegraflashSkipLarger int64, tegraflashSkipLargerChanged bool, force bool, wifi wifiCLIOptions, deviceName string, mode preEnrollMode) error {
	_ = ctx

	if wifi.hasProvisioningFlags() || deviceName != "" || mode == preEnrollForced {
		return fmt.Errorf("--storage %s uses Jetson USB recovery flashing and does not yet support --wifi, --wifi-ssid, --wifi-password, --device-name, or --pre-enroll", storage)
	}

	target := string(storage)
	imgInfo, err := getTegraflashInfo(device.Manifest, selectedVersion, target)
	if err != nil {
		return fmt.Errorf("getting tegraflash info: %w", err)
	}

	if !force {
		fmt.Println()
		action := "write boot firmware"
		if storage == installStorageEMMC {
			action = "ERASE and write onboard eMMC"
		}
		confirmed, err := tui.Confirm(fmt.Sprintf("Jetson USB recovery flashing will %s on %s. Continue?", action, device.Name))
		if err != nil {
			return err
		}
		if !confirmed {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	fmt.Printf("\nPreparing %s %s %s tegraflash bundle...\n", device.Name, selectedVersion, storage)
	bundlePath, err := resolveTegraflashBundle(deviceKey, target, imgInfo)
	if err != nil {
		return fmt.Errorf("resolving tegraflash bundle: %w", err)
	}

	skipLarger := tegraflashSkipLarger
	fullEMMC := storage == installStorageEMMC
	if fullEMMC && !tegraflashSkipLargerChanged {
		skipLarger = 0
	}

	if err := tegraflash.Flash(tegraflash.FlashOptions{
		BundlePath: bundlePath,
		XMLName:    tegraflashXML,
		FullEMMC:   fullEMMC,
		SkipLarger: skipLarger,
		Out:        os.Stdout,
	}); err != nil {
		return fmt.Errorf("tegraflash: %w", err)
	}

	fmt.Printf("\nSuccessfully flashed %s %s to %s.\n", device.Name, imgInfo.Version, storage)
	if storage == installStorageEMMC {
		fmt.Println("The device should reboot from onboard eMMC.")
	} else {
		fmt.Println("The device should reboot with updated Jetson recovery firmware.")
	}
	return nil
}

func osCachedTegraflashPath(deviceKey, target, version string) (string, error) {
	safeDevice := filepath.Base(deviceKey)
	safeTarget := filepath.Base(target)
	safeVersion := filepath.Base(version)
	if safeDevice != deviceKey || safeTarget != target || safeVersion != version ||
		strings.Contains(deviceKey, "..") || strings.Contains(target, "..") || strings.Contains(version, "..") {
		return "", fmt.Errorf("invalid device key, target, or version: %q / %q / %q", deviceKey, target, version)
	}

	dir, err := osCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("%s-%s-%s.tegraflash.tar.gz", safeDevice, safeVersion, safeTarget)), nil
}

func resolveTegraflashBundle(deviceKey, target string, img *imageInfo) (string, error) {
	cached, err := osCachedTegraflashPath(deviceKey, target, img.Version)
	if err != nil {
		return "", err
	}

	if info, statErr := os.Stat(cached); statErr == nil && info.Size() > 0 {
		fmt.Printf("Using cached tegraflash bundle (%s)\n", cached)
		return cached, nil
	}

	downloadPath, err := downloadImage(img)
	if err != nil {
		return "", fmt.Errorf("downloading tegraflash bundle: %w", err)
	}
	if err := os.Rename(downloadPath, cached); err != nil {
		os.Remove(downloadPath)
		return "", fmt.Errorf("caching tegraflash bundle: %w", err)
	}
	return cached, nil
}

// Format: <cache>/os-images/<device>-<version>[-<storage>].img
// The storage suffix is omitted for single-storage devices (StorageUnknown).
func osCachedImagePath(deviceKey, version string, storageType StorageType) (string, error) {
	// Sanitize to prevent path traversal from user-supplied --version flag.
	safeDevice := filepath.Base(deviceKey)
	safeVersion := filepath.Base(version)
	if safeDevice != deviceKey || safeVersion != version ||
		strings.Contains(deviceKey, "..") || strings.Contains(version, "..") {
		return "", fmt.Errorf("invalid device key or version: %q / %q", deviceKey, version)
	}

	dir, err := osCacheDir()
	if err != nil {
		return "", err
	}
	suffix := ""
	if storageType != StorageUnknown {
		suffix = "-" + string(storageType)
	}
	return filepath.Join(dir, fmt.Sprintf("%s-%s%s.img", safeDevice, safeVersion, suffix)), nil
}


// zipReadCloser wraps a zip.ReadCloser and its entry's ReadCloser so both
// are released with a single Close call.
type zipReadCloser struct {
	archive *zip.ReadCloser
	entry   io.ReadCloser
}

func (z *zipReadCloser) Read(p []byte) (int, error) { return z.entry.Read(p) }

func (z *zipReadCloser) Close() error {
	err := z.entry.Close()
	if err2 := z.archive.Close(); err == nil {
		err = err2
	}
	return err
}

// streamZipImageEntry opens a zip archive and returns a streaming reader over
// the first .img, .raw, or .wic entry it finds, plus the uncompressed size.
// The caller must Close the returned reader.
func streamZipImageEntry(zipPath string) (io.ReadCloser, int64, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, 0, fmt.Errorf("opening zip: %w", err)
	}

	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(f.Name))
		if ext != ".img" && ext != ".raw" && ext != ".wic" {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			r.Close()
			return nil, 0, fmt.Errorf("opening %s in zip: %w", f.Name, err)
		}

		size := int64(f.UncompressedSize64)
		if size == 0 {
			size = f.FileInfo().Size()
		}
		if size == 0 {
			rc.Close()
			r.Close()
			return nil, 0, fmt.Errorf("zip entry %s has unknown uncompressed size", f.Name)
		}

		return &zipReadCloser{archive: r, entry: rc}, size, nil
	}

	r.Close()
	return nil, 0, fmt.Errorf("no .img, .raw, or .wic file found in zip archive")
}

// resolveOSImage returns the path to a cached file ready for streaming.
// For zip URLs: checks .img cache then .zip cache, then downloads.
// For non-zip URLs: checks .img cache, then downloads directly.
func resolveOSImage(deviceKey string, img *imageInfo) (string, error) {
	isZip := strings.HasSuffix(strings.ToLower(img.DownloadURL), ".zip")

	cached, err := osCachedImagePath(deviceKey, img.Version, img.Storage)
	if err != nil {
		return "", err
	}
	if info, statErr := os.Stat(cached); statErr == nil && info.Size() > 0 {
		cliLogln("Using cached image (%s)", cached)
		return cached, nil
	}

	if isZip {
		// Zip cache: same path but with .zip extension so openOSImageStream detects it.
		zipCached := strings.TrimSuffix(cached, ".img") + ".zip"
		if info, statErr := os.Stat(zipCached); statErr == nil && info.Size() > 0 {
			fmt.Printf("Using cached image (%s)\n", zipCached)
			return zipCached, nil
		}
		downloadPath, dlErr := downloadImage(img)
		if dlErr != nil {
			return "", fmt.Errorf("downloading image: %w", dlErr)
		}
		os.Remove(zipCached)
		if renameErr := os.Rename(downloadPath, zipCached); renameErr != nil {
			os.Remove(downloadPath)
			return "", fmt.Errorf("caching image: %w", renameErr)
		}
		return zipCached, nil
	}

	downloadPath, err := downloadImage(img)
	if err != nil {
		return "", fmt.Errorf("downloading image: %w", err)
	}
	os.Remove(cached)
	if err := os.Rename(downloadPath, cached); err != nil {
		os.Remove(downloadPath)
		return "", fmt.Errorf("caching image: %w", err)
	}
	return cached, nil
}

// openOSImageStream resolves the cached file for deviceKey+img, then returns
// a streaming reader over the image bytes and the total uncompressed size.
// The caller must Close the returned reader.
func openOSImageStream(deviceKey string, img *imageInfo) (io.ReadCloser, int64, error) {
	cachePath, err := resolveOSImage(deviceKey, img)
	if err != nil {
		return nil, 0, err
	}
	if strings.HasSuffix(strings.ToLower(cachePath), ".zip") {
		return streamZipImageEntry(cachePath)
	}
	f, err := os.Open(cachePath)
	if err != nil {
		return nil, 0, fmt.Errorf("opening cached image: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("stat cached image: %w", err)
	}
	return f, info.Size(), nil
}

// openLocalImageStream opens an arbitrary local file for streaming.
// If the path ends in .zip it finds the first image entry inside it.
// Otherwise it opens the file directly as a reader.
func openLocalImageStream(imagePath string) (io.ReadCloser, int64, error) {
	if strings.HasSuffix(strings.ToLower(imagePath), ".zip") {
		return streamZipImageEntry(imagePath)
	}
	f, err := os.Open(imagePath)
	if err != nil {
		return nil, 0, fmt.Errorf("opening image: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, fmt.Errorf("stat image: %w", err)
	}
	return f, info.Size(), nil
}

// wifiCLIOptions captures the WiFi-related flags coming from cobra so they
// can be threaded through as a single value.
type wifiCLIOptions struct {
	SSID     string   // --wifi-ssid shortcut
	Password string   // --wifi-password (only valid with --wifi-ssid)
	Entries  []string // --wifi, repeatable
	NoWifi   bool     // --no-wifi
}

func (opts wifiCLIOptions) hasProvisioningFlags() bool {
	return opts.SSID != "" || opts.Password != "" || len(opts.Entries) > 0
}

// resolveWiFiCredentialsList builds the ordered list of WiFi credentials to
// write to the config partition. It consults flags first (non-interactive),
// and only falls back to the Bubble Tea prompts when no flag was set and
// stdin is a TTY.
func resolveWiFiCredentialsList(opts wifiCLIOptions) ([]wendyconf.WifiCredential, error) {
	if opts.NoWifi {
		if opts.SSID != "" || len(opts.Entries) > 0 {
			return nil, fmt.Errorf("--no-wifi is incompatible with --wifi / --wifi-ssid")
		}
		return nil, nil
	}
	if opts.Password != "" && opts.SSID == "" {
		return nil, fmt.Errorf("--wifi-password requires --wifi-ssid")
	}

	var creds []wendyconf.WifiCredential

	// --wifi (repeatable) first so priorities stay in the order the user typed.
	for _, raw := range opts.Entries {
		c, err := parseWiFiEntry(raw)
		if err != nil {
			return nil, fmt.Errorf("invalid --wifi %q: %w", raw, err)
		}
		creds = append(creds, c)
	}

	// --wifi-ssid shortcut folds into a single trailing entry.
	if opts.SSID != "" {
		c := wendyconf.WifiCredential{SSID: opts.SSID, Password: opts.Password}
		if c.Password == "" {
			if pw, kerr := lookupKeychainPassword(c.SSID); kerr == nil && pw != "" {
				c.Password = pw
			} else if isInteractiveTerminal() {
				pw, perr := tui.PromptText(fmt.Sprintf("WiFi password for %s", c.SSID), "(leave empty for open network)", nil)
				if perr != nil {
					return nil, fmt.Errorf("reading WiFi password: %w", perr)
				}
				c.Password = pw
			}
		}
		creds = append(creds, c)
	}

	// If any flag supplied creds OR stdin is not a TTY, we're done.
	if len(creds) > 0 || !isInteractiveTerminal() {
		return creds, nil
	}

	// Interactive path: Y/N → loop until the user declines another network.
	enable, err := tui.ConfirmDefaultYes("Set up WiFi on first boot?")
	if err != nil {
		return nil, err
	}
	if !enable {
		return nil, nil
	}

	for {
		c, added, err := promptAddOneCredential(len(creds))
		if err != nil {
			return nil, err
		}
		if !added {
			break
		}
		creds = append(creds, c)

		more, err := tui.Confirm("Add another WiFi network?")
		if err != nil {
			return nil, err
		}
		if !more {
			break
		}
	}

	return creds, nil
}

// parseWiFiEntry parses `ssid=X,password=Y,priority=N,hidden=true,security=wpa2`.
// Only `ssid=` is required; commas inside values can be escaped with `\,`.
func parseWiFiEntry(raw string) (wendyconf.WifiCredential, error) {
	var c wendyconf.WifiCredential
	for _, kv := range splitEscaped(raw, ',') {
		kv = strings.TrimSpace(kv)
		if kv == "" {
			continue
		}
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			return c, fmt.Errorf("expected key=value, got %q", kv)
		}
		k := strings.ToLower(strings.TrimSpace(kv[:eq]))
		v := strings.TrimSpace(kv[eq+1:])
		switch k {
		case "ssid":
			c.SSID = v
		case "password", "pass", "psk":
			c.Password = v
		case "priority":
			n, err := strconv.Atoi(v)
			if err != nil {
				return c, fmt.Errorf("priority must be an integer: %w", err)
			}
			c.Priority = int32(n)
		case "hidden":
			b, err := strconv.ParseBool(v)
			if err != nil {
				return c, fmt.Errorf("hidden must be true/false: %w", err)
			}
			c.Hidden = b
		case "security":
			c.Security = strings.ToLower(v)
		default:
			return c, fmt.Errorf("unknown key %q", k)
		}
	}
	if c.SSID == "" {
		return c, fmt.Errorf("ssid is required")
	}
	return c, nil
}

// splitEscaped splits s on sep, honouring `\sep` as a literal separator char.
func splitEscaped(s string, sep byte) []string {
	var out []string
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && s[i+1] == sep {
			cur.WriteByte(sep)
			i++
			continue
		}
		if s[i] == sep {
			out = append(out, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(s[i])
	}
	out = append(out, cur.String())
	return out
}

// promptAddOneCredential runs the local scan + picker + password prompt to
// collect a single WiFi credential. index is the zero-based count of entries
// already collected (used to suggest a descending priority).
func promptAddOneCredential(index int) (wendyconf.WifiCredential, bool, error) {
	var c wendyconf.WifiCredential

	networks, scanErr := scanLocalWifiNetworks()
	if scanErr == nil && len(networks) > 0 {
		var items []tui.PickerItem
		for _, n := range networks {
			signal := ""
			if n.SignalStrength > 0 {
				signal = fmt.Sprintf("%d%%", n.SignalStrength)
			}
			items = append(items, tui.PickerItem{Name: n.SSID, Type: signal, Value: n.SSID})
		}
		fmt.Println()
		picked, pickErr := pickFromItems("Select WiFi network (or Ctrl+C to type manually)", items)
		if pickErr == nil {
			c.SSID = picked
		}
	}

	if c.SSID == "" {
		ssid, err := tui.PromptText("WiFi SSID", "", nonEmptyValidator)
		if err != nil {
			return c, false, err
		}
		c.SSID = ssid
	}

	if supportsKeychainLookup {
		useKeychain, err := tui.ConfirmDefaultYes(fmt.Sprintf("Look up password for '%s' from keychain? (macOS will ask for permission)", c.SSID))
		if err != nil {
			return c, false, err
		}
		if useKeychain {
			if pw, kerr := lookupKeychainPassword(c.SSID); kerr == nil && pw != "" {
				cliLogln("Using saved password from keychain.")
				c.Password = pw
			} else {
				cliNotice("Password not available from keychain.")
			}
		}
	}

	if c.Password == "" {
		pw, err := tui.PromptText(fmt.Sprintf("Password for %s", c.SSID), "(leave empty for open network)", nil)
		if err != nil {
			return c, false, err
		}
		c.Password = pw
	}

	// First network gets the highest implicit priority; each subsequent one
	// steps down. Users can still override via the non-interactive flags.
	c.Priority = int32(100 - index)
	return c, true, nil
}

func nonEmptyValidator(v string) error {
	if strings.TrimSpace(v) == "" {
		return fmt.Errorf("required")
	}
	return nil
}

// resolveDeviceName returns the device name to pre-configure on first boot.
// If flagName is set it is validated and returned directly. In interactive mode
// the user is prompted; an empty response skips naming (auto-generated on device).
func resolveDeviceName(flagName string) (string, error) {
	validate := func(name string) error {
		if len(name) < 3 || len(name) > 64 {
			return fmt.Errorf("device name must be 3–64 characters")
		}
		for i, c := range name {
			switch {
			case c >= 'a' && c <= 'z':
			case (c >= '0' && c <= '9') || c == '-':
				if i == 0 {
					return fmt.Errorf("device name must start with a lowercase letter")
				}
			default:
				return fmt.Errorf("device name may only contain lowercase letters, digits, and hyphens")
			}
		}
		return nil
	}

	if flagName != "" {
		if err := validate(flagName); err != nil {
			return "", fmt.Errorf("--device-name: %w", err)
		}
		return flagName, nil
	}

	if !isInteractiveTerminal() {
		return "", nil
	}

	fmt.Print("\nDevice name (leave empty to auto-generate): ")
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	name := strings.TrimSpace(line)
	if name == "" {
		return "", nil
	}
	if err := validate(name); err != nil {
		return "", fmt.Errorf("invalid device name: %w", err)
	}
	return name, nil
}

// confirmOverwriteInternalDrive guards against accidentally wiping internal
// (non-removable) drives. The OS-level system disk is already filtered out by
// listAllDrives, but a non-system internal drive (e.g. a secondary SATA SSD
// on Windows) is still selectable via --drive — the existing y/n confirm is
// too easy to autopilot through. For those drives we either require an
// explicit --yes-overwrite-internal flag (non-interactive) or a typed device
// path (interactive).
func confirmOverwriteInternalDrive(d drive, force bool, yesOverwriteInternal bool) error {
	if d.IsRemovable {
		return nil
	}
	if yesOverwriteInternal {
		return nil
	}
	if force {
		return fmt.Errorf("refusing to wipe non-removable drive %s (%s) with --force; pass --yes-overwrite-internal to confirm you really want to overwrite an internal drive", d.Name, d.DevicePath)
	}
	fmt.Printf("\n%s (%s) is an internal (non-removable) drive.\n", d.Name, d.DevicePath)
	fmt.Println("Typically WendyOS is installed to an SD card or USB drive — overwriting an internal drive will destroy whatever filesystem currently lives on it.")
	fmt.Printf("To proceed, type the device path exactly:\n  %s\n> ", d.DevicePath)
	reader := bufio.NewReader(os.Stdin)
	line, _ := reader.ReadString('\n')
	if strings.TrimSpace(line) != d.DevicePath {
		return fmt.Errorf("internal-drive overwrite cancelled (typed value did not match %s)", d.DevicePath)
	}
	return nil
}

// provisionConfigPartition downloads the latest stable arm64 wendy-agent binary
// and writes it (along with zero or more WiFi credentials and an optional
// device name) to the config partition on d.
func provisionConfigPartition(d drive, creds []wendyconf.WifiCredential, deviceName string, provisioningJSON []byte) error {
	release, err := fetchAgentRelease(false)
	if err != nil {
		return fmt.Errorf("fetching latest agent release: %w", err)
	}

	const assetPrefix = "wendy-agent-linux-arm64-"
	var matched *githubReleaseAsset
	for i := range release.Assets {
		a := &release.Assets[i]
		if strings.HasPrefix(a.Name, assetPrefix) && strings.HasSuffix(a.Name, ".tar.gz") {
			matched = a
			break
		}
	}
	if matched == nil {
		return fmt.Errorf("no arm64 asset found in release %s", release.TagName)
	}

	cliLogln("Downloading wendy-agent %s for device...", release.TagName)
	agentBinary, err := downloadAgentBinary(*matched)
	if err != nil {
		return fmt.Errorf("downloading agent binary: %w", err)
	}

	return writeConfigPartition(d, agentBinary, creds, deviceName, provisioningJSON)
}

func installESP32Firmware(ctx context.Context, nightly bool, chip string) error {
	cliLogln("\nScanning for ESP32 devices...")

	serialPort, err := discovery.ResolveESP32SerialPort()
	if err != nil {
		cliNotice("\nNo ESP32 device detected.")
		cliNotice("Make sure your ESP32 is connected via USB and in bootloader mode.")
		cliNotice("To enter bootloader mode: hold the BOOT button, press RESET, then release BOOT.")
		return fmt.Errorf("ESP32 not found: %w", err)
	}

	cliLogln("Found ESP32 at %s", serialPort)

	cliLogln("Fetching latest Wendy Lite firmware...")
	asset, err := fetchFirmwareFromManifest(chip, nightly)
	if err != nil {
		return fmt.Errorf("fetching firmware: %w", err)
	}
	cliLogln("Found firmware: %s v%s", asset.Name, asset.Version)

	// Download with progress bar.
	prog := tui.NewProgress(fmt.Sprintf("Downloading %s %s...", asset.Name, asset.Version))
	p := tea.NewProgram(prog)

	var fwPath string
	var dlErr error

	go func() {
		fwPath, dlErr = downloadFirmware(asset, func(downloaded, total int64) {
			if total > 0 {
				p.Send(tui.ProgressUpdateMsg{Percent: float64(downloaded) / float64(total)})
			}
		})
		if dlErr != nil {
			p.Send(tui.ProgressDoneMsg{Err: dlErr})
		} else {
			p.Send(tui.ProgressDoneMsg{})
		}
	}()

	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("progress TUI: %w", err)
	}

	model := finalModel.(tui.ProgressModel)
	if model.Err() != nil {
		return model.Err()
	}
	defer os.Remove(fwPath)

	// Flash with progress bar.
	fmt.Println()
	flashProg := tui.NewProgress(fmt.Sprintf("Flashing to %s...", serialPort))
	fp := tea.NewProgram(flashProg)

	go func() {
		flashErr := flashFirmware(serialPort, fwPath, func(pct float64) {
			fp.Send(tui.ProgressUpdateMsg{Percent: pct})
		})
		fp.Send(tui.ProgressDoneMsg{Err: flashErr})
	}()

	flashFinal, err := fp.Run()
	if err != nil {
		return fmt.Errorf("flash TUI: %w", err)
	}

	flashModel := flashFinal.(tui.ProgressModel)
	if flashModel.Err() != nil {
		return fmt.Errorf("flashing failed: %w", flashModel.Err())
	}

	cliSuccess("\nSuccessfully flashed Wendy Lite %s!", asset.Version)
	cliLogln("The device will reboot automatically.")
	return nil
}
