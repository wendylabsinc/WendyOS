package fusion

import (
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func optionalFixture(t *testing.T) *fixture {
	f := spatialFixture(t)
	delete(f.cfg.RequiredSensors, "lidar-atlas")
	f.cfg.OptionalSensors = map[string]string{"lidar-atlas": "lidar", "lidar-iris": "lidar"}
	return f
}
func TestOptionalLidarCatalogNeverFallsBackToLegacy(t *testing.T) {
	f := optionalFixture(t)
	f.records = f.records[:4]
	r := f.run(t)
	if r.State != InsufficientEvidence || r.Hazard != nil || r.Spatial == nil || r.Spatial.Associated {
		t.Fatal("no received scan silently became legacy hazard")
	}
}
func TestAnyCurrentAlignedOptionalLidarSupportsHazard(t *testing.T) {
	f := optionalFixture(t)
	if r := f.run(t); r.State != HazardDetected || len(r.Hazard.Inputs) != 5 {
		t.Fatalf("one available scan: %+v", r)
	}
	for _, change := range []func(*world.Component){func(c *world.Component) { c.TTL = time.Millisecond }, func(c *world.Component) { c.Timing.SampleTime = c.Timing.SampleTime.Add(-time.Second) }} {
		f := optionalFixture(t)
		change(&f.records[4])
		r := f.run(t)
		if r.State != InsufficientEvidence || r.Hazard != nil || r.Spatial == nil {
			t.Fatal("unusable scan must be missing support, not legacy output")
		}
	}
}
