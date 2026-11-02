//go:build linux

package carriers

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// Reuses ordinary BlueZ discovery privileges; no raw HCI socket or WOS provider.
func nativeBLESource(self meshid.Device) Source { return bluetoothSource(self, nil) }

func nativeBLEPreflight(context.Context) error { return nil }
