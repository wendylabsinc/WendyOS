package commands

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"time"
)

// firmwareAsset holds the resolved .bin asset info from the GCS manifest.
type firmwareAsset struct {
	Name        string
	DownloadURL string
	Size        int64
	Version     string
}

// deriveAssetName prefers the basename from the manifest URL/path, falling back to the legacy synthesized name.
func deriveAssetName(downloadURL, chip string) string {
	if downloadURL != "" {
		if u, err := url.Parse(downloadURL); err == nil {
			if base := path.Base(u.Path); base != "" && base != "/" && base != "." {
				return base
			}
		}
	}
	return fmt.Sprintf("wendy-lite-%s.bin", chip)
}

// fetchFirmwareFromManifest finds the latest firmware for a chip from the GCS manifest.
func fetchFirmwareFromManifest(chip string, nightly bool) (*firmwareAsset, error) {
	main, err := fetchMainManifest()
	if err != nil {
		return nil, fmt.Errorf("fetching main manifest: %w", err)
	}

	if main.Firmware == nil {
		return nil, fmt.Errorf("no firmware entries in manifest")
	}

	chipEntry, ok := main.Firmware[chip]
	if !ok {
		return nil, fmt.Errorf("chip %s not found in manifest", chip)
	}

	if chipEntry.ManifestPath == "" {
		return nil, fmt.Errorf("no manifest path for chip %s", chip)
	}

	fm, err := fetchFirmwareManifest(chipEntry.ManifestPath)
	if err != nil {
		return nil, fmt.Errorf("fetching firmware manifest for %s: %w", chip, err)
	}

	// Validate that the firmware manifest matches the requested chip.
	if fm.ChipID != "" && fm.ChipID != chip {
		return nil, fmt.Errorf("firmware manifest chip ID %q does not match requested chip %q", fm.ChipID, chip)
	}

	// Find the target version
	var targetVersion string
	if nightly {
		targetVersion = chipEntry.LatestNightly
	} else {
		targetVersion = chipEntry.Latest
	}

	if targetVersion == "" {
		buildType := "stable"
		if nightly {
			buildType = "nightly"
		}
		return nil, fmt.Errorf("no %s firmware version available for %s", buildType, chip)
	}

	info, err := getFirmwareInfo(fm, targetVersion)
	if err != nil {
		return nil, err
	}

	return &firmwareAsset{
		Name:        deriveAssetName(info.DownloadURL, chip),
		DownloadURL: info.DownloadURL,
		Size:        info.ImageSize,
		Version:     targetVersion,
	}, nil
}

// downloadFirmware downloads a firmware .bin to a temp file, reporting progress.
func downloadFirmware(asset *firmwareAsset, progressFn func(downloaded, total int64)) (string, error) {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(asset.DownloadURL)
	if err != nil {
		return "", fmt.Errorf("downloading firmware: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	tmpFile, err := os.CreateTemp("", "wendy-lite-*.bin")
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}

	total := resp.ContentLength
	if asset.Size > 0 && total <= 0 {
		total = asset.Size
	}
	var downloaded int64

	buf := make([]byte, 32*1024)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			if _, err := tmpFile.Write(buf[:n]); err != nil {
				tmpFile.Close()
				os.Remove(tmpFile.Name())
				return "", fmt.Errorf("writing firmware: %w", err)
			}
			downloaded += int64(n)
			if progressFn != nil {
				progressFn(downloaded, total)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			tmpFile.Close()
			os.Remove(tmpFile.Name())
			return "", fmt.Errorf("reading firmware: %w", readErr)
		}
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("closing temp file: %w", err)
	}

	return tmpFile.Name(), nil
}
