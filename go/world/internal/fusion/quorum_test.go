package fusion

import (
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"reflect"
	"testing"
	"time"
)

func quorumFixture(t *testing.T) *fixture {
	f := spatialFixture(t)
	// A separately named third source observes the same rasterized ray as B.
	// A/B and A/C remain viable; B/C has no independent triangulation baseline.
	third := f.records[1]
	var sample sensing.Sample
	if err := json.Unmarshal(third.Payload, &sample); err != nil {
		t.Fatal(err)
	}
	sample.SensorID = "camera-c"
	sample.ID = "run/camera-c/1"
	third.Key.Source.Service = "camera-c"
	third.Key.Entity = "run/camera-c"
	third.Payload, _ = json.Marshal(sample)
	f.records = append(f.records, third)
	delete(f.cfg.RequiredSensors, "camera-a")
	delete(f.cfg.RequiredSensors, "camera-b")
	f.cfg.CameraQuorum = []string{"camera-c", "camera-b", "camera-a"}
	return f
}
func TestQuorumFallbackAndExactWinningEvidence(t *testing.T) {
	f := quorumFixture(t)
	r := f.run(t)
	if r.State != HazardDetected || !reflect.DeepEqual(r.Quorum.Selected, []string{"camera-a", "camera-b"}) || len(r.Quorum.Pairs) != 3 {
		t.Fatalf("%+v", r)
	}
	for _, input := range r.Hazard.Inputs {
		if input.SensorID == "camera-c" {
			t.Fatal("unused camera entered winning provenance")
		}
	}
	f.records = append(f.records[:1], f.records[2:]...)
	fallback := f.run(t)
	if fallback.State != HazardDetected || !reflect.DeepEqual(fallback.Quorum.Selected, []string{"camera-a", "camera-c"}) {
		t.Fatalf("%+v", fallback)
	}
	if len(fallback.Hazard.Inputs) != 5 {
		t.Fatal("wrong selected evidence count")
	}
	component, err := fallback.Component(world.Source{Device: "fusion", App: "fusion", Service: "fusion", Stream: "hazards"}, 1, 1)
	if err != nil || len(component.Provenance.Inputs) != 5 {
		t.Fatalf("component lineage: %v", err)
	}
	f.records = f.records[1:]
	absent := f.run(t)
	if absent.State != InsufficientEvidence || absent.Hazard != nil || len(absent.Quorum.Selected) != 0 {
		t.Fatal("single viewpoint supplied a hazard")
	}
}
func TestQuorumRejectsBadAlternativesWithoutWeakeningGates(t *testing.T) {
	for _, name := range []string{"expired", "calibration", "future", "frame", "media", "conflict"} {
		t.Run(name, func(t *testing.T) {
			f := quorumFixture(t)
			switch name {
			case "expired":
				f.records[1].TTL = time.Millisecond
			case "calibration":
				modifySample(t, &f.records[1], func(s *sensing.Sample) { s.Camera.CalibrationValidForNS = 1 })
			case "future":
				f.records[1].Timing.SampleTime = f.now.Add(time.Second)
			case "frame":
				f.records[1].Frame.Revision = "wrong"
			case "media":
				modifySample(t, &f.records[1], func(s *sensing.Sample) { s.Evidence.SHA256 = string(make([]byte, 64)) })
			case "conflict":
				other := f.records[1]
				other.TTL = time.Millisecond
				f.records = append(f.records, other)
			}
			result := f.run(t)
			if result.State != HazardDetected || !reflect.DeepEqual(result.Quorum.Selected, []string{"camera-a", "camera-c"}) {
				t.Fatalf("%+v", result)
			}
			for _, input := range result.Hazard.Inputs {
				if input.SensorID == "camera-b" {
					t.Fatal("bad alternative entered inference")
				}
			}
		})
	}
}
func TestQuorumOrderAndMandatoryEvidence(t *testing.T) {
	f := quorumFixture(t)
	first := f.run(t)
	for i, j := 0, len(f.records)-1; i < j; i, j = i+1, j-1 {
		f.records[i], f.records[j] = f.records[j], f.records[i]
	}
	second := f.run(t)
	a, _ := json.Marshal(first)
	b, _ := json.Marshal(second)
	if string(a) != string(b) {
		t.Fatal("record order changed winner or diagnostics")
	}
	for _, sensor := range []string{"gas-1", "audio-1", "lidar-atlas"} {
		t.Run(sensor, func(t *testing.T) {
			f := quorumFixture(t)
			filtered := f.records[:0]
			for _, record := range f.records {
				if record.Key.Source.Service != sensor {
					filtered = append(filtered, record)
				}
			}
			f.records = filtered
			r := f.run(t)
			if r.State != InsufficientEvidence || r.Hazard != nil {
				t.Fatal("camera quorum waived required corroboration")
			}
		})
	}
}
func TestQuorumConfigurationAndExpiry(t *testing.T) {
	f := quorumFixture(t)
	f.cfg.CameraQuorum = []string{"camera-a", "camera-a"}
	if _, err := Evaluate(t.Context(), f.cfg, f.records, f.load); err == nil {
		t.Fatal("duplicate cameras accepted")
	}
	f = quorumFixture(t)
	r := f.run(t)
	f.now = r.Hazard.ValidUntil
	expired := f.run(t)
	if expired.Hazard != nil {
		t.Fatal("quorum renewed expired evidence")
	}
}
