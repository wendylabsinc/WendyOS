package commands

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// setStatfsType stores a file system magic number the way the kernel reports
// it: Statfs_t.Type is int64 on 64-bit ABIs, int32 on most 32-bit ones (where
// btrfs's 0x9123683e reads negative), and uint32 on s390x.
func setStatfsType[T int32 | int64 | uint32](field *T, magic uint32) { *field = T(magic) }

func TestLocalFSAllowlistLinux(t *testing.T) {
	for _, tc := range []struct {
		name  string
		magic uint32
		want  bool
	}{
		{"ext4 (and ext2, ext3)", unix.EXT4_SUPER_MAGIC, true},
		{"xfs", unix.XFS_SUPER_MAGIC, true},
		{"btrfs", unix.BTRFS_SUPER_MAGIC, true},
		{"tmpfs", unix.TMPFS_MAGIC, true},
		{"overlayfs", unix.OVERLAYFS_SUPER_MAGIC, true},
		{"zfs", zfsSuperMagic, true},
		{"f2fs", unix.F2FS_SUPER_MAGIC, true},
		{"bcachefs", unix.BCACHEFS_SUPER_MAGIC, true},
		{"nfs", unix.NFS_SUPER_MAGIC, false}, // stat comes from the attribute cache
		{"fuse (sshfs)", unix.FUSE_SUPER_MAGIC, false},
		{"cifs", unix.CIFS_SUPER_MAGIC, false},
		{"smb2", unix.SMB2_SUPER_MAGIC, false},
		{"9p", unix.V9FS_MAGIC, false},
		{"vfat", unix.MSDOS_SUPER_MAGIC, false}, // ctime == mtime, and touch -r resets both
		{"exfat", unix.EXFAT_SUPER_MAGIC, false},
		{"unknown", 0, false},
	} {
		var st unix.Statfs_t
		setStatfsType(&st.Type, tc.magic)
		if got := digestCacheTrustsFS(&st); got != tc.want {
			t.Errorf("digestCacheTrustsFS(%s, %#x) = %v, want %v", tc.name, tc.magic, got, tc.want)
		}
	}
}

// TestLocalFSOnThisLinux checks the live statfs where the answer does not
// depend on the machine: procfs is not eligible, and a statfs error disables
// the cache.
func TestLocalFSOnThisLinux(t *testing.T) {
	if digestCacheFSEligible("/proc") {
		t.Fatal("procfs is eligible for the digest cache")
	}
	if digestCacheFSEligible(filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("a path statfs cannot see is eligible for the digest cache")
	}
}
