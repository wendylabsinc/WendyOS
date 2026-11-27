package replay

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func fixture(t *testing.T, kind string) (world.Component, Config) {
	t.Helper()
	anchor := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	sensor := kind + "-1"
	s := sensing.Sample{SchemaVersion: sensing.Schema, ID: "capture/" + sensor + "/7", SiteID: "office", Frame: world.FrameRef{ID: "floor", Revision: "v1"}, SensorID: sensor, Kind: kind, Sequence: 7, SampleNS: int64(10*time.Hour + 250*time.Millisecond), RunID: "capture", ScenarioVersion: "office-v1", Seed: 7, PackageSHA256: strings.Repeat("a", 64), SensorModel: "test-" + kind, PositionM: []float64{1, 2, 3}, UncertaintyM: .1, CalibrationRevision: "cal-v1"}
	switch kind {
	case "gas":
		s.Gas = &sensing.Gas{ValuePPM: 100, UncertaintyPPM: 2, Species: "synthetic"}
	case "audio":
		s.Audio = &sensing.Audio{SampleRateHz: 8000, Channels: 1, DurationNS: int64(200 * time.Millisecond)}
		s.Evidence = &sensing.EvidenceRef{SHA256: strings.Repeat("b", 64), ContentType: "audio/wav", Size: 1234}
	case "camera":
		s.Camera = &sensing.Camera{Width: 64, Height: 48, FX: 40, FY: 40, CX: 32, CY: 24, CameraToSite: []float64{1, 0, 0, 1, 0, 1, 0, 2, 0, 0, 1, 3, 0, 0, 0, 1}, CalibrationValidForNS: int64(time.Second)}
		s.Evidence = &sensing.EvidenceRef{SHA256: strings.Repeat("c", 64), ContentType: "image/png", Size: 345}
	}
	raw, _ := json.Marshal(s)
	source := world.Source{Device: "capturing-device", App: "sensor-app", Service: sensor, Stream: "samples"}
	cv, err := sensing.New(sensing.Config{Tenant: "tenant", Site: s.SiteID, RunID: s.RunID, Frame: s.Frame, Source: source, Epoch: 3, Anchor: anchor, TTL: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	original, err := cv.Convert(raw)
	if err != nil {
		t.Fatal(err)
	}
	original.Timing.ErrorBound = 20 * time.Millisecond
	original.Timing.AgeHint = time.Minute
	cfg := Config{Tenant: "tenant", Site: "office", RunID: "replay-one", RecordingSHA256: strings.Repeat("d", 64), Source: world.Source{Device: "replay-device", App: "replay-app", Service: sensor, Stream: "replays"}, Epoch: 9, Sequence: 15, CaptureAnchor: anchor.Add(10 * time.Hour), ReplayAnchor: anchor.AddDate(0, 0, 7), TTL: 5 * time.Second}
	return original, cfg
}

func TestReplayPreservesOriginalAndMapsExcerptAtOneTimesSpeed(t *testing.T) {
	for _, kind := range []string{"gas", "camera", "audio"} {
		t.Run(kind, func(t *testing.T) {
			original, cfg := fixture(t, kind)
			c, err := Build(original, cfg)
			if err != nil {
				t.Fatal(err)
			}
			s, env, err := FromComponent(c)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(original, env.Original) || s.RunID != "capture" || s.SampleNS != int64(10*time.Hour+250*time.Millisecond) || env.RunID != cfg.RunID {
				t.Fatal("original observation or run identity changed", env)
			}
			if !c.Timing.SampleTime.Equal(cfg.ReplayAnchor.Add(250*time.Millisecond)) || c.Timing.ClockDomain != "simulation:replay-one" || c.Timing.ErrorBound != original.Timing.ErrorBound || c.Timing.AgeHint != 0 || c.TTL != cfg.TTL {
				t.Fatal("replay timeline or error changed", c.Timing)
			}
			if c.Provenance.Origin != world.Replayed || !c.Provenance.Synthetic() || c.Provenance.Simulation.RunID != "capture" || !c.Provenance.OriginalCaptureTime.Equal(original.Timing.SampleTime) || c.Provenance.OriginalClockDomain != original.Timing.ClockDomain || c.Key.Source == original.Key.Source || c.Version.Sequence != 15 {
				t.Fatal("capture ancestry or replay publisher lost", c.Provenance)
			}
			// A relay may increase replay staleness without rewriting history.
			c.Timing.AgeHint = 3 * time.Second
			if _, _, err := FromComponent(c); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestBuildOwnsOriginalPayloadAndSimulationMetadata(t *testing.T) {
	original, cfg := fixture(t, "gas")
	c, err := Build(original, cfg)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]byte(nil), c.Payload...)
	original.Payload[0] = ' '
	original.Provenance.Simulation.RunID = "mutated"
	if !bytes.Equal(c.Payload, want) || c.Provenance.Simulation.RunID != "capture" {
		t.Fatal("output aliases caller storage")
	}
	if _, _, err := FromComponent(c); err != nil {
		t.Fatal(err)
	}
}

func TestRejectsInvalidReplayScopeTimelineAndLifetime(t *testing.T) {
	for name, change := range map[string]func(*world.Component, *Config){
		"tenant":                func(_ *world.Component, c *Config) { c.Tenant = "other" },
		"site":                  func(_ *world.Component, c *Config) { c.Site = "other" },
		"same run":              func(_ *world.Component, c *Config) { c.RunID = "capture" },
		"same source":           func(o *world.Component, c *Config) { c.Source = o.Key.Source },
		"other service":         func(_ *world.Component, c *Config) { c.Source.Service = "camera-1" },
		"zero epoch":            func(_ *world.Component, c *Config) { c.Epoch = 0 },
		"zero sequence":         func(_ *world.Component, c *Config) { c.Sequence = 0 },
		"bad recording digest":  func(_ *world.Component, c *Config) { c.RecordingSHA256 = "bad" },
		"missing replay anchor": func(_ *world.Component, c *Config) { c.ReplayAnchor = time.Time{} },
		"negative interval":     func(o *world.Component, c *Config) { c.CaptureAnchor = o.Timing.SampleTime.Add(time.Second) },
		"overlong excerpt":      func(_ *world.Component, c *Config) { c.CaptureAnchor = c.CaptureAnchor.Add(-25 * time.Hour) },
		"zero TTL":              func(_ *world.Component, c *Config) { c.TTL = 0 },
		"TTL below uncertainty": func(_ *world.Component, c *Config) { c.TTL = 10 * time.Millisecond },
		"unknown clock":         func(o *world.Component, _ *Config) { o.Timing.ErrorKnown = false },
		"too much clock error":  func(o *world.Component, _ *Config) { o.Timing.ErrorBound = o.TTL },
		"overlong original TTL": func(o *world.Component, _ *Config) { o.TTL = 25 * time.Hour },
		"inferred original":     func(o *world.Component, _ *Config) { o.Provenance.Origin = world.Inferred },
		"measured original": func(o *world.Component, _ *Config) {
			o.Provenance = world.Provenance{Origin: world.Measured, AppVersion: "other"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			original, cfg := fixture(t, "gas")
			change(&original, &cfg)
			if _, err := Build(original, cfg); err == nil {
				t.Fatal("accepted invalid replay")
			}
		})
	}
	original, cfg := fixture(t, "gas")
	cfg.TTL = time.Hour
	c, err := Build(original, cfg)
	if err != nil || c.TTL != original.TTL {
		t.Fatal("replay must cap original lifetime", c.TTL, err)
	}
	if _, err := Build(c, cfg); err == nil {
		t.Fatal("accepted recursive replay")
	}
}

func TestEntireAudioIntervalAndCameraCalibrationRemainBounded(t *testing.T) {
	original, cfg := fixture(t, "audio")
	cfg.CaptureAnchor = original.Timing.SampleTime.Add(-100 * time.Millisecond)
	if _, err := Build(original, cfg); err == nil {
		t.Fatal("audio starts before excerpt")
	}
	original, cfg = fixture(t, "audio")
	cfg.TTL = 210 * time.Millisecond
	if _, err := Build(original, cfg); err == nil {
		t.Fatal("clock error plus audio exhausts replay TTL")
	}
	original.TTL = 100 * time.Millisecond
	if _, err := Build(original, cfg); err == nil {
		t.Fatal("invalid original interval accepted")
	}
	original, cfg = fixture(t, "camera")
	original.Timing.ErrorBound = 2 * time.Second
	if _, err := Build(original, cfg); err == nil {
		t.Fatal("invalid original calibration accepted")
	}
}

func TestOuterBindingRejectsTimingSourceProvenanceAndOriginalMutation(t *testing.T) {
	for name, change := range map[string]func(*world.Component){
		"entity":         func(c *world.Component) { c.Key.Entity = "other/gas-1" },
		"site":           func(c *world.Component) { c.Key.Site = "other" },
		"service":        func(c *world.Component) { c.Key.Source.Service = "other" },
		"type":           func(c *world.Component) { c.Key.Type = sensing.ComponentType },
		"frame":          func(c *world.Component) { c.Frame.Revision = "other" },
		"clock":          func(c *world.Component) { c.Timing.ClockDomain = "simulation:capture" },
		"time":           func(c *world.Component) { c.Timing.SampleTime = c.Timing.SampleTime.Add(time.Millisecond) },
		"certainty":      func(c *world.Component) { c.Timing.ErrorBound = 0 },
		"negative age":   func(c *world.Component) { c.Timing.AgeHint = -1 },
		"TTL":            func(c *world.Component) { c.TTL = time.Hour },
		"origin":         func(c *world.Component) { c.Provenance.Origin = world.Simulated },
		"capture time":   func(c *world.Component) { x := c.Timing.SampleTime; c.Provenance.OriginalCaptureTime = &x },
		"original clock": func(c *world.Component) { c.Provenance.OriginalClockDomain = "simulation:other" },
		"original run":   func(c *world.Component) { c.Provenance.Simulation.RunID = "other" },
		"app version":    func(c *world.Component) { c.Provenance.AppVersion = "other" },
		"tombstone":      func(c *world.Component) { c.Tombstone = true },
		"original mutation": func(c *world.Component) {
			var e Envelope
			_ = json.Unmarshal(c.Payload, &e)
			e.Original.Key.Source.Device = "other"
			c.Payload, _ = json.Marshal(e)
		},
	} {
		t.Run(name, func(t *testing.T) {
			o, cfg := fixture(t, "gas")
			c, err := Build(o, cfg)
			if err != nil {
				t.Fatal(err)
			}
			change(&c)
			if _, _, err := FromComponent(c); err == nil {
				t.Fatal("accepted inconsistent replay")
			}
		})
	}
}

func TestDecodeRejectsMalformedIncompleteOrOversizedEnvelope(t *testing.T) {
	o, cfg := fixture(t, "gas")
	c, err := Build(o, cfg)
	if err != nil {
		t.Fatal(err)
	}
	base := string(c.Payload)
	for name, raw := range map[string]string{
		"duplicate":               strings.Replace(base, `"schema_version":`, `"run_id":"dup","schema_version":`, 1),
		"unknown":                 strings.Replace(base, `"schema_version":`, `"unknown":0,"schema_version":`, 1),
		"trailing":                base + `{}`,
		"null time":               strings.Replace(base, `"capture_anchor":"2026-01-02T13:04:05Z"`, `"capture_anchor":null`, 1),
		"missing age":             strings.Replace(base, `,"age_hint_ns":60000000000`, ``, 1),
		"null age":                strings.Replace(base, `"age_hint_ns":60000000000`, `"age_hint_ns":null`, 1),
		"missing simulation seed": strings.Replace(base, `,"seed":7`, ``, 1),
		"oversized":               strings.Repeat(" ", MaxEnvelopeBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if raw == base {
				t.Fatal("test did not change input")
			}
			if _, err := Decode([]byte(raw)); err == nil {
				t.Fatal("malformed replay accepted")
			}
		})
	}
}

func TestPhysicalReplicaRejectsReplayEvenWithOriginalSimulationRetained(t *testing.T) {
	o, cfg := fixture(t, "gas")
	c, err := Build(o, cfg)
	if err != nil {
		t.Fatal(err)
	}
	r, err := world.NewReplica(world.Config{Tenant: cfg.Tenant, Site: cfg.Site, ClockDomain: c.Timing.ClockDomain, Now: func() time.Time { return c.Timing.SampleTime }, MaxEntries: 10, Frames: []world.FrameRef{c.Frame}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Apply(c.Key.Source, c); !errors.Is(err, world.ErrSynthetic) {
		t.Fatal("physical replica accepted replay", err)
	}
}
