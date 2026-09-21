//go:build linux

package rtps

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

type networkNamespace struct {
	identity    namespaceIdentity
	host        bool
	file        *os.File
	pid         uint32
	start       string
	verifyOwner func() bool
}

func namespaceID(file *os.File) (namespaceIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return namespaceIdentity{}, err
	}
	return namespaceIdentity{device: uint64(stat.Dev), inode: stat.Ino}, nil
}

// start time distinguishes a recycled PID even if the container ID is unchanged.
func processStart(pid uint32) (string, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	end := strings.LastIndexByte(string(b), ')')
	if end < 0 {
		return "", fmt.Errorf("rtps: malformed process stat")
	}
	fields := strings.Fields(string(b[end+1:]))
	if len(fields) <= 19 {
		return "", fmt.Errorf("rtps: short process stat")
	}
	return fields[19], nil
}

func captureNetworkNamespace(pid uint32, verify func() bool) (*networkNamespace, error) {
	n := &networkNamespace{pid: pid, verifyOwner: verify}
	path := "/proc/self/ns/net"
	var err error
	if pid != 0 {
		n.start, err = processStart(pid)
		if err != nil {
			return nil, err
		}
		path = fmt.Sprintf("/proc/%d/ns/net", pid)
	}
	n.file, err = os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("rtps: opening network namespace: %w", err)
	}
	n.identity, err = namespaceID(n.file)
	if err == nil {
		err = n.verify()
	}
	if err != nil {
		n.close()
		return nil, err
	}
	host, err := os.Open("/proc/self/ns/net")
	if err != nil {
		n.close()
		return nil, err
	}
	hostID, err := namespaceID(host)
	_ = host.Close()
	if err != nil {
		n.close()
		return nil, err
	}
	n.host = n.identity == hostID
	return n, nil
}

func (n *networkNamespace) close() { _ = n.file.Close() }

func (n *networkNamespace) verify() error {
	if err := verifyNamespaceTarget(n.pid, n.verifyOwner); err != nil {
		return err
	}
	if n.pid == 0 {
		return nil
	}
	start, err := processStart(n.pid)
	if err != nil || start != n.start {
		return fmt.Errorf("rtps: network namespace process %d changed", n.pid)
	}
	current, err := os.Open(fmt.Sprintf("/proc/%d/ns/net", n.pid))
	if err != nil {
		return err
	}
	defer current.Close()
	id, err := namespaceID(current)
	if err != nil {
		return err
	}
	if id != n.identity {
		return fmt.Errorf("rtps: network namespace process %d changed", n.pid)
	}
	return nil
}

func (n *networkNamespace) run(fn func() error) (err error) {
	runtime.LockOSThread()
	restored := true
	defer func() {
		if restored {
			runtime.UnlockOSThread()
		}
	}()
	// Read the calling thread's namespace, not the thread-group leader's.
	host, err := os.Open(fmt.Sprintf("/proc/self/task/%d/ns/net", unix.Gettid()))
	if err != nil {
		return err
	}
	defer host.Close()
	if err := n.verify(); err != nil {
		return err
	}
	hostID, err := namespaceID(host)
	if err != nil {
		return err
	}
	if hostID == n.identity {
		return fn()
	}
	if err := unix.Setns(int(n.file.Fd()), unix.CLONE_NEWNET); err != nil {
		return fmt.Errorf("rtps: entering network namespace: %w", err)
	}
	restored = false
	defer func() {
		if restoreErr := unix.Setns(int(host.Fd()), unix.CLONE_NEWNET); restoreErr != nil {
			err = combineNamespaceRestoreError(err, restoreErr)
			return
		}
		restored = true
	}()
	return fn()
}

func withNetworkNamespace(pid uint32, verify func() bool, fn func() error) error {
	n, err := captureNetworkNamespace(pid, verify)
	if err != nil {
		return err
	}
	defer n.close()
	return n.run(fn)
}
