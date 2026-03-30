//go:build darwin || linux

package commands

import (
	"bufio"
	"fmt"
	"os"
	"sort"
	"strings"

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
		version, err = pickVersion(dev)
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
		fmt.Printf("\nImage already cached: %s (%.1f MB)\n", cached, sizeMB)

		if !overwrite {
			fmt.Print("Re-download and overwrite? [y/N] ")

			reader := bufio.NewReader(os.Stdin)
			line, err := reader.ReadString('\n')
			if err != nil {
				return err
			}
			if answer := strings.TrimSpace(strings.ToLower(line)); answer != "y" && answer != "yes" {
				fmt.Println("Keeping existing cached image.")
				return nil
			}
		}

		// Remove stale cache entry before re-downloading.
		if err := os.Remove(cached); err != nil {
			return fmt.Errorf("removing cached image: %w", err)
		}
	}

	fmt.Printf("\nDownloading %s %s...\n", dev.Name, version)
	path, err := resolveOSImage(selectedKey, imgInfo)
	if err != nil {
		return err
	}

	fmt.Printf("\nCached at: %s\n", path)
	return nil
}

// pickVersion presents an interactive picker for available versions of a device.
func pickVersion(dev deviceInfo) (string, error) {
	if dev.Manifest == nil || len(dev.Manifest.Versions) == 0 {
		return "", fmt.Errorf("no versions available for %s", dev.Name)
	}

	// Collect and sort versions (newest first by string sort, reversed).
	var versions []string
	for v := range dev.Manifest.Versions {
		versions = append(versions, v)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(versions)))

	var items []tui.PickerItem
	for _, v := range versions {
		ver := dev.Manifest.Versions[v]
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
	return pickFromItems("Select a version", items)
}
