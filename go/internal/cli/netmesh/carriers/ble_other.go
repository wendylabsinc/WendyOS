//go:build !darwin && !linux

package carriers

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Automatic native BLE metadata is currently implemented on Linux/macOS only.
func nativeBLESource(meshid.Device) Source { return nil }

func nativeBLEPreflight(context.Context) error {
	return errors.New("native BLE mesh discovery is supported on macOS and Linux")
}
