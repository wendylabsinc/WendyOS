// Package bluetooth provides Bluetooth peripheral management using BlueZ D-Bus on Linux.
// On non-Linux platforms, a stub implementation is returned.
package bluetooth

import (
	"context"

	"go.uber.org/zap"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// Manager defines the interface for Bluetooth operations.
type Manager interface {
	Scan(ctx context.Context) (<-chan []*agentpb.DiscoveredBluetoothPeripheral, error)
	Connect(ctx context.Context, address string, pair, trust bool) error
	Disconnect(ctx context.Context, address string) error
	Forget(ctx context.Context, address string) error
}

// NewManager creates a platform-appropriate Bluetooth manager.
// On Linux, it attempts to connect to BlueZ via D-Bus.
// On other platforms, it returns a stub that reports bluetooth as unsupported.
func NewManager(logger *zap.Logger) Manager {
	return newPlatformManager(logger)
}
