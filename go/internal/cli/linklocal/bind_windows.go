package linklocal

import (
	"math/bits"
	"net"

	"golang.org/x/sys/windows"
)

// ipUnicastIF is IP_UNICAST_IF from ws2ipdef.h; x/sys/windows does not export it.
const ipUnicastIF = 31

// bindToInterface sends fd's IPv4 traffic out ifi with IP_UNICAST_IF, which
// takes the interface index in network byte order.
func bindToInterface(fd uintptr, ifi net.Interface) error {
	return windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, ipUnicastIF, int(bits.ReverseBytes32(uint32(ifi.Index))))
}
