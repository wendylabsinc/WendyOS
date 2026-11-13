package commands

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// The build-context digest cache remembers each context file's SHA-256
// between runs, so a project with digest-pinned bases (a Stagefile) does not
// re-read every file to compute its deploy fingerprint (WDY-3216). An entry
// is reused only while the file's fileIdentity (size, mtime, ctime, device,
// inode) is unchanged, and a file is cached only once it has settled (see
// fileIdentity.settled). A wrong or missing cache can therefore only cost a
// re-read, never a skipped rebuild.
//
// Trust: identity only proves a file unchanged where stat is authoritative.
// The cache is used only when the context root is on a local file system
// whose kernel maintains ctime (the per-OS allowlist, fscache_*.go), and only
// for files on the root's device, since any other device is another mount.
// On NFS, SMB or sshfs, stat comes from an attribute cache that can hide
// another host's edit for up to a minute; on FAT and exFAT, ctime is mtime,
// so touch -r restores a rewritten file's whole identity. There every file is
// read, as before the cache existed.
//
// Layout: one JSON file per (context directory, Dockerfile) pair under
// <user cache dir>/wendy/context-digests/, holding only the files that pair
// hashed on its latest run. Different Dockerfiles in one directory can use
// different ignore files, so they keep separate caches instead of evicting
// each other's entries.
//
// Bounds: at most contextDigestMaxEntries entries and contextDigestMaxFileBytes
// bytes per cache file (a larger context is hashed without a cache), and at
// most contextDigestMaxFiles cache files, least recently used first out.
//
// Concurrency: a cache file is replaced by an atomic rename, so a reader
// sees either the old or the new file, never a torn one. Concurrent runs on
// one project may drop each other's new entries (last writer wins), which
// only costs a re-read. A corrupt, unreadable or other-version file reads as
// empty.

const (
	contextDigestCacheVersion = 1
	contextDigestMaxFiles     = 64
)

// contextDigestMaxEntries and contextDigestMaxFileBytes are vars so the
// size-bound tests can lower them.
var (
	contextDigestMaxEntries   = 100_000
	contextDigestMaxFileBytes = int64(32 << 20)
)

// contextDigestCacheTestDir, when non-empty, overrides the cache directory.
var contextDigestCacheTestDir string

// contextDigestClock is time.Now; tests move it forward so freshly written
// files count as settled.
var contextDigestClock = time.Now

// contextDigestRootDevice returns the device of the context root when the
// cache can trust stat on the root's file system (digestCacheFSEligible), and
// false otherwise. Tests replace it to stand in for another file system.
var contextDigestRootDevice = func(root string) (uint64, bool) {
	if !digestCacheFSEligible(root) {
		return 0, false
	}
	info, err := os.Stat(root)
	if err != nil {
		return 0, false
	}
	id, ok := fileIdentityOf(info)
	return id.Dev, ok
}

type contextDigestEntry struct {
	ID     fileIdentity `json:"id"`
	Digest string       `json:"sha256"` // lowercase hex
}

type contextDigestFile struct {
	Version int                           `json:"version"`
	Entries map[string]contextDigestEntry `json:"entries"`
}

// contextDigestCache is one run's view of one cache file. It is not safe for
// concurrent use; each computeBuildInputHash call opens its own.
type contextDigestCache struct {
	path    string // "" when the cache is off: nothing is served or persisted
	dev     uint64 // the context root's device; files on any other are not cached
	now     time.Time
	prev    map[string]contextDigestEntry // loaded, by forward-slash relative path
	next    map[string]contextDigestEntry // this run's files, saved by save
	changed bool                          // next gained an entry prev did not have
	reads   int                           // files read this run (tests)
}

func contextDigestCacheDir() (string, bool) {
	if contextDigestCacheTestDir != "" {
		return contextDigestCacheTestDir, true
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(base, "wendy", "context-digests"), true
}

// contextDigestCachePath names the cache file for a context directory and the
// Dockerfile whose ignore rules chose its files.
func contextDigestCachePath(cwd, dockerfilePath string) (string, bool) {
	dir, ok := contextDigestCacheDir()
	if !ok {
		return "", false
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return "", false
	}
	absDockerfile, err := filepath.Abs(dockerfilePath)
	if err != nil {
		return "", false
	}
	sum := sha256.Sum256([]byte(absCwd + "\x00" + absDockerfile))
	return filepath.Join(dir, hex.EncodeToString(sum[:16])+".json"), true
}

// openContextDigestCache loads the cache for (cwd, dockerfilePath). now is
// when hashing starts; files that have not settled by then are not cached.
// The cache is off when there is no cache directory, or when cwd's file
// system is not one whose stat it can trust (see contextDigestRootDevice).
func openContextDigestCache(cwd, dockerfilePath string, now time.Time) *contextDigestCache {
	c := &contextDigestCache{
		now:  now,
		prev: map[string]contextDigestEntry{},
		next: map[string]contextDigestEntry{},
	}
	p, ok := contextDigestCachePath(cwd, dockerfilePath)
	if !ok {
		return c
	}
	dev, ok := contextDigestRootDevice(cwd)
	if !ok {
		return c
	}
	c.path, c.dev = p, dev
	if info, err := os.Stat(p); err != nil || info.Size() > contextDigestMaxFileBytes {
		return c
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return c
	}
	var f contextDigestFile
	if json.Unmarshal(data, &f) != nil || f.Version != contextDigestCacheVersion || f.Entries == nil {
		return c
	}
	c.prev = f.Entries
	return c
}

// fileDigest returns the lowercase hex SHA-256 of the file at abs, whose
// walk-time lstat is info. It reuses the cached digest when the file's
// identity is unchanged and otherwise reads the file.
func (c *contextDigestCache) fileDigest(abs, rel string, info fs.FileInfo) (string, error) {
	if id, ok := fileIdentityOf(info); ok && c.trusts(id) {
		if e, hit := c.prev[rel]; hit && e.ID == id && validSHA256Hex(e.Digest) {
			c.next[rel] = e
			return e.Digest, nil
		}
	}
	c.reads++
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	digest := hex.EncodeToString(h.Sum(nil))
	// Cache only when the file we read is settled and did not change while we
	// read it. Otherwise the digest is still right for this run's hash (the
	// same bytes the old uncached code would have hashed); it just is not
	// remembered.
	after, err := f.Stat()
	if err != nil {
		return digest, nil
	}
	idBefore, ok1 := fileIdentityOf(before)
	idAfter, ok2 := fileIdentityOf(after)
	if ok1 && ok2 && idBefore == idAfter && c.trusts(idBefore) && idBefore.settled(c.now) {
		e := contextDigestEntry{ID: idBefore, Digest: digest}
		if old, had := c.prev[rel]; !had || old != e {
			c.changed = true
		}
		c.next[rel] = e
	}
	return digest, nil
}

// trusts reports whether the cache may serve or store the digest of a file
// with identity id: only while the cache is on, and only for a file on the
// context root's device. A file on another device is on another mount (NFS,
// a FAT stick, sshfs), whose stat need not be authoritative.
func (c *contextDigestCache) trusts(id fileIdentity) bool {
	return c.path != "" && id.Dev == c.dev
}

// save persists this run's entries. It is best effort: a failure only means
// the next run reads more files. An unchanged cache is not rewritten; its
// mtime is bumped instead, which is what the LRU bound orders by.
func (c *contextDigestCache) save() {
	if c.path == "" {
		return
	}
	if !c.changed && len(c.next) == len(c.prev) {
		if len(c.next) > 0 {
			_ = os.Chtimes(c.path, c.now, c.now)
		}
		return
	}
	if len(c.next) > contextDigestMaxEntries {
		// Too large to be worth parsing on every run: hash this context
		// without a cache rather than keep a stale oversized file.
		_ = os.Remove(c.path)
		return
	}
	data, err := json.Marshal(contextDigestFile{Version: contextDigestCacheVersion, Entries: c.next})
	if err != nil {
		return
	}
	if int64(len(data)) > contextDigestMaxFileBytes {
		// openContextDigestCache would never read it back; writing it would
		// only cost a rewrite on every run. Drop the old one too.
		_ = os.Remove(c.path)
		return
	}
	dir := filepath.Dir(c.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(data)
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), c.path) != nil {
		_ = os.Remove(tmp.Name())
		return
	}
	pruneContextDigestCaches(dir, contextDigestMaxFiles)
}

// pruneContextDigestCaches keeps the keep most recently used files in dir.
// Stale temp files from a crashed writer age out the same way.
func pruneContextDigestCaches(dir string, keep int) {
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) <= keep {
		return
	}
	type aged struct {
		name string
		mod  time.Time
	}
	files := make([]aged, 0, len(entries))
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, aged{e.Name(), info.ModTime()})
	}
	if len(files) <= keep {
		return
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	for _, f := range files[keep:] {
		_ = os.Remove(filepath.Join(dir, f.name))
	}
}

func validSHA256Hex(s string) bool {
	if len(s) != sha256.Size*2 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
