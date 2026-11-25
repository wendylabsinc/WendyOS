// Package hostpath validates host-owned runtime directories without following
// symbolic links or repairing the ownership/permissions of existing paths.
package hostpath

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

var ErrUnsafe = errors.New("host directory ownership, permissions or traversal refused")

// EnsureDirectory walks from a pinned root descriptor. Ancestors must belong
// to root or this effective UID and cannot be group/other writable, except for
// root-owned sticky directories such as /tmp. The final directory must belong
// to this effective UID and have no permission bits beyond mode.
//
// Missing directories are created only when requested: intermediates use 0700
// and the final directory uses mode. Existing paths are never chmod/chowned.
// The owner and root remain trusted; this does not defend against their renames.
func EnsureDirectory(path string, mode os.FileMode, create bool) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" || mode != mode.Perm() || mode&0700 != 0700 || mode&0022 != 0 {
		return ErrUnsafe
	}
	parent, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(parent) }()
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	uid := uint32(os.Geteuid())
	for i, part := range parts {
		last := i == len(parts)-1
		next, err := unix.Openat(parent, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if errors.Is(err, unix.ENOENT) && create {
			permissions := uint32(0700)
			if last {
				permissions = uint32(mode.Perm())
			}
			if err = unix.Mkdirat(parent, part, permissions); err != nil && !errors.Is(err, unix.EEXIST) {
				return err
			}
			next, err = unix.Openat(parent, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if err != nil {
			return err
		}
		var st unix.Stat_t
		if unix.Fstat(next, &st) != nil || !safeDirectory(st.Uid, uint32(st.Mode), uid, uint32(mode.Perm()), last) {
			_ = unix.Close(next)
			return ErrUnsafe
		}
		_ = unix.Close(parent)
		parent = next
	}
	return nil
}

func safeDirectory(owner, permissions, uid, requested uint32, final bool) bool {
	if permissions&unix.S_IFMT != unix.S_IFDIR {
		return false
	}
	if final {
		return owner == uid && permissions&0777&^requested == 0 && permissions&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) == 0
	}
	trustedOwner := owner == 0 || owner == uid
	protected := permissions&0022 == 0 || owner == 0 && permissions&unix.S_ISVTX != 0
	return trustedOwner && protected
}
