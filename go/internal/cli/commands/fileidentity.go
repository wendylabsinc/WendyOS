package commands

import "time"

// fileIdentity is what the build-context digest cache (contextdigest.go)
// compares to decide that a file's content is unchanged since it was last
// hashed. Size and mtime catch ordinary edits. The inode and device catch a
// file replaced by a rename, even when the tool that wrote it preserved size
// and mtime. ctime catches an in-place rewrite whose mtime was reset (cp -p,
// rsync -t, touch -d): the kernel sets ctime on every change, and user space
// cannot set it back. That holds only where stat is authoritative and ctime
// is real, so the cache first checks the context's file system against an
// allowlist (fscache_darwin.go, fscache_linux.go).
//
// It exists only on darwin and linux (fileidentity_darwin.go,
// fileidentity_linux.go). Elsewhere, including Windows, whose os.FileInfo
// carries no inode or ctime, fileIdentityOf reports false and every context
// file is read on every run, exactly as before the cache existed.
type fileIdentity struct {
	Size    int64  `json:"size"`
	MtimeNs int64  `json:"mtime"`
	CtimeNs int64  `json:"ctime"`
	Dev     uint64 `json:"dev"`
	Ino     uint64 `json:"ino"`
}

// digestRacyWindow is how old a file's mtime and ctime must both be before
// its digest may be cached. A file written again in the same timestamp tick
// as the read we cached would keep its size and times, so the cache would
// never see the change (git's "racy clean" problem). Two seconds covers every
// timestamp granularity we can meet: FAT's 2 s, HFS+'s 1 s, and Linux's
// coarse per-tick clock.
const digestRacyWindow = 2 * time.Second

// settled reports whether id's content can be cached as of now, the moment
// hashing started. A later write sets ctime (and, unless reset, mtime) to at
// least now, so it cannot reproduce a timestamp that is digestRacyWindow
// older. A zero mtime or ctime means the filesystem does not track it (some
// FUSE mounts), and a future one means clock skew; neither is cached.
func (id fileIdentity) settled(now time.Time) bool {
	if id.MtimeNs <= 0 || id.CtimeNs <= 0 {
		return false
	}
	cutoff := now.Add(-digestRacyWindow).UnixNano()
	return id.MtimeNs <= cutoff && id.CtimeNs <= cutoff
}
