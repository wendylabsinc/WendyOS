package discovery

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveLiteDiscovery drives the real scanner and a real GATT read against
// real hardware: power on a Wendy Lite board and run
//
//	WENDY_BLE_LIVE_SCAN=1 go test ./internal/shared/discovery -run TestLiveLiteDiscovery -v
//
// It is skipped otherwise, because CI has no radio — the same gate as
// ble/scan's TestLiveScan. Windows has no GATT client, so the stream stays
// empty there by design; macOS and Linux can both pass this.
func TestLiveLiteDiscovery(t *testing.T) {
	if os.Getenv("WENDY_BLE_LIVE_SCAN") == "" {
		t.Skip("set WENDY_BLE_LIVE_SCAN=1 to run a real BLE scan")
	}

	// permission.Preflight re-execs os.Executable() as "__ble-check" — under
	// `go test` that's this test binary, which doesn't understand that
	// argument. Skip the canary here; a human running this against real
	// hardware has already dealt with Bluetooth permission.
	origPreflight := blePreflightFn
	t.Cleanup(func() { blePreflightFn = origPreflight })
	blePreflightFn = func(context.Context) error { return nil }

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	ch, err := BLELiteDeviceDiscoverContinuous(ctx)
	if err != nil {
		t.Fatalf("BLELiteDeviceDiscoverContinuous: %v", err)
	}

	for devices := range ch {
		for _, d := range devices {
			t.Logf("%s rssi=%d name=%q psm=%d id=%q device=%q display=%q mtls=%t",
				d.Address, d.RSSI, d.Name, d.Info.PSM, d.Info.DeviceID,
				d.Info.DeviceName, d.Info.DisplayName, d.Info.MTLSEnabled)
		}
		if len(devices) > 0 {
			if devices[0].Info.PSM == 0 {
				t.Errorf("device %s reported PSM 0; a probed device always has one", devices[0].Address)
			}
			return
		}
	}
	t.Fatal("no Wendy Lite device identified over BLE — is a board powered on and in range?")
}
