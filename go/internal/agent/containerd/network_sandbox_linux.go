//go:build linux

package containerd

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

// networkSandboxHealthy fails closed unless path is an owner-controlled nsfs
// bind mount containing an UP eth0 with the exact CNI-assigned address.
func networkSandboxHealthy(path, expectedIP string) bool {
	if filepath.Dir(path) != cniNetnsBindDir || expectedIP == "" {
		return false
	}
	return networkNamespaceHasIP(path, expectedIP)
}

// networkNamespaceHasIP checks a live namespace without trusting its path as
// an ownership proof. Callers must separately bind it to a verified task.
func networkNamespaceHasIP(path, expectedIP string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &stat); err != nil || stat.Type != unix.NSFS_MAGIC {
		return false
	}
	ns, err := netns.GetFromPath(path)
	if err != nil {
		return false
	}
	defer ns.Close()
	// LinkByName and AddrList need only NETLINK_ROUTE. The default handle
	// opens every supported family and fails on Jetson kernels that omit one.
	h, err := netlink.NewHandleAt(ns, unix.NETLINK_ROUTE)
	if err != nil {
		return false
	}
	defer h.Close()
	link, err := h.LinkByName("eth0")
	if err != nil || link.Attrs() == nil || link.Attrs().Flags&net.FlagUp == 0 {
		return false
	}
	addrs, err := h.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return false
	}
	want := net.ParseIP(expectedIP)
	for _, addr := range addrs {
		if want != nil && addr.IP.Equal(want) {
			return true
		}
	}
	return false
}

func taskUsesNetworkSandbox(path string, pid uint32) bool {
	sandbox, err := os.Stat(path)
	if err != nil {
		return false
	}
	task, err := os.Stat(fmt.Sprintf("/proc/%d/ns/net", pid))
	return err == nil && os.SameFile(sandbox, task)
}
