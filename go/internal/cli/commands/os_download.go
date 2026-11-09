//go:build darwin || linux || windows

package commands

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

// confirmRedownloadFn asks whether to replace a cached download; a seam for tests.
var confirmRedownloadFn = func(question string) (bool, error) { return tui.Confirm(question) }

func newOSDownloadCmd() *cobra.Command {
	var version string
	var storage string
	var overwrite bool

	cmd := &cobra.Command{
		Use:   "download",
		Short: "Download a WendyOS image to the local cache",
		Long: "Download a WendyOS image for a supported device. The image is stored in the local cache for later use by `wendy os install`.\n\n" +
			"Jetson boards that install over USB recovery download their flashpack instead, so a full USB flash with `wendy os install` needs no flashpack download.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOSDownload(version, storage, overwrite)
		},
	}

	cmd.Flags().StringVar(&version, "version", "", "OS version to download (interactive if omitted)")
	cmd.Flags().StringVar(&storage, "storage", "", "Jetson flashpack target: nvme (default) or emmc (AGX Orin only)")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "Overwrite cached image without prompting")

	return cmd
}

func runOSDownload(flagVersion, flagStorage string, overwrite bool) error {
	if err := validDownloadStorage(flagStorage); err != nil {
		return err
	}
	selectedKey, dev, err := pickLinuxDevice()
	if err != nil {
		return err
	}

	// Resolve version — use flag, or pick interactively from available versions.
	version := flagVersion
	if version == "" {
		version, err = pickManifestVersion("Select a version", dev.Manifest)
		if err != nil {
			return err
		}
	}
	return downloadOSVersion(selectedKey, dev, version, flagStorage, overwrite)
}

// downloadOSVersion caches what `wendy os install` needs for one picked version:
// the flashpack for USB-recovery Jetsons, the disk image for everything else.
func downloadOSVersion(selectedKey string, dev deviceInfo, version, flagStorage string, overwrite bool) error {
	v, ok := dev.Manifest.Versions[version]
	if !ok {
		return fmt.Errorf("version %s not found in device manifest", version)
	}
	if installsFromFlashpack(selectedKey, v) {
		storage, err := flashpackDownloadStorage(selectedKey, flagStorage)
		if err != nil {
			return err
		}
		return runFlashpackDownload(dev, selectedKey, version, storage, overwrite)
	}
	if flagStorage != "" {
		return fmt.Errorf("--storage applies only to Jetson flashpacks; %s %s downloads a disk image", dev.Name, version)
	}

	// download has no target drive to probe, so default to the device's natural
	// removable image (SD for Raspberry Pi, NVMe for Jetson) rather than the
	// legacy field, which for dual-variant devices like RPi 5 is the NVMe image.
	storage := defaultManifestStorage(v)

	// Validate version exists in manifest before touching the filesystem.
	imgInfo, err := getImageInfo(dev.Manifest, version, storage)
	if err != nil {
		return fmt.Errorf("getting image info: %w", err)
	}

	// Check if already cached — zip format (new) or legacy extracted img.
	cached := ""
	if zipPath, zpErr := osCachedZipPath(selectedKey, version, storage); zpErr == nil {
		if info, statErr := os.Stat(zipPath); statErr == nil && info.Size() > 0 {
			cached = zipPath
		}
	}
	if cached == "" {
		if imgPath, ipErr := osCachedImagePath(selectedKey, version, storage); ipErr == nil {
			if info, statErr := os.Stat(imgPath); statErr == nil && info.Size() > 0 {
				cached = imgPath
			}
		}
	}

	if cached != "" {
		var sizeMB float64
		if info, err := os.Stat(cached); err == nil {
			sizeMB = float64(info.Size()) / (1024 * 1024)
		}
		cliLogln("\nImage already cached: %s (%.1f MB)", cached, sizeMB)

		if !overwrite {
			confirmed, err := confirmRedownloadFn("Re-download and overwrite?")
			if err != nil {
				return err
			}
			if !confirmed {
				cliLogln("Keeping existing cached image.")
				return nil
			}
		}

		if err := os.Remove(cached); err != nil {
			return fmt.Errorf("removing cached image: %w", err)
		}
	}

	cliLogln("\nDownloading %s %s...", dev.Name, version)
	path, err := resolveOSImage(selectedKey, imgInfo)
	if err != nil {
		return err
	}

	cliSuccess("\nCached at: %s", path)
	return nil
}
