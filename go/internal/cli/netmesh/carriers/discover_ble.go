package carriers

import (
	"context"
	"errors"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/ble/scan"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

var startBLEDiscoveryScan = scan.DiscoverBluetoothContinuous

// DiscoverBLE lists fresh, same-network native radio hints for a finite window.
// It does not connect, authenticate a peer, or turn a short hint into a UUID.
func DiscoverBLE(ctx context.Context, self meshid.Device, duration time.Duration) ([]Candidate, error) {
	if !self.Tenant().IsValid() {
		return nil, errors.New("BLE mesh discovery requires the selected tenant")
	}
	if duration <= 0 || duration > 30*time.Second {
		return nil, errors.New("BLE discovery timeout must be greater than zero and at most 30s")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	window, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	updates, err := startBLEDiscoveryScan(window, scan.Options{MaxDevices: 256, Preflight: nativeBLEPreflight})
	if err != nil {
		return nil, err
	}
	defer func() {
		cancel()
		// The scan closes its channel after releasing the native scanner.
		for range updates {
		}
	}()
	return collectBLECandidates(ctx, window, self, updates)
}

func collectBLECandidates(parent, window context.Context, self meshid.Device, updates <-chan []scan.BLEDeviceInfo) ([]Candidate, error) {
	var latest []scan.BLEDeviceInfo
	for {
		select {
		case <-window.Done():
			return bleCandidates(latest, self, time.Now()), parent.Err()
		case devices, ok := <-updates:
			if !ok {
				if window.Err() != nil {
					return bleCandidates(latest, self, time.Now()), parent.Err()
				}
				return bleCandidates(latest, self, time.Now()), errors.New("Bluetooth scan ended before the discovery window completed")
			}
			latest = devices
		}
	}
}
