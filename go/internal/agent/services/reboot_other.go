//go:build !linux

package services

import (
	"fmt"
	"time"

	"go.uber.org/zap"
)

// newRebooter returns a rebooter whose restart actions all report that rebooting
// is unsupported. WendyOS A/B OTA is Linux-only, so there is no slot for a
// non-Linux host (the macOS agent) to reboot into.
func newRebooter(logger *zap.Logger) rebooter {
	if logger == nil {
		logger = zap.NewNop()
	}
	unsupported := func() error { return fmt.Errorf("reboot not supported on this platform") }
	return rebooter{
		logger:    logger,
		sync:      func() {},
		immediate: unsupported,
		clean:     unsupported,
		grace:     rebootGrace,
		sleep:     time.Sleep,
	}
}

// newArgumentRebooter is newRebooter: there is no argument to pass to a
// restart that cannot happen.
func newArgumentRebooter(logger *zap.Logger, _ string) rebooter {
	return newRebooter(logger)
}

func rebootSystem() error {
	return newRebooter(nil).reboot()
}
