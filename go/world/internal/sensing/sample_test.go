package sensing

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func sampleFixture() Sample {
	return Sample{SchemaVersion: Schema, ID: "run/gas-1/1", SiteID: "office", Frame: world.FrameRef{ID: "floor", Revision: "1"}, SensorID: "gas-1", Kind: "gas", Sequence: 1, SampleNS: 250000000, RunID: "run", ScenarioVersion: "v1", PackageSHA256: strings.Repeat("a", 64), SensorModel: "gas", PositionM: []float64{1, 2, 3}, UncertaintyM: .1, CalibrationRevision: "c1", Gas: &Gas{ValuePPM: 100, UncertaintyPPM: 2, Species: "synthetic"}}
}
func TestStrictSampleRejectsMissingDuplicateUnknownAndMalformedFields(t *testing.T) {
	base, _ := json.Marshal(sampleFixture())
	var fields map[string]json.RawMessage
	json.Unmarshal(base, &fields)
	for _, key := range []string{"sample_ns", "seed", "position_m", "uncertainty_m"} {
		t.Run(key, func(t *testing.T) {
			copy := map[string]json.RawMessage{}
			for k, v := range fields {
				copy[k] = v
			}
			delete(copy, key)
			b, _ := json.Marshal(copy)
			if _, e := Decode(b); e == nil {
				t.Fatal("accepted missing field")
			}
		})
	}
	for _, b := range [][]byte{[]byte(strings.Replace(string(base), `"sequence":1`, `"sequence":1,"sequence":1`, 1)), append(base, []byte(` {}`)...), []byte(strings.Replace(string(base), `"position_m":[1,2,3]`, `"position_m":[1,2]`, 1)),
		[]byte(strings.Replace(string(base), `"position_m":[1,2,3]`, `"position_m":[null,2,3]`, 1)), []byte(strings.Replace(string(base), `"value_ppm":100`, `"value_ppm":null`, 1)), []byte(strings.Replace(string(base), `"schema_version":`, `"extra":0,"schema_version":`, 1))} {
		if _, e := Decode(b); e == nil {
			t.Fatalf("accepted malformed %s", b)
		}
	}
}
func TestConverterBindsServiceAndPreservesRunWithoutAdvancingOnFailure(t *testing.T) {
	now := time.Unix(1700000000, 0)
	s := sampleFixture()
	source := world.Source{Device: "gateway", App: "sensors", Service: "gas-1", Stream: "samples"}
	converter, e := New(Config{Tenant: "tenant", Site: s.SiteID, RunID: s.RunID, Frame: s.Frame, Source: source, Epoch: 2, Anchor: now, TTL: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	b, _ := json.Marshal(s)
	c, e := converter.Convert(b)
	if e != nil {
		t.Fatal(e)
	}
	if c.Key.Source != source || c.Version.Epoch != 2 || !c.Timing.SampleTime.Equal(now.Add(250*time.Millisecond)) || !c.Provenance.Synthetic() {
		t.Fatal(c)
	}
	if _, e = FromComponent(c); e != nil {
		t.Fatal(e)
	}
	bad := c
	bad.Key.Source.Service = "camera-a"
	if _, e = FromComponent(bad); e == nil {
		t.Fatal("service spoof accepted")
	}
	if _, e = converter.Convert(b); e == nil {
		t.Fatal("duplicate sequence accepted")
	}
	s.Sequence = 2
	s.ID = "run/gas-1/2"
	s.SampleNS = 0
	b, _ = json.Marshal(s)
	if _, e = converter.Convert(b); e == nil {
		t.Fatal("clock regression accepted")
	}
	s.SampleNS = 300000000
	b, _ = json.Marshal(s)
	if _, e = converter.Convert(b); e != nil {
		t.Fatal(e)
	}
	s.SensorID = "camera-a"
	s.Sequence = 3
	s.ID = "run/camera-a/3"
	b, _ = json.Marshal(s)
	if _, e = converter.Convert(b); e == nil {
		t.Fatal("publisher impersonated other service")
	}
}
func TestCameraTransformAndEvidenceRequireCompleteCalibration(t *testing.T) {
	s := sampleFixture()
	s.SensorID = "camera-a"
	s.ID = "run/camera-a/1"
	s.Kind = "camera"
	s.Gas = nil
	s.Evidence = &EvidenceRef{SHA256: strings.Repeat("b", 64), ContentType: "image/png", Size: 100}
	s.Camera = &Camera{Width: 64, Height: 48, FX: 40, FY: 40, CX: 32, CY: 24, CameraToSite: []float64{1, 0, 0, 1, 0, 1, 0, 2, 0, 0, 1, 3, 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Second)}
	if e := s.Validate(); e != nil {
		t.Fatal(e)
	}
	s.Camera.CameraToSite[0] = -1
	if e := s.Validate(); e == nil {
		t.Fatal("reflection accepted")
	}
	s.Camera.CameraToSite[0] = 1
	s.Camera.CameraToSite[3] = 5
	if e := s.Validate(); e == nil {
		t.Fatal("inconsistent pose accepted")
	}
}
