//go:build darwin || linux

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
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/internal/shared/discovery"
)

func newOSInstallCmd() *cobra.Command {
	var nightly bool
	var force bool

	cmd := &cobra.Command{
		Use:   "install [image] [drive]",
		Short: "Install WendyOS or Wendy Lite firmware on a device",
		Long: `Interactively select a supported device, download the latest OS image or firmware, and write it to the target.

When called with positional arguments, skips interactive prompts:
  wendy os install <image-path> <drive-id> --force`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 2 {
				return runOSInstallDirect(args[0], args[1], force)
			}
			return runOSInstall(cmd.Context(), nightly)
		},
	}

	cmd.Flags().BoolVar(&nightly, "nightly", false, "Use nightly/prerelease builds")
	cmd.Flags().BoolVar(&force, "force", false, "Skip confirmation prompt")

	return cmd
}

// runOSInstallDirect writes a local image file to the specified drive without interactive prompts.
func runOSInstallDirect(imagePath string, driveID string, force bool) error {
	// Verify the image file exists.
	if _, err := os.Stat(imagePath); err != nil {
		return fmt.Errorf("image file: %w", err)
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

	if !force {
		reader := bufio.NewReader(os.Stdin)
		fmt.Printf("Writing will ERASE ALL DATA on %s (%s). Continue? [y/N] ", targetDrive.Name, targetDrive.DevicePath)
		line, err := reader.ReadString('\n')
		if err != nil {
			return err
		}
		if answer := strings.TrimSpace(strings.ToLower(line)); answer != "y" && answer != "yes" {
			fmt.Println("Cancelled.")
			return nil
		}
	}

	fmt.Printf("Writing image to %s...\n", targetDrive.DevicePath)
	fmt.Println("You may be prompted for your password (sudo is required).")
	if err := writeImageToDisk(imagePath, *targetDrive, nil); err != nil {
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
	Category   string // e.g. "Linux" or "Wendy Lite"
	IsESP32    bool
	ESP32Chip  string          // e.g. "esp32c6", "esp32c5"
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

func runOSInstall(ctx context.Context, nightly bool) error {
	fmt.Println("Fetching available devices...")

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

	// Add ESP32 entries.
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

	if len(items) == 0 {
		return fmt.Errorf("no devices available")
	}

	fmt.Println()
	selected, err := pickFromItems("Select a device", items)
	if err != nil {
		return err
	}

	device := deviceMap[selected]

	if device.IsESP32 {
		return installESP32Firmware(ctx, nightly, device.ESP32Chip)
	}
	return installLinuxImage(ctx, selected, device)
}

// installLinuxImage handles the Linux device path: pick drive → download → write.
func installLinuxImage(ctx context.Context, deviceKey string, device pickerDevice) error {
	// List external drives.
	drives, err := listExternalDrives()
	if err != nil {
		return fmt.Errorf("listing drives: %w", err)
	}
	if len(drives) == 0 {
		return fmt.Errorf("no external drives found — insert an SD card or USB drive and try again")
	}

	// Drive picker.
	var driveItems []tui.PickerItem
	driveMap := make(map[string]drive)
	for _, d := range drives {
		desc := d.DevicePath
		if d.Size != "" {
			desc += "  " + d.Size
		}
		driveItems = append(driveItems, tui.PickerItem{
			Name:        d.Name,
			Description: desc,
			Value:       d.DevicePath,
		})
		driveMap[d.DevicePath] = d
	}

	fmt.Println()
	sel, err := pickFromItems("Select target drive", driveItems)
	if err != nil {
		return err
	}
	targetDrive := driveMap[sel]

	// Confirm destructive write.
	reader := bufio.NewReader(os.Stdin)
	fmt.Printf("\nWriting will ERASE ALL DATA on %s (%s). Continue? [y/N] ", targetDrive.Name, targetDrive.DevicePath)
	line, err := reader.ReadString('\n')
	if err != nil {
		return err
	}
	if answer := strings.TrimSpace(strings.ToLower(line)); answer != "y" && answer != "yes" {
		fmt.Println("Cancelled.")
		return nil
	}

	// Resolve image (cached or download).
	fmt.Printf("\nPreparing %s image...\n", device.Name)
	imgInfo, err := getImageInfo(device.Manifest, device.RawVersion)
	if err != nil {
		return fmt.Errorf("getting image info: %w", err)
	}

	imagePath, err := resolveOSImage(deviceKey, imgInfo)
	if err != nil {
		return fmt.Errorf("resolving OS image: %w", err)
	}

	// Get image size for progress tracking.
	imgStat, err := os.Stat(imagePath)
	if err != nil {
		return fmt.Errorf("stat image: %w", err)
	}
	totalSize := imgStat.Size()

	// Pre-authenticate sudo so the password prompt works on the raw terminal
	// before we start the Bubble Tea TUI.
	fmt.Println("You may be prompted for your password (sudo is required).")
	if err := exec.Command("sudo", "-v").Run(); err != nil {
		return fmt.Errorf("sudo authentication failed: %w", err)
	}

	// Write image to drive with progress bar.
	fmt.Printf("Writing image to %s...\n", targetDrive.DevicePath)
	writeProg := tui.NewProgress(fmt.Sprintf("Writing to %s...", targetDrive.DevicePath))
	wp := tea.NewProgram(writeProg)

	go func() {
		writeErr := writeImageToDisk(imagePath, targetDrive, func(written int64) {
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

	fmt.Printf("\nSuccessfully installed %s %s on %s.\n", device.Name, imgInfo.Version, targetDrive.Name)
	fmt.Println("You can now insert the drive into your device and power it on.")
	return nil
}

// downloadImage downloads an OS image to a temp file with a progress bar.
func downloadImage(img *imageInfo) (string, error) {
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Get(img.DownloadURL)
	if err != nil {
		return "", fmt.Errorf("downloading: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned status %d", resp.StatusCode)
	}

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

	total := resp.ContentLength
	if img.ImageSize > 0 {
		total = img.ImageSize
	}

	prog := tui.NewProgress(fmt.Sprintf("Downloading %s...", img.Version))
	p := tea.NewProgram(prog)

	var downloaded int64
	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, readErr := resp.Body.Read(buf)
			if n > 0 {
				if _, writeErr := tmpFile.Write(buf[:n]); writeErr != nil {
					p.Send(tui.ProgressDoneMsg{Err: writeErr})
					return
				}
				downloaded += int64(n)
				if total > 0 {
					p.Send(tui.ProgressUpdateMsg{
						Percent: float64(downloaded) / float64(total),
						Written: downloaded,
						Total:   total,
					})
				}
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
					if totalSize > 0 {
						p.Send(tui.ProgressUpdateMsg{
							Percent: float64(extracted) / float64(totalSize),
							Written: extracted,
							Total:   totalSize,
						})
					}
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
// Format: <cache>/os-images/<device>-<version>.img
func osCachedImagePath(deviceKey, version string) (string, error) {
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
	return filepath.Join(dir, fmt.Sprintf("%s-%s.img", safeDevice, safeVersion)), nil
}

// resolveOSImage returns the path to a ready-to-write .img file.
// It checks the local cache first; on a miss it downloads (and extracts if
// zipped), then stores the result in the cache.
func resolveOSImage(deviceKey string, img *imageInfo) (string, error) {
	cached, err := osCachedImagePath(deviceKey, img.Version)
	if err != nil {
		return "", err
	}

	// Cache hit.
	if info, statErr := os.Stat(cached); statErr == nil && info.Size() > 0 {
		fmt.Printf("Using cached image (%s)\n", cached)
		return cached, nil
	}

	// Download.
	downloadPath, err := downloadImage(img)
	if err != nil {
		return "", fmt.Errorf("downloading image: %w", err)
	}

	// Extract from zip if needed, otherwise the download is the image.
	imagePath := downloadPath
	if strings.HasSuffix(strings.ToLower(img.DownloadURL), ".zip") {
		extracted, err := extractImageFromZipWithProgress(downloadPath)
		os.Remove(downloadPath) // zip no longer needed
		if err != nil {
			return "", fmt.Errorf("extracting image: %w", err)
		}
		imagePath = extracted
	}

	// Move into cache. Both files are in the same cache directory so
	// Rename is always a same-filesystem operation.
	if err := os.Rename(imagePath, cached); err != nil {
		os.Remove(imagePath)
		return "", fmt.Errorf("caching image: %w", err)
	}

	return cached, nil
}

// installESP32Firmware handles the ESP32 path: detect device → download → flash.
// chip is e.g. "esp32c6" or "esp32c5".
func installESP32Firmware(ctx context.Context, nightly bool, chip string) error {
	fmt.Println("\nScanning for ESP32 devices...")

	serialPort, err := discovery.ResolveESP32SerialPort()
	if err != nil {
		fmt.Println("\nNo ESP32 device detected.")
		fmt.Println("Make sure your ESP32 is connected via USB and in bootloader mode.")
		fmt.Println("To enter bootloader mode: hold the BOOT button, press RESET, then release BOOT.")
		return fmt.Errorf("ESP32 not found: %w", err)
	}

	fmt.Printf("Found ESP32 at %s\n", serialPort)

	fmt.Println("Fetching latest Wendy Lite firmware...")
	asset, err := fetchFirmwareFromManifest(chip, nightly)
	if err != nil {
		return fmt.Errorf("fetching firmware: %w", err)
	}
	fmt.Printf("Found firmware: %s v%s\n", asset.Name, asset.Version)

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

	fmt.Printf("\nSuccessfully flashed Wendy Lite %s!\n", asset.Version)
	fmt.Println("The device will reboot automatically.")
	return nil
}
