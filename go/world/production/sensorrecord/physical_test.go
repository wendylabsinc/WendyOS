package sensorrecord

import (
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

func TestPhysicalCurrentUsesConservativeAuthenticatedBound(t *testing.T) {
	capture, _ := testCapture(t, "gas")
	window := trustedtime.Interval{Earliest: testWall.Add(4 * time.Second), Latest: testWall.Add(MaxPhysicalAge), BootID: "physical-boot", Monotonic: time.Second, Domain: "physical", Authenticated: true, Source: "test-authenticated-anchor"}
	if err := PhysicalCurrent(capture, window); err != nil {
		t.Fatal("exact conservative bound refused", err)
	}
	for name, change := range map[string]func(*trustedtime.Interval){
		"uncertainty exceeds age": func(w *trustedtime.Interval) { w.Latest = w.Latest.Add(time.Nanosecond) },
		"future capture": func(w *trustedtime.Interval) {
			w.Earliest, w.Latest = testWall.Add(-time.Second), testWall.Add(-time.Nanosecond)
		},
		"unauthenticated":   func(w *trustedtime.Interval) { w.Authenticated = false },
		"simulation domain": func(w *trustedtime.Interval) { w.Domain = "simulation:run-a" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := window
			change(&bad)
			if err := PhysicalCurrent(capture, bad); err == nil {
				t.Fatal("accepted untrusted/expired physical capture interval")
			}
		})
	}
	capture.CapturedUnixNS = 0
	if err := PhysicalCurrent(capture, window); err == nil {
		t.Fatal("accepted absent original capture time")
	}
}
