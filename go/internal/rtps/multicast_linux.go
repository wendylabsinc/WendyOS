//go:build linux

package rtps

import (
	"net"

	"golang.org/x/sys/unix"
)

// Linux enables IP_MULTICAST_ALL by default, delivering packets for multicast
// memberships on other sockets/interfaces too. A host loopback participant
// must not discover the physical interface's cameras under a second identity.
func restrictMulticastToJoinedInterfaces(conn *net.UDPConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var socketErr error
	if err := raw.Control(func(fd uintptr) {
		socketErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_MULTICAST_ALL, 0)
	}); err != nil {
		return err
	}
	return socketErr
}
