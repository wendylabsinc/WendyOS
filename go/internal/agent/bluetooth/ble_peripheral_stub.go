//go:build !linux

package bluetooth

import (
	"context"
	"crypto/tls"

	"go.uber.org/zap"
)

func startAdvertising(_ context.Context, _ *zap.Logger) error {
	return nil
}

func startL2CAPServer(_ context.Context, _ *zap.Logger, _ *Dispatcher, _ *tls.Config) error {
	return nil
}
