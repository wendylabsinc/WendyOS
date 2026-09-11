package scan

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveScan drives the real platform backend against real hardware. It is
// skipped unless WENDY_BLE_LIVE_SCAN is set, because CI has no radio — and on
// macOS no CI job compiles this package's cgo at all, so this is the only thing
// that exercises the CoreBluetooth bridge.
//
//	WENDY_BLE_LIVE_SCAN=1 go test ./internal/shared/ble/scan -run TestLiveScan -v
//
// Set WENDY_BLE_LIVE_SERVICES to a comma-separated UUID list to exercise
// filtering; leave it unset to report every device in range.
func TestLiveScan(t *testing.T) {
	if os.Getenv("WENDY_BLE_LIVE_SCAN") == "" {
		t.Skip("set WENDY_BLE_LIVE_SCAN=1 to run a real BLE scan")
	}

	var services []string
	if raw := os.Getenv("WENDY_BLE_LIVE_SERVICES"); raw != "" {
		services = splitCommaList(raw)
		t.Logf("filtering on %v", services)
	} else {
		t.Log("no filter: reporting every device in range")
	}

	duration := 15 * time.Second
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	ch, err := DiscoverBluetoothContinuous(ctx, Options{Services: services})
	if err != nil {
		t.Fatalf("DiscoverBluetoothContinuous: %v", err)
	}

	emits := 0
	var last []BLEDeviceInfo
	for devices := range ch {
		emits++
		last = devices
		t.Logf("emit %d: %d device(s)", emits, len(devices))
		for _, d := range devices {
			t.Logf("    %-38s rssi=%-5d name=%-24q services=%v",
				d.Address, d.RSSI, d.Name, d.ServiceUUIDs)
		}
	}

	// Not a failure: an empty room is a legitimate result. Say so plainly
	// rather than reporting a pass that proves nothing.
	if emits == 0 {
		t.Logf("no devices seen in %s — nothing was advertising, or the radio is off", duration)
		return
	}
	t.Logf("%d emit(s), %d device(s) at the end", emits, len(last))
}

// splitCommaList is a test-only reader for WENDY_BLE_LIVE_SERVICES. The
// production comma parser lives in the darwin file, which is not built
// everywhere.
func splitCommaList(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			if part := s[start:i]; part != "" {
				out = append(out, part)
			}
			start = i + 1
		}
	}
	return out
}
