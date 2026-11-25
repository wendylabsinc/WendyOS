//go:build darwin

package carriers

import (
	"github.com/wendylabsinc/wendy/go/internal/shared/ble/permission"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Adapted from Joannis Orlandos's CoreBluetooth full-node discovery in PR2466,
// 199b6ae42cf8. The callback owns actual TLS and Node admission; this source
// transports only fresh advertisements and never derives a full UUID.
func nativeBLESource(self meshid.Device) Source { return bluetoothSource(self, permission.Preflight) }
