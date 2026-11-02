//go:build !darwin && !linux

package carriers

import "github.com/wendylabsinc/wendy/go/internal/shared/meshid"

// Automatic native BLE metadata is currently implemented on Linux/macOS only.
func nativeBLESource(meshid.Device) Source { return nil }
