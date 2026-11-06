package carriers

import (
	"context"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/ble/scan"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func bluetoothSource(self meshid.Device, preflight func(context.Context) error) Source {
	return func(ctx context.Context, emit func([]Candidate)) error {
		// Name-only fallback packets may omit the tenant UUID. The bounded backend
		// scans all reports, then the protocol/network filter runs on live bytes.
		updates, err := scan.DiscoverBluetoothContinuous(ctx, scan.Options{MaxDevices: 256, Preflight: preflight})
		if err != nil {
			return err
		}
		for {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case devices, ok := <-updates:
				if !ok {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					return errors.New("Bluetooth scan ended")
				}
				emit(bleCandidates(devices, self, time.Now()))
			}
		}
	}
}
