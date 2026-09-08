package commands

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCacheBlob(t *testing.T, root, app, digest string, content []byte) string {
	t.Helper()
	dir := filepath.Join(root, app, "blobs", "sha256")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, digest)
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// 64-char names so isBlobPath accepts them (real digests are hex sha256).
const digestA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const digestB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
const digestC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

func TestDedupBuildCache_LinksIdenticalBlobsAcrossApps(t *testing.T) {
	root := t.TempDir()
	shared := []byte("the same multi-GB base layer, pretend")
	p1 := writeCacheBlob(t, root, "app1", digestA, shared)
	p2 := writeCacheBlob(t, root, "app2", digestA, shared)         // dup of p1
	pUniq := writeCacheBlob(t, root, "app2", digestB, []byte("x")) // unrelated

	// Distinct inodes before dedup.
	if fi1, _ := os.Stat(p1); func() bool { fi2, _ := os.Stat(p2); return os.SameFile(fi1, fi2) }() {
		t.Fatal("precondition: p1 and p2 should start as separate inodes")
	}

	reclaimed := dedupBuildCache(root)
	if reclaimed != int64(len(shared)) {
		t.Fatalf("reclaimed = %d, want %d", reclaimed, len(shared))
	}

	fi1, _ := os.Stat(p1)
	fi2, _ := os.Stat(p2)
	if !os.SameFile(fi1, fi2) {
		t.Fatal("p1 and p2 should share one inode after dedup")
	}
	// Content must be byte-identical and unrelated blob untouched.
	if got, _ := os.ReadFile(p2); string(got) != string(shared) {
		t.Fatalf("p2 content corrupted: %q", got)
	}
	if got, _ := os.ReadFile(pUniq); string(got) != "x" {
		t.Fatalf("unrelated blob changed: %q", got)
	}
	// No stray temp file left behind.
	if _, err := os.Stat(p2 + ".deduptmp"); !os.IsNotExist(err) {
		t.Fatal("temp file not cleaned up")
	}
}

func TestEnforceBuildCacheSizeCap_EvictsOldestUnderCap(t *testing.T) {
	root := t.TempDir()
	// Three ~100-byte app dirs holding DISTINCT layers, oldest -> newest.
	mk := func(app, digest string, ageMinutes int) string {
		p := writeCacheBlob(t, root, app, digest, make([]byte, 100))
		mt := time.Now().Add(-time.Duration(ageMinutes) * time.Minute)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
		return filepath.Dir(filepath.Dir(filepath.Dir(p))) // the app dir
	}
	oldDir := mk("old", digestA, 120)
	midDir := mk("mid", digestB, 60)
	newDir := mk("new", digestC, 30)

	// Cap at 250 bytes: total ~300, one dir (the oldest) must go.
	reclaimed := enforceBuildCacheSizeCap(250, nil, 10*time.Minute, root)
	if reclaimed < 100 {
		t.Fatalf("reclaimed = %d, want >= 100", reclaimed)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatal("oldest dir should have been evicted")
	}
	for _, d := range []string{midDir, newDir} {
		if _, err := os.Stat(d); err != nil {
			t.Fatalf("dir %s should survive: %v", d, err)
		}
	}
}

// A layer shared (hardlinked) between two app dirs must be counted once and
// freed only when the LAST referencing dir is evicted.
func TestEnforceBuildCacheSizeCap_SharedBlobFreedOnLastRef(t *testing.T) {
	root := t.TempDir()
	shared := make([]byte, 200)
	old := writeCacheBlob(t, root, "old", digestA, shared)
	new1 := writeCacheBlob(t, root, "new", digestA, shared)
	// Hardlink them so digestA is one 200-byte inode across both dirs.
	if !relinkBlob(old, new1) {
		t.Fatal("setup: relink failed")
	}
	ageDir := func(p string, min int) {
		mt := time.Now().Add(-time.Duration(min) * time.Minute)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	ageDir(old, 120)
	ageDir(new1, 30)
	oldDir := filepath.Dir(filepath.Dir(filepath.Dir(old)))
	newDir := filepath.Dir(filepath.Dir(filepath.Dir(new1)))

	// Real usage is 200 bytes (counted once). Pin the newer dir so only the old
	// one is evictable: removing it frees nothing, because the pinned dir still
	// links the shared layer.
	reclaimed := enforceBuildCacheSizeCap(100, map[string]bool{newDir: true}, 10*time.Minute, root)
	if reclaimed != 0 {
		t.Fatalf("evicting one of two linkers frees nothing, got %d", reclaimed)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatal("oldest (unpinned) dir should still be removed (it just didn't free bytes)")
	}
	if _, err := os.Stat(newDir); err != nil {
		t.Fatalf("pinned dir must survive: %v", err)
	}
}

func TestEnforceBuildCacheSizeCap_SkipsKeepAndActive(t *testing.T) {
	root := t.TempDir()
	p := writeCacheBlob(t, root, "busy", digestA, make([]byte, 100))
	// Freshly written (mtime now) => inside the active window => never evicted,
	// even far below-cap pressure.
	appDir := filepath.Dir(filepath.Dir(filepath.Dir(p)))
	reclaimed := enforceBuildCacheSizeCap(10, nil, 10*time.Minute, root)
	if reclaimed != 0 {
		t.Fatalf("active-window dir must not be evicted, reclaimed=%d", reclaimed)
	}
	if _, err := os.Stat(appDir); err != nil {
		t.Fatalf("active dir should survive: %v", err)
	}
}

// A layout dir whose lock another wendy process holds (build → read → push,
// see lockOCILayoutDir) is in use no matter how old its blobs look — a long
// chunk push reads blobs without writing any — and must never be evicted.
func TestEnforceBuildCacheSizeCap_SkipsLockedLayoutDir(t *testing.T) {
	root := t.TempDir()
	p := writeCacheBlob(t, root, "busy", digestA, make([]byte, 100))
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(p, stale, stale); err != nil {
		t.Fatal(err)
	}
	appDir := filepath.Dir(filepath.Dir(filepath.Dir(p)))

	release, err := lockOCILayoutDir(context.Background(), appDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := enforceBuildCacheSizeCap(10, nil, 10*time.Minute, root); got != 0 {
		t.Fatalf("locked dir must not be evicted, reclaimed=%d", got)
	}
	if _, err := os.Stat(appDir); err != nil {
		t.Fatalf("locked dir should survive: %v", err)
	}
	release()

	// Once the owner lets go, the same stale dir is fair game again.
	if got := enforceBuildCacheSizeCap(10, nil, 10*time.Minute, root); got != 100 {
		t.Fatalf("unlocked stale dir should be evicted, reclaimed=%d", got)
	}
	if _, err := os.Stat(appDir); !os.IsNotExist(err) {
		t.Fatal("unlocked stale dir should have been removed")
	}
}

// The dedup-aware total behind both the size cap and `wendy cache list` counts
// a hardlinked blob once, however many app dirs link it, so the number the
// user sees next to the cap is the number the cap is enforced against.
func TestScanBuildCache_CountsSharedBlobOnce(t *testing.T) {
	root := t.TempDir()
	shared := make([]byte, 200)
	a1 := writeCacheBlob(t, root, "app1", digestA, shared)
	a2 := writeCacheBlob(t, root, "app2", digestA, shared)
	if !relinkBlob(a1, a2) {
		t.Fatal("setup: relink failed")
	}
	writeCacheBlob(t, root, "app2", digestB, make([]byte, 50))

	scan := scanBuildCache(nil, root)
	if scan.total != 250 {
		t.Fatalf("total = %d, want 250 (shared blob counted once)", scan.total)
	}
	if len(scan.units) != 2 {
		t.Fatalf("units = %d, want 2", len(scan.units))
	}
	// Per-row sizes are per-link by design and so can sum past the real total.
	s1, _ := dirSizeAndMtime(filepath.Dir(filepath.Dir(filepath.Dir(a1))))
	s2, _ := dirSizeAndMtime(filepath.Dir(filepath.Dir(filepath.Dir(a2))))
	if s1+s2 != 450 {
		t.Fatalf("per-link row sizes = %d, want 450", s1+s2)
	}
}

func TestBuildCacheMaxBytes_EnvOverride(t *testing.T) {
	t.Setenv("WENDY_BUILD_CACHE_MAX_BYTES", "12345")
	if got := buildCacheMaxBytes(); got != 12345 {
		t.Fatalf("got %d, want 12345", got)
	}
	t.Setenv("WENDY_BUILD_CACHE_MAX_BYTES", "garbage")
	if got := buildCacheMaxBytes(); got != defaultBuildCacheMaxBytes {
		t.Fatalf("garbage should fall back to default, got %d", got)
	}
}
