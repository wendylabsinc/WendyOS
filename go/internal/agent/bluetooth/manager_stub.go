//go:build !linux

package bluetooth

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// StubManager is a no-op Bluetooth manager for platforms that do not support BlueZ.
type StubManager struct {
	logger *zap.Logger
}

func newPlatformManager(logger *zap.Logger, _ managerOptions) Manager {
	return &StubManager{logger: logger}
}

var errUnsupported = fmt.Errorf("bluetooth is not supported on this platform")

func (m *StubManager) Scan(_ context.Context) (<-chan []*agentpb.DiscoveredBluetoothPeripheral, error) {
	return nil, errUnsupported
}

func (m *StubManager) Connect(_ context.Context, _ string, _, _ bool) (bool, error) {
	return false, errUnsupported
}

func (m *StubManager) Disconnect(_ context.Context, _ string) error {
	return errUnsupported
}

func (m *StubManager) Forget(_ context.Context, _ string) error {
	return errUnsupported
}

func (m *StubManager) ReconnectTrusted(_ context.Context) {}
