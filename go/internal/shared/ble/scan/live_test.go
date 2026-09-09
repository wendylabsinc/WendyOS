package scan

import (
	"context"
	"os"
	"reflect"
	"strings"
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
//
// Entries are trimmed here so "180F, 180A" reads as two UUIDs rather than one
// with a leading space. CanonicalUUID trims again downstream, so this is not
// what makes the filter work — it keeps the logged filter honest and stops the
// values this test hands to Options.Services depending on that later trim.
func splitCommaList(s string) []string {
	var out []string
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == ',' {
			// Trim before the empty check, so a whitespace-only entry is
			// dropped instead of becoming "".
			if part := strings.TrimSpace(s[start:i]); part != "" {
				out = append(out, part)
			}
			start = i + 1
		}
	}
	return out
}

// TestSplitCommaList runs everywhere, unlike TestLiveScan above: it is the only
// thing that checks the env var reader without a radio present.
func TestSplitCommaList(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"no spaces", "180F,180A", []string{"180F", "180A"}},
		{"space after comma", "180F, 180A", []string{"180F", "180A"}},
		{"spaces everywhere", "  180F , 180A  ", []string{"180F", "180A"}},
		{"whitespace-only entry", "180F, ,180A", []string{"180F", "180A"}},
		{"single entry", "180F", []string{"180F"}},
		{"trailing comma", "180F,", []string{"180F"}},
		{"full 128-bit form", "0000180F-0000-1000-8000-00805F9B34FB, 180A",
			[]string{"0000180F-0000-1000-8000-00805F9B34FB", "180A"}},
		{"empty", "", nil},
		{"only a comma", ",", nil},
		{"only whitespace", "   ", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := splitCommaList(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("splitCommaList(%q) = %#v, want %#v", tt.in, got, tt.want)
			}
		})
	}
}
