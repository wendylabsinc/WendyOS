package meshsession

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/netip"

	"github.com/quic-go/quic-go"
)

// A connected userspace UDP socket already pins its remote endpoint. Expose
// only that endpoint to QUIC; connection migration must not silently redirect
// packets through a socket that still sends to the original peer.
type connectedQUICPackets struct {
	net.Conn
	remote net.Addr
}

func (p *connectedQUICPackets) ReadFrom(b []byte) (int, net.Addr, error) {
	n, err := p.Conn.Read(b)
	return n, p.remote, err
}

func (p *connectedQUICPackets) WriteTo(b []byte, address net.Addr) (int, error) {
	if address == nil || address.Network() != p.remote.Network() || address.String() != p.remote.String() {
		return 0, errors.New("QUIC packet destination differs from connected peer")
	}
	return p.Conn.Write(b)
}

func (m *DirectManager) dialPromotionQUICSocket(ctx context.Context, address netip.AddrPort, config *tls.Config, options *quic.Config) (*quic.Conn, func(), error) {
	if m.cfg.PromotionDialContext == nil {
		conn, err := dialAppQUIC(ctx, m.cfg.Credentials, address, config, options)
		return conn, func() {}, err
	}
	return dialConnectedQUIC(ctx, m.cfg.PromotionDialContext, address, config, options)
}

func dialConnectedQUIC(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error), address netip.AddrPort, config *tls.Config, quicConfig *quic.Config) (*quic.Conn, func(), error) {
	raw, err := dial(ctx, "udp", address.String())
	if err != nil {
		return nil, nil, err
	}
	packets := &connectedQUICPackets{Conn: raw, remote: net.UDPAddrFromAddrPort(address)}
	transport := &quic.Transport{Conn: packets}
	cleanup := func() {
		transport.Close()
		raw.Close()
	}
	conn, err := transport.Dial(ctx, packets.remote, config, quicConfig)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	// Setup cancellation does not own the successfully established connection.
	// The promotion worker closes these sockets and joins QUIC on every exit.
	return conn, cleanup, nil
}
