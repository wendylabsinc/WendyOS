//go:build linux

package localmesh

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// This file opts a privileged agent into changing host routing and forwarding.
// Open relative to a protected directory descriptor, refuse symlinks/special
// files, and inspect the actual open file so a path swap cannot bypass checks.
func openTCPConfig(path string) (*os.File, error) {
	dir, err := unix.Open(filepath.Dir(path), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dir)
	var parent unix.Stat_t
	if err := unix.Fstat(dir, &parent); err != nil {
		return nil, err
	}
	if int(parent.Uid) != os.Geteuid() || parent.Mode&0022 != 0 {
		return nil, errors.New("wendy net config directory must be owned by the agent and not writable by group or others")
	}
	fd, err := unix.Openat(dir, filepath.Base(path), unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		f.Close()
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || int(stat.Uid) != os.Geteuid() || stat.Mode&0022 != 0 {
		f.Close()
		return nil, errors.New("wendy net config must be a regular file owned by the agent and not writable by group or others")
	}
	return f, nil
}
