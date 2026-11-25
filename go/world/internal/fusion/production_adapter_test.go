package fusion

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// The generic hook tests inject decoded captures. Certificate verification is
// tested separately by production/fusioncapture using actual device proofs.
func captureFixture(t *testing.T) (*fixture, map[string]CapturedSample) {
	t.Helper()
	f := newFixture(t)
	decoded := map[string]CapturedSample{}
	f.cfg.CaptureType = "wendy.sensor.capture.v1"
	f.cfg.Sources = map[string]world.Source{}
	for i := range f.records {
		c := &f.records[i]
		s, err := sensing.FromComponent(*c)
		if err != nil {
			t.Fatal(err)
		}
		c.Key.Type, c.Key.Entity = f.cfg.CaptureType, "sensor/"+s.SensorID
		c.Key.Source.Stream, c.Key.Source.Service = s.SensorID, "reader-"+s.SensorID
		c.Version = world.Version{Epoch: 20, Sequence: 31}
		sum := sha256.Sum256([]byte("explicit-test-proof-" + s.SensorID))
		decoded[s.SensorID] = CapturedSample{Sample: s, ProofSHA256: hex.EncodeToString(sum[:]), SceneBootID: "boot-a", SimulationAnchor: time.Unix(1700000000, 0)}
		f.cfg.Sources[s.SensorID] = c.Key.Source
	}
	f.cfg.DecodeCapture = func(c world.Component) (CapturedSample, error) { return decoded[c.Key.Source.Stream], nil }
	return f, decoded
}

func TestCaptureFusionKeepsNativeAndAgentIdentityAndProofAncestry(t *testing.T) {
	f, decoded := captureFixture(t)
	result := f.run(t)
	if result.State != HazardDetected {
		t.Fatal(result)
	}
	for _, input := range result.Hazard.Inputs {
		want := decoded[input.SensorID]
		if input.Sample.Sequence != 1 || input.Version != (world.Version{Epoch: 20, Sequence: 31}) || input.Source != f.cfg.Sources[input.SensorID] || input.Capture == nil || input.Capture.ProofSHA256 != want.ProofSHA256 || input.Capture.SceneBootID != want.SceneBootID || !input.Capture.SimulationAnchor.Equal(want.SimulationAnchor) {
			t.Fatalf("rewritten capture identity: %+v", input)
		}
	}
	component, err := result.Component(world.Source{Device: "fusion", App: "fusion", Service: "fusion", Stream: "hazards"}, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range component.Provenance.Inputs {
		found := false
		for _, sample := range decoded {
			found = found || input.ID == "sha256:"+sample.ProofSHA256
		}
		if !found || input.Origin != world.Simulated {
			t.Fatal("derived ancestry did not retain exact proof", input)
		}
	}
	before := result.InputDigest
	changed := decoded["gas-1"]
	sum := sha256.Sum256([]byte("other-exact-proof"))
	changed.ProofSHA256 = hex.EncodeToString(sum[:])
	decoded["gas-1"] = changed
	if after := f.run(t); after.State != HazardDetected || before == after.InputDigest {
		t.Fatal("input digest did not include proof identity")
	}
}

func TestCaptureFusionRejectsSourceBootAnchorAndAdapterMismatch(t *testing.T) {
	cases := map[string]func(*fixture, map[string]CapturedSample){
		"different source": func(f *fixture, _ map[string]CapturedSample) { f.records[0].Key.Source.Service = "other-reader" },
		"new stream":       func(f *fixture, _ map[string]CapturedSample) { f.records[0].Key.Source.Stream = "camera-other" },
		"different boot": func(_ *fixture, decoded map[string]CapturedSample) {
			d := decoded["camera-a"]
			d.SceneBootID = "boot-b"
			decoded["camera-a"] = d
		},
		"different anchor": func(f *fixture, decoded map[string]CapturedSample) {
			d := decoded["camera-a"]
			d.SimulationAnchor = d.SimulationAnchor.Add(time.Millisecond)
			decoded["camera-a"] = d
			f.records[0].Timing.SampleTime = f.records[0].Timing.SampleTime.Add(time.Millisecond)
		},
		"missing proof": func(_ *fixture, decoded map[string]CapturedSample) {
			d := decoded["camera-a"]
			d.ProofSHA256 = ""
			decoded["camera-a"] = d
		},
		"wrong native timing": func(_ *fixture, decoded map[string]CapturedSample) {
			d := decoded["camera-a"]
			d.Sample.SampleNS++
			decoded["camera-a"] = d
		},
		"wrong native site": func(_ *fixture, decoded map[string]CapturedSample) {
			d := decoded["camera-a"]
			d.Sample.SiteID = "other"
			decoded["camera-a"] = d
		},
		"tombstone": func(f *fixture, _ map[string]CapturedSample) { f.records[0].Tombstone = true },
		"forged measured ancestry": func(f *fixture, _ map[string]CapturedSample) {
			f.records[0].Provenance = world.Provenance{Origin: world.Measured, AppVersion: "v1"}
		},
		"new stale version": func(f *fixture, _ map[string]CapturedSample) {
			c := f.records[0]
			c.Version.Sequence++
			c.TTL = 50 * time.Millisecond
			f.records = append(f.records, c)
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f, decoded := captureFixture(t)
			change(f, decoded)
			if result := f.run(t); result.State != InsufficientEvidence {
				t.Fatal("accepted mismatched capture:", result)
			}
		})
	}
	f, _ := captureFixture(t)
	f.cfg.Sources = nil
	if _, err := Evaluate(context.Background(), f.cfg, f.records, f.load); err == nil {
		t.Fatal("capture mode accepted implicit source authority")
	}
}

func TestCaptureFusionDuplicateProofAndTimelineDigest(t *testing.T) {
	f, decoded := captureFixture(t)
	old := f.run(t).InputDigest
	for id, d := range decoded {
		d.SceneBootID = "another-commissioned-boot"
		decoded[id] = d
	}
	if changed := f.run(t); changed.InputDigest == old || changed.State != HazardDetected {
		t.Fatal("scene boot absent from input digest")
	}
	old = f.run(t).InputDigest
	for i := range f.records {
		f.records[i].Timing.SampleTime = f.records[i].Timing.SampleTime.Add(time.Millisecond)
	}
	for id, d := range decoded {
		d.SimulationAnchor = d.SimulationAnchor.Add(time.Millisecond)
		decoded[id] = d
	}
	if changed := f.run(t); changed.InputDigest == old || changed.State != HazardDetected {
		t.Fatal("simulation anchor absent from input digest")
	}
	f, decoded = captureFixture(t)
	f.records = append(f.records, f.records[0])
	if result := f.run(t); result.State != HazardDetected {
		t.Fatal("identical proof duplicate refused", result)
	}
	calls := 0
	f.cfg.DecodeCapture = func(c world.Component) (CapturedSample, error) {
		d := decoded[c.Key.Source.Stream]
		if c.Key.Source.Stream == "camera-a" {
			calls++
			if calls > 1 {
				sum := sha256.Sum256([]byte(fmt.Sprint(calls)))
				d.ProofSHA256 = hex.EncodeToString(sum[:])
			}
		}
		return d, nil
	}
	if result := f.run(t); result.State != InsufficientEvidence {
		t.Fatal("accepted conflicting original proof for same agent version", result)
	}
}

func TestCaptureOriginCopiesOriginalEvidence(t *testing.T) {
	ref := &sensing.EvidenceRef{SHA256: "source", ContentType: "image/png", Size: 1}
	d := CapturedSample{OriginalEvidence: ref}
	o := d.origin()
	if !reflect.DeepEqual(o.OriginalEvidence, ref) || o.OriginalEvidence == ref {
		t.Fatal("capture source evidence is not owned")
	}
}
