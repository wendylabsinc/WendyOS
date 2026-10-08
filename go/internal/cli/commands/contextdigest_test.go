package commands

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// useContextDigestCacheDir points the digest cache at a fresh directory.
func useContextDigestCacheDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	contextDigestCacheTestDir = dir
	t.Cleanup(func() { contextDigestCacheTestDir = "" })
	return dir
}

// digestOf runs one cached digest of rel in cwd as a hash run would: open,
// digest, save. It returns the digest and how many files were read.
func digestOf(t *testing.T, cwd, rel string, now time.Time) (string, int) {
	t.Helper()
	abs := filepath.Join(cwd, filepath.FromSlash(rel))
	info, err := os.Lstat(abs)
	if err != nil {
		t.Fatal(err)
	}
	c := openContextDigestCache(cwd, filepath.Join(cwd, "Dockerfile"), now)
	d, err := c.fileDigest(abs, rel, info)
	if err != nil {
		t.Fatal(err)
	}
	c.save()
	return d, c.reads
}

// useContextDigestRoot stands in for the check of the context root's file
// system (contextDigestRootDevice) until the test ends.
func useContextDigestRoot(t *testing.T, f func(root string) (uint64, bool)) {
	t.Helper()
	orig := contextDigestRootDevice
	contextDigestRootDevice = f
	t.Cleanup(func() { contextDigestRootDevice = orig })
}

// setCachedDigest rewrites rel's digest in the cache file p, so a test can
// tell a digest served from the cache from one read from the file.
func setCachedDigest(t *testing.T, p, rel, digest string) {
	t.Helper()
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var f contextDigestFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	e, ok := f.Entries[rel]
	if !ok {
		t.Fatalf("%s is not cached: %v", rel, f.Entries)
	}
	e.Digest = digest
	f.Entries[rel] = e
	if data, err = json.Marshal(f); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func sha256HexOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// settledNow is a clock an hour ahead, so files the test just wrote count as
// settled (outside digestRacyWindow).
func settledNow() time.Time { return time.Now().Add(time.Hour) }

func TestContextDigestCacheReusesAnUnchangedFile(t *testing.T) {
	requireFileIdentity(t)
	useContextDigestCacheDir(t)
	cwd := t.TempDir()
	writeFile(t, cwd, "model.bin", "weights v1\n")

	d1, reads := digestOf(t, cwd, "model.bin", settledNow())
	if d1 != sha256HexOf("weights v1\n") || reads != 1 {
		t.Fatalf("cold: digest %s, reads %d", d1, reads)
	}
	d2, reads := digestOf(t, cwd, "model.bin", settledNow())
	if d2 != d1 || reads != 0 {
		t.Fatalf("warm: digest %s (want %s), reads %d (want 0)", d2, d1, reads)
	}
}

// TestContextDigestCacheSkipsRacyFiles: a file written within
// digestRacyWindow of hashing is read every time until it settles, so a
// second same-size write in the same timestamp tick cannot hide.
func TestContextDigestCacheSkipsRacyFiles(t *testing.T) {
	requireFileIdentity(t)
	useContextDigestCacheDir(t)
	cwd := t.TempDir()
	writeFile(t, cwd, "app.py", "print('v1')\n")
	p := filepath.Join(cwd, "app.py")
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	mtime := info.ModTime()

	if _, reads := digestOf(t, cwd, "app.py", time.Now()); reads != 1 {
		t.Fatalf("reads = %d, want 1", reads)
	}
	// Same size, same mtime: only a re-read can see this edit.
	writeFile(t, cwd, "app.py", "print('v2')\n")
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	d, reads := digestOf(t, cwd, "app.py", time.Now())
	if reads != 1 || d != sha256HexOf("print('v2')\n") {
		t.Fatalf("racy file served from cache: digest %s, reads %d", d, reads)
	}
}

// TestContextDigestCacheRereadsAChangedFile covers the edits that keep size
// and mtime: an in-place rewrite with the mtime reset (ctime changes) and a
// replacement by rename (inode changes).
func TestContextDigestCacheRereadsAChangedFile(t *testing.T) {
	requireFileIdentity(t)
	useContextDigestCacheDir(t)
	cwd := t.TempDir()
	p := filepath.Join(cwd, "app.py")
	writeFile(t, cwd, "app.py", "print('v1')\n")
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if _, reads := digestOf(t, cwd, "app.py", settledNow()); reads != 1 {
		t.Fatalf("cold reads = %d", reads)
	}

	time.Sleep(50 * time.Millisecond) // a new coarse-clock tick for ctime
	writeFile(t, cwd, "app.py", "print('v2')\n")
	if err := os.Chtimes(p, old, old); err != nil {
		t.Fatal(err)
	}
	if d, reads := digestOf(t, cwd, "app.py", settledNow()); reads != 1 || d != sha256HexOf("print('v2')\n") {
		t.Fatalf("in-place rewrite: digest %s, reads %d", d, reads)
	}

	tmp := filepath.Join(cwd, "app.py.tmp")
	writeFile(t, cwd, "app.py.tmp", "print('v3')\n")
	if err := os.Chtimes(tmp, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
	if d, reads := digestOf(t, cwd, "app.py", settledNow()); reads != 1 || d != sha256HexOf("print('v3')\n") {
		t.Fatalf("replaced file: digest %s, reads %d", d, reads)
	}
}

// TestContextDigestCacheSkipsFilesOnAnotherDevice: a file whose device is not
// the context root's lives on another mount (NFS, a FAT stick, sshfs), whose
// stat the cache cannot trust even when the root's file system is on the
// allowlist. It is never served from the cache, and never stored in it.
func TestContextDigestCacheSkipsFilesOnAnotherDevice(t *testing.T) {
	requireFileIdentity(t)
	useContextDigestCacheDir(t)
	cwd := t.TempDir()
	writeFile(t, cwd, "mnt/model.bin", "weights v1\n")
	want := sha256HexOf("weights v1\n")
	if _, reads := digestOf(t, cwd, "mnt/model.bin", settledNow()); reads != 1 {
		t.Fatalf("cold reads = %d", reads)
	}
	// A cached entry whose identity still matches, but whose digest a lookup
	// would now put in the hash.
	p, _ := contextDigestCachePath(cwd, filepath.Join(cwd, "Dockerfile"))
	setCachedDigest(t, p, "mnt/model.bin", sha256HexOf("stale"))

	sameFS := contextDigestRootDevice
	useContextDigestRoot(t, func(root string) (uint64, bool) {
		dev, ok := sameFS(root)
		return dev + 1, ok // the root is on another device than the file
	})
	for run := range 2 {
		if d, reads := digestOf(t, cwd, "mnt/model.bin", settledNow()); reads != 1 || d != want {
			t.Fatalf("run %d: digest %s (want %s), reads %d (want 1)", run, d, want, reads)
		}
	}
	if c := openContextDigestCache(cwd, filepath.Join(cwd, "Dockerfile"), settledNow()); len(c.prev) != 0 {
		t.Fatalf("a file on another device was cached: %v", c.prev)
	}
}

func TestContextDigestCacheToleratesACorruptFile(t *testing.T) {
	requireFileIdentity(t)
	useContextDigestCacheDir(t)
	cwd := t.TempDir()
	writeFile(t, cwd, "app.py", "print('v1')\n")
	p, ok := contextDigestCachePath(cwd, filepath.Join(cwd, "Dockerfile"))
	if !ok {
		t.Fatal("no cache path")
	}
	for name, garbage := range map[string]string{
		"not json":        "{not json",
		"other version":   `{"version":99,"entries":{"app.py":{"id":{},"sha256":"x"}}}`,
		"null entries":    `{"version":1,"entries":null}`,
		"truncated write": `{"version":1,"entries":{"app.py":{"id":{"size":12`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(p, []byte(garbage), 0o644); err != nil {
				t.Fatal(err)
			}
			d, reads := digestOf(t, cwd, "app.py", settledNow())
			if reads != 1 || d != sha256HexOf("print('v1')\n") {
				t.Fatalf("digest %s, reads %d", d, reads)
			}
			if _, reads := digestOf(t, cwd, "app.py", settledNow()); reads != 0 {
				t.Fatalf("the corrupt file was not replaced: reads %d", reads)
			}
		})
	}
}

// TestContextDigestCacheRejectsABadDigest: an entry whose identity matches but
// whose digest is not a SHA-256 is a miss, not a value to hash.
func TestContextDigestCacheRejectsABadDigest(t *testing.T) {
	requireFileIdentity(t)
	useContextDigestCacheDir(t)
	cwd := t.TempDir()
	writeFile(t, cwd, "app.py", "print('v1')\n")
	digestOf(t, cwd, "app.py", settledNow())

	p, _ := contextDigestCachePath(cwd, filepath.Join(cwd, "Dockerfile"))
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var f contextDigestFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	e := f.Entries["app.py"]
	e.Digest = "not-a-digest"
	f.Entries["app.py"] = e
	data, _ = json.Marshal(f)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if d, reads := digestOf(t, cwd, "app.py", settledNow()); reads != 1 || d != sha256HexOf("print('v1')\n") {
		t.Fatalf("digest %s, reads %d", d, reads)
	}
}

func TestContextDigestCacheIsBounded(t *testing.T) {
	requireFileIdentity(t)
	dir := useContextDigestCacheDir(t)

	// Per file: a context over the entry cap is hashed without a cache.
	orig := contextDigestMaxEntries
	contextDigestMaxEntries = 2
	t.Cleanup(func() { contextDigestMaxEntries = orig })
	cwd := t.TempDir()
	c := openContextDigestCache(cwd, filepath.Join(cwd, "Dockerfile"), settledNow())
	for i := range 3 {
		rel := fmt.Sprintf("f%d", i)
		writeFile(t, cwd, rel, rel)
		info, err := os.Lstat(filepath.Join(cwd, rel))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.fileDigest(filepath.Join(cwd, rel), rel, info); err != nil {
			t.Fatal(err)
		}
	}
	c.save()
	if p, _ := contextDigestCachePath(cwd, filepath.Join(cwd, "Dockerfile")); fileExists(p) {
		t.Fatal("a cache over contextDigestMaxEntries was written")
	}
	contextDigestMaxEntries = orig

	// Across projects: at most contextDigestMaxFiles cache files, least
	// recently used out first. Fill the directory with stale caches, then
	// save a new one.
	for i := range contextDigestMaxFiles + 2 {
		p := filepath.Join(dir, fmt.Sprintf("stale%02d.json", i))
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		stamp := time.Now().Add(-time.Duration(i+1) * time.Hour)
		if err := os.Chtimes(p, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	proj := t.TempDir()
	writeFile(t, proj, "app.py", "x")
	digestOf(t, proj, "app.py", settledNow())
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != contextDigestMaxFiles {
		t.Fatalf("%d cache files, want %d", len(entries), contextDigestMaxFiles)
	}
	if p, _ := contextDigestCachePath(proj, filepath.Join(proj, "Dockerfile")); !fileExists(p) {
		t.Fatal("the newest cache was pruned")
	}
	for _, gone := range []int{contextDigestMaxFiles + 1, contextDigestMaxFiles, contextDigestMaxFiles - 1} {
		if fileExists(filepath.Join(dir, fmt.Sprintf("stale%02d.json", gone))) {
			t.Fatalf("stale%02d.json, among the oldest, survived", gone)
		}
	}
}

// TestContextDigestCacheDropsAnOversizedFile: a cache file over
// contextDigestMaxFileBytes is never read back, so save removes the old one
// rather than write a file that only costs a rewrite on every run.
func TestContextDigestCacheDropsAnOversizedFile(t *testing.T) {
	requireFileIdentity(t)
	useContextDigestCacheDir(t)
	cwd := t.TempDir()
	writeFile(t, cwd, "app.py", "print('v1')\n")
	p, _ := contextDigestCachePath(cwd, filepath.Join(cwd, "Dockerfile"))
	digestOf(t, cwd, "app.py", settledNow())
	if !fileExists(p) {
		t.Fatal("test setup: no cache was written")
	}

	orig := contextDigestMaxFileBytes
	contextDigestMaxFileBytes = 64 // below any one-entry cache
	t.Cleanup(func() { contextDigestMaxFileBytes = orig })
	if _, reads := digestOf(t, cwd, "app.py", settledNow()); reads != 1 {
		t.Fatalf("an oversized cache was used: reads %d", reads)
	}
	if fileExists(p) {
		t.Fatal("an oversized cache file was kept")
	}

	fresh := t.TempDir()
	writeFile(t, fresh, "app.py", "print('v1')\n")
	digestOf(t, fresh, "app.py", settledNow())
	if p, _ := contextDigestCachePath(fresh, filepath.Join(fresh, "Dockerfile")); fileExists(p) {
		t.Fatal("an oversized cache file was written")
	}
}

// TestContextDigestCacheConcurrentRuns: runs racing on one project (two
// terminals, or Compose services sharing a context) leave a valid cache.
func TestContextDigestCacheConcurrentRuns(t *testing.T) {
	requireFileIdentity(t)
	useContextDigestCacheDir(t)
	cwd := t.TempDir()
	for i := range 20 {
		writeFile(t, cwd, fmt.Sprintf("src/f%02d.py", i), fmt.Sprintf("print(%d)\n", i))
	}
	now := settledNow()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			c := openContextDigestCache(cwd, filepath.Join(cwd, "Dockerfile"), now)
			for i := range 20 {
				rel := fmt.Sprintf("src/f%02d.py", i)
				abs := filepath.Join(cwd, filepath.FromSlash(rel))
				info, err := os.Lstat(abs)
				if err != nil {
					t.Error(err)
					return
				}
				d, err := c.fileDigest(abs, rel, info)
				if err != nil || d != sha256HexOf(fmt.Sprintf("print(%d)\n", i)) {
					t.Errorf("%s: digest %s, err %v", rel, d, err)
				}
			}
			c.save()
		})
	}
	wg.Wait()
	c := openContextDigestCache(cwd, filepath.Join(cwd, "Dockerfile"), now)
	if len(c.prev) != 20 {
		t.Fatalf("cache holds %d entries after concurrent runs, want 20", len(c.prev))
	}
}
