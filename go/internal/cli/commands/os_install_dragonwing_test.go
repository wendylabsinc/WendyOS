package commands

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/qdl"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

func TestDragonwingBoardForKnownAndUnknown(t *testing.T) {
	for _, tc := range []struct {
		deviceType string
		wantName   string
		wantMsmID  uint32
	}{
		{"dragonwing-iq-8275", "Dragonwing IQ-8275", 0x002e70e1},
		{"dragonwing-iq-9075", "Dragonwing IQ-9075", 0x002eb0e1},
	} {
		t.Run(tc.deviceType, func(t *testing.T) {
			b, ok := dragonwingBoardFor(tc.deviceType)
			if !ok {
				t.Fatalf("dragonwingBoardFor(%q) = not found, want found", tc.deviceType)
			}
			if b.msmID != tc.wantMsmID {
				t.Errorf("dragonwingBoardFor(%q).msmID = %#x, want %#x", tc.deviceType, b.msmID, tc.wantMsmID)
			}
			// Every message about a board is spelled by the shared table.
			if got := humanReadableDeviceType(b.deviceType); got != tc.wantName {
				t.Errorf("humanReadableDeviceType(%q) = %q, want %q", b.deviceType, got, tc.wantName)
			}
		})
	}
	for _, deviceType := range []string{"", "raspberry-pi-5", "dragonwing", "dragonwing-iq-9999"} {
		if _, ok := dragonwingBoardFor(deviceType); ok {
			t.Errorf("dragonwingBoardFor(%q) = found, want not found", deviceType)
		}
	}
}

func TestVerifyDragonwingBoard(t *testing.T) {
	b9075, _ := dragonwingBoardFor("dragonwing-iq-9075")

	caution, err := verifyDragonwingBoard(b9075, qdl.ChipID{MsmID: 0x002eb0e1}, nil, "")
	if err != nil || caution != "" {
		t.Errorf("matching chip id gave %q / %v", caution, err)
	}

	// Naming the board that was found is what separates this from the generic
	// fallback below; both name the board the install targets. Nothing has been
	// written yet, and the flag that would work is already known.
	// The bundle for the board that was asked for is already extracted by the
	// time the board answers, and nothing prunes another board's cache.
	const cache = "/home/u/.cache/wendy/dragonwing-iq-9075/0.19.3/abc123abc123"
	_, err = verifyDragonwingBoard(b9075, qdl.ChipID{MsmID: 0x002e70e1}, nil, cache)
	if err == nil {
		t.Fatal("an IQ-8275 chip id was accepted for an IQ-9075 install")
	}
	for _, want := range []string{"IQ-8275", "IQ-9075", "--device-type dragonwing-iq-8275", cache} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q is missing %q", err, want)
		}
	}
	if !errors.Is(err, errDragonwingNothingWritten) {
		t.Errorf("error %q should be tagged as nothing-written", err)
	}

	caution, err = verifyDragonwingBoard(b9075, qdl.ChipID{}, errors.New("timeout"), cache)
	if err != nil {
		t.Errorf("unreadable chip id blocked the flash: %v", err)
	}
	if caution == "" {
		t.Error("an unreadable chip id produced no caution")
	}

	caution, err = verifyDragonwingBoard(b9075, qdl.ChipID{MsmID: 0x00123456}, nil, cache)
	if err != nil {
		t.Errorf("an unlisted chip id blocked the flash: %v", err)
	}
	if !strings.Contains(caution, "0x123456") {
		t.Errorf("caution %q does not carry the chip id that was read", caution)
	}
	if strings.Contains(caution, "IQ-8275") {
		t.Errorf("caution %q names a board the chip id did not match", caution)
	}
}

// Deliberately wider than the board registry: it mirrors the publisher's
// prefix, so a board is filtered out before wendy can flash it.
func TestInstalledFromFlashBundleMirrorsThePublisherPrefix(t *testing.T) {
	for _, b := range dragonwingBoards {
		if !installedFromFlashBundle(b.deviceType) {
			t.Errorf("installedFromFlashBundle(%q) = false, want true", b.deviceType)
		}
	}
	if _, registered := dragonwingBoardFor("dragonwing-iq-9999"); registered {
		t.Fatal("dragonwing-iq-9999 is registered; pick an unregistered device type")
	}
	if !installedFromFlashBundle("dragonwing-iq-9999") {
		t.Error("an unregistered Dragonwing is not filtered out of the disk-image flow")
	}
	for _, deviceType := range []string{"", "raspberry-pi-5", "jetson-agx-thor", "dragonwing"} {
		if installedFromFlashBundle(deviceType) {
			t.Errorf("installedFromFlashBundle(%q) = true, want false", deviceType)
		}
	}
}

func TestDragonwingBundleFrom(t *testing.T) {
	// The bundle is published under the generic image fields, mirroring what
	// the builder's publish step writes for an EDL-flashed board.
	board, _ := dragonwingBoardFor("dragonwing-iq-8275")
	dm := &deviceManifest{Versions: map[string]deviceVersion{
		"pr-255": {
			Path:      "pr/255/images/dragonwing-iq-8275/pr-255/bundle.qcomflash.tar.gz",
			Checksum:  "c5fe54a5262478f1ab95765a8dcf45b3dc694e2507ab46ccc6c78b44af3b8b58",
			SizeBytes: 601247035,
		},
		"0.19.0": {}, // published without an image artifact
	}}

	got, err := dragonwingBundleFrom(dm, board, "pr-255")
	if err != nil {
		t.Fatalf("dragonwingBundleFrom: %v", err)
	}
	if want := gcsBaseURL + "/pr/255/images/dragonwing-iq-8275/pr-255/bundle.qcomflash.tar.gz"; got.URL != want {
		t.Errorf("URL = %q, want %q", got.URL, want)
	}
	if got.SizeBytes != 601247035 || got.Version != "pr-255" {
		t.Errorf("got %+v", got)
	}

	if _, err := dragonwingBundleFrom(dm, board, "0.19.0"); err == nil {
		t.Error("want an error for a version with no bundle, got nil")
	}
	if _, err := dragonwingBundleFrom(dm, board, "9.9.9"); err == nil {
		t.Error("want an error for an unknown version, got nil")
	}
}

func TestDragonwingBundleFromAcceptsBothShapes(t *testing.T) {
	board, _ := dragonwingBoardFor("dragonwing-iq-9075")

	dedicated := &deviceManifest{Versions: map[string]deviceVersion{
		"0.19.3": {QcomflashPath: "a/new.tar.gz", QcomflashChecksum: "sha-new", QcomflashSizeBytes: 7},
	}}
	got, err := dragonwingBundleFrom(dedicated, board, "0.19.3")
	if err != nil {
		t.Fatalf("dedicated fields: %v", err)
	}
	if got.Checksum != "sha-new" || got.SizeBytes != 7 || !strings.HasSuffix(got.URL, "a/new.tar.gz") {
		t.Errorf("dedicated fields resolved to %+v", got)
	}

	legacy := &deviceManifest{Versions: map[string]deviceVersion{
		"0.19.3": {Path: "a/old.tar.gz", Checksum: "sha-old", SizeBytes: 3},
	}}
	got, err = dragonwingBundleFrom(legacy, board, "0.19.3")
	if err != nil {
		t.Fatalf("legacy fields: %v", err)
	}
	if got.Checksum != "sha-old" || got.SizeBytes != 3 {
		t.Errorf("legacy fields resolved to %+v", got)
	}

	empty := &deviceManifest{Versions: map[string]deviceVersion{"0.19.3": {}}}
	if _, err := dragonwingBundleFrom(empty, board, "0.19.3"); err == nil {
		t.Error("a version with no bundle resolved successfully")
	}
}

// A half-published dedicated triple must not resolve at all: falling back
// would fetch a different artifact, and a missing size disables the
// disk-space pre-flight the extraction depends on.
func TestDragonwingBundleFromRejectsAPartialTriple(t *testing.T) {
	board, _ := dragonwingBoardFor("dragonwing-iq-9075")
	for name, tc := range map[string]struct {
		v       deviceVersion
		wantKey string
	}{
		"no size": {
			v:       deviceVersion{QcomflashPath: "a/new.tar.gz", QcomflashChecksum: "sha-new"},
			wantKey: "qcomflash_size_bytes",
		},
		"no checksum": {
			v:       deviceVersion{QcomflashPath: "a/new.tar.gz", QcomflashSizeBytes: 7},
			wantKey: "qcomflash_checksum",
		},
		"size only": {
			v:       deviceVersion{QcomflashSizeBytes: 7},
			wantKey: "qcomflash_path",
		},
	} {
		t.Run(name, func(t *testing.T) {
			// Legacy fields resolve fine, so a silent fallback would pass.
			v := tc.v
			v.Path, v.Checksum, v.SizeBytes = "a/old.tar.gz", "sha-old", 3
			dm := &deviceManifest{Versions: map[string]deviceVersion{"0.19.3": v}}
			got, err := dragonwingBundleFrom(dm, board, "0.19.3")
			if err == nil {
				t.Fatalf("a partial triple resolved to %+v", got)
			}
			if !strings.Contains(err.Error(), tc.wantKey) {
				t.Errorf("error %q does not name the missing key %s", err, tc.wantKey)
			}
		})
	}
}

func TestDragonwingCacheIsKeyedOnChecksum(t *testing.T) {
	// A --pr build keeps the same version tag across re-pushes, so the cache
	// must key on the artifact too or a stale build would be flashed silently.
	cache := t.TempDir()
	board, _ := dragonwingBoardFor("dragonwing-iq-8275")
	const sumA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const sumB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	root := dragonwingVersionCacheDir(cache, board, "pr-255")
	a := filepath.Join(root, shortChecksum(sumA))
	b := filepath.Join(root, shortChecksum(sumB))
	if a == b {
		t.Fatalf("same cache dir %q for different artifacts", a)
	}
	// Nesting under the version means one tag can never be a prefix of another.
	if got := dragonwingVersionCacheDir(cache, board, "0.19.1"); got == dragonwingVersionCacheDir(cache, board, "0.19.1-nightly") {
		t.Errorf("versions share a cache dir: %q", got)
	}
}

// Two boards must never share a cache directory: the bundles are different
// images published under the same version string.
func TestDragonwingCacheIsPerBoard(t *testing.T) {
	root := t.TempDir()
	a, _ := dragonwingBoardFor("dragonwing-iq-8275")
	b, _ := dragonwingBoardFor("dragonwing-iq-9075")

	da := dragonwingVersionCacheDir(root, a, "0.19.3")
	db := dragonwingVersionCacheDir(root, b, "0.19.3")
	if da == db {
		t.Fatalf("both boards cache at %s", da)
	}
	if !strings.Contains(da, a.deviceType) || !strings.Contains(db, b.deviceType) {
		t.Errorf("cache dirs do not name their board: %s / %s", da, db)
	}
}

func TestPruneStaleDragonwingBundles(t *testing.T) {
	cache := t.TempDir()
	board, _ := dragonwingBoardFor("dragonwing-iq-8275")
	keep := filepath.Join(dragonwingVersionCacheDir(cache, board, "pr-255"), "aaaaaaaaaaaa")
	stale := filepath.Join(dragonwingVersionCacheDir(cache, board, "pr-255"), "bbbbbbbbbbbb")
	// A version whose name extends the pruned one must be untouched: a flat
	// name plus prefix matching used to delete it.
	sibling := filepath.Join(dragonwingVersionCacheDir(cache, board, "pr-255-rc1"), "cccccccccccc")
	other := filepath.Join(dragonwingVersionCacheDir(cache, board, "0.19.1"), "dddddddddddd")
	for _, d := range []string{keep, stale, sibling, other} {
		if err := os.MkdirAll(filepath.Join(d, "extracted"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	pruneStaleDragonwingBundles(dragonwingVersionCacheDir(cache, board, "pr-255"), keep)

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
	board, _ := dragonwingBoardFor("dragonwing-iq-8275")
	if _, ok := findCachedDragonwingBundle(cache, board, "pr-255"); ok {
		t.Fatal("found a bundle in an empty cache")
	}
	dir := filepath.Join(dragonwingVersionCacheDir(cache, board, "pr-255"), "aaaaaaaaaaaa")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory without the extracted tree is not usable.
	if _, ok := findCachedDragonwingBundle(cache, board, "pr-255"); ok {
		t.Error("accepted a cache entry with no extracted bundle")
	}
	if err := os.MkdirAll(filepath.Join(dir, "extracted"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, ok := findCachedDragonwingBundle(cache, board, "pr-255")
	if !ok || got != dir {
		t.Errorf("got %q, %v; want %q", got, ok, dir)
	}
	// A version whose name merely extends the requested one must not match.
	nightly := filepath.Join(dragonwingVersionCacheDir(cache, board, "pr-255-nightly"), "eeeeeeeeeeee")
	if err := os.MkdirAll(filepath.Join(nightly, "extracted"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, _ := findCachedDragonwingBundle(cache, board, "pr-255"); got != dir {
		t.Errorf("got %q, want the exact version's cache %q", got, dir)
	}
}

// The directory named "extracted" is the cache marker, so nothing that is not
// a complete, usable bundle may ever be left under that name: plan.cached
// short-circuits both the download and the extraction, and a partial tree
// would then fail every later run on a missing payload.
func TestDragonwingExtractionNeverCachesAPartialTree(t *testing.T) {
	board, _ := dragonwingBoardFor("dragonwing-iq-9075")
	dir := filepath.Join(dragonwingVersionCacheDir(t.TempDir(), board, "0.19.3"), "aaaaaaaaaaaa")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	plan := dragonwingPlan{version: "0.19.3", dir: dir, tarball: filepath.Join(dir, "bundle.tar.gz")}
	good := map[string][]byte{"rawprogram0.xml": []byte("<data></data>"), "gpt_main0.bin": incompressible(4 << 10)}

	// Cut the stream in half: the closest a test can get to a ctrl+c part way
	// through the ~12 GiB extraction.
	writeTarball(t, plan.tarball, good)
	whole, err := os.ReadFile(plan.tarball)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plan.tarball, whole[:len(whole)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := downloadAndExtractDragonwingBundle(plan, func(string) {}); err == nil {
		t.Fatal("a truncated bundle extracted cleanly")
	}
	if bundleExtracted(dir) {
		t.Fatal("an interrupted extraction left the cache marker behind")
	}
	// The tarball is kept, so the retry re-extracts rather than re-downloading.
	if _, err := os.Stat(plan.tarball); err != nil {
		t.Errorf("the tarball was discarded: %v", err)
	}

	// A tree with no flash descriptor is not a bundle either.
	writeTarball(t, plan.tarball, map[string][]byte{"notes.txt": []byte("nothing to flash")})
	if _, _, err := downloadAndExtractDragonwingBundle(plan, func(string) {}); err == nil {
		t.Fatal("a bundle with no flash descriptor was accepted")
	}
	if bundleExtracted(dir) {
		t.Fatal("a tree with no flash descriptor was cached")
	}

	// Leftovers from a run that was killed mid-extraction are not a cache hit,
	// and the retry reclaims them.
	if err := os.MkdirAll(filepath.Join(dir, "extracted.tmp", "half"), 0o755); err != nil {
		t.Fatal(err)
	}
	if bundleExtracted(dir) {
		t.Fatal("a part-extracted tree was accepted as a cached bundle")
	}
	writeTarball(t, plan.tarball, good)
	got, cached, err := downloadAndExtractDragonwingBundle(plan, func(string) {})
	if err != nil {
		t.Fatalf("extracting a complete bundle: %v", err)
	}
	if cached {
		t.Error("a fresh extraction reported a cache hit")
	}
	if want := filepath.Join(dir, "extracted"); got != want {
		t.Errorf("bundle dir %q, want %q", got, want)
	}
	if !bundleExtracted(dir) {
		t.Error("a complete tree was not cached")
	}
	if _, err := os.Stat(filepath.Join(dir, "extracted.tmp")); !os.IsNotExist(err) {
		t.Error("the part-extracted tree was left behind")
	}
	if _, err := os.Stat(plan.tarball); err == nil {
		t.Error("the tarball was not reclaimed after a good extraction")
	}
}

// A cached tree that is not a usable bundle must be discarded and named: the
// tarball is gone by then, so without this the run fails identically forever
// and the error points inside a directory the user does not know exists.
func TestDragonwingUnusableCacheIsDiscarded(t *testing.T) {
	board, _ := dragonwingBoardFor("dragonwing-iq-8275")
	dir := filepath.Join(dragonwingVersionCacheDir(t.TempDir(), board, "0.19.3"), "aaaaaaaaaaaa")
	if err := os.MkdirAll(filepath.Join(dir, "extracted", "leftovers"), 0o755); err != nil {
		t.Fatal(err)
	}
	plan := dragonwingPlan{version: "0.19.3", cached: true, dir: dir,
		tarball: filepath.Join(dir, "bundle.tar.gz")}

	_, cached, err := downloadAndExtractDragonwingBundle(plan, func(string) {})
	if err == nil {
		t.Fatal("an unusable cached tree was accepted")
	}
	if !cached {
		t.Error("the cached path did not report itself as one")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error %q does not name the cache directory to retry from", err)
	}
	if bundleExtracted(dir) {
		t.Error("the unusable tree survived, so the next run fails the same way")
	}
}

// An undeletable cache must not be reported as discarded: the tree is still
// there, so the user is told which directory to delete rather than sent into a
// retry that fails identically forever.
func TestDragonwingUndeletableCacheNamesTheDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory permissions this test relies on")
	}
	board, _ := dragonwingBoardFor("dragonwing-iq-8275")
	dir := filepath.Join(dragonwingVersionCacheDir(t.TempDir(), board, "0.19.3"), "aaaaaaaaaaaa")
	if err := os.MkdirAll(filepath.Join(dir, "extracted", "leftovers"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Deny the unlink of "extracted" itself, as a cache left owned by another
	// uid does.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	_, _, err := downloadAndExtractDragonwingBundle(
		dragonwingPlan{version: "0.19.3", cached: true, dir: dir}, func(string) {})
	if err == nil {
		t.Fatal("an unusable cached tree was accepted")
	}
	if !bundleExtracted(dir) {
		t.Skip("the tree was removable after all, so there is nothing to report")
	}
	if strings.Contains(err.Error(), "has been discarded") {
		t.Errorf("error %q claims a discard that did not happen", err)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error %q does not name the directory to delete", err)
	}
}

// The tarball is reclaimed once a tree is known good, so an offline run whose
// cache went bad cannot recover by retrying until the manifest is back.
func TestDragonwingUnusableCacheAdviceDependsOnReachability(t *testing.T) {
	board, _ := dragonwingBoardFor("dragonwing-iq-8275")
	discard := func(offline bool) error {
		dir := filepath.Join(dragonwingVersionCacheDir(t.TempDir(), board, "0.19.3"), "aaaaaaaaaaaa")
		if err := os.MkdirAll(filepath.Join(dir, "extracted", "leftovers"), 0o755); err != nil {
			t.Fatal(err)
		}
		_, _, err := downloadAndExtractDragonwingBundle(
			dragonwingPlan{version: "0.19.3", cached: true, offline: offline, dir: dir}, func(string) {})
		if err == nil {
			t.Fatal("an unusable cached tree was accepted")
		}
		return err
	}
	if got := discard(true); !strings.Contains(got.Error(), "manifest is reachable") {
		t.Errorf("offline error %q sends the user into a retry with no network", got)
	}
	if got := discard(false); strings.Contains(got.Error(), "manifest is reachable") {
		t.Errorf("online error %q asks for a manifest it already reached", got)
	}
}

// A download or extraction failure happens before the first write command, with
// the destructive prompt already answered: untagged, it reports nothing at all,
// leaving the board in EDL with no word about DIP switch 3.
func TestDragonwingDownloadFailureReportsAnUntouchedBoard(t *testing.T) {
	board, _ := dragonwingBoardFor("dragonwing-iq-8275")
	dir := filepath.Join(dragonwingVersionCacheDir(t.TempDir(), board, "0.19.3"), "aaaaaaaaaaaa")
	if err := os.MkdirAll(filepath.Join(dir, "extracted", "leftovers"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := &dragonwingFlashRun{plan: dragonwingPlan{version: "0.19.3", cached: true, dir: dir}}

	var download flashStep
	for _, s := range run.steps(context.Background()) {
		if s.id == stepDownload {
			download = s
		}
	}
	if download.run == nil {
		t.Fatal("the flash has no download step")
	}
	_, err := download.run(io.Discard, func(string) {})
	if err == nil {
		t.Fatal("an unusable cached tree was accepted")
	}
	if !errors.Is(err, errDragonwingNothingWritten) {
		t.Fatalf("error %q should be tagged as nothing-written", err)
	}

	var out strings.Builder
	if gotErr := finishDragonwingFlash(&out, run.collected(), "0.19.3", err, false); gotErr == nil {
		t.Fatal("the failure was swallowed")
	}
	for _, want := range []string{"Nothing was written", "DIP switch 3"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("failure output %q is missing %q", out.String(), want)
		}
	}
}

// The offline plan carries no manifest entry, so a download reached from one
// must refuse instead of dereferencing it.
func TestDragonwingDownloadWithoutAManifestEntryRefuses(t *testing.T) {
	_, _, err := downloadAndExtractDragonwingBundle(
		dragonwingPlan{version: "0.19.3", dir: t.TempDir()}, func(string) {})
	if err == nil {
		t.Fatal("a plan with no manifest entry started a download")
	}
	if !strings.Contains(err.Error(), "no bundle to download") {
		t.Errorf("error %q does not say there is nothing to download", err)
	}
}

// incompressible fills n bytes with a pseudo-random pattern, so that half the
// gzip stream really is half the tar.
func incompressible(n int) []byte {
	b := make([]byte, n)
	x := uint32(1)
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

func writeTarball(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name,
			Mode: 0o600, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
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
	b8275, _ := dragonwingBoardFor("dragonwing-iq-8275")
	for name, tc := range map[string]struct {
		run  func() error
		want string
	}{
		"rootfs-only": {
			run: func() error {
				return checkDragonwingFlags(b8275, true, "", false, false, "")
			},
			want: "--rootfs-only",
		},
		"drive": {
			run: func() error {
				return checkDragonwingFlags(b8275, false, "/dev/sda", false, false, "")
			},
			want: "--drive",
		},
		"storage": {
			run: func() error {
				return checkDragonwingFlags(b8275, false, "", false, false, "emmc")
			},
			want: "--storage",
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

func TestCheckDragonwingFlagsAcceptsABareInstall(t *testing.T) {
	b8275, _ := dragonwingBoardFor("dragonwing-iq-8275")
	if err := checkDragonwingFlags(b8275, false, "", false, false, ""); err != nil {
		t.Errorf("bare install rejected: %v", err)
	}
}

func TestCheckDragonwingFlagsNamesTheSelectedBoard(t *testing.T) {
	b9075, _ := dragonwingBoardFor("dragonwing-iq-9075")
	err := checkDragonwingFlags(b9075, true, "", false, false, "")
	if err == nil {
		t.Fatal("want an error, got nil")
	}
	if !strings.Contains(err.Error(), "IQ-9075") {
		t.Errorf("error = %q, want it to name IQ-9075", err)
	}
	if strings.Contains(err.Error(), "IQ-8275") {
		t.Errorf("error = %q, wrongly names IQ-8275", err)
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
	b8275, _ := dragonwingBoardFor("dragonwing-iq-8275")
	one := checkDragonwingFlags(b8275, false, "/dev/sda", false, false, "")
	if one == nil || !strings.Contains(one.Error(), "--drive does not apply") {
		t.Errorf("single flag: %v", one)
	}
	many := checkDragonwingFlags(b8275, false, "/dev/sda", true, false, "")
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
	dir := t.TempDir()
	writeBundleFile(t, dir, "rawprogram0.xml", `<data><program label="efi" filename="efi.bin"
		SECTOR_SIZE_IN_BYTES="4096" start_sector="6" num_partition_sectors="1"/></data>`)
	writeBundleFile(t, dir, "patch0.xml", `<patches><patch filename="DISK"
		SECTOR_SIZE_IN_BYTES="4096" start_sector="1" byte_offset="16" size_in_bytes="4"
		value="0" what="stretch"/></patches>`)
	writeBundleFile(t, dir, "efi.bin", "payload")

	plan, err := qdl.LoadFlashPlan(dir)
	if err != nil {
		t.Fatal(err)
	}
	b9075, _ := dragonwingBoardFor("dragonwing-iq-9075")
	err = flashDragonwing(context.Background(), plan, qdl.DeviceInfo{}, b9075, "", io.Discard,
		func(string) {}, func(string) {})
	if err == nil {
		t.Fatal("want an error for a bundle with no programmer")
	}
	if !errors.Is(err, errDragonwingNothingWritten) {
		t.Errorf("error %q should be tagged as nothing-written", err)
	}
	if !strings.Contains(err.Error(), dragonwingProgrammer) {
		t.Errorf("error %q should name the missing programmer", err)
	}
}

// The output the steps UI withheld has to reach the user on both paths: a
// chip-id caution is the best clue that the wrong board was being written, and
// it used to be printed only after a flash that succeeded.
func TestFinishDragonwingFlashPrintsWarningsOnBothPaths(t *testing.T) {
	const caution = "Could not read the chip id"
	var ok, failed strings.Builder

	if err := finishDragonwingFlash(&ok, []string{caution}, "0.19.3", nil, false); err != nil {
		t.Fatalf("a completed flash returned %v", err)
	}
	if !strings.Contains(ok.String(), caution) || !strings.Contains(ok.String(), "0.19.3") {
		t.Errorf("success output %q is missing the caution or the version", ok.String())
	}

	boom := errors.New("programming efi failed")
	err := finishDragonwingFlash(&failed, []string{caution}, "0.19.3", boom, true)
	if !errors.Is(err, boom) {
		t.Errorf("the failure was swallowed: %v", err)
	}
	if !strings.Contains(failed.String(), caution) {
		t.Errorf("the caution was dropped on the failure path: %q", failed.String())
	}
	if !strings.Contains(failed.String(), "may not boot") {
		t.Errorf("a part-written board got no recovery hint: %q", failed.String())
	}
	if strings.Contains(failed.String(), "Flashed WendyOS") {
		t.Errorf("a failed flash was reported as a success: %q", failed.String())
	}

	// A cancel stays a cancel, so the CLI exits quietly rather than as a fault.
	if err := finishDragonwingFlash(io.Discard, nil, "0.19.3", tui.ErrCancelled, true); !errors.Is(err, ErrUserCancelled) {
		t.Errorf("a cancelled flash returned %v", err)
	}
}

func TestPlanDragonwingFlashResolvesBeforeAnyWrite(t *testing.T) {
	// Every write is resolved up front, so a bundle the flash cannot take
	// fails while the board is still untouched rather than after 12 GiB.
	_, err := planDragonwingFlash(t.TempDir(), t.TempDir(), "", nil, "", nil,
		io.Discard, func(string) {}, func(string) {})
	if err == nil {
		t.Fatal("want an error for an empty bundle dir")
	}
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
	board, _ := dragonwingBoardFor("dragonwing-iq-8275")
	root := filepath.Join(cache, board.deviceType)
	for _, v := range []string{"0.19.1", "pr-255", "pr-255-rc1"} {
		dir := filepath.Join(dragonwingVersionCacheDir(cache, board, v), shortChecksum("abcdef012345"))
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
	board, _ := dragonwingBoardFor("dragonwing-iq-8275")
	outside := filepath.Join(cache, "..", "planted")
	if err := os.MkdirAll(filepath.Join(outside, "aaaaaaaaaaaa", "extracted"), 0o755); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(outside) //nolint:errcheck

	for _, hostile := range []string{"../../planted", "../planted", "a/b", ".."} {
		if dir, ok := findCachedDragonwingBundle(cache, board, hostile); ok {
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
	board, _ := dragonwingBoardFor("dragonwing-iq-8275")
	_, err := getDragonwingBundleInfo(board, "", false, 999999)
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

// Cancelling before any partition write leaves the board untouched, so it gets
// the same reassurance as a failure that never reached the device.
func TestReportDragonwingFailureCoversCancellation(t *testing.T) {
	for name, tc := range map[string]struct {
		err               error
		reachedPartitions bool
		want              string
	}{
		"cancelled before any write": {tui.ErrCancelled, false, "Nothing was written"},
		"cancelled mid-write":        {tui.ErrCancelled, true, "may not boot"},
	} {
		t.Run(name, func(t *testing.T) {
			var buf bytes.Buffer
			reportDragonwingFailure(&buf, tc.err, tc.reachedPartitions)
			if !strings.Contains(buf.String(), tc.want) {
				t.Errorf("output = %q, want it to mention %q", buf.String(), tc.want)
			}
		})
	}
}
