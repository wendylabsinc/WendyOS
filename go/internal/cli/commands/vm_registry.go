package commands

import (
	"context"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
)

var vmRegistryPort = func(ctx context.Context, name string) (int, error) {
	s, err := vm.NewStore()
	if err != nil {
		return 0, err
	}
	return s.RegistryPort(ctx, name)
}

// Only translate the selected VM's registry port. Cloud dialers already own
// their routing, and LAN devices retain their ordinary host:port destination.
func registryHostPortForAgent(ctx context.Context, conn *grpcclient.AgentConnection, port int) (int, error) {
	if conn.RegistryDialer != nil || port != vm.DeviceRegistryPort {
		return port, nil
	}
	name, err := userVMForConnection(conn)
	if err != nil {
		return 0, err
	}
	if name == "" {
		return port, nil
	}
	port, err = vmRegistryPort(ctx, name)
	if err != nil {
		return 0, fmt.Errorf("resolving VM %q registry: %w", name, err)
	}
	return port, nil
}
