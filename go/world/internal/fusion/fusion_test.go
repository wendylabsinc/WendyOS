package fusion

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func pngFixture(red bool) []byte {
	im := image.NewRGBA(image.Rect(0, 0, 64, 48))
	c := color.RGBA{G: 255, A: 255}
	if red {
		c = color.RGBA{R: 255, A: 255}
	}
	for y := 0; y < 48; y++ {
		for x := 0; x < 64; x++ {
			im.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	png.Encode(&b, im)
	return b.Bytes()
}
func wavFixture(tone float64) []byte {
	const rate, count = 8000, 2000
	data := make([]byte, 44+2*count)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	copy(data[8:12], "WAVE")
	copy(data[12:16], "fmt ")
	binary.LittleEndian.PutUint32(data[16:20], 16)
	binary.LittleEndian.PutUint16(data[20:22], 1)
	binary.LittleEndian.PutUint16(data[22:24], 1)
	binary.LittleEndian.PutUint32(data[24:28], rate)
	binary.LittleEndian.PutUint32(data[28:32], rate*2)
	binary.LittleEndian.PutUint16(data[32:34], 2)
	binary.LittleEndian.PutUint16(data[34:36], 16)
	copy(data[36:40], "data")
	binary.LittleEndian.PutUint32(data[40:44], count*2)
	for i := 0; i < count; i++ {
		value := int16(.4 * 32767 * math.Sin(2*math.Pi*tone*float64(i)/rate))
		binary.LittleEndian.PutUint16(data[44+i*2:], uint16(value))
	}
	return data
}

type fixture struct {
	now     time.Time
	cfg     Config
	records []world.Component
	blobs   map[string][]byte
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	anchor := time.Unix(1700000000, 0).UTC()
	f := &fixture{now: anchor.Add(500 * time.Millisecond), blobs: map[string][]byte{}}
	f.cfg = Config{Tenant: "tenant", Site: "office", RunID: "run", Frame: world.FrameRef{ID: "floor", Revision: "1"}, ClockDomain: "simulation:run", Now: func() time.Time { return f.now }}
	for _, id := range []string{"camera-a", "camera-b", "gas-1", "audio-1"} {
		source := world.Source{Device: "device-" + id, App: "sensors", Service: id, Stream: "samples"}
		s := sensing.Sample{SchemaVersion: sensing.Schema, ID: "run/" + id + "/1", SiteID: "office", Frame: f.cfg.Frame, SensorID: id, Sequence: 1, SampleNS: 250000000, RunID: "run", ScenarioVersion: "v1", PackageSHA256: strings.Repeat("a", 64), SensorModel: "fixture", PositionM: []float64{1, 2, 3}, UncertaintyM: .1, CalibrationRevision: "c1"}
		var b []byte
		contentType := ""
		switch id {
		case "camera-a", "camera-b":
			s.Kind = "camera"
			s.Camera = &sensing.Camera{Width: 64, Height: 48, FX: 40, FY: 40, CX: 32, CY: 24, CameraToSite: []float64{1, 0, 0, 1, 0, 1, 0, 2, 0, 0, 1, 3, 0, 0, 0, 1}, CalibrationValidForNS: int64(5 * time.Second)}
			b = pngFixture(true)
			contentType = "image/png"
		case "gas-1":
			s.Kind = "gas"
			s.Gas = &sensing.Gas{ValuePPM: 100, UncertaintyPPM: 2, Species: "synthetic-test-gas"}
		case "audio-1":
			s.Kind = "audio"
			s.Audio = &sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: 250000000}
			b = wavFixture(880)
			contentType = "audio/wav"
		}
		if b != nil {
			hash := sha256.Sum256(b)
			ref := sensing.EvidenceRef{SHA256: hex.EncodeToString(hash[:]), Size: int64(len(b)), ContentType: contentType}
			s.Evidence = &ref
			f.blobs[ref.SHA256] = b
		}
		converter, e := sensing.New(sensing.Config{Tenant: "tenant", Site: "office", RunID: "run", Frame: f.cfg.Frame, Source: source, Epoch: 1, Anchor: anchor, TTL: 2 * time.Second})
		if e != nil {
			t.Fatal(e)
		}
		raw, _ := json.Marshal(s)
		c, e := converter.Convert(raw)
		if e != nil {
			t.Fatal(e)
		}
		f.records = append(f.records, c)
	}
	return f
}
func (f *fixture) load(ctx context.Context, ref sensing.EvidenceRef) ([]byte, error) {
	b, ok := f.blobs[ref.SHA256]
	if !ok {
		return nil, errors.New("missing")
	}
	return append([]byte(nil), b...), nil
}
func (f *fixture) run(t *testing.T) Result {
	t.Helper()
	r, e := Evaluate(context.Background(), f.cfg, f.records, f.load)
	if e != nil {
		t.Fatal(e)
	}
	return r
}
func modifySample(t *testing.T, c *world.Component, fn func(*sensing.Sample)) {
	t.Helper()
	s, e := sensing.Decode(c.Payload)
	if e != nil {
		t.Fatal(e)
	}
	fn(&s)
	c.Payload, _ = json.Marshal(s)
}
func TestHazardUsesVerifiedBytesAndKeepsExpiryAncestry(t *testing.T) {
	f := newFixture(t)
	r := f.run(t)
	if r.State != HazardDetected || len(r.Hazard.Inputs) != 4 || len(r.Hazard.Evidence) != 3 {
		t.Fatalf("%+v", r)
	}
	if r.Hazard.Audio.ToneFraction < .99 || r.Hazard.Cameras[0].RedFraction < .99 || r.Hazard.ConfidenceMeaning == "" {
		t.Fatal(r.Hazard)
	}
	source := world.Source{Device: "fusion", App: "app", Service: "fusion", Stream: "hazards"}
	c, e := r.Component(source, 3, 1)
	if e != nil {
		t.Fatal(e)
	}
	if e = c.Provenance.Validate(); e != nil {
		t.Fatal(e)
	}
	if !c.Provenance.Synthetic() || len(c.Provenance.Inputs) != 4 || c.Provenance.Origin != world.Inferred {
		t.Fatal(c.Provenance)
	}
	expiry := c.Timing.SampleTime.Add(c.TTL)
	f.now = f.now.Add(time.Second)
	again := f.run(t)
	c2, e := again.Component(source, 3, 2)
	if e != nil {
		t.Fatal(e)
	}
	if !expiry.Equal(c2.Timing.SampleTime.Add(c2.TTL)) || again.InputDigest != r.InputDigest {
		t.Fatal("duplicate inputs renewed or changed")
	}
	f.now = expiry
	if r = f.run(t); r.State != InsufficientEvidence {
		t.Fatal(r)
	}
	if _, e = again.Component(source, 3, 3); e == nil {
		t.Fatal("expired result published")
	}
}
func TestMissingStaleMisalignedAndRecursiveInputsFailClosed(t *testing.T) {
	cases := map[string]func(*fixture){
		"missing":       func(f *fixture) { f.records = f.records[:3] },
		"expired":       func(f *fixture) { f.now = f.now.Add(3 * time.Second) },
		"unknown-clock": func(f *fixture) { f.records[0].Timing.ErrorKnown = false },
		"future":        func(f *fixture) { f.records[0].Timing.SampleTime = f.now.Add(time.Millisecond) },
		"frame":         func(f *fixture) { f.records[0].Frame.Revision = "other" },
		"skew":          func(f *fixture) { f.records[0].Timing.SampleTime = f.records[0].Timing.SampleTime.Add(-time.Second) },
		"calibration": func(f *fixture) {
			modifySample(t, &f.records[0], func(s *sensing.Sample) { s.Camera.CalibrationValidForNS = 1 })
		},
		"age-hint":              func(f *fixture) { f.records[0].Timing.AgeHint = f.records[0].TTL },
		"same-source-duplicate": func(f *fixture) { f.records[1] = f.records[0] },
		"self-output":           func(f *fixture) { f.records[0].Key.Type = "wendy.hazard.v1" },
		"ambiguous-source": func(f *fixture) {
			c := f.records[0]
			c.Key.Source.Device = "impostor"
			f.records = append(f.records, c)
		},
		"conflicting-version": func(f *fixture) { c := f.records[0]; c.TTL += time.Second; f.records = append(f.records, c) },
		"missing-evidence":    func(f *fixture) { f.blobs = map[string][]byte{} },
		"corrupt-evidence": func(f *fixture) {
			for k := range f.blobs {
				f.blobs[k] = []byte("corrupt")
			}
		},
		"mixed-package": func(f *fixture) {
			modifySample(t, &f.records[0], func(s *sensing.Sample) { s.PackageSHA256 = strings.Repeat("b", 64) })
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			change(f)
			if r := f.run(t); r.State != InsufficientEvidence || r.Hazard != nil {
				t.Fatalf("%+v", r)
			}
		})
	}
}
func TestNegativeEvidenceIsNotClearance(t *testing.T) {
	for _, kind := range []string{"gas", "camera", "audio"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t)
			switch kind {
			case "gas":
				modifySample(t, &f.records[2], func(s *sensing.Sample) { s.Gas.ValuePPM = 51; s.Gas.UncertaintyPPM = 2 })
			case "camera", "audio":
				idx := 0
				blob := pngFixture(false)
				if kind == "audio" {
					idx = 3
					blob = wavFixture(440)
				}
				hash := sha256.Sum256(blob)
				digest := hex.EncodeToString(hash[:])
				f.blobs[digest] = blob
				modifySample(t, &f.records[idx], func(s *sensing.Sample) { s.Evidence.SHA256 = digest; s.Evidence.Size = int64(len(blob)) })
			}
			r := f.run(t)
			if r.State != NotDetected || r.Hazard != nil || !strings.Contains(r.Reason, "not clearance") {
				t.Fatal(r)
			}
		})
	}
}
func TestEvidenceDelayAndSourceBinding(t *testing.T) {
	f := newFixture(t)
	load := func(ctx context.Context, ref sensing.EvidenceRef) ([]byte, error) {
		f.now = f.now.Add(time.Second)
		return f.load(ctx, ref)
	}
	r, e := Evaluate(context.Background(), f.cfg, f.records, load)
	if e != nil || r.State != InsufficientEvidence {
		t.Fatal(r, e)
	}
	f = newFixture(t)
	f.cfg.Sources = map[string]world.Source{}
	for _, c := range f.records {
		f.cfg.Sources[c.Key.Source.Service] = c.Key.Source
	}
	bad := f.cfg.Sources["camera-a"]
	bad.Device = "other"
	f.cfg.Sources["camera-a"] = bad
	if r = f.run(t); r.State != InsufficientEvidence {
		t.Fatal(r)
	}
	delete(f.cfg.Sources, "camera-a")
	f.cfg.Sources["wrong-key"] = bad
	if _, e = Evaluate(context.Background(), f.cfg, f.records, f.load); e == nil {
		t.Fatal("same-count wrong binding keys accepted")
	}
}
func TestPNGAndWAVMetadataCannotInventObservations(t *testing.T) {
	if _, e := RedFraction(pngFixture(true), sensing.Camera{Width: 32, Height: 24}); e == nil {
		t.Fatal("wrong PNG dimensions accepted")
	}
	audio := sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: 250000000}
	for _, mutate := range []func([]byte) []byte{
		func(b []byte) []byte { return b[:len(b)-1] },
		func(b []byte) []byte { binary.LittleEndian.PutUint16(b[22:24], 2); return b },
		func(b []byte) []byte { binary.LittleEndian.PutUint32(b[40:44], 0xffffffff); return b },
	} {
		if _, e := WAVFeatures(mutate(wavFixture(880)), audio); e == nil {
			t.Fatal("invalid WAV accepted")
		}
	}
	quiet, e := WAVFeatures(wavFixture(0), audio)
	if e != nil || quiet.RMS != 0 || quiet.ToneFraction != 0 {
		t.Fatal(quiet, e)
	}
}

func TestAudioCaptureIntervalMustAlignWithOtherObservations(t *testing.T) {
	f := newFixture(t)
	modifySample(t, &f.records[3], func(s *sensing.Sample) {
		s.SampleNS = int64(2 * time.Second)
		s.Audio.DurationNS = int64(2 * time.Second)
	})
	// Ending now cannot make a two-second recording contemporaneous with the
	// fresh camera and gas samples. Reject before loading mismatched media.
	f.records[3].Timing.SampleTime = f.now
	if r := f.run(t); r.State != InsufficientEvidence {
		t.Fatalf("long audio interval accepted: %+v", r)
	}
}
