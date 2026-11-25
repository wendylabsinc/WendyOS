package gateway

import (
	"context"
	"os"
	"os/signal"
	"syscall"
)

// Keep the existing subprocess runtime tests on the same signal lifecycle.
func run(path string) error {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return Run(ctx, path)
}
