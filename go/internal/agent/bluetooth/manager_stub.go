//go:build !linux

package bluetooth

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// StubManager is a no-op Bluetooth manager for platforms that do not support BlueZ.
type StubManager struct {
	logger *zap.Logger
}

func newPlatformManager(logger *zap.Logger) Manager {
	return &StubManager{logger: logger}
}

var errUnsupported = fmt.Errorf("bluetooth is not supported on this platform")

// Scan returns an error indicating Bluetooth is not supported.
func (m *StubManager) Scan(_ context.Context) (<-chan []*agentpb.DiscoveredBluetoothPeripheral, error) {
	return nil, errUnsupported
}

// Connect returns an error indicating Bluetooth is not supported.
func (m *StubManager) Connect(_ context.Context, _ string, _, _ bool) error {
	return errUnsupported
}

// Disconnect returns an error indicating Bluetooth is not supported.
func (m *StubManager) Disconnect(_ context.Context, _ string) error {
	return errUnsupported
}

// Forget returns an error indicating Bluetooth is not supported.
func (m *StubManager) Forget(_ context.Context, _ string) error {
	return errUnsupported
}
