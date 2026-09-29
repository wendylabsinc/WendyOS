package commands

import "golang.org/x/sys/unix"

// digestCacheTrustsFS reports whether st, a statfs result, names a local file
// system whose stat the build-context digest cache can trust: APFS or HFS+,
// where the kernel sets ctime on every change. Everything else reads every
// file. NFS, SMB, AFP and WebDAV serve stat from an attribute cache that can
// hide an edit made on another host; FAT and exFAT (msdos, exfat) have no
// real ctime, so touch -r after a same-size in-place write restores the whole
// identity; a FUSE mount (sshfs) reports whatever its daemon says.
func digestCacheTrustsFS(st *unix.Statfs_t) bool {
	switch unix.ByteSliceToString(st.Fstypename[:]) {
	case "apfs", "hfs":
		return true
	}
	return false
}

// digestCacheFSEligible reports whether the digest cache may be used for a
// build context rooted at root. A statfs error means no cache.
func digestCacheFSEligible(root string) bool {
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil {
		return false
	}
	return digestCacheTrustsFS(&st)
}
