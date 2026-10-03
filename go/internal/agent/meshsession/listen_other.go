//go:build !linux

package meshsession

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"net"
)

func listenAppPackets(ctx context.Context, _ *localmesh.Credentials, addr string) (net.PacketConn, error) {
	return (&net.ListenConfig{}).ListenPacket(ctx, "udp", addr)
}
