//go:build darwin || linux || windows

package commands

import (
	"archive/zip"
	"bufio"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
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

	"google.golang.org/protobuf/proto"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	"github.com/wendylabsinc/wendy/go/internal/shared/wendyconf"
)

type preEnrollMode int

type t234InstallOptions struct {
	DeviceType    string
	DeviceName    string
	Version       string
	Storage       string
	Artifact      *recoveryFlashpackInfo
	Force         bool
	WiFi          wifiCLIOptions
	RequestedName string
	PreEnroll     preEnrollOptions
}

const (
	preEnrollAuto   preEnrollMode = iota // prompt if interactive terminal + auth session exists
	preEnrollForced                      // --pre-enroll explicitly set to true
	preEnrollSkip                        // --pre-enroll explicitly set to false
)

func newOSInstallCmd() *cobra.Command {
	var nightly bool
	var force bool
	var noBmap bool
	var rootfsOnly bool
	var storageOverride string
	var yesOverwriteInternal bool
	var preEnroll bool
	var deviceType string
	var versionFlag string
	var driveFlag string
	var wifiSSID string
	var wifiPassword string
	var wifiEntries []string
	var noWifi bool
	var deviceName string
	var enrollCloudGRPC string
	var prNumber int

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
			if rootfsOnly && len(args) > 0 {
				return fmt.Errorf("--rootfs-only cannot be combined with positional [image] [drive] arguments")
			}
			if prNumber > 0 {
				if nightly || versionFlag != "" || len(args) > 0 {
					return fmt.Errorf("--pr cannot be combined with --nightly, --version, or positional image/drive arguments")
				}
				fmt.Fprintln(cmd.ErrOrStderr(), tui.WarningMessage("PR images are unhardened debug builds (passwordless root, SSH on). Do not use in production."))
			}
			// Positional direct-install mode is incompatible with manifest-backed flags.
			if len(args) > 0 && (deviceType != "" || versionFlag != "" || driveFlag != "" || wifiSSID != "" || wifiPassword != "" || len(wifiEntries) > 0 || noWifi || deviceName != "" || enrollCloudGRPC != "") {
				return fmt.Errorf("positional [image] [drive] arguments cannot be combined with --device-type, --version, --drive, --wifi-ssid, --wifi-password, --wifi, --no-wifi, --device-name, or --cloud-grpc")
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
			rootfsOnlyExplicit := cmd.Flags().Changed("rootfs-only")
			return runOSInstall(cmd.Context(), nightly, deviceType, versionFlag, driveFlag, force, yesOverwriteInternal, noBmap, rootfsOnly, rootfsOnlyExplicit, storageOverride, opts, deviceName, preEnrollOptions{mode: mode, cloudGRPC: enrollCloudGRPC}, prNumber)
		},
	}

	cmd.Flags().BoolVar(&nightly, "nightly", false, "Use nightly/prerelease builds")
	cmd.Flags().BoolVar(&force, "force", false, "Skip confirmation prompt")
	cmd.Flags().BoolVar(&noBmap, "no-bmap", false, "Disable bmap-accelerated flashing even when a block map is available")
	cmd.Flags().BoolVar(&rootfsOnly, "rootfs-only", false, "Write only the SD/NVMe rootfs image; does not update Jetson QSPI boot firmware")
	cmd.Flags().StringVar(&storageOverride, "storage", "", "Force image storage variant: nvme or sd (default: auto-detect — real NVMe drives use nvme; a USB-attached drive uses the device's published image, SD for Raspberry Pi / NVMe for Jetson SSD enclosures). For jetson-agx-orin, emmc flashes the onboard eMMC over USB recovery instead of writing a drive")
	cmd.Flags().BoolVar(&yesOverwriteInternal, "yes-overwrite-internal", false, "Required to wipe an internal (non-removable) drive in non-interactive mode")
	cmd.Flags().StringVar(&deviceType, "device-type", "", "Device type from manifest (e.g. raspberry-pi-5)")
	cmd.Flags().StringVar(&versionFlag, "version", "", "WendyOS version to install (interactive if omitted)")
	cmd.Flags().StringVar(&driveFlag, "drive", "", "Target drive path (e.g. /dev/disk4)")
	cmd.Flags().StringVar(&wifiSSID, "wifi-ssid", "", "Pre-configure a single WiFi SSID on first boot (shortcut for --wifi)")
	cmd.Flags().StringVar(&wifiPassword, "wifi-password", "", "Password for --wifi-ssid")
	cmd.Flags().StringArrayVar(&wifiEntries, "wifi", nil, "Pre-configure a WiFi network. Repeatable. Format: ssid=X[,password=Y][,priority=N][,hidden=true][,security=wpa2]")
	cmd.Flags().BoolVar(&noWifi, "no-wifi", false, "Skip WiFi setup entirely (no interactive prompt, no pre-seeded networks)")
	cmd.Flags().StringVar(&deviceName, "device-name", "", "Set device name on first boot (e.g. brave-dolphin)")
	cmd.Flags().BoolVar(&preEnroll, "pre-enroll", false, "Pre-enroll this device with Wendy Cloud during imaging (requires 'wendy auth login')")
	cmd.Flags().StringVar(&enrollCloudGRPC, "cloud-grpc", "", "Cloud gRPC endpoint of the auth session to use for pre-enrollment (optional when a default is set via 'wendy auth use')")
	cmd.Flags().IntVar(&prNumber, "pr", 0, "Install the image built by wendyos-builder PR #N (debug build; mutually exclusive with --nightly, --version, and positional [image] [drive])")

	return cmd
}

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
			fmt.Println("Cancelled.")
			return nil
		}
	}

	stream, err := openLocalImageStream(imagePath)
	if err != nil {
		return fmt.Errorf("opening image: %w", err)
	}
	defer stream.Close()

	fmt.Printf("Writing image to %s...\n", targetDrive.DevicePath)
	fmt.Println(elevationHint())
	if err := writeImageToDisk(stream, stream.uncompressedSize, *targetDrive, nil); err != nil {
		return fmt.Errorf("writing image: %w", err)
	}

	fmt.Printf("\nSuccessfully installed image on %s.\n", targetDrive.Name)
	return nil
}

// pickerDevice is a unified entry for the device selection picker.
type pickerDevice struct {
	Name       string
	Version    string // display version (e.g. "0.10.5 (nightly)")
	RawVersion string // exact version key for manifest lookup
	IsESP32    bool
	ESP32Board string
	Manifest   *deviceManifest // cached manifest for Linux devices
}

// pickLinuxDevice fetches available Linux devices from the manifest and presents
// an interactive picker. Returns the selected device key and its deviceInfo.
func pickLinuxDevice() (string, deviceInfo, error) {
	fmt.Println("Fetching available devices...")

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

// pickWendyLiteBoard asks which Wendy Lite board to install. When target is
// non-empty, only boards for that ESP32 target are offered; an empty target
// lists every catalog board with published firmware. nightly selects which
// release channel a board's firmware must be published on to be offered.
// Returns ErrUserCancelled when the user quits the picker.
func pickWendyLiteBoard(target string, nightly bool) (string, error) {
	boards, err := WendyLiteBoardsWithFirmware(nightly)
	if err != nil {
		return "", err
	}
	items := make([]tui.PickerItem, 0, len(boards))
	for _, v := range boards {
		if target != "" && v.Target != target {
			continue
		}
		items = append(items, tui.PickerItem{
			Name:    v.DisplayName,
			SortKey: strings.ToLower(v.DisplayName),
			Value:   v.Board,
		})
	}
	fmt.Println()
	return pickFromItems("Select your board model", items)
}

func runOSInstall(ctx context.Context, nightly bool, flagDeviceType, flagVersion, flagDrive string, force bool, yesOverwriteInternal bool, noBmap, rootfsOnly, rootfsOnlyExplicit bool, storageOverride string, wifi wifiCLIOptions, deviceName string, preOpts preEnrollOptions, prNumber int) error {
	if storageOverride != "" && storageOverride != "nvme" && storageOverride != "sd" && storageOverride != "emmc" {
		return fmt.Errorf("invalid --storage %q: must be \"nvme\", \"sd\", or \"emmc\" (jetson-agx-orin only)", storageOverride)
	}
	if storageOverride == "emmc" && flagDeviceType != "" && flagDeviceType != orinDeviceType {
		return fmt.Errorf("--storage emmc is supported only for --device-type %s", orinDeviceType)
	}
	if rootfsOnly && storageOverride == "emmc" {
		return fmt.Errorf("--storage emmc cannot be combined with --rootfs-only; eMMC is available only through full recovery")
	}
	if isT234RecoveryDevice(flagDeviceType) && !rootfsOnly {
		if err := rejectRecoveryDriveFlags(flagDrive, noBmap, yesOverwriteInternal); err != nil {
			return err
		}
	}

	// AGX Thor flashes over USB recovery (not a drive), via its own flashpack
	// artifact and device selection — a separate path from the disk-image flow below.
	if flagDeviceType == thorDeviceType {
		if rootfsOnly {
			return fmt.Errorf("--rootfs-only is not available for Jetson AGX Thor")
		}
		if err := rejectRecoveryDriveFlags(flagDrive, noBmap, yesOverwriteInternal); err != nil {
			return err
		}
		if storageOverride != "" {
			return fmt.Errorf("--storage does not apply to Jetson AGX Thor recovery")
		}
		return installThor(ctx, flagVersion, nightly, force, wifi, deviceName, preOpts, prNumber)
	}
	fmt.Println("Fetching available devices...")

	// Fetch Linux devices from GCS manifest — the per-PR manifest when --pr is
	// set, otherwise the standard release manifest.
	var linuxDevices []deviceInfo
	var err error
	if prNumber > 0 {
		linuxDevices, err = getAvailablePRDevices(prNumber)
	} else {
		linuxDevices, err = getAvailableDevices()
	}
	if err != nil {
		log.Printf("WARNING: could not fetch Linux device manifest: %v", err)
	}

	// Fetch Wendy Lite boards and targets, keeping only boards with published
	// firmware — a catalog entry with no build yet would otherwise reach the
	// picker and fail immediately after selection.
	wliteBoards, err := WendyLiteBoardsWithFirmware(nightly)
	if err != nil {
		log.Printf("WARNING: could not fetch Wendy Lite board catalog: %v", err)
	}
	wliteTargets, err := WendyLiteTargets()
	if err != nil {
		log.Printf("WARNING: could not fetch Wendy Lite target catalog: %v", err)
	}
	// Drop targets with no board left so the picker never offers a target
	// that leads to an empty second picker.
	boardTargets := make(map[string]bool, len(wliteBoards))
	for _, b := range wliteBoards {
		boardTargets[b.Target] = true
	}
	var targetsWithBoards []WendyLiteTarget
	for _, t := range wliteTargets {
		if boardTargets[t.Name] {
			targetsWithBoards = append(targetsWithBoards, t)
		}
	}
	wliteTargets = targetsWithBoards

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
			Manifest:   dev.Manifest,
		}
		deviceMap[dev.Key] = pd

		items = append(items, tui.PickerItem{
			Name:        dev.Name,
			Description: displayVersion,
			Section:     "WendyOS",
			SortKey:     "0_wendyos_" + strings.ToLower(dev.Name),
			Value:       dev.Key,
		})
	}

	// When --device-type is not provided, also offer ESP32 entries in the picker.
	// Never under --pr: firmware isn't a PR build target and installESP32Firmware
	// ignores prNumber, so offering ESP32 here would silently install release
	// firmware instead of a PR build.
	if flagDeviceType == "" && prNumber == 0 {
		for _, v := range wliteBoards {
			deviceMap["wlite_"+v.Board] = pickerDevice{
				Name:       v.DisplayName,
				Version:    v.Version,
				IsESP32:    true,
				ESP32Board: v.Board,
			}
		}

		for _, t := range wliteTargets {
			items = append(items, tui.PickerItem{
				Name:    t.DisplayName,
				Section: "Wendy Lite",
				SortKey: "1_lite_" + strings.ToLower(t.DisplayName),
				Value:   "wlite_target_" + t.Name,
			})
		}

		items = append(items, tui.PickerItem{
			Name:        "Linux Desktop",
			Description: "Install wendy-agent on an existing Linux machine",
			Section:     "Linux Desktop",
			SortKey:     "2_linux_desktop",
			Value:       linuxDesktopValue,
		})

		items = append(items, tui.PickerItem{
			Name:        "Headless Mac",
			Description: "Install wendy-agent on an existing Mac (Apple Silicon)",
			Section:     "Headless Mac",
			SortKey:     "3_headless_mac",
			Value:       headlessMacValue,
		})
	}

	// Resolve device — use flag or interactive picker.
	var selected string
	if flagDeviceType != "" {
		// --device-type is only supported for Linux devices, not ESP32/Wendy Lite.
		switch flagDeviceType {
		case "esp32-c5", "esp32-c6", "esp32-c61", "esp32-p4", "esp32-s3":
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
			if prNumber > 0 {
				return fmt.Errorf("device %q not built by PR %d (built: %s)", flagDeviceType, prNumber, strings.Join(available, ", "))
			}
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

		if target, ok := strings.CutPrefix(selected, "wlite_target_"); ok {
			board, err := pickWendyLiteBoard(target, nightly)
			if err != nil {
				return err
			}
			selected = "wlite_" + board
		}
	}

	// Thor flashes over USB recovery via its flashpack, never to a drive. The
	// interactive picker reaches here too (the flag is empty), so route it to
	// installThor here as well — otherwise it falls through to the disk-image
	// flow and dd's the wrong artifact onto an external drive.
	if selected == thorDeviceType {
		if rootfsOnly {
			return fmt.Errorf("--rootfs-only is not available for Jetson AGX Thor")
		}
		if err := rejectRecoveryDriveFlags(flagDrive, noBmap, yesOverwriteInternal); err != nil {
			return err
		}
		if storageOverride != "" {
			return fmt.Errorf("--storage does not apply to Jetson AGX Thor recovery")
		}
		return installThor(ctx, flagVersion, nightly, force, wifi, deviceName, preOpts, prNumber)
	}

	if selected == linuxDesktopValue {
		return installDesktop(ctx, preOpts, deviceName, linuxDesktopMachineLabel)
	}

	if selected == headlessMacValue {
		return installDesktop(ctx, preOpts, deviceName, headlessMacMachineLabel)
	}

	device := deviceMap[selected]

	if device.IsESP32 {
		return installESP32Firmware(ctx, nightly, device.ESP32Board, discovery.SerialPortInfo{}, wifi, deviceName, preOpts)
	}
	if storageOverride == "emmc" && selected != orinDeviceType {
		return fmt.Errorf("--storage emmc is supported only for --device-type %s", orinDeviceType)
	}

	selectedVersion := device.RawVersion
	if flagVersion != "" {
		selectedVersion = flagVersion
	}
	ver, ok := device.Manifest.Versions[selectedVersion]
	if !ok {
		return fmt.Errorf("version %q not found for %s", selectedVersion, device.Name)
	}
	if rootfsOnly && !isT234RecoveryDevice(selected) {
		return fmt.Errorf("--rootfs-only is supported only for Jetson Orin recovery targets")
	}
	if isT234RecoveryDevice(selected) && storageOverride == "emmc" && ver.InstallMode != "recovery" {
		return fmt.Errorf("version %s does not publish an eMMC recovery flashpack", selectedVersion)
	}
	if isT234RecoveryDevice(selected) && ver.InstallMode != "recovery" && !rootfsOnly {
		// The version has no recovery flashpack, so its legacy SD/NVMe image is
		// the only way to install it. Fall back to rootfs-only imaging instead
		// of making the user rediscover the flag — unless they explicitly
		// pinned --rootfs-only=false, which asks for a flash this version
		// cannot deliver.
		if rootfsOnlyExplicit {
			return fmt.Errorf("version %s predates full recovery flashpacks and cannot flash boot firmware; drop --rootfs-only=false or pick a newer version", selectedVersion)
		}
		fmt.Fprintln(os.Stderr, tui.WarningMessage(fmt.Sprintf("Version %s predates full recovery flashpacks; falling back to its SD/NVMe OS image. QSPI boot firmware will not be updated.", selectedVersion)))
		rootfsOnly = true
		// Windows: the raw disk write needs elevation, and the UAC relaunch
		// starts a fresh process — pin the device and fallback mode on the
		// child's command line so the elevated instance resumes at the
		// storage question. No-op on Unix.
		if err := elevateForT234Flash(selected, true); err != nil {
			return err
		}
	}
	// Interactively choose the flash mode for Jetson Orin recovery targets so users
	// don't have to know the --rootfs-only flag. shouldPromptFlashMode skips it
	// whenever a flag already pins the mode or we're non-interactive, keeping
	// today's behavior unchanged.
	if shouldPromptFlashMode(selected, ver.InstallMode, rootfsOnlyExplicit, storageOverride, isInteractiveTerminal()) {
		rootfsOnly, err = chooseT234FlashMode()
		if err != nil {
			return err // ErrUserCancelled -> clean exit
		}
		if rootfsOnly {
			// Windows: the raw disk write needs elevation, and the UAC relaunch
			// starts a fresh process — elevate now with the device and mode
			// pinned on the child's command line so the elevated instance
			// resumes at the storage question instead of re-asking these. The
			// full-recovery choice elevates below, after its flag validation.
			if err := elevateForT234Flash(selected, true); err != nil {
				return err
			}
		}
	}
	if isT234RecoveryDevice(selected) && ver.InstallMode == "recovery" && !rootfsOnly {
		if err := rejectRecoveryDriveFlags(flagDrive, noBmap, yesOverwriteInternal); err != nil {
			return err
		}
		// Windows: elevate now, before the remaining wizard questions. The UAC
		// relaunch starts a fresh process in a new console, so any answers
		// already given would have to be re-entered there; the device choice
		// and flash mode carry over as --device-type/--rootfs-only. Pinning
		// the mode also keeps the elevated child (whose fresh console IS
		// interactive) from raising the flash-mode prompt a non-interactive
		// parent never showed.
		if err := elevateForT234Flash(selected, false); err != nil {
			return err
		}
		storage, err := chooseT234RecoveryStorage(selected, storageOverride)
		if err != nil {
			return err
		}
		artifact, err := getRecoveryFlashpackInfo(device.Manifest, selected, selectedVersion, storage)
		if err != nil {
			return err
		}
		return installOrin(ctx, t234InstallOptions{
			DeviceType: selected, DeviceName: device.Name, Version: selectedVersion,
			Storage: storage, Artifact: artifact, Force: force, WiFi: wifi,
			RequestedName: deviceName, PreEnroll: preOpts,
		})
	}
	if isT234RecoveryDevice(selected) && rootfsOnly {
		// The legacy-version fallback above prints its own QSPI notice, and
		// recommending full recovery would be wrong there — the version has
		// no flashpack to recover with.
		if ver.InstallMode == "recovery" {
			fmt.Fprintln(os.Stderr, tui.WarningMessage("Rootfs-only imaging does not update Jetson QSPI boot firmware; use full recovery to guarantee matching boot firmware and rootfs."))
		}
		storageOverride, err = chooseT234RootfsOnlyStorage(selected, storageOverride)
		if err != nil {
			return err
		}
	}
	return installLinuxImage(ctx, selected, device, nightly, flagVersion, flagDrive, force, yesOverwriteInternal, noBmap, rootfsOnly, storageOverride, wifi, deviceName, preOpts)
}

func rejectRecoveryDriveFlags(drive string, noBmap, overwriteInternal bool) error {
	var flags []string
	if drive != "" {
		flags = append(flags, "--drive")
	}
	if noBmap {
		flags = append(flags, "--no-bmap")
	}
	if overwriteInternal {
		flags = append(flags, "--yes-overwrite-internal")
	}
	if len(flags) > 0 {
		return fmt.Errorf("%s require --rootfs-only; full recovery selects storage on the Jetson over USB", strings.Join(flags, ", "))
	}
	return nil
}

// shouldPromptFlashMode reports whether to interactively ask the user to choose
// between full USB recovery (firmware + OS) and rootfs-only imaging. It fires
// only for Jetson Orin recovery targets when full recovery is actually available
// (InstallMode == "recovery"), the user did not pin the mode via --rootfs-only,
// the mode is not already forced by --storage emmc (full recovery only), and we
// have an interactive terminal. Non-interactive runs and any explicit flag keep
// today's behavior untouched.
func shouldPromptFlashMode(deviceType, installMode string, rootfsOnlyExplicit bool, storageOverride string, interactive bool) bool {
	return isT234RecoveryDevice(deviceType) &&
		installMode == "recovery" &&
		!rootfsOnlyExplicit &&
		storageOverride != "emmc" &&
		interactive
}

// chooseT234FlashMode prompts the user to pick between OS-image-only imaging
// and a full recovery flash (firmware + OS), returning rootfsOnly=true for the
// image-only choice. Image-only is listed first because it is the common case;
// a full flash needs the device jumpered into recovery mode. A cancelled picker
// surfaces ErrUserCancelled, which the top-level command maps to a clean exit.
func chooseT234FlashMode() (rootfsOnly bool, err error) {
	choice, err := pickFromItems("Select flash mode", []tui.PickerItem{
		{Name: "OS image only (no firmware update)", Description: "Write to an NVMe/SD drive attached to this computer", Value: "rootfs"},
		{Name: "Full flash over UART", Description: "Updates boot firmware + OS (device in recovery mode)", Value: "full"},
	})
	if err != nil {
		return false, err // ErrUserCancelled propagates unchanged
	}
	return choice == "rootfs", nil
}

func chooseT234RecoveryStorage(device, override string) (string, error) {
	if device == orinNanoDeviceType {
		if override != "" && override != "nvme" {
			return "", fmt.Errorf("%s recovery supports NVMe only", orinNanoDeviceType)
		}
		return "nvme", nil
	}
	if device != orinDeviceType {
		return "", fmt.Errorf("unsupported T234 recovery device %q", device)
	}
	if override == "nvme" || override == "emmc" {
		return override, nil
	}
	if override != "" {
		return "", fmt.Errorf("%s recovery storage must be nvme or emmc", orinDeviceType)
	}
	if !isInteractiveTerminal() {
		return "", fmt.Errorf("--storage nvme or --storage emmc is required for noninteractive AGX Orin recovery")
	}
	return pickFromItems("Select recovery storage", []tui.PickerItem{
		{Name: "NVMe SSD", Description: "erase QSPI and the installed NVMe", Value: "nvme"},
		{Name: "Onboard eMMC", Description: "erase QSPI and onboard eMMC", Value: "emmc"},
	})
}

func chooseT234RootfsOnlyStorage(device, override string) (string, error) {
	switch device {
	case orinNanoDeviceType:
		if override == "" {
			return "nvme", nil
		}
		if override == "nvme" || override == "sd" {
			return override, nil
		}
		return "", fmt.Errorf("%s rootfs-only imaging supports NVMe or SD", orinNanoDeviceType)
	case orinDeviceType:
		if override == "" || override == "nvme" {
			return "nvme", nil
		}
		return "", fmt.Errorf("%s rootfs-only imaging supports NVMe only", orinDeviceType)
	default:
		return "", fmt.Errorf("unsupported T234 rootfs-only device %q", device)
	}
}

// installLinuxImage handles the Linux device path: pick version → pick drive → download → write.
// nightly, flagVersion, flagDrive, and force allow skipping the corresponding interactive prompts.
func installLinuxImage(ctx context.Context, deviceKey string, device pickerDevice, nightly bool, flagVersion, flagDrive string, force bool, yesOverwriteInternal bool, noBmap, rootfsOnly bool, storageOverride string, wifi wifiCLIOptions, deviceName string, preOpts preEnrollOptions) error {
	// Authenticate elevation up front so we don't pay for the multi-hundred-MB
	// image download just to discover the user can't write to a raw disk. On
	// Windows this offers a UAC re-launch when not elevated; on Unix it
	// pre-caches the sudo timestamp.
	if err := preAuthElevation(); err != nil {
		return err
	}
	// The download can take minutes or hours, which is longer than the default
	// sudo credential cache window. Refresh in the background so the cached
	// timestamp never expires before writeImageToDisk runs.
	elevationCtx, cancelElevation := context.WithCancel(ctx)
	defer cancelElevation()
	keepElevationAlive(elevationCtx)

	// Step 1: Resolve version — use flag, nightly shortcut, or pick interactively.
	selectedVersion := device.RawVersion // default: latest (or nightly if --nightly)
	if flagVersion != "" {
		// Validate the requested version exists in the manifest.
		if _, ok := device.Manifest.Versions[flagVersion]; !ok {
			return fmt.Errorf("version %q not found for %s", flagVersion, device.Name)
		}
		selectedVersion = flagVersion
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
			fmt.Println("Cancelled.")
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

	// Resolve pre-enrollment before provisioning — the config partition is mounted
	// and unmounted inside provisionConfigWithRetry below.
	provisioningJSON, err := resolveProvisioningJSON(ctx, preOpts, provDeviceName)
	if err != nil {
		return err
	}

	// Step 5: Resolve image metadata for the target storage. A USB-attached
	// drive is ambiguous (SD card in a reader vs NVMe SSD in an enclosure), so
	// the variant is chosen from what this manifest version publishes — see
	// manifestStorage. Pass --storage to override.
	ver, ok := device.Manifest.Versions[selectedVersion]
	if !ok {
		return fmt.Errorf("version %s not found in device manifest", selectedVersion)
	}
	storage := manifestStorage(ver, targetDrive.StorageType, targetDrive.MediaFixed, storageOverride)

	// A USB-attached SD reader and an SSD enclosure are indistinguishable by bus
	// type, and not every platform reports fixed-media signals. When the variant
	// is a guess on a dual-variant device, ask the user which slot the drive is
	// for rather than silently picking the wrong image (a wrong guess can produce
	// a non-booting device). Non-interactive runs keep the manifest default.
	if storageOverride == "" && storageChoiceAmbiguous(ver, targetDrive.StorageType, targetDrive.MediaFixed) && isInteractiveTerminal() {
		bootNVMe, err := tui.ConfirmNoDefault(fmt.Sprintf(
			"Can't tell if %s is an SD card or an SSD. Will it boot from the NVMe/SSD slot?",
			targetDrive.DevicePath))
		if err != nil {
			return err
		}
		if bootNVMe {
			storage = "nvme"
		} else {
			storage = "sd"
		}
	} else if storageOverride == "" && targetDrive.MediaFixed && storage == "nvme" {
		// Auto-detected an SSD enclosure. Surface the inference and the override
		// so a surprising choice is at least visible and reversible.
		fmt.Printf("Detected a fixed SSD on %s — using the NVMe image (pass --storage sd to override).\n", targetDrive.DevicePath)
	}

	fmt.Printf("\nPreparing %s %s image...\n", device.Name, selectedVersion)
	var imgInfo *imageInfo
	if rootfsOnly && ver.InstallMode == "recovery" {
		imgInfo, err = getRootfsOnlyImageInfo(device.Manifest, selectedVersion, storage)
	} else {
		imgInfo, err = getImageInfo(device.Manifest, selectedVersion, storage)
	}
	if err != nil {
		return fmt.Errorf("getting image info: %w", err)
	}

	// Step 5a: Prefer the seekable-zstd fast path. When the manifest advertises a
	// .zst for this storage plus a usable bmap (and --no-bmap wasn't passed), we
	// download only the .zst + bmap and write mapped ranges, skipping holes —
	// and crucially we do NOT download the full .zip image at all.
	var seekableZst, seekableBmap string
	var seekableTotal int64
	if !noBmap && imgInfo.ZstURL != "" && imgInfo.BmapURL != "" {
		zstPath, zerr := resolveSeekableZst(deviceKey, selectedVersion, storage, imgInfo.ZstURL)
		bmapCandidate, berr := osCachedBmapPath(deviceKey, selectedVersion, storage)
		switch {
		case zerr != nil:
			fmt.Printf("Note: could not fetch seekable image (%v); flashing the full image.\n", zerr)
		case berr != nil:
			fmt.Printf("Note: cannot resolve block-map cache path (%v); flashing the full image.\n", berr)
		default:
			if derr := downloadBmap(imgInfo.BmapURL, bmapCandidate); derr != nil {
				fmt.Printf("Note: could not fetch block map (%v); flashing the full image.\n", derr)
			} else if parsed, perr := parseBmap(readFileOrNil(bmapCandidate)); perr != nil {
				fmt.Printf("Note: block map unusable (%v); flashing the full image.\n", perr)
			} else {
				seekableZst, seekableBmap, seekableTotal = zstPath, bmapCandidate, mappedBytes(parsed)
			}
		}
	}

	// Step 5b: Fallback path — resolve the .zip/.img stream only when NOT using
	// the seekable path (so the seekable path never downloads the .zip). For
	// compressed images, measure the size (skipped when a bmap is present, since
	// the bmap's ImageSize is the exact total) and prepare the legacy block map.
	var stream *imageStream
	var bmapPath string
	if seekableZst == "" {
		stream, err = openOSImageStream(deviceKey, imgInfo)
		if err != nil {
			return fmt.Errorf("opening OS image: %w", err)
		}
		defer stream.Close()

		if stream.uncompressedSize == 0 && stream.sourcePath != "" && imgInfo.BmapURL == "" {
			if err := measureImageWithProgress(stream); err != nil {
				if errors.Is(err, context.Canceled) {
					return err
				}
				fmt.Printf("Could not determine image size: %v\n", err)
			}
		}

		if !noBmap && imgInfo.BmapURL != "" {
			candidate, derr := osCachedBmapPath(deviceKey, selectedVersion, storage)
			if derr != nil {
				fmt.Printf("Note: cannot resolve block-map cache path (%v); flashing the full image.\n", derr)
			} else if derr := downloadBmap(imgInfo.BmapURL, candidate); derr != nil {
				fmt.Printf("Note: could not fetch block map (%v); flashing the full image.\n", derr)
			} else if parsed, perr := parseBmap(readFileOrNil(candidate)); perr != nil {
				fmt.Printf("Note: block map unusable (%v); flashing the full image.\n", perr)
			} else if stream.uncompressedSize > 0 && parsed.ImageSize != stream.uncompressedSize {
				fmt.Printf("Note: block map is for a %d-byte image but this image is %d bytes; flashing the full image.\n", parsed.ImageSize, stream.uncompressedSize)
			} else {
				bmapPath = candidate
			}
		}
	}

	// Step 6: Write image to drive with progress bar.
	fmt.Printf("Writing image to %s...\n", targetDrive.DevicePath)
	writeProg := tui.NewProgress(fmt.Sprintf("Writing to %s...", targetDrive.DevicePath))
	if seekableZst != "" || bmapPath != "" {
		// bmap failures fall back silently; suppress the TUI error render
		writeProg = writeProg.WithoutErrorView()
	}
	wp := tui.NewProgressProgram(writeProg)

	// Track how far the fast path got and which path ran, so a failure can
	// report the offset it reached and frame the primary error (WDY-1841).
	var lastWritten atomic.Int64
	var writeTotal int64
	var fastPathLabel string
	switch {
	case seekableZst != "":
		writeTotal, fastPathLabel = seekableTotal, "seekable block-map write"
	case bmapPath != "":
		writeTotal, fastPathLabel = stream.uncompressedSize, "block-map write"
	default:
		writeTotal, fastPathLabel = stream.uncompressedSize, "image write"
	}

	go func() {
		var writeErr error
		switch {
		case seekableZst != "":
			fmt.Println("Using seekable block map for faster flashing.")
			writeErr = writeImageWithBmapSeekable(seekableZst, seekableBmap, targetDrive, func(written int64) {
				lastWritten.Store(written)
				var pct float64
				if seekableTotal > 0 {
					pct = float64(written) / float64(seekableTotal)
				}
				wp.Send(tui.ProgressUpdateMsg{
					Percent: pct,
					Written: written,
					Total:   seekableTotal,
				})
			})
		case bmapPath != "":
			fmt.Println("Using block map for faster flashing.")
			writeErr = writeImageWithBmap(stream, stream.uncompressedSize, targetDrive, bmapPath, func(written int64) {
				lastWritten.Store(written)
				if msg, ok := stream.writeProgressMsg(written); ok {
					wp.Send(msg)
				}
			})
		default:
			writeErr = writeImageToDisk(stream, stream.uncompressedSize, targetDrive, func(written int64) {
				lastWritten.Store(written)
				if msg, ok := stream.writeProgressMsg(written); ok {
					wp.Send(msg)
				}
			})
		}
		wp.Send(tui.ProgressDoneMsg{Err: writeErr})
	}()

	writeFinal, err := wp.Run()
	if err != nil {
		return fmt.Errorf("progress TUI: %w", err)
	}

	writeModel := writeFinal.(tui.ProgressModel)
	if primaryErr := writeModel.Err(); primaryErr != nil {
		// User cancellation is not a write failure: don't classify it, and
		// don't start a full-image fallback write on a flash the user just
		// cancelled (same handling as the measure step above).
		if errors.Is(primaryErr, context.Canceled) {
			return primaryErr
		}

		// Frame the primary (fast-path) error with the offset it reached. This
		// error is never discarded again below — it is the real failure, and
		// WDY-1841 was caused by dropping it in favor of the fallback's error.
		primary := framePrimaryFlashError(fastPathLabel, lastWritten.Load(), writeTotal, primaryErr)

		// A device-level failure (permissions, a read-only card lock, no space,
		// the device vanished, an I/O error, a short write) means the dd fallback
		// writes to the same broken device and cannot succeed — skip it and fail
		// fast with the real error plus actionable hints. Integrity failures
		// (bmap checksum/size mismatch) and unrecognized causes still fall back.
		if isDeviceFlashFailure(primaryErr) {
			return fmt.Errorf("%w\n%s", primary, flashDeviceFailureHint)
		}

		// Bmap write failed (typically a checksum mismatch between the published
		// bmap and the actual image). Fall back to a full sequential dd write.
		// For the seekable-zstd path the .zst is already on disk — open it as a
		// sequential reader to avoid re-downloading the full image. For the regular
		// bmap path the zip is also already cached.
		var fallbackReader io.Reader
		var fallbackSize int64
		var fallbackCloser io.Closer
		switch {
		case seekableZst != "":
			si, ferr := openSeekableZstd(seekableZst)
			if ferr != nil {
				return fmt.Errorf("%w; could not open image for full-image fallback: %v", primary, ferr)
			}
			fallbackReader, fallbackSize, fallbackCloser = si, si.Size(), si
		case bmapPath != "":
			fs, ferr := openOSImageStream(deviceKey, imgInfo)
			if ferr != nil {
				return fmt.Errorf("%w; could not open image for full-image fallback: %v", primary, ferr)
			}
			fallbackReader, fallbackSize, fallbackCloser = fs, fs.uncompressedSize, fs
		default:
			// Plain dd primary (no bmap): there is no faster path to fall back
			// from, so surface the framed primary error directly.
			return primary
		}
		defer fallbackCloser.Close()
		fallbackProg := tui.NewProgress(fmt.Sprintf("Writing to %s...", targetDrive.DevicePath))
		fp := tui.NewProgressProgram(fallbackProg)
		go func() {
			fp.Send(tui.ProgressDoneMsg{Err: writeImageToDisk(fallbackReader, fallbackSize, targetDrive, func(written int64) {
				if fallbackSize > 0 {
					fp.Send(tui.ProgressUpdateMsg{
						Percent: float64(written) / float64(fallbackSize),
						Written: written,
						Total:   fallbackSize,
					})
				}
			})})
		}()
		fallbackFinal, ferr := fp.Run()
		if ferr != nil {
			return fmt.Errorf("progress TUI: %w", ferr)
		}
		if fm := fallbackFinal.(tui.ProgressModel); fm.Err() != nil {
			// Both writes failed: surface the primary (real) error AND the
			// fallback's, clearly labeled, so we never again debug the wrong one.
			return combineFlashFailure(primary, fm.Err())
		}
	}

	hasProvisioningData := provisioningRequired(provCreds, provDeviceName, provisioningJSON)
	var provisionErr error

	if !configPartitionSupported {
		// writeConfigPartition is not supported on this platform. Skip the
		// agent download — paying 5–30s of network for a guaranteed-skipped
		// step is the bug from WDY-1118.
		if hasProvisioningData {
			ejectDisk(targetDrive)
			return fmt.Errorf("the OS image was written to %s, but --wifi, --device-name, and --pre-enroll cannot be applied on this platform: writing to the device's config partition is not supported here. Re-run on a platform that supports config-partition provisioning to apply provisioning, or omit those flags to image without provisioning", targetDrive.Name)
		}
		fmt.Println("\nNote: config-partition provisioning is not yet supported on this platform; skipping. The device will run the agent baked into the image and fetch updates after first boot.")
	} else {
		fmt.Printf("\nWriting provisioning data to config partition...\n")
		// A provisioning failure is not a flash failure: the OS image already
		// landed on the drive above. provisionConfigWithRetry warns and offers
		// an interactive retry rather than aborting, so the user knows the
		// device still boots. It returns an error only when the user asked for
		// provisioning and it was not applied — reported below, after the eject,
		// so the drive is still safe to remove.
		provisionErr = provisionConfigWithRetry(targetDrive, provCreds, provDeviceName, provisioningJSON, hasProvisioningData)
	}

	ejectDisk(targetDrive)

	// Requested provisioning that never reached the card is a failed install:
	// the device would boot with none of the configuration the user asked for,
	// and reporting success here is what made WendyOS#1562 look like a device
	// bug. The image itself is fine, which the warning above already said.
	if provisionErr != nil {
		return fmt.Errorf("%s %s was written to %s, but the requested provisioning was not applied: %w",
			device.Name, imgInfo.Version, targetDrive.Name, provisionErr)
	}

	fmt.Printf("\nSuccessfully installed %s %s on %s.\n", device.Name, imgInfo.Version, targetDrive.Name)
	fmt.Println("You can now insert the drive into your device and power it on.")
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

// downloadImage downloads an OS image to a temp file behind a standalone
// progress-bar TUI. Prefer downloadImageInto when embedding the download inside
// another progress UI (it takes a plain byte callback and owns no TUI).
func downloadImage(img *imageInfo) (string, error) {
	prog := tui.NewProgress(fmt.Sprintf("Downloading %s...", img.Version))
	p := tui.NewProgressProgram(prog)
	sendProgress := throttledProgress(p, 33*time.Millisecond)

	type result struct {
		path string
		err  error
	}
	resC := make(chan result, 1)
	go func() {
		path, err := downloadImageInto(img, sendProgress)
		resC <- result{path, err}
		p.Send(tui.ProgressDoneMsg{Err: err})
	}()

	if _, err := p.Run(); err != nil {
		if r := <-resC; r.path != "" {
			os.Remove(r.path)
		}
		return "", fmt.Errorf("progress TUI: %w", err)
	}
	r := <-resC
	return r.path, r.err
}

// downloadImageInto downloads img to a temp file in the OS cache directory,
// reporting progress via onProgress(downloaded, total). It runs synchronously
// and owns no TUI, so callers can drive their own progress display (or pass a
// nil-effect callback). On any error it removes the partial file. If the server
// supports HTTP range requests it downloads in parallel across
// parallelDownloadWorkers connections; otherwise it streams sequentially.
func downloadImageInto(img *imageInfo, onProgress func(downloaded, total int64)) (string, error) {
	if onProgress == nil {
		onProgress = func(int64, int64) {}
	}
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

	contentLength, supportsRanges := probeRangeSupport(client, img)
	var dlErr error
	if supportsRanges {
		if err := tmpFile.Truncate(contentLength); err != nil {
			tmpFile.Close()
			os.Remove(tmpFile.Name())
			return "", fmt.Errorf("pre-allocating: %w", err)
		}
		dlErr = downloadParallel(client, img.DownloadURL, contentLength, tmpFile, onProgress)
	} else {
		dlErr = downloadSequential(client, img, tmpFile, onProgress)
	}
	if dlErr != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return "", dlErr
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpFile.Name())
		return "", err
	}
	return tmpFile.Name(), nil
}

// downloadSequential streams img into dst over a single connection, reporting
// progress via onProgress. Used when the server does not support range requests.
func downloadSequential(client *http.Client, img *imageInfo, dst *os.File, onProgress func(downloaded, total int64)) error {
	resp, err := client.Get(img.DownloadURL)
	if err != nil {
		return fmt.Errorf("downloading: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned status %d", resp.StatusCode)
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
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return writeErr
			}
			downloaded += int64(n)
			onProgress(downloaded, total)
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

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

// readFileOrNil reads path, returning nil on error (used for best-effort bmap
// validation where failure just disables the bmap fast path).
func readFileOrNil(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return data
}

// osCachedPath builds a cache file path for deviceKey+version, keyed by the
// storage variant ("sd"/"nvme") when one is given so an SD image and an NVMe
// image of the same device+version never share one file. ext includes the dot
// (e.g. ".img", ".zip", ".bmap", ".img.zst"). Inputs are sanitized to prevent
// path traversal from the user-supplied --version flag and the storage key.
func osCachedPath(deviceKey, version, storage, ext string) (string, error) {
	safeDevice := filepath.Base(deviceKey)
	safeVersion := filepath.Base(version)
	if safeDevice != deviceKey || safeVersion != version ||
		strings.Contains(deviceKey, "..") || strings.Contains(version, "..") {
		return "", fmt.Errorf("invalid device key or version: %q / %q", deviceKey, version)
	}
	if storage != "" {
		safeStorage := filepath.Base(storage)
		if safeStorage != storage || strings.Contains(storage, "..") {
			return "", fmt.Errorf("invalid storage key: %q", storage)
		}
	}

	dir, err := osCacheDir()
	if err != nil {
		return "", err
	}
	name := safeVersion
	if storage != "" {
		name = fmt.Sprintf("%s-%s", safeVersion, storage)
	}
	return filepath.Join(dir, fmt.Sprintf("%s-%s%s", safeDevice, name, ext)), nil
}

func osCachedImagePath(deviceKey, version, storage string) (string, error) {
	return osCachedPath(deviceKey, version, storage, ".img")
}

func osCachedZipPath(deviceKey, version, storage string) (string, error) {
	return osCachedPath(deviceKey, version, storage, ".zip")
}

func osCachedBmapPath(deviceKey, version, storage string) (string, error) {
	return osCachedPath(deviceKey, version, storage, ".bmap")
}

// manifestStorage picks the manifest storage key ("sd"/"nvme") for a target
// drive, honoring an explicit override.
//
// A real NVMe controller is unambiguous → nvme. A USB-attached drive is harder:
// an SD card in a USB reader and an NVMe SSD in a USB enclosure both enumerate
// as USB on every platform (the StorageType enum has no SD value), so bus type
// alone can't tell them apart. We disambiguate with mediaFixed — positive
// evidence from the platform lister that the media is fixed, solid-state (an
// SSD), not removable (an SD card / thumb drive). An SSD in a USB enclosure is
// bound for the device's NVMe slot, so when the device also publishes an NVMe
// variant we pick it. Without that evidence we fall back to what the manifest
// publishes via defaultManifestStorage: the SD image when the device ships one
// (Raspberry Pi), else the NVMe image (a Jetson SSD enclosure), else legacy/sd.
// Unknown buses (built-in card readers) → sd. Pass --storage to override.
func manifestStorage(v deviceVersion, st StorageType, mediaFixed bool, override string) string {
	switch override {
	case "nvme", "sd":
		return override
	}
	switch st {
	case StorageNVMe:
		return "nvme"
	case StorageUSB:
		// Fixed, solid-state media behind a USB bridge is an SSD headed for the
		// NVMe slot, not an SD card in a reader. Prefer nvme when the device
		// actually publishes that variant.
		if mediaFixed && v.NVMEPath != "" {
			return "nvme"
		}
		return defaultManifestStorage(v)
	default:
		return "sd"
	}
}

// storageChoiceAmbiguous reports whether the image variant for a USB target
// can't be determined from bus and media signals alone, so the caller should
// ask the user which slot the drive is for. It is only ambiguous when the bus
// is USB (an SD reader and an SSD enclosure look identical), the platform found
// no positive fixed-media evidence, and the device publishes BOTH variants —
// otherwise there is nothing to choose between.
func storageChoiceAmbiguous(v deviceVersion, st StorageType, mediaFixed bool) bool {
	return st == StorageUSB && !mediaFixed && v.SDPath != "" && v.NVMEPath != ""
}

// defaultManifestStorage returns the device's natural removable image variant
// based on which artifacts the manifest version publishes: an SD variant when
// present (the common Raspberry Pi case), otherwise an NVMe variant (a Jetson
// NVMe SSD), otherwise "sd" so resolveTriple falls back to the legacy image.
// Used when there is no target-drive protocol to consult (wendy os download)
// and as the ambiguous-USB tiebreaker for manifestStorage.
func defaultManifestStorage(v deviceVersion) string {
	switch {
	case v.SDPath != "":
		return "sd"
	case v.NVMEPath != "":
		return "nvme"
	default:
		return "sd"
	}
}

func osCachedZstPath(deviceKey, version, storage string) (string, error) {
	return osCachedPath(deviceKey, version, storage, ".img.zst")
}

// resolveSeekableZst downloads (or cache-hits) the seekable .img.zst for
// deviceKey+version from zstURL, returning the cached path. Reuses downloadImage
// (streams to a temp file with progress) then renames into the cache.
func resolveSeekableZst(deviceKey, version, storage, zstURL string) (string, error) {
	cached, err := osCachedZstPath(deviceKey, version, storage)
	if err != nil {
		return "", err
	}
	if info, statErr := os.Stat(cached); statErr == nil && info.Size() > 0 {
		fmt.Printf("Using cached seekable image (%s)\n", cached)
		return cached, nil
	}
	downloadPath, err := downloadImage(&imageInfo{DownloadURL: zstURL, Version: version})
	if err != nil {
		return "", fmt.Errorf("downloading seekable image: %w", err)
	}
	os.Remove(cached) // clear stale/0-byte so Rename succeeds on Windows
	if err := os.Rename(downloadPath, cached); err != nil {
		os.Remove(downloadPath)
		return "", fmt.Errorf("caching seekable image: %w", err)
	}
	return cached, nil
}

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
// the first .img, .raw, .wic, or .sdimg entry it finds. The zip directory
// stores an exact 64-bit uncompressed size, so the stream carries it for
// byte-accurate progress. The caller must Close the returned reader.
func streamZipImageEntry(zipPath string) (*imageStream, error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, fmt.Errorf("opening zip: %w", err)
	}

	for _, f := range r.File {
		if f.FileInfo().IsDir() {
			continue
		}
		ext := strings.ToLower(filepath.Ext(f.Name))
		// .sdimg is the A/B disk image RPi targets now produce.
		if ext != ".img" && ext != ".raw" && ext != ".wic" && ext != ".sdimg" {
			continue
		}

		rc, err := f.Open()
		if err != nil {
			r.Close()
			return nil, fmt.Errorf("opening %s in zip: %w", f.Name, err)
		}

		size := int64(f.UncompressedSize64)
		if size == 0 {
			size = f.FileInfo().Size()
		}
		if size == 0 {
			rc.Close()
			r.Close()
			return nil, fmt.Errorf("zip entry %s has unknown uncompressed size", f.Name)
		}

		return &imageStream{
			ReadCloser:       &zipReadCloser{archive: r, entry: rc},
			uncompressedSize: size,
		}, nil
	}

	r.Close()
	return nil, fmt.Errorf("no .img, .raw, .wic, or .sdimg file found in zip archive")
}

func resolveOSImage(deviceKey string, img *imageInfo) (string, error) {
	isZip := strings.HasSuffix(strings.ToLower(img.DownloadURL), ".zip")

	// Legacy .img cache hit (backward compat with pre-streaming caches).
	imgCached, err := osCachedImagePath(deviceKey, img.Version, img.Storage)
	if err != nil {
		return "", err
	}
	if info, statErr := os.Stat(imgCached); statErr == nil && info.Size() > 0 {
		fmt.Printf("Using cached image (%s)\n", imgCached)
		return imgCached, nil
	}

	if isZip {
		// Zip cache hit.
		zipCached, zipErr := osCachedZipPath(deviceKey, img.Version, img.Storage)
		if zipErr != nil {
			return "", zipErr
		}
		if info, statErr := os.Stat(zipCached); statErr == nil && info.Size() > 0 {
			fmt.Printf("Using cached image (%s)\n", zipCached)
			return zipCached, nil
		}
		// Cache miss: download zip, rename to zip cache path.
		downloadPath, dlErr := downloadImage(img)
		if dlErr != nil {
			return "", fmt.Errorf("downloading image: %w", dlErr)
		}
		os.Remove(zipCached) // remove stale/0-byte file if present so Rename succeeds on Windows
		if renameErr := os.Rename(downloadPath, zipCached); renameErr != nil {
			os.Remove(downloadPath)
			return "", fmt.Errorf("caching image: %w", renameErr)
		}
		return zipCached, nil
	}

	// Non-zip URL: download img directly and cache as .img.
	downloadPath, err := downloadImage(img)
	if err != nil {
		return "", fmt.Errorf("downloading image: %w", err)
	}
	os.Remove(imgCached) // remove stale/0-byte file if present so Rename succeeds on Windows
	if err := os.Rename(downloadPath, imgCached); err != nil {
		os.Remove(downloadPath)
		return "", fmt.Errorf("caching image: %w", err)
	}
	return imgCached, nil
}

// imageStream couples a streaming image reader with the size information
// available for driving a progress bar. Exactly one of two progress modes
// applies: when uncompressedSize is non-zero it is exact and the bar tracks
// decompressed bytes written; otherwise compressedRead/compressedSize track
// how much of the compressed source has been consumed.
type imageStream struct {
	io.ReadCloser
	// uncompressedSize is the exact decompressed byte count, or 0 when the
	// source cannot report one reliably.
	uncompressedSize int64
	// compressedRead reports compressed source bytes consumed so far.
	// nil when uncompressedSize is known.
	compressedRead func() int64
	// compressedSize is the total compressed source size in bytes.
	compressedSize int64
	// sourcePath is the compressed file on disk; set when uncompressedSize
	// can be determined by measureUncompressedSize.
	sourcePath string
}

// measureUncompressedSize determines the exact decompressed size by
// decompressing the source once and counting the bytes, then records it in a
// sidecar file so later flashes of the same image skip the pass. No-op when
// the size is already known or the source is not measurable. progress
// receives (compressedBytesRead, compressedTotal).
func (s *imageStream) measureUncompressedSize(progress func(read, total int64)) error {
	if s.uncompressedSize > 0 || s.sourcePath == "" {
		return nil
	}
	size, err := measureGzipImage(s.sourcePath, progress)
	if err != nil {
		return err
	}
	s.uncompressedSize = size
	writeImageSizeSidecar(s.sourcePath, s.compressedSize, size)
	return nil
}

// measureGzipImage decompresses the gzip file at path, discarding the bytes,
// and returns the exact decompressed size. This is the only reliable way to
// size a gzip stream: the ISIZE trailer is truncated mod 2^32, and the
// compressed-consumption fraction is wildly nonlinear for zero-heavy disk
// images. The pass is CPU-bound and costs a few tens of seconds for a
// multi-GiB image, against minutes of disk writing.
func measureGzipImage(path string, progress func(read, total int64)) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("opening gzip image: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat gzip image: %w", err)
	}

	cr := &countingReader{r: f}
	gr, err := gzip.NewReader(cr)
	if err != nil {
		return 0, fmt.Errorf("creating gzip reader: %w", err)
	}
	defer gr.Close()

	var size int64
	buf := make([]byte, 1<<20)
	for {
		n, readErr := gr.Read(buf)
		size += int64(n)
		if progress != nil && n > 0 {
			progress(cr.n.Load(), info.Size())
		}
		if readErr == io.EOF {
			return size, nil
		}
		if readErr != nil {
			return 0, fmt.Errorf("decompressing image: %w", readErr)
		}
	}
}

// measureImageWithProgress runs stream.measureUncompressedSize behind a
// progress bar tracking compressed bytes read. Returns context.Canceled when
// the user quits the bar.
func measureImageWithProgress(stream *imageStream) error {
	prog := tui.NewProgress("Determining image size (one-time per image)...")
	p := tui.NewProgressProgram(prog)
	sendProgress := throttledProgress(p, 33*time.Millisecond)

	go func() {
		p.Send(tui.ProgressDoneMsg{Err: stream.measureUncompressedSize(sendProgress)})
	}()

	final, err := p.Run()
	if err != nil {
		return fmt.Errorf("progress TUI: %w", err)
	}
	return final.(tui.ProgressModel).Err()
}

// imageSizeSidecar caches a measured decompressed size next to the compressed
// image, keyed to the compressed size so a replaced file invalidates it.
type imageSizeSidecar struct {
	CompressedSize   int64 `json:"compressed_size"`
	UncompressedSize int64 `json:"uncompressed_size"`
}

func imageSizeSidecarPath(imagePath string) string { return imagePath + ".size" }

// readImageSizeSidecar returns the cached decompressed size for imagePath,
// or 0 when no valid sidecar exists for the given compressed size.
func readImageSizeSidecar(imagePath string, compressedSize int64) int64 {
	data, err := os.ReadFile(imageSizeSidecarPath(imagePath))
	if err != nil {
		return 0
	}
	var sc imageSizeSidecar
	if json.Unmarshal(data, &sc) != nil || sc.CompressedSize != compressedSize || sc.UncompressedSize <= 0 {
		return 0
	}
	return sc.UncompressedSize
}

// writeImageSizeSidecar persists a measured size. Best-effort: a failure just
// means the next flash measures again.
func writeImageSizeSidecar(imagePath string, compressedSize, uncompressedSize int64) {
	data, err := json.Marshal(imageSizeSidecar{
		CompressedSize:   compressedSize,
		UncompressedSize: uncompressedSize,
	})
	if err != nil {
		return
	}
	_ = os.WriteFile(imageSizeSidecarPath(imagePath), data, 0o644)
}

// writeProgressMsg converts dd's running byte count into a progress update.
// Returns false when the stream carries no usable size information.
func (s *imageStream) writeProgressMsg(written int64) (tui.ProgressUpdateMsg, bool) {
	switch {
	case s.uncompressedSize > 0:
		return tui.ProgressUpdateMsg{
			Percent: float64(written) / float64(s.uncompressedSize),
			Written: written,
			Total:   s.uncompressedSize,
		}, true
	case s.compressedRead != nil && s.compressedSize > 0:
		// Decompression is demand-driven by dd's reads, so the fraction of
		// compressed input consumed tracks write completion. Total stays 0:
		// the decompressed size is unknown and must not be guessed at.
		return tui.ProgressUpdateMsg{
			Percent: float64(s.compressedRead()) / float64(s.compressedSize),
			Written: written,
		}, true
	default:
		return tui.ProgressUpdateMsg{}, false
	}
}

// countingReader counts bytes read through it. The count is read from a
// progress goroutine while Read is called from the dd-feeding goroutine,
// hence the atomic.
type countingReader struct {
	r io.Reader
	n atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// gzipReadCloser wraps a gzip.Reader so that closing it also closes the
// underlying file, matching the io.ReadCloser contract.
type gzipReadCloser struct {
	gz *gzip.Reader
	f  *os.File
}

func (g *gzipReadCloser) Read(p []byte) (int, error) { return g.gz.Read(p) }
func (g *gzipReadCloser) Close() error {
	err := g.gz.Close()
	if err2 := g.f.Close(); err == nil {
		err = err2
	}
	return err
}

// streamGzipImage opens a gzip-compressed image file and returns a streaming
// reader over the decompressed bytes. The gzip format cannot report the
// uncompressed size of large images — its ISIZE trailer is stored mod 2^32,
// so a 19.2 GiB image reports 3.2 GiB and the progress bar overshoots its
// total. The exact size comes from a sidecar written by a previous
// measureUncompressedSize pass when available; otherwise it is 0 and the
// caller should measure (or fall back to compressed-consumption progress).
func streamGzipImage(path string) (*imageStream, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening gzip image: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat gzip image: %w", err)
	}

	cr := &countingReader{r: f}
	gr, err := gzip.NewReader(cr)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("creating gzip reader: %w", err)
	}
	return &imageStream{
		ReadCloser:       &gzipReadCloser{gz: gr, f: f},
		uncompressedSize: readImageSizeSidecar(path, info.Size()),
		compressedRead:   cr.n.Load,
		compressedSize:   info.Size(),
		sourcePath:       path,
	}, nil
}

// isGzipFile returns true when path begins with the gzip magic bytes (0x1f 0x8b).
// Used to detect compressed images regardless of file extension — GCS images
// are sometimes served as .img.gz but cached under the .img extension.
func isGzipFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [2]byte
	_, err = io.ReadFull(f, magic[:])
	return err == nil && magic[0] == 0x1f && magic[1] == 0x8b
}

// VM downloads and content-addressed cache entries have extensionless format
// names (.img/.image). Identify ZIP containers by their signature as well.
func isZipFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	if _, err := io.ReadFull(f, magic[:]); err != nil {
		return false
	}
	return string(magic[:]) == "PK\x03\x04" || string(magic[:]) == "PK\x05\x06" || string(magic[:]) == "PK\x07\x08"
}

// openOSImageStream resolves the cached file for deviceKey+img, then returns
// a streaming reader over the image bytes. The caller must Close it.
func openOSImageStream(deviceKey string, img *imageInfo) (*imageStream, error) {
	cachePath, err := resolveOSImage(deviceKey, img)
	if err != nil {
		return nil, err
	}
	return openLocalImageStream(cachePath)
}

// openLocalImageStream opens an arbitrary local file for streaming.
// ZIP, gzip and zstd are detected by content, independently of cache filenames.
// ZIP archives yield their first image entry; other files are raw disk images.
func openLocalImageStream(imagePath string) (*imageStream, error) {
	if strings.HasSuffix(strings.ToLower(imagePath), ".zip") || isZipFile(imagePath) {
		return streamZipImageEntry(imagePath)
	}
	if isGzipFile(imagePath) {
		return streamGzipImage(imagePath)
	}
	if isZstdFile(imagePath) {
		return streamZstdImage(imagePath)
	}
	return openRawImageStream(imagePath)
}

// openRawImageStream opens an uncompressed image file; its size on disk is
// the exact byte count that will be written.
func openRawImageStream(path string) (*imageStream, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening image: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("stat image: %w", err)
	}
	return &imageStream{ReadCloser: f, uncompressedSize: info.Size()}, nil
}

// wifiCLIOptions captures the WiFi-related flags coming from cobra so they
// can be threaded through as a single value.
type wifiCLIOptions struct {
	SSID     string   // --wifi-ssid shortcut
	Password string   // --wifi-password (only valid with --wifi-ssid)
	Entries  []string // --wifi, repeatable
	NoWifi   bool     // --no-wifi
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
				pw, perr := tui.PromptPassword(fmt.Sprintf("WiFi password for %s", c.SSID), "(leave empty for open network)", nil)
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

// Interactive hooks used by promptAddOneCredential, declared as package vars
// so unit tests can stub them (same pattern as promptDeviceName below).
var (
	selectWifiNetworkFromScan = selectWifiNetworkStreaming
	confirmManualWifiEntry    = func() (bool, error) {
		return tui.ConfirmDefaultYes("Enter WiFi network details manually?")
	}
	confirmKeychainLookup = func(ssid string) (bool, error) {
		return tui.ConfirmDefaultYes(fmt.Sprintf("Look up password for '%s' from keychain? (macOS will ask for permission)", ssid))
	}
	promptWifiSSID = func() (string, error) {
		return tui.PromptText("WiFi SSID", "", nonEmptyValidator)
	}
	promptWifiPassword = func(ssid string) (string, error) {
		return tui.PromptPassword(fmt.Sprintf("Password for %s", ssid), "(leave empty for open network)", nil)
	}
)

// wifiScanSelection is the outcome of the streaming scan-and-pick step.
type wifiScanSelection struct {
	SSID        string // empty when the user made no selection
	HadNetworks bool   // the scan returned at least one network
	ScanErr     error  // scan failure recorded while the picker was open
}

// selectWifiNetworkStreaming shows the WiFi picker immediately and streams
// the scan results in: the CoreWLAN/nmcli/netsh scan can take several
// seconds, and a visible "Scanning..." list reads better than blocking
// before any UI appears.
func selectWifiNetworkStreaming() (wifiScanSelection, error) {
	var sel wifiScanSelection

	picker := tui.NewPickerWithTitleAndColumns("Select WiFi network (or esc to type manually)", wifiPickerColumns())
	picker.Filterable = true
	p := tea.NewProgram(picker)

	// The user can quit the picker before the scan goroutine finishes, so
	// every access to sel is mutex-guarded.
	var mu sync.Mutex
	go func() {
		defer p.Send(tui.PickerDoneMsg{})
		// Stream the host scan: cached results paint the picker instantly, then
		// the fresh rescan fills it in — so SSIDs trickle in rather than the
		// picker sitting on "Scanning..." until the whole scan completes
		// (matching the device-side picker in pickWifiNetwork).
		hadNetworks := false
		err := streamLocalWifiScan(func(batch []localWifiNetwork) {
			if len(batch) > 0 {
				hadNetworks = true
			}
			p.Send(tui.PickerAddMsg{Items: localWifiPickerItems(batch)})
		})
		mu.Lock()
		sel.ScanErr = err
		sel.HadNetworks = hadNetworks
		mu.Unlock()
	}()
	fmt.Println()
	finalModel, runErr := p.Run()
	if runErr != nil {
		return wifiScanSelection{}, fmt.Errorf("scanning WiFi networks: %w", runErr)
	}
	pm, ok := finalModel.(tui.PickerModel)
	if !ok {
		return wifiScanSelection{}, fmt.Errorf("scanning WiFi networks: unexpected picker model %T", finalModel)
	}
	mu.Lock()
	defer mu.Unlock()
	if picked := pm.Selected(); picked != nil {
		sel.SSID, _ = picked.Value.(string)
	}
	return sel, nil
}

// promptAddOneCredential runs the local scan + picker + password prompt to
// collect a single WiFi credential. index is the zero-based count of entries
// already collected (used to suggest a descending priority). Returns
// added=false (with nil error) when the user chooses to skip WiFi setup
// after a failed or empty scan (WDY-1474).
func promptAddOneCredential(index int) (wendyconf.WifiCredential, bool, error) {
	var c wendyconf.WifiCredential

	sel, err := selectWifiNetworkFromScan()
	if err != nil {
		return c, false, err
	}
	c.SSID = sel.SSID

	// No selection and nothing usable was scanned: say why, then let the
	// user choose between manual entry and skipping WiFi setup entirely
	// instead of trapping them in a mandatory SSID prompt (WDY-1474). A
	// deliberate esc while networks are listed falls straight through to
	// the manual prompt, as the picker title advertises.
	if c.SSID == "" && (sel.ScanErr != nil || !sel.HadNetworks) {
		fmt.Println(wifiScanFailureNotice(sel.ScanErr))
		manual, err := confirmManualWifiEntry()
		if err != nil {
			return c, false, err
		}
		if !manual {
			fmt.Println("Skipping WiFi setup. You can configure WiFi later with 'wendy wifi connect' once the device is reachable (e.g. over ethernet or USB).")
			return c, false, nil
		}
	}

	if c.SSID == "" {
		ssid, err := promptWifiSSID()
		if err != nil {
			return c, false, err
		}
		c.SSID = ssid
	}

	if supportsKeychainLookup {
		useKeychain, err := confirmKeychainLookup(c.SSID)
		if err != nil {
			return c, false, err
		}
		if useKeychain {
			if pw, kerr := lookupKeychainPassword(c.SSID); kerr == nil && pw != "" {
				fmt.Println("Using saved password from keychain.")
				c.Password = pw
			} else {
				fmt.Println("Password not available from keychain.")
			}
		}
	}

	if c.Password == "" {
		pw, err := promptWifiPassword(c.SSID)
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

// maxDeviceNameLen caps the device name so the derived hostname stays a valid
// DNS label. The agent builds the hostname as "wendyos-<name>" (see
// generate-hostname.sh / configpartition.applyDeviceName); with the 8-character
// "wendyos-" prefix, a 55-character name yields a 63-octet label — the RFC 1035
// maximum. Longer names produce an invalid hostname label on the device.
const maxDeviceNameLen = 55

func validateDeviceName(name string) error {
	if len(name) < 3 || len(name) > maxDeviceNameLen {
		return fmt.Errorf("device name must be 3–%d characters", maxDeviceNameLen)
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

func optionalDeviceNameValidator(name string) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	return validateDeviceName(name)
}

var promptDeviceName = func(prompt, hint string, validate tui.ValidateFunc) (string, error) {
	return tui.PromptText(prompt, hint, validate)
}

func resolveDeviceName(flagName string) (string, error) {
	if flagName != "" {
		if err := validateDeviceName(flagName); err != nil {
			return "", fmt.Errorf("--device-name: %w", err)
		}
		return flagName, nil
	}

	if !isInteractiveTerminal() {
		return "", nil
	}

	fmt.Println()
	name, err := promptDeviceName(
		"Device name",
		fmt.Sprintf("(a-z, 0-9 and hyphens, starts with a letter, 3–%d chars; empty = auto-generate)", maxDeviceNameLen),
		optionalDeviceNameValidator,
	)
	if err != nil {
		if errors.Is(err, tui.ErrCancelled) {
			return "", ErrUserCancelled
		}
		return "", fmt.Errorf("device-name prompt: %w", err)
	}
	return strings.TrimSpace(name), nil
}

// resolveProvisioningJSON runs pre-enrollment per preOpts and returns the
// marshaled provisioning.json, or nil when enrollment is skipped or not available.
func resolveProvisioningJSON(ctx context.Context, preOpts preEnrollOptions, deviceName string) ([]byte, error) {
	if preOpts.mode == preEnrollSkip {
		return nil, nil
	}
	cfg, err := config.Load()
	if err != nil {
		if preOpts.mode == preEnrollForced {
			return nil, fmt.Errorf("--pre-enroll: loading config: %w", err)
		}
		cfg = &config.Config{} // auto mode: treat an unreadable config as not logged in
	}
	prov, err := resolvePreEnrollment(ctx, cfg, preOpts, isInteractiveTerminal(), deviceName)
	if err != nil || prov == nil {
		return nil, err
	}
	return json.Marshal(prov)
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

// provisionConfigPartitionFn is the provisioning entry point used by
// provisionConfigWithRetry. It is a package var so tests can stub the real
// network + disk work it performs.
var provisionConfigPartitionFn = provisionConfigPartition

// confirmProvisioningRetry asks whether to re-attempt config-partition
// provisioning after a failure. Declared as a var so tests can drive the loop.
var confirmProvisioningRetry = func() (bool, error) {
	return tui.Confirm("Retry writing provisioning data to the config partition?")
}

// provisionConfigWithRetry writes provisioning data — the agent binary, WiFi
// credentials, device name, and pre-enrollment material — to the config
// partition of a freshly imaged drive.
//
// A provisioning failure is never fatal. By the time we get here the OS image
// is already written to the drive, so the device boots regardless: it runs the
// agent baked into the image and fetches updates and configuration after first
// boot. Treating "couldn't download the agent update" or "couldn't locate the
// config partition" as a failure to flash the OS is misleading — so we surface
// it as a warning instead, loudly when the user explicitly asked for --wifi /
// --device-name / --pre-enroll (that input did not reach the device), and on an
// interactive terminal we offer to retry — e.g. after re-seating an SD card
// whose config partition could not be located.
// It returns the final provisioning error when the user explicitly asked for
// provisioning and it was not applied, so the caller can exit non-zero instead
// of printing "Successfully installed" over dropped input (WendyOS#1562: the
// config-partition write failed, --wifi and --device-name never reached the
// card, and the install still reported success — which read as the device
// ignoring its configuration rather than never receiving it). When provisioning
// was not requested it stays advisory and returns nil.
func provisionConfigWithRetry(d drive, creds []wendyconf.WifiCredential, deviceName string, provisioningJSON []byte, requested bool) error {
	for {
		err := provisionConfigPartitionFn(d, creds, deviceName, provisioningJSON)
		if err == nil {
			return nil
		}

		if requested {
			fmt.Printf("\nWarning: could not apply provisioning data — --wifi / --device-name / --pre-enroll were requested but not written: %v\n", err)
		} else {
			fmt.Printf("\nWarning: could not pre-configure the agent on the config partition: %v\n", err)
		}
		fmt.Println("The OS image itself was written successfully — the device will still boot, run the agent baked into the image, and fetch updates after first boot.")

		if !isInteractiveTerminal() {
			if requested {
				fmt.Println("Re-run 'wendy os install' to apply WiFi / device-name / pre-enrollment, or configure the device after it boots.")
				return err
			}
			return nil
		}

		retry, confirmErr := confirmProvisioningRetry()
		if confirmErr != nil || !retry {
			if requested {
				fmt.Println("Skipping provisioning. Re-run 'wendy os install' to apply it, or configure the device after it boots.")
				return err
			}
			return nil
		}
	}
}

// provisionConfigPartition downloads the latest stable arm64 wendy-agent binary
func provisionConfigPartition(d drive, creds []wendyconf.WifiCredential, deviceName string, provisioningJSON []byte) error {
	fmt.Printf("Downloading wendy-agent for device...\n")
	agentBinary, agentVer, _, err := resolveAgentArtifact("linux", "arm64", false)
	if err != nil {
		return fmt.Errorf("resolving agent binary: %w", err)
	}
	fmt.Printf("Using wendy-agent %s\n", agentVer)

	return writeConfigPartition(d, agentBinary, creds, deviceName, provisioningJSON)
}

// maxFlashAttempts bounds installESP32Firmware's retry loop so a permanently
// unresolvable failure (an unkillable port holder, a device that never comes
// back) stops instead of looping on default-Yes prompts forever.
const maxFlashAttempts = 5

// flashRetryAction decides what happens after a flash attempt finished with
// flashErr. Pure — no prompting, no I/O — so the rules stay testable without
// hardware. Exactly one of the results is meaningful:
//
//	retry=false, fatal=nil   the flash succeeded; stop
//	retry=false, fatal!=nil  give up and return fatal
//	retry=true,  fatal=nil   the caller may run its interactive retry flow
func flashRetryAction(flashErr error, interactive bool) (retry bool, fatal error) {
	if flashErr == nil {
		return false, nil
	}

	// User cancellation is not a flash failure. tui.ProgressModel reports q,
	// Ctrl+C and SIGINT as context.Canceled, and the retry prompt defaults to
	// Yes — without this, one Enter after Ctrl+C restarts the very flash the
	// user just stopped. Same guard the disk-flash path above uses.
	if errors.Is(flashErr, context.Canceled) {
		return false, flashErr
	}

	if !interactive {
		return false, fmt.Errorf("flashing failed: %w", flashErr)
	}
	return true, nil
}

// installESP32Firmware handles the ESP32 path: detect device → download → flash.
// board is a Wendy Lite catalog board name, e.g. "esp32c6_generic". An empty
// serialDevice means the device should be located by scanning install-capable
// serial ports. A supplied device carries its already-known transport.
func installESP32Firmware(ctx context.Context, nightly bool, board string, serialDevice discovery.SerialPortInfo, wifi wifiCLIOptions, deviceName string, preOpts preEnrollOptions) error {
	target, err := WendyLiteTargetForBoard(board)
	if err != nil {
		return err
	}
	expectedChip, err := chipModelForTarget(target)
	if err != nil {
		return err
	}

	provCreds, err := resolveWiFiCredentialsList(wifi)
	if err != nil {
		return err
	}

	provDeviceName, err := resolveDeviceName(deviceName)
	if err != nil {
		return err
	}

	var enrolledState *PreProvisionedState
	if preOpts.mode != preEnrollSkip {
		cfg, cfgErr := config.Load()
		if cfgErr != nil {
			if preOpts.mode == preEnrollForced {
				return fmt.Errorf("--pre-enroll: loading config: %w", cfgErr)
			}
			cfg = &config.Config{} // auto mode: treat an unreadable config as not logged in
		}
		var resolveErr error
		enrolledState, resolveErr = resolvePreEnrollment(ctx, cfg, preOpts, isInteractiveTerminal(), provDeviceName)
		if resolveErr != nil {
			return resolveErr
		}
	}

	wendyConf, err := buildWendyConf(provCreds, provDeviceName, enrolledState)
	if err != nil {
		return fmt.Errorf("building Wendy config: %w", err)
	}

	if wendyConf.Wifi != nil && len(wendyConf.Wifi.Networks) > 1 {
		return fmt.Errorf("this device only supports one Wi-Fi network")
	}

	// An empty serial device means "find it for me", which stays true across
	// retries below: a reset re-enumerates the USB device and the node can
	// come back under a different path. An explicitly supplied device is used
	// verbatim on every attempt.
	autoDiscoverPort := serialDevice.Port == ""
	if autoDiscoverPort {
		fmt.Println("\nScanning for ESP32 devices...")

		// A device that's rebooting on its own can drop off and reappear on
		// the USB bus as it re-enumerates, so a single scan can miss it.
		// Retry briefly rather than failing on the very first attempt.
		deadline := time.Now().Add(1 * time.Second)
		for {
			serialDevice, err = discovery.ResolveESP32InstallSerialPort()
			if err == nil || time.Now().After(deadline) {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			fmt.Println("\nNo ESP32 device detected.")
			fmt.Println("Make sure your ESP32 is connected via USB and in bootloader mode.")
			fmt.Println("To enter bootloader mode: hold the BOOT button, press RESET, then release BOOT.")
			return fmt.Errorf("ESP32 not found: %w", err)
		}
	}

	fmt.Println(esp32InstallCandidateMessage(serialDevice))
	if serialDevice.Transport == discovery.SerialTransportUARTBridge {
		fmt.Fprintln(os.Stderr, tui.WarningMessage(esp32UARTBridgeInstallWarning(len(provCreds) > 0)))
		if requiresESP32UARTConfirmation(serialDevice, len(provCreds) > 0, isInteractiveTerminal()) &&
			!confirmDefaultNoFn("Continue flashing through USB-UART without WiFi?") {
			return ErrUserCancelled
		}
	}

	fmt.Println("Fetching latest Wendy Lite firmware...")
	firmwareID, err := WendyLiteFirmwareID(board)
	if err != nil {
		return err
	}
	asset, err := fetchFirmwareFromManifest(firmwareID, nightly)
	if err != nil {
		return fmt.Errorf("fetching firmware: %w", err)
	}
	fmt.Printf("Found firmware: %s v%s\n", asset.Name, asset.Version)

	// Reuse a cached download from a previous run when one exists; otherwise
	// download with a progress bar. The cached file persists across runs and
	// is only removed via `wendy cache clear`/`cache list`.
	fwPath, err := resolveFirmware(asset)
	if err != nil {
		return fmt.Errorf("resolving firmware: %w", err)
	}

	// Include configuration into the flash image.

	img, err := LoadEspFlashImage(fwPath)
	if err != nil {
		return fmt.Errorf("loading firmware image: %w", err)
	}

	confBytes, err := proto.Marshal(wendyConf)
	if err != nil {
		return fmt.Errorf("serializing device config: %w", err)
	}
	payload := make([]byte, 8+len(confBytes))
	copy(payload[0:4], "WYC0")
	binary.LittleEndian.PutUint32(payload[4:8], uint32(len(confBytes)))
	copy(payload[8:], confBytes)
	if err := img.SetPartition("wendy_conf", payload); err != nil {
		return fmt.Errorf("writing config to firmware image: %w", err)
	}

	// Flash with progress bar. Retries on failure: a busy port gets an extra
	// offer to identify and kill whatever holds it; any other failure just
	// gets the plain retry prompt. Prompting is skipped entirely when the run
	// isn't interactive or is producing JSON, so those fail immediately on the
	// first attempt, unchanged from before this loop existed.
	interactive := !jsonOutput && isInteractiveTerminal()
	for attempt := 1; ; attempt++ {
		// Native USB reset can re-enumerate at a different path (macOS:
		// /dev/cu.usbmodem101 → /dev/cu.usbmodem1101). Re-scan install-capable
		// ports before each retry so both the path and transport stay current.
		// A failed re-scan keeps the last known candidate and lets the attempt
		// report the real error.
		if attempt > 1 && autoDiscoverPort {
			if rescanned, rescanErr := discovery.ResolveESP32InstallSerialPort(); rescanErr == nil && rescanned != serialDevice {
				fmt.Printf("ESP32 reappeared at %s\n", rescanned.Port)
				serialDevice = rescanned
			}
		}

		fmt.Println()
		flashProg := tui.NewProgress(fmt.Sprintf("Flashing to %s...", serialDevice.Port))
		fp := tui.NewProgressProgram(flashProg)

		go func() {
			flashErr := flashFirmwareImage(serialDevice.Port, img, serialDevice.Transport, expectedChip, func(pct float64) {
				fp.Send(tui.ProgressUpdateMsg{Percent: pct})
			})
			fp.Send(tui.ProgressDoneMsg{Err: flashErr})
		}()

		flashFinal, err := fp.Run()
		if err != nil {
			return fmt.Errorf("flash TUI: %w", err)
		}

		flashModel := flashFinal.(tui.ProgressModel)
		flashErr := flashModel.Err()

		retry, fatal := flashRetryAction(flashErr, interactive)
		if fatal != nil {
			return fatal
		}
		if !retry {
			break
		}

		// Cap the attempts: a holder that can't be killed (root-owned, say)
		// would otherwise fail busy identically forever, and every prompt in
		// this loop defaults to Yes.
		if attempt >= maxFlashAttempts {
			return fmt.Errorf("flashing failed after %d attempts: %w", attempt, flashErr)
		}

		retriedAutomatically := false
		if errors.Is(flashErr, errPortBusy) {
			retriedAutomatically = offerPortBusyRetry(serialDevice.Port)
		}
		if !retriedAutomatically && !confirmFn("Do you want to try again?") {
			return fmt.Errorf("flashing failed: %w", flashErr)
		}
	}

	fmt.Printf("\nSuccessfully flashed Wendy Lite %s!\n", asset.Version)
	fmt.Println("The device will reboot automatically.")
	return nil
}

func esp32InstallCandidateMessage(device discovery.SerialPortInfo) string {
	if device.Transport == discovery.SerialTransportUARTBridge {
		return fmt.Sprintf("Found USB-UART candidate at %s (the ESP32 model will be verified before writing)", device.Port)
	}
	return fmt.Sprintf("Found native ESP32 USB candidate at %s (the ESP32 model will be verified before writing)", device.Port)
}

func esp32UARTBridgeInstallWarning(hasWiFi bool) string {
	const base = "This USB-UART connection can flash Wendy Lite, but it cannot carry Wendy's runtime USB connection after installation."
	if hasWiFi {
		return base + " Wendy will rely on the configured WiFi network, or you can reconnect using the board's native USB port."
	}
	return base + " No WiFi network is configured, so reconnect using the board's native USB port or reinstall with WiFi to manage the device."
}

func requiresESP32UARTConfirmation(device discovery.SerialPortInfo, hasWiFi, interactive bool) bool {
	return device.Transport == discovery.SerialTransportUARTBridge && !hasWiFi && interactive
}
