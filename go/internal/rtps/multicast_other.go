//go:build !linux

package rtps

import "net"

// IP_MULTICAST_ALL is Linux-specific; other platforms retain their normal
// per-socket multicast membership behavior.
func restrictMulticastToJoinedInterfaces(_ *net.UDPConn) error { return nil }
