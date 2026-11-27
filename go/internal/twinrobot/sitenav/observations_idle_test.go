package sitenav

import (
	"testing"
	"time"
)

func TestIdleObservationToleranceDoesNotRelaxScanOrFutureClockChecks(t *testing.T) {
	now := time.Now()
	for _, scenario := range []string{"accepted idle pair", "old pose receipt", "old pose capture", "old scan", "future pose receipt", "future pose capture", "future scan", "missing pose", "missing geometry"} {
		t.Run(scenario, func(t *testing.T) {
			o := Observations{Pose: Pose{Received: now.Add(-850 * time.Millisecond), Captured: now.Add(-850 * time.Millisecond)}, CloudAt: now.Add(-700 * time.Millisecond), Surfaces: SurfaceCloud{Surfaces: make([]Surface, 24)}}
			switch scenario {
			case "old pose receipt":
				o.Pose.Received = now.Add(-IdlePoseMaxAge - time.Nanosecond)
			case "old pose capture":
				o.Pose.Captured = now.Add(-IdlePoseMaxAge - time.Nanosecond)
			case "old scan":
				o.CloudAt = now.Add(-MaxAge - time.Nanosecond)
			case "future pose receipt":
				o.Pose.Received = now.Add(51 * time.Millisecond)
			case "future pose capture":
				o.Pose.Captured = now.Add(51 * time.Millisecond)
			case "future scan":
				o.CloudAt = now.Add(51 * time.Millisecond)
			case "missing pose":
				o.Pose.Received = time.Time{}
			case "missing geometry":
				o.Surfaces = SurfaceCloud{}
			}
			if scenario == "accepted idle pair" {
				if err := o.IdleMeasurementsReady(now); err != nil {
					t.Fatal(err)
				}
				if err := o.IdleSurfaceReady(now); err != nil {
					t.Fatal(err)
				}
			} else if scenario == "missing geometry" {
				if o.IdleMeasurementsReady(now) != nil || o.IdleSurfaceReady(now) == nil {
					t.Fatal("geometry requirement changed")
				}
			} else if o.IdleMeasurementsReady(now) == nil || o.IdleSurfaceReady(now) == nil {
				t.Fatal("idle tolerance bypassed a timing or missing-data guard")
			}
			if o.MeasurementsReady(now) == nil || o.Ready(now) == nil || o.SurfaceReady(now) == nil {
				t.Fatal("motion readiness inherited the idle allowance")
			}
		})
	}
}
