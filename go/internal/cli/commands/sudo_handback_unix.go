//go:build darwin || linux

package commands

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// HandBackSudoFiles gives the user who ran wendy through sudo back ownership of
// the root-owned entries in wendy's config and cache directories. A sudo run
// keeps that user's HOME (macOS sudo does by default, and the Thor re-exec asks
// for it), so what it creates there — config.json, the auth refresh lock, the
// flashpack cache, flash logs — would otherwise stay root-owned and fail the
// user's later runs with "permission denied". It does nothing unless this
// process is root because of sudo, and is best-effort.
func HandBackSudoFiles() {
	uid, gid, ok := sudoInvoker(os.Geteuid(), os.Getenv)
	if !ok {
		return
	}
	h := ownershipHandBack{uid: uid, gid: gid, owner: statOwner, lchown: (*os.Root).Lchown}
	for _, dir := range []func() (string, error){config.ConfigDir, config.CacheDir} {
		if d, err := dir(); err == nil {
			_ = h.handBack(d)
		}
	}
}

// sudoInvoker returns the user whose sudo made this process root, which sudo
// records in SUDO_UID and SUDO_GID. ok is false when this process isn't root,
// wasn't started by sudo, or was started by root itself.
func sudoInvoker(euid int, getenv func(string) string) (uid, gid int, ok bool) {
	if euid != 0 {
		return 0, 0, false
	}
	uid, uerr := strconv.Atoi(getenv("SUDO_UID"))
	gid, gerr := strconv.Atoi(getenv("SUDO_GID"))
	if uerr != nil || gerr != nil || uid <= 0 || gid < 0 {
		return 0, 0, false
	}
	return uid, gid, true
}

// ownershipHandBack chowns root-owned entries to the sudo invoker. owner and
// lchown are fields so tests can fake root-owned files.
type ownershipHandBack struct {
	uid, gid int
	// owner returns the uid that owns fi and its hard link count.
	owner  func(fi fs.FileInfo) (uid int, nlink uint64)
	lchown func(r *os.Root, name string, uid, gid int) error
}

// statOwner is ownershipHandBack.owner for real files. An owner it can't read
// is -1, which never matches root or the invoker.
func statOwner(fi fs.FileInfo) (uid int, nlink uint64) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return -1, 0
	}
	return int(st.Uid), uint64(st.Nlink)
}

// handBack returns the root-owned entries in dir, and root-owned directories
// above it (a ~/.cache the sudo run created), to the invoker. It works inside
// the nearest directory at or above dir that root doesn't own, and only when
// the invoker owns that directory, so a tree like /root is left alone. Working
// through os.Root, no symlink can take it outside that directory; it never
// follows one, and it skips files with more than one hard link, which could be
// links to system files.
func (h ownershipHandBack) handBack(dir string) error {
	dir, err := filepath.EvalSymlinks(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	anchor, err := h.invokerAnchor(dir)
	if err != nil || anchor == "" {
		return err
	}
	root, err := os.OpenRoot(anchor)
	if err != nil {
		return err
	}
	defer root.Close()
	// The directory could have been swapped since it was checked: confirm the
	// one actually opened is the invoker's.
	fi, err := root.Stat(".")
	if err != nil {
		return err
	}
	if uid, _ := h.owner(fi); uid != h.uid {
		return nil
	}

	rel, err := filepath.Rel(anchor, dir)
	if err != nil {
		return err
	}
	if rel != "." {
		parts := strings.Split(rel, string(filepath.Separator))
		for i := 1; i < len(parts); i++ {
			h.handBackEntry(root, filepath.Join(parts[:i]...))
		}
	}
	return fs.WalkDir(root.FS(), filepath.ToSlash(rel), func(name string, _ fs.DirEntry, err error) error {
		if err == nil {
			h.handBackEntry(root, filepath.FromSlash(name))
		}
		return nil // best-effort: an unreadable entry is skipped, not fatal
	})
}

// invokerAnchor returns the nearest directory at or above dir that root doesn't
// own, or "" when that directory belongs to someone other than the invoker or
// root owns every directory up to /.
func (h ownershipHandBack) invokerAnchor(dir string) (string, error) {
	for p := dir; ; p = filepath.Dir(p) {
		fi, err := os.Lstat(p)
		if err != nil {
			return "", err
		}
		if uid, _ := h.owner(fi); uid != 0 {
			if uid != h.uid {
				return "", nil
			}
			return p, nil
		}
		if filepath.Dir(p) == p {
			return "", nil
		}
	}
}

// handBackEntry chowns name, relative to root, to the invoker when root owns it
// and it isn't a file with other hard links. Directories always have several.
func (h ownershipHandBack) handBackEntry(root *os.Root, name string) {
	fi, err := root.Lstat(name)
	if err != nil {
		return
	}
	uid, nlink := h.owner(fi)
	if uid != 0 || (!fi.IsDir() && nlink > 1) {
		return
	}
	_ = h.lchown(root, name, h.uid, h.gid)
}
