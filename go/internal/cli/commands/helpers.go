package commands

import (
	"context"
	"fmt"

	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/shared/config"
)

const defaultAgentPort = 50051

// resolveDeviceAddress returns the gRPC address for the target device.
// It checks the --device flag first, then the default device from config.
func resolveDeviceAddress() (string, error) {
	hostname := deviceFlag
	if hostname == "" {
		cfg, err := config.Load()
		if err != nil {
			return "", fmt.Errorf("loading config: %w", err)
		}
		hostname = cfg.DefaultDevice
	}
	if hostname == "" {
		return "", fmt.Errorf("no device specified; use --device flag or set a default with 'wendy device set-default'")
	}
	return fmt.Sprintf("%s:%d", hostname, defaultAgentPort), nil
}

// connectToAgent establishes a gRPC connection to the target device.
func connectToAgent(ctx context.Context) (*grpcclient.AgentConnection, error) {
	addr, err := resolveDeviceAddress()
	if err != nil {
		return nil, err
	}
	return grpcclient.Connect(addr)
}
