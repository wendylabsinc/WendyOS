//go:build darwin || linux || windows

package commands

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
)

func newOSDownloadCmd() *cobra.Command {
	var version string
	var overwrite bool

	cmd := &cobra.Command{
		Use:   "download",
		Short: "Download a WendyOS image to the local cache",
		Long:  "Download (and extract) a WendyOS image for a supported device. The image is stored in the local cache for later use by `wendy os install`.",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOSDownload(version, overwrite)
		},
	}

	cmd.Flags().StringVar(&version, "version", "", "OS version to download (interactive if omitted)")
	cmd.Flags().BoolVar(&overwrite, "overwrite", false, "Overwrite cached image without prompting")

	return cmd
}

func runOSDownload(flagVersion string, overwrite bool) error {
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

	// Validate version exists in manifest before touching the filesystem.
	imgInfo, err := getImageInfo(dev.Manifest, version)
	if err != nil {
		return fmt.Errorf("getting image info: %w", err)
	}

	// Check if already cached.
	cached, err := osCachedImagePath(selectedKey, version)
	if err != nil {
		return err
	}

	if info, statErr := os.Stat(cached); statErr == nil && info.Size() > 0 {
		sizeMB := float64(info.Size()) / (1024 * 1024)
		cliLogln("\nImage already cached: %s (%.1f MB)", cached, sizeMB)

		if !overwrite {
			confirmed, err := tui.Confirm("Re-download and overwrite?")
			if err != nil {
				return err
			}
			if !confirmed {
				cliLogln("Keeping existing cached image.")
				return nil
			}
		}

		// Remove stale cache entry before re-downloading.
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
