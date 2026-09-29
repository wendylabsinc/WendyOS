package commands

import "golang.org/x/sys/unix"

// zfsSuperMagic is OpenZFS's statfs f_type (ZFS_SUPER_MAGIC in its
// include/sys/zfs_vfsops.h). ZFS is out of tree, so golang.org/x/sys/unix
// has no constant for it.
const zfsSuperMagic = 0x2fc12fc1

// digestCacheTrustsFS reports whether st, a statfs result, names a local file
// system whose stat the build-context digest cache can trust: one whose kernel
// driver sets ctime on every change. Everything else reads every file. NFS,
// CIFS/SMB and 9p serve stat from an attribute cache that can hide an edit
// made on another host; vfat and exFAT have no real ctime, so touch -r after
// a same-size in-place write restores the whole identity; a FUSE mount
// (sshfs) reports whatever its daemon says.
func digestCacheTrustsFS(st *unix.Statfs_t) bool {
	// Every file system magic fits in 32 bits. Statfs_t.Type is int32 on most
	// 32-bit ABIs, where btrfs's 0x9123683e reads negative, so compare the
	// low 32 bits.
	switch uint32(st.Type) {
	case unix.EXT4_SUPER_MAGIC, // ext2 and ext3 share it
		unix.XFS_SUPER_MAGIC,
		unix.BTRFS_SUPER_MAGIC,
		unix.TMPFS_MAGIC,
		unix.OVERLAYFS_SUPER_MAGIC,
		zfsSuperMagic,
		unix.F2FS_SUPER_MAGIC,
		unix.BCACHEFS_SUPER_MAGIC:
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
