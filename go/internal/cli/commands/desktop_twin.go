package commands

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"net"
)

// DesktopTwinConnection uses authenticated container exec to reach the twin's
// loopback listener without publishing a host port or requiring a shell image.
func DesktopTwinConnection(ctx context.Context, conn *grpcclient.AgentConnection) (net.Conn, error) {
	return openContainerConnection(ctx, conn.ContainerService, "dev.wendy.digital-twin", []string{"/usr/local/bin/wendy-twin", "--proxy"}, "Digital Twin connection")
}
