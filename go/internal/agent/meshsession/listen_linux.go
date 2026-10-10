//go:build linux

package meshsession

import (
	"context"
	"net"
	"syscall"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"golang.org/x/sys/unix"
)

func listenAppPackets(ctx context.Context, credentials *localmesh.Credentials, addr string) (net.PacketConn, error) {
	address, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	if address.IP.IsUnspecified() || len(address.IP) == 0 {
		vip, _, err := localmesh.Addresses(credentials.Org, credentials.Asset)
		if err != nil {
			return nil, err
		}
		address.IP = net.IP(vip.AsSlice())
	}
	// A wildcard quic-go listener caches the first packet's ingress ifindex in
	// IP_PKTINFO for all replies. Babel can change TUNs without changing either
	// UDP endpoint; replies then use the old link and fail its forwarding gate.
	// Bind the stable host VIP instead, retaining native UDP batching/GSO/ECN
	// while allowing each send to follow the current kernel route.
	lc := net.ListenConfig{}
	network := "udp6"
	if address.IP.To4() != nil {
		network = "udp4"
		// The session server starts before the mesh dummy exists and survives its
		// recreation. FREEBIND permits that bind without pinning any interface or
		// changing host-wide nonlocal-bind policy. Local delivery still needs the VIP.
		lc.Control = func(_, _ string, raw syscall.RawConn) error {
			var sockErr error
			err := raw.Control(func(fd uintptr) { sockErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_FREEBIND, 1) })
			if err != nil {
				return err
			}
			return sockErr
		}
	}
	return lc.ListenPacket(ctx, network, address.String())
}
