//go:build linux

package services

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

// containerStorageOnRootSlot reports whether /var/lib/containerd currently
// resolves to the same filesystem as / (root). Any stat or EvalSymlinks
// failure reports false: an unknown state is not treated as degraded,
// since a false positive here would wrongly refuse image ingestion on a
// host that is actually fine.
func containerStorageOnRootSlot() bool {
	var root unix.Stat_t
	if err := unix.Stat("/", &root); err != nil {
		return false
	}

	resolved, err := filepath.EvalSymlinks("/var/lib/containerd")
	if err != nil {
		return false
	}
	var containerd unix.Stat_t
	if err := unix.Stat(resolved, &containerd); err != nil {
		return false
	}

	return root.Dev == containerd.Dev
}
