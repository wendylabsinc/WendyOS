package incidentchange

import (
	"bytes"
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/gaschange"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/missioncontrol"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestRisingInspectionRetainsExactOriginalsAndTarget(t *testing.T) {
	cfg, records := joinedFixture(t, "rising")
	s, _ := New(cfg)
	now := cfg.Spatial.Gas.Instant(2.15)
	r, e := s.Step(records, now)
	if e != nil {
		t.Fatal(e)
	}
	version := world.Version{Epoch: 2, Sequence: 3}
	c, e := InspectionSelection(cfg, "east", version, r, records, now.Add(time.Millisecond), nil)
	if e != nil {
		t.Fatal(e)
	}
	p, spatial, claim, e := missioncontrol.DecodeRisingSelection(c)
	if e != nil {
		t.Fatal(e)
	}
	joined, _ := Component(cfg, "east", version, r)
	if p.JoinedSHA256 != gasreplica.ComponentHash(joined) || len(claim.Inputs) != 12 || !p.ValidUntil.Equal(r.ValidUntil) {
		t.Fatal("original context differs")
	}
	target, e := missioncontrol.RisingTriggerTarget(c)
	want, _ := missioncontrol.IncidentTriggerTarget(spatial)
	if e != nil || target != want {
		t.Fatal("target differs", target, want, e)
	}
	trigger, e := tasking.TriggerFromComponent(c)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(trigger)
	if len(raw) > 64<<10 {
		t.Fatalf("complete trigger exceeds64KiB: %d", len(raw))
	}
	t.Logf("complete trigger bytes=%d payload=%d", len(raw), len(c.Payload))
	if path := os.Getenv("WENDY_RISING_TRIGGER_FIXTURE"); path != "" {
		if e := os.WriteFile(path, raw, 0600); e != nil {
			t.Fatal(e)
		}
	}
	// Caller-owned observations and returned selections cannot mutate another output.
	records[2].Payload[0] = '!'
	copyC, e := InspectionSelection(cfg, "east", version, r, []world.Component{joined}, now, nil)
	if e == nil {
		t.Fatal("missing exact originals accepted", copyC)
	}
	c.Payload[0] = '!'
	if trigger.Validate(cfg.Spatial.Visual.Site, cfg.Spatial.Visual.Frame) == nil {
		t.Fatal("mutated trigger accepted")
	}
}
func TestRisingInspectionRejectsUnavailableChangedAndTampered(t *testing.T) {
	cfg, records := joinedFixture(t, "rising")
	s, _ := New(cfg)
	now := cfg.Spatial.Gas.Instant(2.15)
	r, _ := s.Step(records, now)
	v := world.Version{Epoch: 1, Sequence: 9}
	if _, e := InspectionSelection(cfg, "east", v, r, records, r.ValidUntil, nil); e == nil {
		t.Fatal("expired accepted")
	}
	altered := r
	altered.InputDigest = "changed"
	if _, e := InspectionSelection(cfg, "east", v, altered, records, now, nil); e == nil {
		t.Fatal("foreign/mutated evaluation accepted")
	}
	steady, inputs := joinedFixture(t, "steady")
	view, _ := New(steady)
	non, _ := view.Step(inputs, now)
	if _, e := InspectionSelection(steady, "east", v, non, inputs, now, nil); e == nil {
		t.Fatal("non-rising accepted")
	}
	c, e := InspectionSelection(cfg, "east", v, r, records, now, nil)
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"joined hash", "original mutation", "null spatial", "wrong run", "duplicate inputs", "wrong target"} {
		t.Run(name, func(t *testing.T) {
			bad := own(c)
			var p missioncontrol.RisingSelection
			_ = json.Unmarshal(bad.Payload, &p)
			switch name {
			case "joined hash":
				p.JoinedSHA256 = "bad"
			case "original mutation":
				p.TemporalJSON[len(p.TemporalJSON)/2] ^= 1
			case "null spatial":
				p.SpatialJSON = []byte("null")
			case "wrong run":
				bad.Timing.ClockDomain = "simulation:other"
			case "duplicate inputs":
				var x world.Component
				_ = json.Unmarshal(p.TemporalJSON, &x)
				var claim missioncontrol.RisingClaim
				_ = json.Unmarshal(x.Payload, &claim)
				claim.Inputs[1] = claim.Inputs[0]
				x.Payload, _ = json.Marshal(claim)
				p.TemporalJSON, _ = json.Marshal(x)
				p.TemporalSHA256 = gasreplica.ComponentHash(x)
			case "wrong target":
				var x world.Component
				_ = json.Unmarshal(p.SpatialJSON, &x)
				var selection missioncontrol.IncidentSelection
				_ = json.Unmarshal(x.Payload, &selection)
				selection.Position[0] += 1
				x.Payload, _ = json.Marshal(selection)
				p.SpatialJSON, _ = json.Marshal(x)
			}
			bad.Payload, _ = json.Marshal(p)
			if name == "wrong target" {
				originalTarget, _ := missioncontrol.RisingTriggerTarget(c)
				if e := missioncontrol.ValidateNativeInspectionTarget(bad, originalTarget, nil); e == nil {
					t.Fatal("fixed target tampering accepted")
				}
				return
			}
			if _, e := missioncontrol.RisingTriggerTarget(bad); e == nil {
				t.Fatal("tampered retained context accepted")
			}
		})
	}
}

// Resign no authority here: this helper only rebuilds a structurally coherent
// retained context to exercise checks beyond the outer digest mismatch.
func rewriteRisingClaim(t *testing.T, c world.Component, mutate func(*missioncontrol.RisingClaim)) world.Component {
	t.Helper()
	var p missioncontrol.RisingSelection
	_ = json.Unmarshal(c.Payload, &p)
	var temporal world.Component
	_ = json.Unmarshal(p.TemporalJSON, &temporal)
	var claim missioncontrol.RisingClaim
	_ = json.Unmarshal(temporal.Payload, &claim)
	before := map[int]string{}
	for i, input := range claim.Inputs {
		before[i] = gasreplica.ComponentHash(input)
	}
	mutate(&claim)
	for i, input := range claim.Inputs {
		old, next := before[i], gasreplica.ComponentHash(input)
		if old == next {
			continue
		}
		raw, _ := json.Marshal(claim.Window)
		raw = bytes.ReplaceAll(raw, []byte(old), []byte(next))
		_ = json.Unmarshal(raw, &claim.Window)
		for _, component := range []*world.Component{&c, &temporal} {
			for j := range component.Provenance.Inputs {
				if component.Provenance.Inputs[j].ID == "sha256:"+old {
					component.Provenance.Inputs[j].ID = "sha256:" + next
				}
			}
		}
		p.JoinedJSON = bytes.ReplaceAll(p.JoinedJSON, []byte(old), []byte(next))
		p.SpatialJSON = bytes.ReplaceAll(p.SpatialJSON, []byte(old), []byte(next))
	}
	hashes := []string{}
	for _, input := range claim.Inputs {
		hashes = append(hashes, gasreplica.ComponentHash(input))
	}
	sort.Strings(hashes)
	claim.Window.InputDigest = gasreplica.Digest(hashes)
	temporal.Payload, _ = json.Marshal(claim)
	p.TemporalJSON, _ = json.Marshal(temporal)
	next := gasreplica.ComponentHash(temporal)
	p.JoinedJSON = bytes.ReplaceAll(p.JoinedJSON, []byte(p.TemporalSHA256), []byte(next))
	p.TemporalSHA256 = next
	var joined world.Component
	_ = json.Unmarshal(p.JoinedJSON, &joined)
	p.JoinedSHA256 = gasreplica.ComponentHash(joined)
	c.Payload, _ = json.Marshal(p)
	return c
}
func TestRisingHistoricalImpossibleTimeAndSensorScope(t *testing.T) {
	cfg, records := joinedFixture(t, "rising")
	s, _ := New(cfg)
	now := cfg.Spatial.Gas.Instant(2.15)
	r, _ := s.Step(records, now)
	c, e := InspectionSelection(cfg, "east", world.Version{Epoch: 1, Sequence: 1}, r, records, now, nil)
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"future window", "changed calibration", "changed source"} {
		t.Run(name, func(t *testing.T) {
			bad := rewriteRisingClaim(t, own(c), func(claim *missioncontrol.RisingClaim) {
				if name == "future window" {
					claim.Window.Evaluated = claim.Window.Traces[0].Points[0].Captured - .1
					return
				}
				var o map[string]any
				_ = json.Unmarshal(claim.Inputs[0].Payload, &o)
				if name == "changed calibration" {
					o["calibration"] = "changed"
				} else {
					claim.Inputs[0].Key.Source.Device = "changed"
					raw, _ := json.Marshal(claim.Inputs[0].Key.Source)
					var source map[string]any
					_ = json.Unmarshal(raw, &source)
					o["source"] = source
				}
				claim.Inputs[0].Payload, _ = json.Marshal(o)
			})
			if _, e := missioncontrol.RisingTriggerTarget(bad); e == nil {
				t.Fatal("inconsistent historical context accepted")
			}
			if dir := os.Getenv("WENDY_RISING_NEGATIVES"); dir != "" {
				raw, _ := json.Marshal(tasking.TriggerRef{SHA256: gasreplica.ComponentHash(bad), Component: bad})
				if e := os.WriteFile(filepath.Join(dir, "invalid-"+strings.ReplaceAll(name, " ", "-")+".json"), raw, 0600); e != nil {
					t.Fatal(e)
				}
			}
		})
	}
}

func TestRisingInspectionExtractsExactSignedPairMember(t *testing.T) {
	f := signedPairFixture(t)
	carrier, _ := f.pair(t, 2)
	records := []world.Component{carrier, f.records[1]}
	session, _ := NewWithPairVerifier(f.cfg, f.verify)
	result, err := session.Step(records, f.now)
	if err != nil || result.State != "supported_rise" {
		t.Fatal(result.State, err)
	}
	_, err = InspectionSelection(f.cfg, "east", world.Version{Epoch: 2, Sequence: 1}, result, records, f.now.Add(time.Millisecond), f.verify)
	if err != nil {
		t.Fatal(err)
	}
}

func TestRisingHistoricalOriginalTransitAgeIdentity(t *testing.T) {
	cfg, records := joinedFixture(t, "rising")
	session, _ := New(cfg)
	now := cfg.Spatial.Gas.Instant(2.15)
	result, _ := session.Step(records, now)
	c, err := InspectionSelection(cfg, "east", world.Version{Epoch: 2, Sequence: 1}, result, records, now, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Exact nonzero transit ages from the retained live v5 originals. They are
	// unsigned transport accounting, excluded from ComponentHash, not new captures.
	ages := []time.Duration{4396916, 32466209, 11421667}
	aged := rewriteRisingClaim(t, own(c), func(p *missioncontrol.RisingClaim) {
		for i := range p.Inputs {
			p.Inputs[i].Timing.AgeHint = ages[i%3]
		}
		window, e := gaschange.Evaluate(cfg.Temporal, p.Inputs, cfg.Spatial.Gas.Instant(p.Window.Evaluated))
		if e != nil {
			t.Fatal(e)
		}
		rawWindow, _ := json.Marshal(window)
		if e := json.Unmarshal(rawWindow, &p.Window); e != nil {
			t.Fatal(e)
		}
	})
	var selection missioncontrol.RisingSelection
	json.Unmarshal(aged.Payload, &selection)
	var temporal world.Component
	json.Unmarshal(selection.TemporalJSON, &temporal)
	if _, err = gaschange.ValidateClaim(cfg.Temporal, temporal, now); err != nil {
		t.Fatal("existing domain validator rejected live transit shape", err)
	}
	_, _, retained, err := missioncontrol.DecodeRisingSelection(aged)
	if err != nil {
		t.Fatal(err)
	}
	for i, input := range retained.Inputs {
		if input.Timing.AgeHint != ages[i%3] {
			t.Fatal("original transit metadata sanitized")
		}
	}
	if dir := os.Getenv("WENDY_RISING_NEGATIVES"); dir != "" {
		raw, _ := json.Marshal(tasking.TriggerRef{SHA256: gasreplica.ComponentHash(aged), Component: aged})
		if err = os.WriteFile(filepath.Join(dir, "trigger-transit-age-fixture.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"negative-age", "exhausted-age", "other-timing-field"} {
		bad := rewriteRisingClaim(t, own(aged), func(p *missioncontrol.RisingClaim) {
			switch name {
			case "negative-age":
				p.Inputs[0].Timing.AgeHint = -1
			case "exhausted-age":
				p.Inputs[0].Timing.AgeHint = p.Inputs[0].TTL
			case "other-timing-field":
				p.Inputs[0].Timing.ErrorBound = time.Nanosecond
			}
		})
		if _, err := missioncontrol.RisingTriggerTarget(bad); err == nil {
			t.Fatal(name, "accepted")
		}
		if dir := os.Getenv("WENDY_RISING_NEGATIVES"); dir != "" {
			raw, _ := json.Marshal(tasking.TriggerRef{SHA256: gasreplica.ComponentHash(bad), Component: bad})
			if err = os.WriteFile(filepath.Join(dir, "invalid-"+name+".json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
	outer := own(aged)
	outer.Timing.AgeHint = 1
	if _, err := missioncontrol.RisingTriggerTarget(outer); err == nil {
		t.Fatal("outer transit age relaxed")
	}
}
