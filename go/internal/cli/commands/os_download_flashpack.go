//go:build darwin || linux || windows

package commands

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/flashpack"
)

// installsFromFlashpack reports whether `wendy os install` flashes this version
// over USB recovery from a flashpack, so the flashpack is what to pre-download.
// Such versions publish no disk image for the generic download to fetch.
func installsFromFlashpack(deviceKey string, v deviceVersion) bool {
	return deviceKey == thorDeviceType || (isT234RecoveryDevice(deviceKey) && v.InstallMode == "recovery")
}

// validDownloadStorage checks the --storage value before any picker runs.
func validDownloadStorage(storage string) error {
	switch storage {
	case "", "nvme", "emmc":
		return nil
	}
	return fmt.Errorf("invalid --storage %q: use nvme or emmc", storage)
}

// flashpackDownloadStorage resolves --storage for a flashpack download with the
// rules `wendy os install` applies: Thor takes none, and eMMC is AGX Orin only.
func flashpackDownloadStorage(deviceKey, storage string) (string, error) {
	if err := validDownloadStorage(storage); err != nil {
		return "", err
	}
	switch {
	case deviceKey == thorDeviceType && storage != "":
		return "", fmt.Errorf("--storage is not supported for %s", thorDeviceType)
	case storage == "emmc" && deviceKey != orinDeviceType:
		return "", fmt.Errorf("--storage emmc is supported only for %s", orinDeviceType)
	case storage == "":
		return "nvme", nil
	}
	return storage, nil
}

// t234FlashpackCached reports whether ref's flashpack is extracted in the cache
// and still matches the manifest, the same test the Orin installer applies.
func t234FlashpackCached(cacheDir string, ref flashpack.RecoveryRef, checksum string) bool {
	extracted := flashpack.RecoveryExtractedCachePath(cacheDir, ref)
	if _, err := os.Stat(filepath.Join(extracted, "manifest.json")); err != nil {
		return false
	}
	return !flashpack.StampStale(extracted, checksum)
}

// runFlashpackDownload caches the flashpack that a full USB flash with
// `wendy os install` uses, in the cache entry the installer reads.
func runFlashpackDownload(dev deviceInfo, deviceKey, version, storage string, overwrite bool) error {
	cacheDir, err := osCacheDir()
	if err != nil {
		return fmt.Errorf("resolving cache dir: %w", err)
	}
	detail, finish := newDownloadDetail(os.Stderr, isStderrTerminalFn())
	var cachedAt string
	if deviceKey == thorDeviceType {
		cachedAt, err = downloadThorFlashpack(cacheDir, dev, version, overwrite, detail)
	} else {
		cachedAt, err = downloadT234Flashpack(cacheDir, dev, deviceKey, version, storage, overwrite, detail)
	}
	finish()
	if err == nil && cachedAt != "" {
		cliSuccess("\nCached at: %s", cachedAt)
	}
	return err
}

// downloadThorFlashpack returns where it cached the flashpack, or "" when it
// kept the cached one.
func downloadThorFlashpack(cacheDir string, dev deviceInfo, version string, overwrite bool, detail func(string)) (string, error) {
	if dev.Manifest != nil {
		if v, ok := dev.Manifest.Versions[version]; ok && v.FlashpackPath == "" {
			return "", fmt.Errorf("%s %s has no flashpack to download; it predates flashpack installs", dev.Name, version)
		}
	}
	plan, err := planThorFlashpack(cacheDir, version, false, 0)
	if err != nil {
		return "", err
	}
	extracted := filepath.Join(cacheDir, flashpack.FlashpackName(plan.version))
	tarball := flashpack.TarballCachePath(cacheDir, plan.version)
	switch {
	case plan.cached && plan.offline:
		if overwrite {
			return "", fmt.Errorf("cannot re-download %s %s: the manifest is unreachable", dev.Name, plan.version)
		}
		cliNotice("Could not reach the manifest; keeping the cached flashpack: %s", existingPath(extracted, tarball))
		return "", nil
	case plan.cached:
		keep, err := keepCachedFlashpack(existingPath(extracted, tarball), overwrite)
		if err != nil || keep {
			return "", err
		}
		// Resolve the replacement before deleting, so a failed lookup keeps the cache.
		info, err := getThorFlashpackInfo(plan.version, false, 0)
		if err != nil {
			return "", err
		}
		if info.Checksum == "" {
			return "", fmt.Errorf("manifest entry for %s has no flashpack checksum; keeping the cached flashpack", plan.version)
		}
		if err := purgeFlashpack(extracted, tarball); err != nil {
			return "", err
		}
		plan = thorFlashPlan{version: info.Version, info: info}
	case plan.refreshed:
		// The planner already purged the outdated build; make sure it is gone.
		if err := purgeFlashpack(extracted, tarball); err != nil {
			return "", err
		}
		cliLogln("\nThe cached flashpack is outdated; downloading the current build.")
	}
	if err := checkThorDiskSpace(cacheDir, plan); err != nil {
		return "", err
	}
	cliLogln("\nCaching the %s %s flashpack...", dev.Name, plan.version)
	if _, _, err := downloadAndExtractFlashpack(cacheDir, plan, detail); err != nil {
		return "", err
	}
	return extracted, nil
}

// downloadT234Flashpack returns where it cached the flashpack, or "" when it
// kept the cached one.
func downloadT234Flashpack(cacheDir string, dev deviceInfo, deviceKey, version, storage string, overwrite bool, detail func(string)) (string, error) {
	info, err := getRecoveryFlashpackInfo(dev.Manifest, deviceKey, version, storage)
	if err != nil {
		return "", err
	}
	ref := flashpack.RecoveryRef{Device: deviceKey, Storage: storage, Version: version}
	extracted := flashpack.RecoveryExtractedCachePath(cacheDir, ref)
	tarball := flashpack.RecoveryTarballCachePath(cacheDir, ref)
	_, statErr := os.Stat(filepath.Join(extracted, "manifest.json"))
	switch {
	case t234FlashpackCached(cacheDir, ref, info.Checksum):
		keep, err := keepCachedFlashpack(extracted, overwrite)
		if err != nil || keep {
			return "", err
		}
		if err := purgeFlashpack(extracted, tarball); err != nil {
			return "", err
		}
	case statErr == nil:
		// Extracted but stamped with another checksum: the manifest republished it.
		if err := purgeFlashpack(extracted, tarball); err != nil {
			return "", err
		}
		cliLogln("\nThe cached flashpack is outdated; downloading the current build.")
	}
	label := map[string]string{"nvme": "NVMe", "emmc": "eMMC"}[storage]
	cliLogln("\nCaching the %s %s %s flashpack...", dev.Name, version, label)
	if _, _, err := resolveT234Flashpack(cacheDir, ref, info, detail); err != nil {
		return "", err
	}
	return extracted, nil
}

// purgeFlashpack removes a cached flashpack and fails if anything survives,
// e.g. files a sudo-run install left owned by root.
func purgeFlashpack(extracted, tarball string) error {
	flashpack.Purge(extracted, tarball)
	for _, p := range []string{extracted, tarball} {
		if _, err := os.Lstat(p); err == nil {
			return fmt.Errorf("could not remove the cached flashpack at %s; check its ownership and permissions", p)
		}
	}
	return nil
}

// existingPath returns the first path that exists, else the first one.
func existingPath(paths ...string) string {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return paths[0]
}

// keepCachedFlashpack reports whether to keep a current cached flashpack,
// asking unless --overwrite already answered.
func keepCachedFlashpack(path string, overwrite bool) (bool, error) {
	cliLogln("\nFlashpack already cached: %s", path)
	if overwrite {
		return false, nil
	}
	redownload, err := confirmRedownloadFn("Re-download and overwrite?")
	if err != nil {
		return false, err
	}
	if !redownload {
		cliLogln("Keeping the cached flashpack.")
	}
	return !redownload, nil
}

// newDownloadDetail prints the flashpack helpers' status updates: rewritten in
// place on a terminal; in a log, every phase but only periodic progress lines.
func newDownloadDetail(w io.Writer, tty bool) (detail func(string), finish func()) {
	const logEvery = 10 * time.Second
	var (
		mu      sync.Mutex
		printed bool
		last    time.Time
	)
	detail = func(msg string) {
		mu.Lock()
		defer mu.Unlock()
		if tty {
			fmt.Fprintf(w, "\r  %-60s", msg)
			printed = true
			return
		}
		// Byte counts (byteProgress) are the only updates that name GiB.
		if strings.Contains(msg, "GiB") && time.Since(last) < logEvery {
			return
		}
		last = time.Now()
		fmt.Fprintf(w, "  %s\n", msg)
	}
	finish = func() {
		mu.Lock()
		defer mu.Unlock()
		if printed {
			fmt.Fprintln(w)
		}
	}
	return detail, finish
}
