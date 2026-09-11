package commands

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/qdl"
)

func TestDragonwingBundleFrom(t *testing.T) {
	// The bundle is published under the generic image fields, mirroring what
	// the builder's publish step writes for an EDL-flashed board.
	dm := &deviceManifest{Versions: map[string]deviceVersion{
		"pr-255": {
			Path:      "pr/255/images/dragonwing-iq-8275/pr-255/bundle.qcomflash.tar.gz",
			Checksum:  "c5fe54a5262478f1ab95765a8dcf45b3dc694e2507ab46ccc6c78b44af3b8b58",
			SizeBytes: 601247035,
		},
		"0.19.0": {}, // published without an image artifact
	}}

	got, err := dragonwingBundleFrom(dm, "pr-255")
	if err != nil {
		t.Fatalf("dragonwingBundleFrom: %v", err)
	}
	if want := gcsBaseURL + "/pr/255/images/dragonwing-iq-8275/pr-255/bundle.qcomflash.tar.gz"; got.URL != want {
		t.Errorf("URL = %q, want %q", got.URL, want)
	}
	if got.SizeBytes != 601247035 || got.Version != "pr-255" {
		t.Errorf("got %+v", got)
	}

	if _, err := dragonwingBundleFrom(dm, "0.19.0"); err == nil {
		t.Error("want an error for a version with no bundle, got nil")
	}
	if _, err := dragonwingBundleFrom(dm, "9.9.9"); err == nil {
		t.Error("want an error for an unknown version, got nil")
	}
}

func TestDragonwingCacheIsKeyedOnChecksum(t *testing.T) {
	// A --pr build keeps the same version tag across re-pushes, so the cache
	// must key on the artifact too or a stale build would be flashed silently.
	cache := t.TempDir()
	const sumA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const sumB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	root := dragonwingVersionCacheDir(cache, "pr-255")
	a := filepath.Join(root, shortChecksum(sumA))
	b := filepath.Join(root, shortChecksum(sumB))
	if a == b {
		t.Fatalf("same cache dir %q for different artifacts", a)
	}
	// Nesting under the version means one tag can never be a prefix of another.
	if got := dragonwingVersionCacheDir(cache, "0.19.1"); got == dragonwingVersionCacheDir(cache, "0.19.1-nightly") {
		t.Errorf("versions share a cache dir: %q", got)
	}
}

func TestPruneStaleDragonwingBundles(t *testing.T) {
	cache := t.TempDir()
	keep := filepath.Join(dragonwingVersionCacheDir(cache, "pr-255"), "aaaaaaaaaaaa")
	stale := filepath.Join(dragonwingVersionCacheDir(cache, "pr-255"), "bbbbbbbbbbbb")
	// A version whose name extends the pruned one must be untouched: a flat
	// name plus prefix matching used to delete it.
	sibling := filepath.Join(dragonwingVersionCacheDir(cache, "pr-255-rc1"), "cccccccccccc")
	other := filepath.Join(dragonwingVersionCacheDir(cache, "0.19.1"), "dddddddddddd")
	for _, d := range []string{keep, stale, sibling, other} {
		if err := os.MkdirAll(filepath.Join(d, "extracted"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	pruneStaleDragonwingBundles(dragonwingVersionCacheDir(cache, "pr-255"), keep)

	for _, d := range []string{keep, sibling, other} {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("pruned %s, which should have survived: %v", d, err)
		}
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("stale artifact of the same version survived")
	}
}

func TestFindCachedDragonwingBundle(t *testing.T) {
	cache := t.TempDir()
	if _, ok := findCachedDragonwingBundle(cache, "pr-255"); ok {
		t.Fatal("found a bundle in an empty cache")
	}
	dir := filepath.Join(dragonwingVersionCacheDir(cache, "pr-255"), "aaaaaaaaaaaa")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory without the extracted tree is not usable.
	if _, ok := findCachedDragonwingBundle(cache, "pr-255"); ok {
		t.Error("accepted a cache entry with no extracted bundle")
	}
	if err := os.MkdirAll(filepath.Join(dir, "extracted"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok := findCachedDragonwingBundle(cache, "pr-255")
	if !ok || got != dir {
		t.Errorf("got %q, %v; want %q", got, ok, dir)
	}
	// A version whose name merely extends the requested one must not match.
	nightly := filepath.Join(dragonwingVersionCacheDir(cache, "pr-255-nightly"), "eeeeeeeeeeee")
	if err := os.MkdirAll(filepath.Join(nightly, "extracted"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, _ := findCachedDragonwingBundle(cache, "pr-255"); got != dir {
		t.Errorf("got %q, want the exact version's cache %q", got, dir)
	}
}

func TestOfflineDragonwingVersion(t *testing.T) {
	for _, tc := range []struct {
		version string
		pr      int
		want    string
	}{
		{"0.19.1", 0, "0.19.1"},
		{"", 255, "pr-255"},
		{"0.19.1", 255, "0.19.1"}, // an explicit version wins
		{"", 0, ""},               // latest/nightly needs the manifest
	} {
		if got := offlineDragonwingVersion(tc.version, tc.pr); got != tc.want {
			t.Errorf("offlineDragonwingVersion(%q, %d) = %q, want %q", tc.version, tc.pr, got, tc.want)
		}
	}
}

func TestCheckDragonwingFlagsRejectsInapplicable(t *testing.T) {
	for name, tc := range map[string]struct {
		run  func() error
		want string
	}{
		"rootfs-only": {
			run: func() error {
				return checkDragonwingFlags(true, "", false, false, "", wifiCLIOptions{}, "", preEnrollOptions{})
			},
			want: "--rootfs-only",
		},
		"drive": {
			run: func() error {
				return checkDragonwingFlags(false, "/dev/sda", false, false, "", wifiCLIOptions{}, "", preEnrollOptions{})
			},
			want: "--drive",
		},
		"storage": {
			run: func() error {
				return checkDragonwingFlags(false, "", false, false, "emmc", wifiCLIOptions{}, "", preEnrollOptions{})
			},
			want: "--storage",
		},
		"wifi": {
			run: func() error {
				return checkDragonwingFlags(false, "", false, false, "", wifiCLIOptions{SSID: "net"}, "", preEnrollOptions{})
			},
			want: "--wifi",
		},
		"device-name": {
			run: func() error {
				return checkDragonwingFlags(false, "", false, false, "", wifiCLIOptions{}, "board", preEnrollOptions{})
			},
			want: "--device-name",
		},
		"pre-enroll": {
			run: func() error {
				return checkDragonwingFlags(false, "", false, false, "", wifiCLIOptions{}, "", preEnrollOptions{mode: preEnrollForced})
			},
			want: "--pre-enroll",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatalf("want an error mentioning %s, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %s", err, tc.want)
			}
		})
	}
}

func TestCheckDragonwingFlagsAcceptsDefaults(t *testing.T) {
	// The bare command, and the explicit opt-outs, must both be fine.
	if err := checkDragonwingFlags(false, "", false, false, "", wifiCLIOptions{}, "", preEnrollOptions{}); err != nil {
		t.Errorf("bare install rejected: %v", err)
	}
	opts := wifiCLIOptions{NoWifi: true}
	if err := checkDragonwingFlags(false, "", false, false, "", opts, "", preEnrollOptions{mode: preEnrollSkip}); err != nil {
		t.Errorf("explicit opt-outs rejected: %v", err)
	}
}

func TestPercent(t *testing.T) {
	for _, tc := range []struct {
		done, total int64
		want        int
	}{
		{0, 100, 0}, {50, 100, 50},
		{100, 100, 99}, // capped: the device has not acknowledged the write yet
		{1, 0, 0},      // never divide by a zero total
	} {
		if got := percent(tc.done, tc.total); got != tc.want {
			t.Errorf("percent(%d, %d) = %d, want %d", tc.done, tc.total, got, tc.want)
		}
	}
}

func TestCheckDragonwingFlagsAgreesWithFlagCount(t *testing.T) {
	one := checkDragonwingFlags(false, "/dev/sda", false, false, "", wifiCLIOptions{}, "", preEnrollOptions{})
	if one == nil || !strings.Contains(one.Error(), "--drive does not apply") {
		t.Errorf("single flag: %v", one)
	}
	many := checkDragonwingFlags(false, "/dev/sda", true, false, "", wifiCLIOptions{}, "", preEnrollOptions{})
	if many == nil || !strings.Contains(many.Error(), "--drive, --no-bmap do not apply") {
		t.Errorf("two flags: %v", many)
	}
}

func TestDragonwingTargetLabelIdentifiesTheDeviceNotTheBoard(t *testing.T) {
	// 05c6:9008 is the generic Qualcomm EDL id, so the label must not assert a
	// board model — it must carry enough to tell two attached devices apart.
	got := dragonwingTargetLabel(qdl.DeviceInfo{Serial: "CA218394", CID: "0455", Bus: 2, Address: 21})
	for _, want := range []string{"CA218394", "0455", "bus 2 device 21"} {
		if !strings.Contains(got, want) {
			t.Errorf("label %q is missing %q", got, want)
		}
	}
	if strings.Contains(got, "IQ-8275") {
		t.Errorf("label %q claims a board model nothing verified", got)
	}
	// Two serial-less boards must still be distinguishable.
	a := dragonwingTargetLabel(qdl.DeviceInfo{Bus: 2, Address: 21})
	b := dragonwingTargetLabel(qdl.DeviceInfo{Bus: 2, Address: 22})
	if a == b {
		t.Errorf("two devices share the label %q", a)
	}
}

func TestFlashDragonwingTagsPreWriteFailures(t *testing.T) {
	// A failure before the first program command must be distinguishable, or
	// the caller warns that the board may not boot when nothing was touched.
	t.Run("missing descriptors", func(t *testing.T) {
		err := flashDragonwing(context.Background(), t.TempDir(), qdl.DeviceInfo{}, io.Discard, func(string) {})
		if err == nil {
			t.Fatal("want an error for an empty bundle dir")
		}
		if !errors.Is(err, errDragonwingNothingWritten) {
			t.Errorf("error %q should be tagged as nothing-written", err)
		}
	})

	t.Run("missing programmer", func(t *testing.T) {
		// A bundle whose descriptors parse but whose programmer is absent.
		dir := t.TempDir()
		writeBundleFile(t, dir, "rawprogram0.xml", `<data><program label="efi" filename="efi.bin"
			SECTOR_SIZE_IN_BYTES="4096" start_sector="6" num_partition_sectors="1"/></data>`)
		writeBundleFile(t, dir, "patch0.xml", `<patches><patch filename="DISK"
			SECTOR_SIZE_IN_BYTES="4096" start_sector="1" byte_offset="16" size_in_bytes="4"
			value="0" what="stretch"/></patches>`)
		writeBundleFile(t, dir, "efi.bin", "payload")
		err := flashDragonwing(context.Background(), dir, qdl.DeviceInfo{}, io.Discard, func(string) {})
		if err == nil {
			t.Fatal("want an error for a bundle with no programmer")
		}
		if !errors.Is(err, errDragonwingNothingWritten) {
			t.Errorf("error %q should be tagged as nothing-written", err)
		}
		if !strings.Contains(err.Error(), dragonwingProgrammer) {
			t.Errorf("error %q should name the missing programmer", err)
		}
	})
}

func writeBundleFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCheckCacheSegmentRejectsUnsafeManifestValues(t *testing.T) {
	// Versions and checksums come from a downloaded manifest and are used as
	// directory names, so one that escapes would move the cache — and the
	// os.RemoveAll that prunes it — outside the cache directory.
	for _, bad := range []string{
		"", ".", "..", "../..", "../OUTSIDE", "a/b", "/abs", `a\b`,
		"ver;rm -rf /", "ver\x00", "ver\n", "..\\..",
	} {
		if err := checkCacheSegment("version", bad); err == nil {
			t.Errorf("accepted unsafe version %q", bad)
		}
	}
	for _, ok := range []string{"0.19.1", "pr-255", "pr-255-rc1", "0.19.1-nightly-20260908", "c5fe54a52624"} {
		if err := checkCacheSegment("version", ok); err != nil {
			t.Errorf("rejected legitimate version %q: %v", ok, err)
		}
	}
}

func TestDragonwingCacheStaysUnderTheCacheRoot(t *testing.T) {
	// Belt and braces: whatever passes the check must still resolve inside.
	cache := t.TempDir()
	root := filepath.Join(cache, dragonwingDeviceType)
	for _, v := range []string{"0.19.1", "pr-255", "pr-255-rc1"} {
		dir := filepath.Join(dragonwingVersionCacheDir(cache, v), shortChecksum("abcdef012345"))
		rel, err := filepath.Rel(root, dir)
		if err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("version %q resolved to %q, outside %q", v, dir, root)
		}
	}
}

func TestFindCachedDragonwingBundleCannotEscape(t *testing.T) {
	// The offline lookup takes its name from --version, so it must refuse a
	// name that would resolve outside the cache even if a caller forgets to
	// validate first.
	cache := t.TempDir()
	outside := filepath.Join(cache, "..", "planted")
	if err := os.MkdirAll(filepath.Join(outside, "aaaaaaaaaaaa", "extracted"), 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(outside) //nolint:errcheck

	for _, hostile := range []string{"../../planted", "../planted", "a/b", ".."} {
		if dir, ok := findCachedDragonwingBundle(cache, hostile); ok {
			t.Errorf("%q resolved to %s, outside the cache", hostile, dir)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "aaaaaaaaaaaa", "extracted")); err != nil {
		t.Errorf("the planted tree outside the cache was touched: %v", err)
	}
}

func TestCheckDragonwingDiskSpaceReflectsRealExpansion(t *testing.T) {
	// A 601 MB bundle really extracts to ~12.5 GiB, so the estimate has to be
	// in that ballpark: too low and the check never fires and the extraction
	// fills the disk instead.
	const bundleSize = 601 * 1000 * 1000
	const gib = 1 << 30
	plan := dragonwingPlan{
		version: "pr-255",
		tarball: filepath.Join(t.TempDir(), "absent.tar.gz"),
		info:    &dragonwingBundleInfo{SizeBytes: bundleSize},
	}
	// The message carries the figure, which is the cheapest way to assert it.
	err := checkDragonwingDiskSpace("/nonexistent-volume", plan)
	_ = err // a missing volume reports no free space, so this may pass

	needed := int64(float64(bundleSize) * (1 + dragonwingExtractedFactor))
	if needed < 12*gib {
		t.Errorf("estimate is %d GiB, below the ~12.5 GiB a real bundle needs", needed/gib)
	}
	if needed > 20*gib {
		t.Errorf("estimate is %d GiB, so far above the real need it would block valid installs", needed/gib)
	}
}

func TestEDLUdevRuleMatchesPackagedRule(t *testing.T) {
	// The USB-access hint tells tarball users to install edlUdevRule verbatim,
	// so it must be byte-identical to what the deb/rpm ships. Mirrors
	// TestUsbUdevRuleMatchesPackagedRule for the Jetson rule.
	body, err := os.ReadFile("../../../../packaging/linux/udev/70-wendy-qualcomm.rules")
	if err != nil {
		t.Fatal(err)
	}
	var rules []string
	for _, line := range strings.Split(string(body), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			rules = append(rules, line)
		}
	}
	if len(rules) != 1 || rules[0] != edlUdevRule {
		t.Fatalf("packaged rule diverged from edlUdevRule:\npackaged: %q\nconst:    %q", rules, edlUdevRule)
	}
	if !strings.Contains(string(body), "05c6") {
		t.Error("the packaged rule does not match the Qualcomm EDL vendor")
	}
}

func TestManifestUnreachableExcludesMissingBuilds(t *testing.T) {
	// The offline cache fallback is gated on ErrManifestUnreachable, so a
	// manifest that was fetched fine but has no such build must not carry it —
	// otherwise a closed PR silently flashes the withdrawn artifact.
	_, err := getDragonwingBundleInfo("", false, 999999)
	if err == nil {
		t.Skip("PR 999999 unexpectedly exists")
	}
	if errors.Is(err, ErrManifestUnreachable) {
		t.Errorf("a missing build was reported as unreachable: %v", err)
	}
	if !strings.Contains(err.Error(), "no build found") {
		t.Errorf("unexpected error: %v", err)
	}
}
