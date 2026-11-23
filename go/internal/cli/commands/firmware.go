package commands

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
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
func resolveFirmware(ctx context.Context, asset *firmwareAsset) (string, error) {
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
	downloadPath, err := downloadFirmware(ctx, asset, dir)
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
// bar. It runs synchronously and returns once the TUI program exits. Leaving
// the progress bar with q or Ctrl+C cancels the download.
func downloadFirmware(ctx context.Context, asset *firmwareAsset, dir string, opts ...tea.ProgramOption) (string, error) {
	downloadCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	prog := tui.NewProgress(fmt.Sprintf("Downloading %s %s...", asset.Name, asset.Version))
	p := tui.NewProgressProgram(prog, opts...)

	type result struct {
		path string
		err  error
	}
	resC := make(chan result, 1)
	go func() {
		path, err := downloadFirmwareInto(downloadCtx, asset, dir, func(downloaded, total int64) {
			if total > 0 {
				p.Send(tui.ProgressUpdateMsg{Percent: float64(downloaded) / float64(total)})
			}
		})
		resC <- result{path, err}
		p.Send(tui.ProgressDoneMsg{Err: err})
	}()

	model, err := tui.RunProgram(p)
	if err != nil {
		err = fmt.Errorf("progress TUI: %w", err)
	} else if progress, ok := model.(tui.ProgressModel); ok {
		err = progress.Err() // context.Canceled for q or Ctrl+C, else the download's error
	}
	if err != nil {
		// The transfer has no deadline, so a closed or failed progress bar
		// must stop it before its result is waited for.
		cancel()
		if r := <-resC; r.path != "" {
			os.Remove(r.path)
		}
		if errors.Is(err, context.Canceled) && ctx.Err() == nil {
			return "", ErrUserCancelled // the person left the progress bar
		}
		return "", err
	}
	r := <-resC
	return r.path, r.err
}

// downloadFirmwareInto downloads asset's .bin into a temp file inside dir,
// reporting progress via progressFn. It runs synchronously and owns no TUI,
// so callers (or tests) can drive their own progress display.
// The transfer has no deadline: it follows imageDownloadPolicy, like an OS image.
func downloadFirmwareInto(ctx context.Context, asset *firmwareAsset, dir string, progressFn func(downloaded, total int64)) (string, error) {
	tmpFile, err := os.CreateTemp(dir, "wendy-lite-*.bin")
	if err != nil {
		return "", fmt.Errorf("creating temp file: %w", err)
	}

	err = downloadResumable(ctx, asset.DownloadURL, tmpFile, 0, func(downloaded, total int64) {
		if asset.Size > 0 && total <= 0 {
			total = asset.Size
		}
		if progressFn != nil {
			progressFn(downloaded, total)
		}
	})
	if err != nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("downloading firmware: %w", err)
	}

	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpFile.Name())
		return "", fmt.Errorf("closing temp file: %w", err)
	}

	return tmpFile.Name(), nil
}
