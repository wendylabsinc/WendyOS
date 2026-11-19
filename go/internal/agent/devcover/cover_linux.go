//go:build linux

package devcover

import (
	"errors"
	"fmt"
	"io/fs"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"
)

// inNamespaceHook runs inside the target namespace, after setns and before the
// mount is committed. Tests use it to stall the goroutine.
var inNamespaceHook = func() {}

// Open pins the mount namespace of pid.
func Open(pid uint32) (*Namespace, error) {
	return openPath("/proc/"+strconv.FormatUint(uint64(pid), 10)+"/ns/mnt", pid)
}

func openPath(nsPath string, pid uint32) (*Namespace, error) {
	fd, err := unix.Open(nsPath, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", nsPath, err)
	}
	return &Namespace{fd: fd, pid: pid}, nil
}

// Inode identifies the namespace; two Namespaces with the same inode are the
// same mount namespace.
func (n *Namespace) Inode() (uint64, error) {
	var st unix.Stat_t
	if err := unix.Fstat(n.fd, &st); err != nil {
		return 0, fmt.Errorf("fstat mount namespace: %w", err)
	}
	return st.Ino, nil
}

// Close releases the namespace.
func (n *Namespace) Close() error {
	return unix.Close(n.fd)
}

const (
	statePending int32 = iota
	stateMounting
	stateAbandoned
)

// Cover bind-mounts src (resolved in the agent's namespace) over dst inside
// the namespace n. The mount is made from a detached clone of src, moved in
// from a locked throwaway thread that has entered n, so the agent's own
// namespace is never changed. A cover either commits before timeout or is
// never applied.
func Cover(n *Namespace, src, dst string, timeout time.Duration) error {
	tfd, err := unix.OpenTree(unix.AT_FDCWD, src, unix.OPEN_TREE_CLONE|unix.OPEN_TREE_CLOEXEC)
	if err != nil {
		return fmt.Errorf("open_tree(%s): %w", src, err)
	}

	hook := inNamespaceHook // read here, not on the goroutine
	var state atomic.Int32
	done := make(chan error, 1)
	go func() {
		// Never unlocked: the thread, with its altered fs and namespace,
		// dies with this goroutine instead of returning to the pool.
		runtime.LockOSThread()
		defer unix.Close(tfd)

		if err := unix.Unshare(unix.CLONE_FS); err != nil {
			done <- fmt.Errorf("unshare(CLONE_FS): %w", err)
			return
		}
		if err := unix.Setns(n.fd, unix.CLONE_NEWNS); err != nil {
			done <- fmt.Errorf("setns(CLONE_NEWNS): %w", err)
			return
		}
		hook()
		// Commit point: after this the caller can no longer abandon.
		if !state.CompareAndSwap(statePending, stateMounting) {
			done <- ErrTimeout
			return
		}
		if err := unix.MoveMount(tfd, "", unix.AT_FDCWD, dst, unix.MOVE_MOUNT_F_EMPTY_PATH); err != nil {
			done <- fmt.Errorf("move_mount(-> %s): %w", dst, err)
			return
		}
		done <- nil
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		if state.CompareAndSwap(statePending, stateAbandoned) {
			return ErrTimeout
		}
		// The goroutine already committed to the mount, which is one
		// syscall; take its result.
		return <-done
	}
}

// Covered reports whether dst inside n is the device src: the two have the
// same device number (st_rdev). src must be a device node; plain files all
// share rdev 0. A dst that does not exist is not covered.
func Covered(n *Namespace, src, dst string) (bool, error) {
	var s, d unix.Stat_t
	if err := unix.Stat(src, &s); err != nil {
		return false, fmt.Errorf("stat %s: %w", src, err)
	}
	inside := "/proc/" + strconv.FormatUint(uint64(n.pid), 10) + "/root" + dst
	if err := unix.Stat(inside, &d); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, fmt.Errorf("stat %s: %w", inside, err)
	}
	return s.Rdev == d.Rdev, nil
}
