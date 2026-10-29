//go:build linux

package containerd

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// cniNetnsBindDir is private runtime state; tests redirect it to a temporary directory.
var cniNetnsBindDir = "/run/wendy/netns"

// bindNetnsForCNI bind-mounts the network namespace anchored by netnsRef to a
// stable, spec-compliant path under /run/wendy/netns/<containerID> and returns
// that path along with a cleanup function that unmounts and removes the bind
// point. Passing a bind-mount path as CNI_NETNS (rather than /proc/self/fd/<n>)
// is the CNI-spec-compliant approach: the spec guarantees the value is a
// filesystem path, not a fd reference (SOC2-CC6, NIST-SI-3, ISO27001-A.8).
//
// On error, the function falls back to the fd-path approach (/proc/self/fd/<n>)
// with the caller responsible for keeping netnsRef open until CNI completes.
func bindNetnsForCNI(containerID string, netnsRef *os.File) (netnsPath string, cleanup func(), persistent bool) {
	// 0o700: owner-only, consistent with /run/wendy/cni and /run/wendy/shm.
	// Network namespace bind-mount points must not be reachable by group-0
	// daemons — opening a netns fd exposes network attack surface
	// (SOC2-CC6, NIST-SI-10, ISO27001-A.8).
	if err := os.MkdirAll(cniNetnsBindDir, 0o700); err != nil {
		return fmt.Sprintf("/proc/self/fd/%d", netnsRef.Fd()), func() { netnsRef.Close() }, false
	}
	// Explicit chmod handles pre-existing directories with wider permissions.
	_ = os.Chmod(cniNetnsBindDir, 0o700)

	// safeJoin validates that containerID contains no path separators and does
	// not escape the bind directory — stronger than filepath.Base alone.
	bindPath, safeErr := safeJoin(cniNetnsBindDir, containerID)
	if safeErr != nil {
		return fmt.Sprintf("/proc/self/fd/%d", netnsRef.Fd()), func() { netnsRef.Close() }, false
	}

	// Create the bind point — must be a regular file (not a dir) for netns bind-mounts.
	f, err := os.OpenFile(bindPath, os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil && !os.IsExist(err) {
		return fmt.Sprintf("/proc/self/fd/%d", netnsRef.Fd()), func() { netnsRef.Close() }, false
	}
	if f != nil {
		f.Close()
	}
	// A previous agent may have exited without running Client.Close. Never
	// stack a new bind mount over that unowned namespace: detach it first, then
	// mount the fd we just anchored. In-process live sandboxes do not reach this
	// helper because StartContainer reuses them before attempting a new bind.
	_ = unix.Unmount(bindPath, unix.MNT_DETACH)

	// Bind-mount the anchored fd path to the stable bind point. Using the fd
	// path as source ensures we mount the inode we already verified, not a
	// potentially-swapped path (SOC2-CC6, NIST-SI-3).
	srcPath := fmt.Sprintf("/proc/self/fd/%d", netnsRef.Fd())
	if err := unix.Mount(srcPath, bindPath, "", unix.MS_BIND, ""); err != nil {
		os.Remove(bindPath)
		return fmt.Sprintf("/proc/self/fd/%d", netnsRef.Fd()), func() { netnsRef.Close() }, false
	}

	// The bind-mount anchors the namespace independently of the fd.
	netnsRef.Close()

	return bindPath, func() {
		unix.Unmount(bindPath, unix.MNT_DETACH) //nolint:errcheck
		os.Remove(bindPath)
	}, true
}

func cleanupNetworkSandboxPath(path string) {
	if filepath.Dir(path) != cniNetnsBindDir {
		return
	}
	unix.Unmount(path, unix.MNT_DETACH) //nolint:errcheck
	os.Remove(path)
}
