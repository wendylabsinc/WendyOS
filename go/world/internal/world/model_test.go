package world

import (
	"math"
	"strings"
	"testing"
	"time"
)

func observation() Observation {
	return Observation{ID: "sample", Tenant: "tenant", Site: "site", Source: Source{"device", "app", "service", "stream"}, Frame: FrameRef{"floor", "1"}, Timing: Timing{SampleTime: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC), ClockDomain: "utc", ErrorKnown: true}, Provenance: Provenance{Origin: Measured, AppVersion: "1"}, Scalar: &ScalarSample{Value: 42, Unit: "ppm", SensorType: "gas", CalibrationRevision: "calibration-1", Uncertainty: 2}}
}
func TestObservationBodiesAndProvenance(t *testing.T) {
	o := observation()
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(*Observation)
	}{
		{"missing unit", func(o *Observation) { o.Scalar.Unit = "" }},
		{"missing calibration", func(o *Observation) { o.Scalar.CalibrationRevision = "" }},
		{"not finite", func(o *Observation) { o.Scalar.Value = math.NaN() }},
		{"two bodies", func(o *Observation) {
			o.Image = &MediaSample{SHA256: strings.Repeat("a", 64), ContentType: "image/jpeg", CalibrationRevision: "1"}
		}},
		{"no body", func(o *Observation) { o.Scalar = nil }},
		{"unattributed service", func(o *Observation) { o.Source.Service = "" }},
		{"unbounded simulation", func(o *Observation) { o.Provenance.Origin = Simulated }},
		{"replay erases original time", func(o *Observation) { o.Provenance.Origin = Replayed }},
		{"inference lacks input ancestry", func(o *Observation) {
			o.Provenance = Provenance{Origin: Inferred, AppVersion: "1", ModelID: "fusion", ModelVersion: "1"}
		}},
		{"wrong simulation clock", func(o *Observation) {
			o.Provenance = Provenance{Origin: Simulated, AppVersion: "1", Simulation: &SimulationContext{RunID: "r", ScenarioVersion: "1", ClockDomain: "sim"}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := observation()
			tt.mutate(&o)
			if err := o.Validate(); err == nil {
				t.Fatal("accepted malformed observation")
			}
		})
	}
}
func TestMediaAndDerivedObservation(t *testing.T) {
	for _, kind := range []string{"image", "audio"} {
		o := observation()
		o.Scalar = nil
		m := &MediaSample{SHA256: strings.Repeat("ab", 32), ContentType: kind + "/raw", CalibrationRevision: "1", Duration: time.Second}
		if kind == "image" {
			o.Image = m
		} else {
			o.Audio = m
		}
		if err := o.Validate(); err != nil {
			t.Fatal(err)
		}
		m.ContentType = "video/raw"
		if err := o.Validate(); err == nil {
			t.Fatal("accepted wrong modality")
		}
	}
	o := observation()
	capture := o.Timing.SampleTime.Add(-time.Hour)
	o.Scalar = nil
	o.Claim = &DerivedClaim{Label: "possible-leak", Confidence: 0.8}
	o.Provenance = Provenance{Origin: Inferred, AppVersion: "1", ModelID: "fusion", ModelVersion: "2", Inputs: []Input{{ID: "gas", Origin: Measured}, {ID: "audio", Origin: Replayed, OriginalCaptureTime: &capture, OriginalClockDomain: "utc"}}}
	if err := o.Validate(); err != nil {
		t.Fatal(err)
	}
	if !o.Provenance.Synthetic() {
		t.Fatal("inference lost replay ancestry")
	}
	o.Provenance.Inputs[1].Origin = Inferred
	if err := o.Validate(); err == nil {
		t.Fatal("accepted unresolved inference ancestry")
	}
}
func TestGeographicAndLocalFrameValidation(t *testing.T) {
	a := GeographicAnchor{LatitudeDegrees: 52.37, LongitudeDegrees: 4.9, AltitudeMetres: 2, AltitudeDatum: "explicit-test-datum", UncertaintyMetres: 1}
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	a.AltitudeDatum = ""
	if err := a.Validate(); err == nil {
		t.Fatal("implicit altitude datum")
	}
	a.AltitudeDatum = "test"
	a.LatitudeDegrees = 91
	if err := a.Validate(); err == nil {
		t.Fatal("invalid latitude")
	}
	now := time.Now()
	f := LocalFrame{Ref: FrameRef{"room", "1"}, Parent: FrameRef{"site", "1"}, RotationXYZW: [4]float64{0, 0, 0, 1}, ValidFrom: now, ValidUntil: now.Add(time.Hour)}
	if err := f.Validate(); err != nil {
		t.Fatal(err)
	}
	f.RotationXYZW[3] = 2
	if err := f.Validate(); err == nil {
		t.Fatal("non-unit quaternion")
	}
}
