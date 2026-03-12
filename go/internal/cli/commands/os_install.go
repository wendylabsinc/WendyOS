//go:build darwin || linux

package commands

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/discovery"
)

func newOSInstallCmd() *cobra.Command {
	var nightly bool

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install WendyOS or Wendy Lite firmware on a device",
		Long:  "Interactively select a supported device, download the latest OS image or firmware, and write it to the target.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOSInstall(cmd.Context(), nightly)
		},
	}

	cmd.Flags().BoolVar(&nightly, "nightly", false, "Use nightly/prerelease builds")

	return cmd
}

// pickerDevice is a unified entry for the device selection picker.
type pickerDevice struct {
	Name       string
	Version    string          // display version (e.g. "0.10.5 (nightly)")
	RawVersion string          // exact version key for manifest lookup
	Category   string          // e.g. "Linux" or "Wendy Lite"
	IsESP32    bool
	ESP32Chip  string          // e.g. "esp32c6", "esp32c5"
	Manifest   *deviceManifest // cached manifest for Linux devices
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
		displayVersion := rawVersion
		if nightly && dev.NightlyVersion != "" {
			rawVersion = dev.NightlyVersion
			displayVersion = rawVersion + " (nightly)"
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
	return installLinuxImage(ctx, device)
}

// installLinuxImage handles the Linux device path: pick drive → download → write.
func installLinuxImage(ctx context.Context, device pickerDevice) error {
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

	// Download image.
	fmt.Printf("\nDownloading %s image...\n", device.Name)
	imgInfo, err := getImageInfo(device.Manifest, device.RawVersion)
	if err != nil {
		return fmt.Errorf("getting image info: %w", err)
	}

	imagePath, err := downloadImage(imgInfo)
	if err != nil {
		return fmt.Errorf("downloading image: %w", err)
	}
	defer os.Remove(imagePath)

	// Write image to drive.
	fmt.Printf("Writing image to %s...\n", targetDrive.DevicePath)
	fmt.Println("You may be prompted for your password (sudo is required).")
	if err := writeImageToDisk(imagePath, targetDrive, nil); err != nil {
		return fmt.Errorf("writing image: %w", err)
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

	tmpFile, err := os.CreateTemp("", "wendyos-*.img")
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
					pct := float64(downloaded) / float64(total)
					p.Send(tui.ProgressUpdateMsg{Percent: pct})
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

	// Fetch release.
	fmt.Println("Fetching latest Wendy Lite firmware...")
	release, err := fetchWendyLiteRelease(nightly)
	if err != nil {
		return fmt.Errorf("fetching release: %w", err)
	}

	asset, err := findBinAsset(release, chip)
	if err != nil {
		return err
	}

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
