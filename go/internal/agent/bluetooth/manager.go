// Package bluetooth provides Bluetooth peripheral management using BlueZ D-Bus on Linux.
// On non-Linux platforms, a stub implementation is returned.
package bluetooth

import (
	"context"
	"crypto/tls"

	"go.uber.org/zap"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// Manager defines the interface for Bluetooth operations.
type Manager interface {
	Scan(ctx context.Context) (<-chan []*agentpb.DiscoveredBluetoothPeripheral, error)
	// Connect connects to the peripheral and reports whether it is paired
	// once the connection is established — a successful connect does not
	// imply pairing (some BLE devices reject pairing yet accept connections).
	Connect(ctx context.Context, address string, pair, trust bool) (paired bool, err error)
	Disconnect(ctx context.Context, address string) error
	Forget(ctx context.Context, address string) error
	// ReconnectTrusted restores connections to trusted audio peripherals that
	// were paired before the agent restarted. It blocks until it has filled
	// what it can, so callers run it in the background.
	ReconnectTrusted(ctx context.Context)
}

// ManagerOption configures NewManager.
type ManagerOption func(*managerOptions)

type managerOptions struct {
	links LinkReporter
}

// WithLinkReporter makes Scan report each connected peripheral's BLE link
// supervision timeout.
func WithLinkReporter(r LinkReporter) ManagerOption {
	return func(o *managerOptions) { o.links = r }
}

func NewManager(logger *zap.Logger, opts ...ManagerOption) Manager {
	var o managerOptions
	for _, opt := range opts {
		opt(&o)
	}
	return newPlatformManager(logger, o)
}

// StartBLEPeripheral starts BLE advertising and the mTLS-protected L2CAP command server.
// tlsConfig must be non-nil (provisioned device cert); BLE is not started without it.
// Both are best-effort: failures are logged but do not affect LAN/USB serving.
func StartBLEPeripheral(ctx context.Context, logger *zap.Logger, d *Dispatcher, tlsConfig *tls.Config) {
	if err := startAdvertising(ctx, logger); err != nil {
		logger.Warn("BLE advertising unavailable", zap.Error(err))
	}
	if err := startL2CAPServer(ctx, logger, d, tlsConfig); err != nil {
		logger.Warn("BLE L2CAP server unavailable", zap.Error(err))
	}
}
