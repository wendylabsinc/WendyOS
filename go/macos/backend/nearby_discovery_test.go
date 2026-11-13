package backend

import (
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/ble/scan"
)

func TestNearbySetupSightingRequiresFreshStrongAdvertisement(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name string
		rssi int
		seen time.Time
		want bool
	}{
		{"nearby", -45, now, true},
		{"threshold", -80, now.Add(-19 * time.Second), true},
		{"too far", -81, now, false},
		{"unknown signal", 0, now, false},
		{"unavailable signal", 127, now, false},
		{"expired", -45, now.Add(-20 * time.Second), false},
		{"cached without sighting", -45, time.Time{}, false},
		{"future sighting", -45, now.Add(time.Second), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := nearbySetupSighting(scan.BLEDeviceInfo{Address: "device", RSSI: tc.rssi, LastSeen: tc.seen}, now); got != tc.want {
				t.Fatalf("visible=%v, want %v", got, tc.want)
			}
		})
	}
}
