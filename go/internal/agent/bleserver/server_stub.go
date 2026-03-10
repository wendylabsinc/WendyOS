//go:build !linux

package bleserver

import (
	"context"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/internal/agent/services"
)

// Server is a no-op stub for non-Linux platforms.
type Server struct{}

// NewServer returns a no-op BLE server on non-Linux platforms.
func NewServer(
	_ *zap.Logger,
	_ services.NetworkManager,
	_ services.HardwareDiscoverer,
	_ services.BluetoothManager,
	_ services.ContainerdClient,
) *Server {
	return &Server{}
}

// Run is a no-op on non-Linux platforms. It blocks until ctx is cancelled.
func (s *Server) Run(ctx context.Context) {
	<-ctx.Done()
}
