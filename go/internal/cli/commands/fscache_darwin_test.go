package commands

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestLocalFSAllowlistDarwin(t *testing.T) {
	for _, tc := range []struct {
		fstype string
		want   bool
	}{
		{"apfs", true},
		{"hfs", true},
		{"msdos", false}, // FAT: ctime == mtime, and touch -r resets both
		{"exfat", false},
		{"nfs", false}, // stat comes from the attribute cache
		{"smbfs", false},
		{"afpfs", false},
		{"webdav", false},
		{"macfuse", false}, // sshfs and other FUSE mounts
		{"osxfuse", false},
		{"devfs", false},
		{"apfsx", false},
		{"", false},
	} {
		var st unix.Statfs_t
		copy(st.Fstypename[:], tc.fstype)
		if got := digestCacheTrustsFS(&st); got != tc.want {
			t.Errorf("digestCacheTrustsFS(%q) = %v, want %v", tc.fstype, got, tc.want)
		}
	}
}

// TestLocalFSOnThisMac checks the live statfs: the temp dir is on APFS, /dev
// is devfs, and a statfs error disables the cache.
func TestLocalFSOnThisMac(t *testing.T) {
	if !digestCacheFSEligible(t.TempDir()) {
		t.Fatal("the temp dir (APFS) is not eligible for the digest cache")
	}
	if digestCacheFSEligible("/dev") {
		t.Fatal("devfs is eligible for the digest cache")
	}
	if digestCacheFSEligible(filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("a path statfs cannot see is eligible for the digest cache")
	}
}
