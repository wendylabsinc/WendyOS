//go:build ios

package permission

import (
	"context"
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/shared/ble/scan"
)

const CheckArg = "__ble-check" // Kept for the shared command catalogue; never re-executed on iOS.

// iOS authorizes the signed application through CoreBluetooth. There is no
// executable helper or terminal TCC canary. Do not cache a denied/powered-off
// result: returning from Settings or turning Bluetooth on must allow a retry.
func Preflight(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	state := scan.RunBLECheck()
	if err := ctx.Err(); err != nil {
		return err
	}
	if state != 0 {
		return errors.New("Bluetooth is unavailable; enable Bluetooth and allow Wendy access in Settings")
	}
	return nil
}
