//go:build !linux

package bluetooth

import (
	"context"

	"go.uber.org/zap"
)

func startAdvertising(_ context.Context, _ *zap.Logger) error {
	return nil
}

func startL2CAPServer(_ context.Context, _ *zap.Logger, _ *Dispatcher) error {
	return nil
}
