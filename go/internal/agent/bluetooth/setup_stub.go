//go:build !linux

package bluetooth

import (
	"context"
	"go.uber.org/zap"
)

func StartBLESetup(_ context.Context, _ *zap.Logger, _ *SetupDispatcher) {}
