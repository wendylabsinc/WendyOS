package linklocal

import (
	"net"

	"golang.org/x/sys/unix"
)

// bindToInterface scopes fd to ifi with IP_BOUND_IF.
func bindToInterface(fd uintptr, ifi net.Interface) error {
	return unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_BOUND_IF, ifi.Index)
}
