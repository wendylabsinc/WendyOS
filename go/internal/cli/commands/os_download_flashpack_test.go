//go:build darwin || linux || windows

package commands

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/flashpack"
)

// pinUserCache keeps downloads and temp files out of the real user cache.
func pinUserCache(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("HOME", base)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(base, "xdg-cache"))
	t.Setenv("LOCALAPPDATA", filepath.Join(base, "local-app-data"))
	cacheDir, err := osCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	return cacheDir
}

// These boards publish no disk image on recovery releases, so `os download`
// must fetch the flashpack the installer flashes instead.
func TestInstallsFromFlashpack(t *testing.T) {
	for _, tc := range []struct {
		device, mode string
		want         bool
	}{
		{thorDeviceType, "", true},
		{orinDeviceType, "recovery", true},
		{orinNanoDeviceType, "recovery", true},
		{orinDeviceType, "", false}, // legacy release with a disk image
		{orinNanoDeviceType, "", false},
		{"raspberry-pi-5", "", false},
		{"raspberry-pi-5", "recovery", false},
	} {
		if got := installsFromFlashpack(tc.device, deviceVersion{InstallMode: tc.mode}); got != tc.want {
			t.Errorf("installsFromFlashpack(%q, mode %q) = %v, want %v", tc.device, tc.mode, got, tc.want)
		}
	}
}

// Mirrors `wendy os install`: Thor takes no --storage, eMMC is AGX Orin only.
func TestFlashpackDownloadStorage(t *testing.T) {
	for _, tc := range []struct {
		device, flag, want string
		wantErr            bool
	}{
		{thorDeviceType, "", "nvme", false},
		{thorDeviceType, "nvme", "", true},
		{orinNanoDeviceType, "", "nvme", false},
		{orinNanoDeviceType, "nvme", "nvme", false},
		{orinNanoDeviceType, "emmc", "", true},
		{orinDeviceType, "", "nvme", false},
		{orinDeviceType, "emmc", "emmc", false},
		{orinDeviceType, "sd", "", true},
	} {
		got, err := flashpackDownloadStorage(tc.device, tc.flag)
		if (err != nil) != tc.wantErr || got != tc.want {
			t.Errorf("flashpackDownloadStorage(%q, %q) = %q, %v; want %q, error %v", tc.device, tc.flag, got, err, tc.want, tc.wantErr)
		}
	}
}

// Every case here must fail before anything is downloaded.
func TestDownloadOSVersionRejectsWithoutDownloading(t *testing.T) {
	cacheDir := pinUserCache(t)
	manifest := func(v deviceVersion) *deviceManifest {
		return &deviceManifest{Versions: map[string]deviceVersion{"1.0": v}}
	}
	for _, tc := range []struct {
		name, device, storage, wantErr string
		version                        deviceVersion
	}{
		{"storage on a disk-image device", "raspberry-pi-5", "nvme", "applies only to Jetson flashpacks", deviceVersion{Path: "rpi5.img.zip"}},
		{"storage on a legacy Orin release", orinNanoDeviceType, "nvme", "applies only to Jetson flashpacks", deviceVersion{NVMEPath: "nano.img.zst"}},
		{"storage on Thor", thorDeviceType, "nvme", "not supported for " + thorDeviceType, deviceVersion{FlashpackPath: "thor.flashpack.tar.zst"}},
		{"eMMC on the Nano", orinNanoDeviceType, "emmc", "supported only for " + orinDeviceType, deviceVersion{InstallMode: "recovery"}},
		{"Orin release without the flashpack", orinNanoDeviceType, "", "no complete nvme recovery flashpack", deviceVersion{InstallMode: "recovery"}},
		{"Thor release from before flashpacks", thorDeviceType, "", "has no flashpack to download", deviceVersion{Path: "thor.tegraflash.tar.gz"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev := deviceInfo{Name: tc.device, Manifest: manifest(tc.version)}
			err := downloadOSVersion(tc.device, dev, "1.0", tc.storage, false)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want one containing %q", err, tc.wantErr)
			}
		})
	}
	if leftovers, _ := os.ReadDir(cacheDir); len(leftovers) != 0 {
		t.Errorf("rejected downloads left %d cache entries", len(leftovers))
	}
}

func writeExtractedT234Flashpack(t *testing.T, cacheDir string, ref flashpack.RecoveryRef, stamp string) string {
	t.Helper()
	extracted := flashpack.RecoveryExtractedCachePath(cacheDir, ref)
	if err := os.MkdirAll(extracted, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extracted, "manifest.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if stamp != "" {
		if err := flashpack.WriteStamp(extracted, stamp); err != nil {
			t.Fatal(err)
		}
	}
	return extracted
}

func TestT234FlashpackCached(t *testing.T) {
	ref := flashpack.RecoveryRef{Device: orinNanoDeviceType, Storage: "nvme", Version: "0.20.2"}

	cacheDir := t.TempDir()
	if t234FlashpackCached(cacheDir, ref, "sha-new") {
		t.Error("empty cache reported as cached")
	}
	writeExtractedT234Flashpack(t, cacheDir, ref, "sha-new")
	if !t234FlashpackCached(cacheDir, ref, "sha-new") {
		t.Error("current extracted flashpack not reported as cached")
	}
	if t234FlashpackCached(cacheDir, ref, "sha-republished") {
		t.Error("flashpack stamped with an older checksum reported as current")
	}

	// A tree extracted before stamping existed is trusted, as the installer does.
	unstamped := t.TempDir()
	writeExtractedT234Flashpack(t, unstamped, ref, "")
	if !t234FlashpackCached(unstamped, ref, "sha-new") {
		t.Error("unstamped extracted flashpack not reported as cached")
	}
}

func stubConfirmRedownload(t *testing.T, answer bool, err error) *int {
	t.Helper()
	calls := 0
	orig := confirmRedownloadFn
	confirmRedownloadFn = func(string) (bool, error) {
		calls++
		return answer, err
	}
	t.Cleanup(func() { confirmRedownloadFn = orig })
	return &calls
}

func TestKeepCachedFlashpack(t *testing.T) {
	calls := stubConfirmRedownload(t, true, nil)
	if keep, err := keepCachedFlashpack("x", true); keep || err != nil || *calls != 0 {
		t.Errorf("--overwrite: keep=%v err=%v prompts=%d; want replace without asking", keep, err, *calls)
	}
	if keep, err := keepCachedFlashpack("x", false); keep || err != nil || *calls != 1 {
		t.Errorf("accepted prompt: keep=%v err=%v prompts=%d; want replace", keep, err, *calls)
	}

	stubConfirmRedownload(t, false, nil)
	if keep, err := keepCachedFlashpack("x", false); !keep || err != nil {
		t.Errorf("declined prompt: keep=%v err=%v; want keep", keep, err)
	}

	boom := errors.New("no terminal")
	stubConfirmRedownload(t, false, boom)
	if _, err := keepCachedFlashpack("x", false); !errors.Is(err, boom) {
		t.Errorf("prompt error = %v, want %v", err, boom)
	}
}

// Declining the re-download must leave the cache as it is and never reach the
// network: the manifest here points at an object that does not exist.
func TestDownloadT234FlashpackKeepsACurrentCache(t *testing.T) {
	const version = "0.20.2"
	ref := flashpack.RecoveryRef{Device: orinNanoDeviceType, Storage: "nvme", Version: version}
	cacheDir := pinUserCache(t)
	extracted := writeExtractedT234Flashpack(t, cacheDir, ref, "sha-current")
	dev := deviceInfo{Name: "Jetson Orin Nano", Manifest: &deviceManifest{Versions: map[string]deviceVersion{
		version: {
			InstallMode:            "recovery",
			NVMEFlashpackPath:      "images/does-not-exist.flashpack.tar.zst",
			NVMEFlashpackChecksum:  "sha-current",
			NVMEFlashpackSizeBytes: 1,
		},
	}}}
	calls := stubConfirmRedownload(t, false, nil)

	cachedAt, err := downloadT234Flashpack(cacheDir, dev, orinNanoDeviceType, version, "nvme", false, func(string) {})
	if err != nil || cachedAt != "" {
		t.Fatalf("keeping the cached flashpack: path %q, err %v; want no new download", cachedAt, err)
	}
	if *calls != 1 {
		t.Errorf("prompted %d times, want once", *calls)
	}
	if _, err := os.Stat(filepath.Join(extracted, "manifest.json")); err != nil {
		t.Errorf("cached flashpack removed after the user kept it: %v", err)
	}
}

// A purge that cannot delete (e.g. root-owned files from a sudo install) must
// fail instead of reporting a fresh download that never happened.
func TestPurgeFlashpackReportsWhatSurvives(t *testing.T) {
	cacheDir := t.TempDir()
	extracted := filepath.Join(cacheDir, "x.flashpack")
	tarball := filepath.Join(cacheDir, "x.flashpack.tar.zst")
	if err := os.MkdirAll(extracted, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extracted, "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tarball, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := purgeFlashpack(extracted, tarball); err != nil {
		t.Fatalf("purge of a writable cache failed: %v", err)
	}
	if existingPath(extracted, tarball) != extracted {
		t.Fatal("existingPath should fall back to its first path when none exists")
	}

	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs POSIX permissions enforced for the caller")
	}
	if err := os.MkdirAll(extracted, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extracted, "f"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(extracted, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(extracted, 0o755) })
	if err := purgeFlashpack(extracted, tarball); err == nil || !strings.Contains(err.Error(), extracted) {
		t.Fatalf("error = %v, want one naming %s", err, extracted)
	}
}

func TestNewDownloadDetail(t *testing.T) {
	var log bytes.Buffer
	detail, finish := newDownloadDetail(&log, false)
	detail("40% · 1.2/3.0 GiB")
	detail("41% · 1.3/3.0 GiB") // within the log interval: dropped
	detail("1.4 GiB")           // size unknown: still a progress update
	detail("verifying download")
	finish()
	if got, want := log.String(), "  40% · 1.2/3.0 GiB\n  verifying download\n"; got != want {
		t.Errorf("log output = %q, want %q", got, want)
	}

	var term bytes.Buffer
	detail, finish = newDownloadDetail(&term, true)
	detail("40% · 1.2/3.0 GiB")
	detail("verifying download")
	finish()
	out := term.String()
	if strings.Count(out, "\r") != 2 || !strings.HasSuffix(out, "\n") || strings.Count(out, "\n") != 1 {
		t.Errorf("terminal output = %q, want two rewritten lines ending in one newline", out)
	}
}

// Extracted flashpacks are directories; the list used to skip them and show
// only their checksum stamps.
func TestListOSCacheEntriesIncludesFlashpacks(t *testing.T) {
	dir := t.TempDir()
	write := func(rel string, n int) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, make([]byte, n), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("jetson-orin-nano-nvme-0.20.2.flashpack/manifest.json", 3)
	write("jetson-orin-nano-nvme-0.20.2.flashpack/flash-images/a.img", 4)
	write("jetson-orin-nano-nvme-0.20.2.flashpack.sha256", 64)
	write("raspberry-pi-5-0.20.2.zip", 10)
	write("dragonwing-iq-9075/extracted/x", 5)

	items, err := listOSCacheEntries(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, it := range items {
		got[it.Name] = it.SizeBytes
	}
	want := map[string]int64{"jetson-orin-nano-nvme-0.20.2.flashpack": 7, "raspberry-pi-5-0.20.2.zip": 10}
	if len(got) != len(want) {
		t.Fatalf("entries = %v, want %v", got, want)
	}
	for name, size := range want {
		if got[name] != size {
			t.Errorf("%s size = %d, want %d (all entries: %v)", name, got[name], size, got)
		}
	}
}
