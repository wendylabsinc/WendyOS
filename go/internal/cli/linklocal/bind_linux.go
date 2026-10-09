package linklocal

import (
	"net"

	"golang.org/x/sys/unix"
)

// bindToInterface pins fd to ifi with SO_BINDTODEVICE, which needs no
// privilege on Linux 5.7 and later.
func bindToInterface(fd uintptr, ifi net.Interface) error {
	return unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, ifi.Name)
}
