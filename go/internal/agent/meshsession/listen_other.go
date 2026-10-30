//go:build !linux

package meshsession

import (
	"context"
	"crypto/tls"
	"net"
	"net/netip"

	quic "github.com/quic-go/quic-go"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func listenAppPackets(ctx context.Context, _ *localmesh.Credentials, addr string) (net.PacketConn, error) {
	return (&net.ListenConfig{}).ListenPacket(ctx, "udp", addr)
}

func dialAppQUIC(ctx context.Context, _ *localmesh.Credentials, endpoint netip.AddrPort, tlsConfig *tls.Config, config *quic.Config) (*quic.Conn, error) {
	return quic.DialAddr(ctx, endpoint.String(), tlsConfig, config)
}

func listenAppTCP(ctx context.Context, _ *localmesh.Credentials, addr string) (net.Listener, error) {
	return (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
}
