//go:build linux

// Package secretmount owns temporary, non-swappable secret files. No plaintext
// is written until the private tmpfs has been mounted successfully.
package secretmount

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const Root = "/run/wendy/secrets"

func Supported() bool { return true }

// Prepare returns a unique mount for one task attempt. Reusing a container name
// never reuses a mount, so delayed cleanup cannot remove a replacement's files.
func Prepare(values map[string][]byte, uid, gid uint32) (string, error) {
	return prepareAt(Root, values, uid, gid, unix.Mount, unix.Unmount)
}

type mountFunc func(string, string, string, uintptr, string) error
type unmountFunc func(string, int) error

func prepareAt(root string, values map[string][]byte, uid, gid uint32, mount mountFunc, unmount unmountFunc) (result string, err error) {
	if len(values) == 0 {
		return "", nil
	}
	if len(values) > 64 {
		return "", errors.New("too many app secrets")
	}
	total := 0
	for name, value := range values {
		if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00") || len(value) > 16<<10 {
			return "", errors.New("invalid app secret file")
		}
		total += len(value)
	}
	if total > 64<<10 {
		return "", errors.New("app secrets exceed size limit")
	}
	if err = os.MkdirAll(root, 0700); err != nil {
		return "", err
	}
	// The direct parent must also be protected against path replacement.
	var parent unix.Stat_t
	if err = unix.Lstat(filepath.Dir(root), &parent); err != nil {
		return "", err
	}
	if parent.Mode&unix.S_IFMT != unix.S_IFDIR || parent.Mode&0022 != 0 || parent.Uid != uint32(os.Geteuid()) {
		return "", errors.New("unsafe app secret parent directory")
	}
	// Root is agent-owned. Never follow a link or reuse a writable directory.
	info, err := os.Lstat(root)
	if err != nil {
		return "", err
	}
	var stat unix.Stat_t
	if err = unix.Lstat(root, &stat); err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || stat.Uid != uint32(os.Geteuid()) {
		return "", errors.New("unsafe app secret directory")
	}
	dir, err := os.MkdirTemp(root, "task-")
	if err != nil {
		return "", err
	}
	mounted := false
	defer func() {
		if err != nil {
			if mounted {
				err = errors.Join(err, unmount(dir, 0))
			}
			_ = os.Remove(dir)
		}
	}()
	flags := uintptr(unix.MS_NODEV | unix.MS_NOSUID | unix.MS_NOEXEC)
	// Refuse unsupported noswap instead of allowing plaintext onto host swap.
	if err = mount("tmpfs", dir, "tmpfs", flags, "mode=0700,size=1m,nr_inodes=128,noswap"); err != nil {
		return "", fmt.Errorf("mounting non-swappable secret tmpfs: %w", err)
	}
	mounted = true
	for name, value := range values {
		var f *os.File
		f, err = os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0400)
		if err != nil {
			return "", err
		}
		if _, err = f.Write(value); err == nil {
			err = f.Chown(int(uid), int(gid))
		}
		err = errors.Join(err, f.Close())
		if err != nil {
			return "", err
		}
	}
	if err = os.Chown(dir, int(uid), int(gid)); err != nil {
		return "", err
	}
	if err = os.Chmod(dir, 0500); err != nil {
		return "", err
	}
	if err = mount("", dir, "", flags|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
		return "", err
	}
	return dir, nil
}

func Cleanup(dir string) error {
	if dir == "" {
		return nil
	}
	if filepath.Dir(dir) != Root || !strings.HasPrefix(filepath.Base(dir), "task-") {
		return errors.New("invalid app secret mount path")
	}
	if err := unix.Unmount(dir, 0); err != nil && !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.EINVAL) {
		return err
	}
	if err := os.Remove(dir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
