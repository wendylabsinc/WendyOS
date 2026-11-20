package commands

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

type firmwareAsset struct {
	Name        string
	DownloadURL string
	Size        int64
	Version     string
}

// deriveAssetName prefers the basename from the manifest URL/path, falling back to the legacy synthesized name.
func deriveAssetName(downloadURL, firmwareID string) string {
	if downloadURL != "" {
		if u, err := url.Parse(downloadURL); err == nil {
			if base := path.Base(u.Path); base != "" && base != "/" && base != "." {
				return base
			}
		}
	}
	return fmt.Sprintf("wendy-lite-%s.bin", firmwareID)
}

// fetchFirmwareFromManifest finds the latest firmware for a chip from the GCS manifest.
func fetchFirmwareFromManifest(firmwareID string, nightly bool) (*firmwareAsset, error) {
	main, err := fetchMainManifest()
	if err != nil {
		return nil, fmt.Errorf("fetching main manifest: %w", err)
	}

	if main.Firmware == nil {
		return nil, fmt.Errorf("no firmware entries in manifest")
	}

	chipEntry, ok := main.Firmware[firmwareID]
	if !ok {
		return nil, fmt.Errorf("firmware %s not found in manifest", firmwareID)
	}

	if chipEntry.ManifestPath == "" {
		return nil, fmt.Errorf("no manifest path for firmware %s", firmwareID)
	}

	fm, err := fetchFirmwareManifest(chipEntry.ManifestPath)
	if err != nil {
		return nil, fmt.Errorf("fetching firmware manifest for %s: %w", firmwareID, err)
	}

	// Validate that the firmware manifest matches the requested chip.
	if fm.FirmwareID != "" && fm.FirmwareID != firmwareID {
		return nil, fmt.Errorf("firmware manifest firmware ID %q does not match requested firmware %q", fm.FirmwareID, firmwareID)
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
		return nil, fmt.Errorf("no %s firmware version available for %s", buildType, firmwareID)
	}

	info, err := getFirmwareInfo(fm, targetVersion)
	if err != nil {
		return nil, err
	}

	return &firmwareAsset{
		Name:        deriveAssetName(info.DownloadURL, firmwareID),
		DownloadURL: info.DownloadURL,
		Size:        info.ImageSize,
		Version:     targetVersion,
	}, nil
}

// firmwareCacheDir returns (creating it if necessary) the directory where
// downloaded Wendy Lite firmware binaries are cached across runs, so the same
// version isn't re-fetched from GCS on every install.
func firmwareCacheDir() (string, error) {
	base, err := config.CacheDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, "wendy-lite-firmware")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("creating firmware cache directory: %w", err)
	}
	return dir, nil
}

// firmwareCachedPath builds the cache file path for asset, keyed by version
// and asset name so different boards/firmware never collide. Inputs are
// sanitized to prevent path traversal from manifest-supplied values.
func firmwareCachedPath(asset *firmwareAsset) (string, error) {
	safeVersion := filepath.Base(asset.Version)
	safeName := filepath.Base(asset.Name)
	if safeVersion != asset.Version || safeName != asset.Name ||
		strings.Contains(asset.Version, "..") || strings.Contains(asset.Name, "..") {
		return "", fmt.Errorf("invalid firmware cache key: %q / %q", asset.Version, asset.Name)
	}
	dir, err := firmwareCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, fmt.Sprintf("%s-%s", safeVersion, safeName)), nil
}

// resolveFirmware returns a local path to asset's firmware binary, reusing a
// cached copy from a previous run instead of re-downloading it. The returned
// path lives in the cache directory and must not be deleted by the caller —
// only `wendy cache clear`/`cache list` remove it.
func resolveFirmware(asset *firmwareAsset) (string, error) {
	cached, err := firmwareCachedPath(asset)
	if err != nil {
		return "", err
	}
	if info, statErr := os.Stat(cached); statErr == nil && info.Size() > 0 {
		fmt.Printf("Using cached firmware (%s)\n", cached)
		return cached, nil
	}

	dir, err := firmwareCacheDir()
	if err != nil {
		return "", err
	}
	downloadPath, err := downloadFirmware(asset, dir)
	if err != nil {
		return "", err
	}
	os.Remove(cached) // clear a stale/0-byte file so Rename succeeds on Windows
	if err := os.Rename(downloadPath, cached); err != nil {
		os.Remove(downloadPath)
		return "", fmt.Errorf("caching firmware: %w", err)
	}
	return cached, nil
}

// downloadFirmware downloads asset's .bin into dir, driving its own progress
// bar. It runs synchronously and returns once the TUI program exits.
func downloadFirmware(asset *firmwareAsset, dir string) (string, error) {
	prog := tui.NewProgress(fmt.Sprintf("Downloading %s %s...", asset.Name, asset.Version))
	p := tui.NewProgressProgram(prog)

	type result struct {
		path string
		err  error
	}
	resC := make(chan result, 1)
	go func() {
		path, err := downloadFirmwareInto(asset, dir, func(downloaded, total int64) {
			if total > 0 {
				p.Send(tui.ProgressUpdateMsg{Percent: float64(downloaded) / float64(total)})
			}
		})
		resC <- result{path, err}
		p.Send(tui.ProgressDoneMsg{Err: err})
	}()

	if _, err := tui.RunProgram(p); err != nil {
		if r := <-resC; r.path != "" {
			os.Remove(r.path)
		}
		return "", fmt.Errorf("progress TUI: %w", err)
	}
	r := <-resC
	return r.path, r.err
}

// downloadFirmwareInto downloads asset's .bin into a temp file inside dir,
// reporting progress via progressFn. It runs synchronously and owns no TUI,
// so callers (or tests) can drive their own progress display.
func downloadFirmwareInto(asset *firmwareAsset, dir string, progressFn func(downloaded, total int64)) (string, error) {
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(asset.DownloadURL)
	if err != nil {
		return "", fmt.Errorf("downloading firmware: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download returned status %d", resp.StatusCode)
	}

	tmpFile, err := os.CreateTemp(dir, "wendy-lite-*.bin")
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
