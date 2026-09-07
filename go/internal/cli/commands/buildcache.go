package commands

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
)

// The persistent build caches under ~/Library/Caches/wendy grow without bound:
// buildx/ holds a per-app+platform BuildKit local cache export (isolated on
// purpose — the local exporter's ingest store is not concurrency-safe, WDY-1689/
// WDY-1711) and ocilayout/ holds the deployable OCI layout per app+platform.
// Two costs accumulate: (1) the SAME sha256 layer is stored once per app dir and
// again in ocilayout, and (2) every app ever built keeps a full copy forever.
//
// dedupBuildCache attacks (1) without merging the stores (which would restore the
// concurrent-ingest corruption the isolation prevents); enforceBuildCacheSizeCap
// attacks (2) by evicting whole least-recently-used app dirs above a cap.

const defaultBuildCacheMaxBytes int64 = 100 << 30 // 100 GiB

// buildCacheMaxBytes is the size ceiling for the on-disk build caches. Override
// with WENDY_BUILD_CACHE_MAX_BYTES (bytes); a non-positive value disables the cap.
func buildCacheMaxBytes() int64 {
	if v := strings.TrimSpace(os.Getenv("WENDY_BUILD_CACHE_MAX_BYTES")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return defaultBuildCacheMaxBytes
}

// buildCacheRoots returns the buildx and ocilayout roots under userCacheDir/wendy.
func buildCacheRoots(userCacheDir string) []string {
	base := filepath.Join(userCacheDir, "wendy")
	return []string{filepath.Join(base, "buildx"), filepath.Join(base, "ocilayout")}
}

// isBlobPath reports whether p is a content-addressed blob file (…/blobs/sha256/<digest>).
// Only these are safe to hardlink-dedup: BuildKit names them by their content
// digest and writes them once via an ingest/ rename, so a file present here is
// complete and immutable — identical name guarantees identical bytes.
func isBlobPath(p string) bool {
	dir, name := filepath.Split(filepath.ToSlash(p))
	return strings.HasSuffix(dir, "/blobs/sha256/") && len(name) == 64 && !strings.ContainsAny(name, "./")
}

// dedupBuildCache hardlinks content-identical blobs across roots so a layer
// shared by many apps (a multi-GB CUDA base) or held in both the buildx cache and
// the ocilayout deploy dir occupies one inode instead of N copies. Replacement is
// atomic (link to a temp name, rename over the victim) so a concurrent reader sees
// either the old inode or the byte-identical new one. ingest/, index.json and
// oci-layout are never touched. Best-effort: any stat/link error skips that blob.
// Returns the bytes reclaimed (sum of the now-shared duplicate copies).
func dedupBuildCache(roots ...string) int64 {
	type canon struct {
		path string
		info os.FileInfo
	}
	canonical := map[string]canon{} // digest -> first-seen copy
	var reclaimed int64

	for _, root := range roots {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !isBlobPath(p) {
				return nil //nolint:nilerr // best-effort: skip unreadable entries
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			digest := d.Name()
			c, seen := canonical[digest]
			if !seen {
				canonical[digest] = canon{path: p, info: fi}
				return nil
			}
			// Same content digest but different size means one copy is corrupt;
			// never link them. Already-shared inodes are nothing to do.
			if c.info.Size() != fi.Size() || os.SameFile(c.info, fi) {
				return nil
			}
			if relinkBlob(c.path, p) {
				reclaimed += fi.Size()
			}
			return nil
		})
	}
	return reclaimed
}

// relinkBlob atomically replaces victim with a hardlink to canonical. Returns
// true on success. On any failure the victim is left untouched.
func relinkBlob(canonical, victim string) bool {
	tmp := victim + ".deduptmp"
	_ = os.Remove(tmp)
	if err := os.Link(canonical, tmp); err != nil {
		return false // e.g. cross-device link; leave both copies
	}
	if err := os.Rename(tmp, victim); err != nil {
		_ = os.Remove(tmp)
		return false
	}
	return true
}

// cacheUnit is one eviction candidate: a per-app subdir of a cache root, with
// the content it holds. blobs lists the sha256 digests it references (shared
// across units by hardlink); ownSize is bytes of its non-shared files (index.json
// etc.) that are freed the moment the dir is removed.
type cacheUnit struct {
	path    string
	blobs   []string
	ownSize int64
	mtime   time.Time
}

// buildCacheScan is the dedup-aware picture of the on-disk build caches. total
// is real usage — a blob hardlinked into N dirs counts once — and is the single
// number both the size cap enforces and `wendy cache list` shows beside it, so
// the two can never disagree. units are the eviction candidates (per-app dirs
// that are neither the shared store nor kept); blobSize/refCount let eviction
// tell when a dir held a blob's LAST reference, i.e. when removing it actually
// frees the inode.
type buildCacheScan struct {
	units    []cacheUnit
	total    int64
	blobSize map[string]int64 // digest -> size (once)
	refCount map[string]int   // digest -> live references (units + shared store)
}

// scanBuildCache walks roots and builds the dedup-aware buildCacheScan. keep
// holds absolute dirs excluded from the eviction candidates (still counted).
func scanBuildCache(keep map[string]bool, roots ...string) buildCacheScan {
	s := buildCacheScan{blobSize: map[string]int64{}, refCount: map[string]int{}}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue // missing root: nothing there
		}
		for _, e := range entries {
			p := filepath.Join(root, e.Name())
			// Reserved top-level names are the shared store: never a candidate, but
			// its blob references must still count so a layer it holds never looks
			// free when the last app dir referencing it is evicted.
			shared := !e.IsDir() || e.Name() == "blobs" || e.Name() == "ingest"
			u := scanCacheUnit(p, s.blobSize, s.refCount)
			s.total += u.ownSize
			if shared || keep[p] {
				continue
			}
			s.units = append(s.units, u)
		}
	}
	// Each unique digest contributes its size to the real total exactly once.
	for d, sz := range s.blobSize {
		if s.refCount[d] > 0 {
			s.total += sz
		}
	}
	return s
}

// enforceBuildCacheSizeCap deletes whole per-app cache subdirs, least-recently
// modified first, until real on-disk usage is at or under maxBytes. Accounting
// is dedup-aware (see scanBuildCache): a layer hardlinked into N app dirs counts
// once, and evicting a dir only reclaims a layer whose LAST reference it held —
// layers still linked by a surviving dir or by the root-level shared store
// (blobs/, never evicted) are not counted as freed. keep holds absolute dirs the
// current build needs. Two guards protect other wendy processes: a dir whose
// layout lock another process holds (lockOCILayoutDir — held for the whole of
// build → read → push, including a long chunk push that writes no blobs) is
// never touched, and dirs modified within activeWindow are skipped as a likely
// in-progress build. A non-positive maxBytes disables the cap. Best-effort: a
// failed removal just leaves that dir for the next run. Returns bytes reclaimed.
func enforceBuildCacheSizeCap(maxBytes int64, keep map[string]bool, activeWindow time.Duration, roots ...string) int64 {
	if maxBytes <= 0 {
		return 0
	}
	s := scanBuildCache(keep, roots...)
	total := s.total
	if total <= maxBytes {
		return 0
	}

	cutoff := time.Now().Add(-activeWindow)
	sort.Slice(s.units, func(i, j int) bool { return s.units[i].mtime.Before(s.units[j].mtime) })
	var reclaimed int64
	for _, u := range s.units {
		if total <= maxBytes {
			break
		}
		if u.mtime.After(cutoff) {
			continue // likely an in-progress build; leave it
		}
		release, ok := tryLockCacheUnit(u.path)
		if !ok {
			continue // another wendy process is building into / pushing from it
		}
		err := os.RemoveAll(u.path)
		release()
		if err != nil {
			continue
		}
		freed := u.ownSize
		for _, d := range u.blobs {
			s.refCount[d]--
			if s.refCount[d] == 0 { // last reference gone: the inode is actually freed
				freed += s.blobSize[d]
			}
		}
		total -= freed
		reclaimed += freed
	}
	return reclaimed
}

// tryLockCacheUnit takes, without blocking, the per-directory lock a wendy
// process holds for the whole of build → read → push → GC on a layout dir
// (lockOCILayoutDir; the lock file sits beside the dir as dir+".lock"). ok is
// false when another process holds it: that dir is in use, however stale its
// blobs look, and must not be evicted. A dir with no lock file (buildx/ units
// are never locked this way) has no owner to consult and is returned as
// lockable with a no-op release; it stays governed by the activeWindow
// heuristic alone. Callers hold the lock across the removal so a build that
// starts meanwhile waits for the dir to be gone instead of racing RemoveAll.
func tryLockCacheUnit(dir string) (release func(), ok bool) {
	f, err := os.OpenFile(dir+".lock", os.O_RDWR, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return func() {}, true
		}
		return nil, false // unreadable lock: can't tell, leave the dir alone
	}
	locked, err := flock.TryLock(f)
	if err != nil || !locked {
		_ = f.Close()
		return nil, false
	}
	return func() {
		_ = flock.Unlock(f)
		_ = f.Close()
	}, true
}

// scanCacheUnit walks one cache dir, recording each blob digest's size and a
// reference into the shared maps, and summing non-blob ("own") bytes. The unit's
// mtime is the newest blob write — the LRU signal is when a build last touched
// it, not when a directory entry changed.
func scanCacheUnit(dir string, blobSize map[string]int64, refCount map[string]int) cacheUnit {
	u := cacheUnit{path: dir}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // best-effort
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if isBlobPath(p) {
			digest := d.Name()
			blobSize[digest] = info.Size()
			refCount[digest]++
			u.blobs = append(u.blobs, digest)
			if info.ModTime().After(u.mtime) {
				u.mtime = info.ModTime()
			}
		} else {
			u.ownSize += info.Size()
		}
		return nil
	})
	return u
}

// dirSizeAndMtime returns a tree's byte size (per-path — hardlinked blobs count
// once per link, so rows can sum past the dedup-aware on-disk total from
// scanBuildCache) and the newest blob mtime, used by `wendy cache list` to show
// a per-app cache's size and last-built time.
func dirSizeAndMtime(dir string) (int64, time.Time) {
	var size int64
	var newest time.Time
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // best-effort
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		size += info.Size()
		// LRU/last-built signal is a blob write, not directory-entry churn.
		if isBlobPath(p) && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
		return nil
	})
	return size, newest
}

// runBuildCacheMaintenance dedups then enforces the size cap over the on-disk
// build caches under userCacheDir. keep protects the active build's dirs. Pure
// filesystem work (no daemon), best-effort. Returns bytes reclaimed by each pass.
func runBuildCacheMaintenance(userCacheDir string, maxBytes int64, keep map[string]bool) (deduped, pruned int64) {
	roots := buildCacheRoots(userCacheDir)
	deduped = dedupBuildCache(roots...)
	pruned = enforceBuildCacheSizeCap(maxBytes, keep, 10*time.Minute, roots...)
	return deduped, pruned
}

// maintainBuildCaches runs the on-disk maintenance and also bounds the separate
// builder-side (buildkitd) cache. keep protects the active build's on-disk dirs.
func maintainBuildCaches(ctx context.Context, userCacheDir string, maxBytes int64, keep map[string]bool) (deduped, pruned int64) {
	deduped, pruned = runBuildCacheMaintenance(userCacheDir, maxBytes, keep)
	pruneBuildkitDaemonCache(ctx, maxBytes)
	return deduped, pruned
}

// pruneBuildkitDaemonCache caps the OCI builder's daemon-side cache — a store
// distinct from the on-disk local cache export, which our file-level dedup/prune
// never touches (it lives inside the buildkitd container). `buildx prune
// --max-used-space` GCs it down to maxBytes and, unlike a create-time flag,
// applies to the already-running builder. Best-effort with its own deadline so a
// stuck daemon never hangs the CLI; older buildx without the flag just errors and
// is ignored.
func pruneBuildkitDaemonCache(ctx context.Context, maxBytes int64) {
	if maxBytes <= 0 {
		return
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
	defer cancel()
	_ = exec.CommandContext(pctx, "docker", "buildx", "prune",
		"--builder", ociBuilderName(),
		"--max-used-space", strconv.FormatInt(maxBytes, 10),
		"--force",
	).Run()
}
