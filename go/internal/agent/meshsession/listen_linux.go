//go:build linux

package meshsession

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"
	"syscall"

	quic "github.com/quic-go/quic-go"
	"golang.org/x/sys/unix"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func listenAppPackets(ctx context.Context, credentials *localmesh.Credentials, addr string) (net.PacketConn, error) {
	address, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	if address.IP.IsUnspecified() || len(address.IP) == 0 {
		address.IP = net.IP(credentials.Device.MeshAddress().AsSlice())
	}
	// A wildcard quic-go listener caches the first packet's ingress ifindex in
	// PKTINFO for all replies. Babel can change TUNs without changing either
	// UDP endpoint; replies then use the old link and fail its forwarding gate.
	// Bind the device's own mesh address instead, retaining native UDP
	// batching/GSO/ECN while allowing each send to follow the current kernel
	// route.
	lc := net.ListenConfig{}
	network, level, freebind := "udp6", unix.IPPROTO_IPV6, unix.IPV6_FREEBIND
	if address.IP.To4() != nil {
		network, level, freebind = "udp4", unix.IPPROTO_IP, unix.IP_FREEBIND
	}
	// The session server starts before the mesh host interface carries this
	// device's address and survives that interface's recreation. FREEBIND
	// permits the bind without pinning any interface or changing host-wide
	// nonlocal-bind policy; local delivery still needs the address to exist.
	lc.Control = func(_, _ string, raw syscall.RawConn) error {
		var sockErr error
		err := raw.Control(func(fd uintptr) { sockErr = unix.SetsockoptInt(int(fd), level, freebind, 1) })
		if err != nil {
			return err
		}
		return sockErr
	}
	packets, err := lc.ListenPacket(ctx, network, address.String())
	if err != nil {
		return nil, err
	}
	if meshid.MeshPrefix.Contains(address.AddrPort().Addr()) {
		return &routedAppPackets{UDPConn: packets.(*net.UDPConn)}, nil
	}
	return packets, nil
}

// routedAppPackets keeps a temporary loss of the signed kernel route from
// terminating QUIC's send loop. The kernel still rejects the packet; reporting
// that offer as lost lets QUIC retransmit after route repair, subject to its
// existing idle timeout and independent credential lifetime. Other socket
// errors remain fatal. Embedding UDPConn preserves native batching, GSO and ECN.
type routedAppPackets struct{ *net.UDPConn }

var _ quic.OOBCapablePacketConn = (*routedAppPackets)(nil)

func routingUnavailable(err error) bool {
	return errors.Is(err, unix.ENETUNREACH) || errors.Is(err, unix.EHOSTUNREACH)
}
func (c *routedAppPackets) WriteTo(packet []byte, peer net.Addr) (int, error) {
	n, err := c.UDPConn.WriteTo(packet, peer)
	if routingUnavailable(err) {
		return len(packet), nil
	}
	return n, err
}
func (c *routedAppPackets) WriteMsgUDP(packet, oob []byte, peer *net.UDPAddr) (int, int, error) {
	n, oobn, err := c.UDPConn.WriteMsgUDP(packet, oob, peer)
	if routingUnavailable(err) {
		return len(packet), len(oob), nil
	}
	return n, oobn, err
}

func dialAppQUIC(ctx context.Context, credentials *localmesh.Credentials, endpoint netip.AddrPort, tlsConfig *tls.Config, config *quic.Config) (*quic.Conn, error) {
	// Non-mesh endpoints are used by local tests and explicit callers. Only the
	// routed mesh socket needs FREEBIND and route-repair loss semantics.
	if !meshid.MeshPrefix.Contains(endpoint.Addr()) {
		return quic.DialAddr(ctx, endpoint.String(), tlsConfig, config)
	}
	packets, err := listenAppPackets(ctx, credentials, ":0")
	if err != nil {
		return nil, err
	}
	transport := &quic.Transport{Conn: packets}
	conn, err := transport.Dial(ctx, net.UDPAddrFromAddrPort(endpoint), tlsConfig, config)
	if err != nil {
		transport.Close()
		packets.Close()
		return nil, err
	}
	go func() {
		<-conn.Context().Done()
		transport.Close()
		packets.Close()
	}()
	return conn, nil
}

// Bind the stable mesh host address while allowing it to appear after startup.
// TCP remains free to use a different routed carrier for later packets.
func listenAppTCP(ctx context.Context, credentials *localmesh.Credentials, addr string) (net.Listener, error) {
	address, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return nil, err
	}
	if address.IP.IsUnspecified() || len(address.IP) == 0 {
		address.IP = net.IP(credentials.Device.MeshAddress().AsSlice())
	}
	lc := net.ListenConfig{}
	network := "tcp6"
	if address.IP.To4() != nil {
		network = "tcp4"
	}
	if meshid.MeshPrefix.Contains(address.AddrPort().Addr()) {
		lc.Control = func(_, _ string, raw syscall.RawConn) error {
			var sockErr error
			err := raw.Control(func(fd uintptr) { sockErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, unix.IPV6_FREEBIND, 1) })
			if err != nil {
				return err
			}
			return sockErr
		}
	}
	return lc.Listen(ctx, network, address.String())
}
