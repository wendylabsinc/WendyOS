//go:build darwin || linux

package commands

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestSudoInvoker(t *testing.T) {
	env := func(uid, gid string) func(string) string {
		return func(k string) string {
			switch k {
			case "SUDO_UID":
				return uid
			case "SUDO_GID":
				return gid
			}
			return ""
		}
	}
	tests := []struct {
		name     string
		euid     int
		getenv   func(string) string
		wantUID  int
		wantGID  int
		wantOkay bool
	}{
		{"root via sudo", 0, env("1000", "1000"), 1000, 1000, true},
		{"macOS staff group", 0, env("501", "20"), 501, 20, true},
		{"not root", 1000, env("1000", "1000"), 0, 0, false},
		{"root without sudo", 0, env("", ""), 0, 0, false},
		{"sudo from root", 0, env("0", "0"), 0, 0, false},
		{"garbled uid", 0, env("abc", "1000"), 0, 0, false},
		{"missing gid", 0, env("1000", ""), 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			uid, gid, ok := sudoInvoker(tt.euid, tt.getenv)
			if ok != tt.wantOkay || uid != tt.wantUID || gid != tt.wantGID {
				t.Errorf("sudoInvoker = (%d, %d, %v), want (%d, %d, %v)", uid, gid, ok, tt.wantUID, tt.wantGID, tt.wantOkay)
			}
		})
	}
}

// fakeOwners makes the named entries look root-owned (or owned by someone
// else) to an ownershipHandBack, since a test can't create root-owned files.
// Every other entry belongs to invokerUID with a single link.
type fakeOwners struct {
	uids   map[string]int    // base name → owner uid
	nlinks map[string]uint64 // base name → hard link count
}

const invokerUID = 4242

func (f fakeOwners) owner(fi fs.FileInfo) (int, uint64) {
	uid, ok := f.uids[fi.Name()]
	if !ok {
		uid = invokerUID
	}
	nlink, ok := f.nlinks[fi.Name()]
	if !ok {
		nlink = 1
	}
	return uid, nlink
}

// newTestHandBack returns a hand-back that records what it would chown. Each
// chown really runs, to this test's own uid/gid (allowed without root), so a
// path that escaped the root or didn't exist would fail the test.
func newTestHandBack(t *testing.T, owners fakeOwners) (ownershipHandBack, *[]string) {
	t.Helper()
	var chowned []string
	h := ownershipHandBack{
		uid:   invokerUID,
		gid:   invokerUID,
		owner: owners.owner,
		lchown: func(r *os.Root, name string, uid, gid int) error {
			if uid != invokerUID || gid != invokerUID {
				t.Errorf("chown %s to %d:%d, want the invoker %d:%d", name, uid, gid, invokerUID, invokerUID)
			}
			chowned = append(chowned, filepath.ToSlash(name))
			if err := r.Lchown(name, os.Getuid(), os.Getgid()); err != nil {
				t.Errorf("chown %s: %v", name, err)
			}
			return nil
		},
	}
	return h, &chowned
}

func mkTree(t *testing.T, base string, dirs, files []string) {
	t.Helper()
	for _, d := range append([]string{"."}, dirs...) {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(base, f), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertChowned(t *testing.T, got []string, want ...string) {
	t.Helper()
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("chowned %q, want %q", got, want)
	}
}

func TestOwnershipHandBack_GivesBackRootOwnedEntries(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wendy")
	mkTree(t, dir, []string{"os-images/flashpack"}, []string{"config.json", "analytics_id", "os-images/flashpack/tool", "os-images/keep"})
	h, chowned := newTestHandBack(t, fakeOwners{uids: map[string]int{
		"analytics_id": 0, "os-images": 0, "flashpack": 0, "tool": 0,
	}})

	if err := h.handBack(dir); err != nil {
		t.Fatal(err)
	}
	// config.json and os-images/keep belong to the invoker already.
	assertChowned(t, *chowned, "analytics_id", "os-images", "os-images/flashpack", "os-images/flashpack/tool")
}

// On a fresh host the elevated run can be the first to create ~/.cache, so
// root-owned directories sit between the invoker's home and the wendy cache.
func TestOwnershipHandBack_GivesBackRootCreatedParents(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	dir := filepath.Join(home, ".cache", "wendy")
	mkTree(t, dir, []string{"logs"}, []string{"logs/thor-flash.log"})
	h, chowned := newTestHandBack(t, fakeOwners{uids: map[string]int{
		".cache": 0, "wendy": 0, "logs": 0, "thor-flash.log": 0,
	}})

	if err := h.handBack(dir); err != nil {
		t.Fatal(err)
	}
	assertChowned(t, *chowned, ".cache", ".cache/wendy", ".cache/wendy/logs", ".cache/wendy/logs/thor-flash.log")
}

func TestOwnershipHandBack_LeavesTreesTheInvokerDoesNotOwn(t *testing.T) {
	t.Run("another user's directory", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "wendy")
		mkTree(t, dir, nil, []string{"config.json"})
		h, chowned := newTestHandBack(t, fakeOwners{uids: map[string]int{"wendy": 7, "config.json": 0}})
		if err := h.handBack(dir); err != nil {
			t.Fatal(err)
		}
		assertChowned(t, *chowned)
	})
	t.Run("root-owned all the way up, as with HOME=/root", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "wendy")
		mkTree(t, dir, nil, []string{"config.json"})
		h, chowned := newTestHandBack(t, fakeOwners{})
		h.owner = func(fs.FileInfo) (int, uint64) { return 0, 1 }
		if err := h.handBack(dir); err != nil {
			t.Fatal(err)
		}
		assertChowned(t, *chowned)
	})
	t.Run("missing directory", func(t *testing.T) {
		h, chowned := newTestHandBack(t, fakeOwners{})
		if err := h.handBack(filepath.Join(t.TempDir(), "absent")); err != nil {
			t.Fatalf("a missing directory has nothing to hand back, got %v", err)
		}
		assertChowned(t, *chowned)
	})
}

// A root-owned file with a second hard link may be a link the invoker made to
// a system file; chowning it would hand them that file.
func TestOwnershipHandBack_SkipsHardLinkedFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "wendy")
	mkTree(t, dir, []string{"sub"}, []string{"linked", "single"})
	h, chowned := newTestHandBack(t, fakeOwners{
		uids:   map[string]int{"linked": 0, "single": 0, "sub": 0},
		nlinks: map[string]uint64{"linked": 2, "sub": 3},
	})

	if err := h.handBack(dir); err != nil {
		t.Fatal(err)
	}
	// Directories always have several links; only files are suspect.
	assertChowned(t, *chowned, "single", "sub")
}

func TestOwnershipHandBack_DoesNotFollowSymlinks(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	dir := filepath.Join(base, "wendy")
	mkTree(t, outside, nil, []string{"secret"})
	mkTree(t, dir, nil, nil)
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	h, chowned := newTestHandBack(t, fakeOwners{uids: map[string]int{"escape": 0, "outside": 0, "secret": 0}})

	if err := h.handBack(dir); err != nil {
		t.Fatal(err)
	}
	// The root-owned link itself goes back; what it points at is untouched.
	assertChowned(t, *chowned, "escape")
}
