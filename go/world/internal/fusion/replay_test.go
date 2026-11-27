package fusion

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/replay"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func replayFixture(t *testing.T) (*fixture, []world.Component) {
	t.Helper()
	f := newFixture(t)
	originals := append([]world.Component(nil), f.records...)
	captureAnchor := originals[3].Timing.SampleTime.Add(-250 * time.Millisecond)
	anchor := captureAnchor.Add(24 * time.Hour)
	f.now = anchor.Add(500 * time.Millisecond)
	f.cfg.RunID, f.cfg.ClockDomain = "replay-run", "simulation:replay-run"
	f.cfg.Replay, f.cfg.RecordingSHA256 = true, strings.Repeat("b", 64)
	f.cfg.Sources = map[string]world.Source{}
	for i, original := range originals {
		source := original.Key.Source
		source.Device, source.App = "replay-device", "recording-reader"
		c, err := replay.Build(original, replay.Config{Tenant: f.cfg.Tenant, Site: f.cfg.Site, RunID: f.cfg.RunID, RecordingSHA256: f.cfg.RecordingSHA256, Source: source, Epoch: 2, Sequence: uint64(i + 1), CaptureAnchor: captureAnchor, ReplayAnchor: anchor, TTL: 2 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		f.records[i] = c
		f.cfg.Sources[source.Service] = source
	}
	return f, originals
}

func rebuildReplay(t *testing.T, c *world.Component, change func(*world.Component, *replay.Config)) {
	t.Helper()
	_, e, err := replay.FromComponent(*c)
	if err != nil {
		t.Fatal(err)
	}
	cfg := replay.Config{Tenant: c.Key.Tenant, Site: c.Key.Site, RunID: e.RunID, RecordingSHA256: e.RecordingSHA256, Source: c.Key.Source, Epoch: c.Version.Epoch, Sequence: c.Version.Sequence, CaptureAnchor: e.CaptureAnchor, ReplayAnchor: e.ReplayAnchor, TTL: c.TTL}
	change(&e.Original, &cfg)
	*c, err = replay.Build(e.Original, cfg)
	if err != nil {
		t.Fatal(err)
	}
}

func TestReplayFusionPreservesCapturedEvidenceAndAncestry(t *testing.T) {
	live := newFixture(t)
	liveResult := live.run(t)
	f, originals := replayFixture(t)
	r := f.run(t)
	if r.State != HazardDetected || r.Hazard == nil || len(r.Hazard.Inputs) != 4 {
		t.Fatalf("%+v", r)
	}
	if r.Hazard.GasValuePPM != liveResult.Hazard.GasValuePPM || !reflect.DeepEqual(r.Hazard.Audio, liveResult.Hazard.Audio) || !reflect.DeepEqual(r.Hazard.Cameras, liveResult.Hazard.Cameras) || !reflect.DeepEqual(r.Hazard.Evidence, liveResult.Hazard.Evidence) {
		t.Fatal("replay changed the input bytes or fusion decision")
	}
	bySensor := map[string]world.Component{}
	for _, c := range originals {
		bySensor[c.Key.Source.Service] = c
	}
	for _, in := range r.Hazard.Inputs {
		original := bySensor[in.SensorID]
		if in.Replay == nil || !reflect.DeepEqual(in.Replay.Original, original) || in.Replay.RecordingSHA256 != f.cfg.RecordingSHA256 || in.Sample.RunID != "run" || in.Source == original.Key.Source || in.SampleTime.Equal(original.Timing.SampleTime) {
			t.Fatalf("replay input lost source or timing separation: %+v", in)
		}
		if !in.SampleTime.Equal(in.Replay.ReplayAnchor.Add(original.Timing.SampleTime.Sub(in.Replay.CaptureAnchor))) {
			t.Fatal("replay timeline changed")
		}
	}
	source := world.Source{Device: "fusion", App: "app", Service: "fusion", Stream: "hazards"}
	c, err := r.Component(source, 9, 1)
	if err != nil {
		t.Fatal(err)
	}
	if c.Key.Type != ReplayHazardType || c.Key.Type == HazardType || c.Provenance.Origin != world.Inferred || !c.Provenance.Synthetic() || c.Timing.ClockDomain != "simulation:replay-run" {
		t.Fatalf("replay result lost isolation: %+v", c)
	}
	if err := c.Provenance.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := tasking.TriggerFromComponent(c); err == nil {
		t.Fatal("inspection trigger accepted a replay evaluation as a live hazard")
	}
	for _, in := range c.Provenance.Inputs {
		if in.Origin != world.Replayed || in.Simulation == nil || in.Simulation.RunID != "run" || in.Simulation.ClockDomain != "simulation:run" || in.OriginalCaptureTime == nil || !in.OriginalCaptureTime.Equal(originals[0].Timing.SampleTime) || in.OriginalClockDomain != "simulation:run" {
			t.Fatalf("replay ancestry lost: %+v", in)
		}
	}
	var decoded Hazard
	if err := json.Unmarshal(c.Payload, &decoded); err != nil || len(decoded.Inputs) != 4 || decoded.Inputs[0].Replay == nil {
		t.Fatal("published payload lost original snapshot", err)
	}
}

func TestReplayFusionRequiresExplicitModeAndBindings(t *testing.T) {
	f, _ := replayFixture(t)
	f.cfg.Replay, f.cfg.RecordingSHA256 = false, ""
	if r := f.run(t); r.State != InsufficientEvidence {
		t.Fatal("live mode consumed replay", r)
	}
	for _, change := range []func(*Config){
		func(c *Config) { c.Sources = nil },
		func(c *Config) { c.RecordingSHA256 = "" },
		func(c *Config) { c.Replay = false },
	} {
		f, _ := replayFixture(t)
		change(&f.cfg)
		if _, err := Evaluate(context.Background(), f.cfg, f.records, f.load); err == nil {
			t.Fatal("ambiguous replay configuration accepted")
		}
	}
}

func TestReplayFusionRejectsMixedAndRewrittenInputs(t *testing.T) {
	cases := map[string]func(*fixture, []world.Component){
		"mixed-live": func(f *fixture, originals []world.Component) { f.records = append(f.records, originals[0]) },
		"wrong-recording": func(f *fixture, _ []world.Component) {
			rebuildReplay(t, &f.records[0], func(_ *world.Component, cfg *replay.Config) { cfg.RecordingSHA256 = strings.Repeat("c", 64) })
		},
		"different-timeline": func(f *fixture, _ []world.Component) {
			rebuildReplay(t, &f.records[0], func(_ *world.Component, cfg *replay.Config) {
				cfg.CaptureAnchor = cfg.CaptureAnchor.Add(-time.Nanosecond)
				cfg.ReplayAnchor = cfg.ReplayAnchor.Add(-time.Nanosecond)
			})
		},
		"different-original-run": func(f *fixture, _ []world.Component) {
			rebuildReplay(t, &f.records[0], func(original *world.Component, _ *replay.Config) {
				modifySample(t, original, func(s *sensing.Sample) { s.RunID = "other"; s.ID = "other/" + s.SensorID + "/1" })
				original.Key.Entity = "other/" + original.Key.Source.Service
				original.Provenance.Simulation.RunID = "other"
				original.Provenance.Simulation.ClockDomain = "simulation:other"
				original.Timing.ClockDomain = "simulation:other"
			})
		},
		"wrong-runtime-run": func(f *fixture, _ []world.Component) {
			rebuildReplay(t, &f.records[0], func(_ *world.Component, cfg *replay.Config) { cfg.RunID = "other-replay" })
		},
		"rewritten-capture": func(f *fixture, _ []world.Component) {
			var e replay.Envelope
			if err := json.Unmarshal(f.records[0].Payload, &e); err != nil {
				t.Fatal(err)
			}
			e.Original.Key.Source.Device = "claimed-original-device"
			f.records[0].Payload, _ = json.Marshal(e)
		},
		"wrong-replayer":     func(f *fixture, _ []world.Component) { f.records[0].Key.Source.Device = "other" },
		"wrong-runtime-time": func(f *fixture, _ []world.Component) { f.records[0].Timing.SampleTime = f.now },
		"expired":            func(f *fixture, _ []world.Component) { f.now = f.now.Add(3 * time.Second) },
		"missing-audio":      func(f *fixture, _ []world.Component) { f.records = f.records[:3] },
		"missing-media":      func(f *fixture, _ []world.Component) { f.blobs = map[string][]byte{} },
		"corrupt-media": func(f *fixture, _ []world.Component) {
			for k := range f.blobs {
				f.blobs[k] = []byte("altered")
			}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f, originals := replayFixture(t)
			change(f, originals)
			if r := f.run(t); r.State != InsufficientEvidence || r.Hazard != nil {
				t.Fatalf("%+v", r)
			}
		})
	}
}

func TestReplayFusionDoesNotRenewRecordedInputLifetimes(t *testing.T) {
	f, _ := replayFixture(t)
	r := f.run(t)
	if r.Hazard == nil {
		t.Fatal(r)
	}
	expiry := r.Hazard.ValidUntil
	f.now = f.now.Add(time.Second)
	again := f.run(t)
	if again.Hazard == nil || !again.Hazard.ValidUntil.Equal(expiry) || again.InputDigest != r.InputDigest {
		t.Fatal("query replay renewed an input", again)
	}
	f.now = expiry
	if r := f.run(t); r.State != InsufficientEvidence {
		t.Fatal(r)
	}

	f, _ = replayFixture(t)
	load := func(ctx context.Context, ref sensing.EvidenceRef) ([]byte, error) {
		f.now = f.now.Add(time.Second)
		return f.load(ctx, ref)
	}
	r, err := Evaluate(context.Background(), f.cfg, f.records, load)
	if err != nil || r.State != InsufficientEvidence {
		t.Fatal(r, err)
	}
}
