package hostpath

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func root(t *testing.T) string {
	t.Helper()
	// macOS /var is a symlink. Production rejects it rather than silently
	// resolving a commissioned path; tests explicitly choose the real path.
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}
func TestCreateAndOpenWithoutChangingExistingPermissions(t *testing.T) {
	base := root(t)
	path := filepath.Join(base, "intermediate", "sockets")
	if err := EnsureDirectory(path, 0750, false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing path opened: %v", err)
	}
	if err := EnsureDirectory(path, 0750, true); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []struct {
		path string
		max  os.FileMode
	}{{filepath.Dir(path), 0700}, {path, 0750}} {
		info, err := os.Lstat(entry.path)
		if err != nil || info.Mode().Perm()&^entry.max != 0 {
			t.Fatalf("directory permissions: %v %v", info, err)
		}
	}
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDirectory(path, 0750, true); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0700 {
		t.Fatal("existing permissions silently changed")
	}
}
func TestUnsafeAncestorsAndFinalDirectoriesAreRefused(t *testing.T) {
	for _, mode := range []string{"symlink ancestor", "symlink final", "writable ancestor", "broad final", "plain file", "dirty path"} {
		t.Run(mode, func(t *testing.T) {
			base := root(t)
			parent := filepath.Join(base, "parent")
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(parent, "state")
			switch mode {
			case "symlink ancestor":
				actual := filepath.Join(base, "actual")
				if err := os.Mkdir(actual, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(parent); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(actual, parent); err != nil {
					t.Fatal(err)
				}
			case "symlink final":
				if err := os.Symlink(parent, path); err != nil {
					t.Fatal(err)
				}
			case "writable ancestor":
				if err := os.Chmod(parent, 0777); err != nil {
					t.Fatal(err)
				}
			case "broad final":
				if err := os.Mkdir(path, 0755); err != nil {
					t.Fatal(err)
				}
			case "plain file":
				if err := os.WriteFile(path, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
			case "dirty path":
				path = parent + "/../state"
			}
			if err := EnsureDirectory(path, 0700, true); err == nil {
				t.Fatal("unsafe path accepted")
			}
			if mode == "writable ancestor" {
				info, _ := os.Stat(parent)
				if info.Mode().Perm() != 0777 {
					t.Fatal("unsafe permissions repaired")
				}
			}
		})
	}
}
func TestOwnerAndStickyRules(t *testing.T) {
	const uid = 1234
	for _, tc := range []struct {
		name        string
		owner, mode uint32
		final, want bool
	}{
		{"owned parent", uid, unix.S_IFDIR | 0755, false, true},
		{"root parent", 0, unix.S_IFDIR | 0755, false, true},
		{"other owner", 5678, unix.S_IFDIR | 0700, false, false},
		{"root sticky", 0, unix.S_IFDIR | unix.S_ISVTX | 0777, false, true},
		{"user sticky", uid, unix.S_IFDIR | unix.S_ISVTX | 0777, false, false},
		{"root writable", 0, unix.S_IFDIR | 0777, false, false},
		{"wrong final owner", 0, unix.S_IFDIR | 0700, true, false},
		{"owned final", uid, unix.S_IFDIR | 0700, true, true},
		{"broad final", uid, unix.S_IFDIR | 0755, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := safeDirectory(tc.owner, tc.mode, uid, 0750, tc.final); got != tc.want {
				t.Fatalf("safe=%v want %v", got, tc.want)
			}
		})
	}
}
func TestActualWrongOwnerWhenRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing a temporary directory's owner requires root")
	}
	for _, final := range []bool{false, true} {
		base := root(t)
		parent := filepath.Join(base, "parent")
		if err := os.Mkdir(parent, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(parent, "state")
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		changed := parent
		if final {
			changed = path
		}
		if err := os.Chown(changed, 65534, 65534); err != nil {
			t.Fatal(err)
		}
		if err := EnsureDirectory(path, 0700, false); err == nil {
			t.Fatal("foreign-owner path admitted")
		}
	}
}
