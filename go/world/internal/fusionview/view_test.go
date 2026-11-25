package fusionview

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

var anchor = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func config() Config {
	return Config{Tenant: "simulation", Site: "studio-01", RunID: "test", ClockDomain: "simulation:test", Frame: world.FrameRef{ID: "floor", Revision: "1"}, Gateways: map[string]world.Source{"a": {Device: "a", App: "fusion", Service: "hazard", Stream: "claims"}, "b": {Device: "b", App: "fusion", Service: "hazard", Stream: "claims"}, "c": {Device: "c", App: "fusion", Service: "hazard", Stream: "claims"}}, Hold: time.Second, Improvement: .2}
}
func claim(id string, seq uint64, at time.Duration, u, x float64) Report {
	cfg := config()
	now := anchor.Add(at)
	until := now.Add(2 * time.Second)
	h := fusion.Hazard{Label: "possible-gas-hazard", Confidence: .8, ConfidenceMeaning: "synthetic rule score", PositionM: [3]float64{x, 0, 0}, UncertaintyM: u, ValidUntil: until, InputDigest: fmt.Sprintf("%064x", int64(at)+1)}
	inputs := []world.Input{}
	for _, sensor := range []string{"camera-a", "camera-b", "gas-1", "audio-1"} {
		source := world.Source{Device: "sensors", App: "capture", Service: sensor, Stream: "samples"}
		b, _ := json.Marshal(source)
		hash := sha256.Sum256(b)
		h.Inputs = append(h.Inputs, fusion.InputRef{SampleID: fmt.Sprintf("test/%s/%d", sensor, seq), SensorID: sensor, Source: source, Version: world.Version{Epoch: 1, Sequence: seq}, SampleTime: now, CaptureStart: now, ValidUntil: until})
		inputs = append(inputs, world.Input{ID: fmt.Sprintf("%x/1/test/%s/%d", hash, sensor, seq), Origin: world.Simulated, Simulation: &world.SimulationContext{RunID: "test", ClockDomain: "simulation:test", ScenarioVersion: "test", Seed: 1}})
	}
	payload, _ := json.Marshal(h)
	c := world.Component{Key: world.Key{Tenant: cfg.Tenant, Site: cfg.Site, Entity: "test/possible-gas-hazard", Type: fusion.HazardType, Source: cfg.Gateways[id]}, Version: world.Version{Epoch: 1, Sequence: seq}, Frame: cfg.Frame, Timing: world.Timing{SampleTime: now, ClockDomain: cfg.ClockDomain, ErrorKnown: true}, TTL: 2 * time.Second, Provenance: world.Provenance{Origin: world.Inferred, AppVersion: fusion.AppVersion, ModelID: fusion.ModelID, ModelVersion: fusion.ModelVersion, Inputs: inputs}, Payload: payload}
	return Report{Gateway: id, Version: c.Version, EvaluatedAt: now, State: fusion.HazardDetected, Claim: &c}
}
func selector(t *testing.T) *Selector {
	t.Helper()
	s, e := New(config())
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func update(t *testing.T, s *Selector, at time.Duration, rs ...Report) View {
	t.Helper()
	v, e := s.Update(anchor.Add(at), rs)
	if e != nil {
		t.Fatal(e)
	}
	return v
}
func TestHysteresisRequiresAdvancingEvidenceAndPreservesClaim(t *testing.T) {
	s := selector(t)
	a := claim("a", 1, 0, 1, 0)
	v := update(t, s, 0, a)
	if v.Selected != "a" {
		t.Fatal(v)
	}
	b := claim("b", 1, 100*time.Millisecond, .5, 0)
	v = update(t, s, 100*time.Millisecond, b)
	if v.Selected != "a" || v.Challenger != "b" {
		t.Fatal(v)
	}
	v = update(t, s, 1500*time.Millisecond, b)
	if v.Selected != "a" || v.HoldProgress != 0 {
		t.Fatal("duplicate completed hold", v)
	}
	b = claim("b", 2, 1600*time.Millisecond, .5, 0)
	v = update(t, s, 1600*time.Millisecond, b)
	if v.Selected != "b" || !reflect.DeepEqual(v.Claim, b.Claim) {
		t.Fatal("did not select intact claim", v)
	}
	v.Claim.Payload[0] = '!'
	v = update(t, s, 1700*time.Millisecond)
	if v.Claim.Payload[0] != '{' {
		t.Fatal("caller mutated retained claim")
	}
}
func TestExpiryFailoverPartitionRejoinAndDuplicates(t *testing.T) {
	s := selector(t)
	a := claim("a", 1, 0, .4, 0)
	b := claim("b", 1, time.Second, .6, 0)
	update(t, s, 0, a)
	v := update(t, s, time.Second, b)
	if v.Selected != "a" {
		t.Fatal(v)
	}
	v = update(t, s, 2*time.Second)
	if v.Selected != "b" {
		t.Fatal("expiry must bypass hold", v)
	}
	v = update(t, s, 3*time.Second, a, b)
	if v.State != "unavailable" || v.Claim != nil {
		t.Fatal("duplicate resurrected expired evidence", v)
	}
	v = update(t, s, 4*time.Second, claim("a", 2, 4*time.Second, .4, 0))
	if v.Selected != "a" {
		t.Fatal(v)
	}
	v = update(t, s, 4100*time.Millisecond, a)
	if v.Selected != "a" {
		t.Fatal("old delivery replaced new", v)
	}
}
func TestQualityFluctuationAndDeliveryGapResetHold(t *testing.T) {
	s := selector(t)
	update(t, s, 0, claim("a", 1, 0, 1, 0))
	update(t, s, 100*time.Millisecond, claim("b", 1, 100*time.Millisecond, .5, 0))
	v := update(t, s, 500*time.Millisecond, claim("b", 2, 500*time.Millisecond, .9, 0))
	if v.Challenger != "" {
		t.Fatal("small advantage continued hold")
	}
	update(t, s, time.Second, claim("b", 3, time.Second, .5, 0))
	v = update(t, s, 1200*time.Millisecond, claim("b", 4, 1200*time.Millisecond, .5, 0))
	if v.Selected != "a" || v.HoldProgress != .2 {
		t.Fatal(v)
	}
	// No polls during the gap: the incumbent is refreshed, but challenger must
	// start a new hold because its previous evidence expired before delivery.
	v = update(t, s, 3500*time.Millisecond, claim("a", 2, 3500*time.Millisecond, 1, 0), claim("b", 5, 3500*time.Millisecond, .5, 0))
	if v.Selected != "a" || v.HoldProgress != 0 {
		t.Fatal("delivery gap counted as support", v)
	}
}
func TestConflictingLocationsWithdrawUnifiedLocation(t *testing.T) {
	s := selector(t)
	update(t, s, 0, claim("a", 1, 0, .2, 0))
	v := update(t, s, time.Millisecond, claim("b", 1, time.Millisecond, .2, 2))
	if v.State != "disputed" || v.Selected != "" || v.Claim != nil {
		t.Fatal(v)
	}
	v = update(t, s, 100*time.Millisecond, Report{Gateway: "b", Version: world.Version{Epoch: 1, Sequence: 2}, EvaluatedAt: anchor.Add(100 * time.Millisecond), State: fusion.InsufficientEvidence})
	if v.Selected != "a" {
		t.Fatal(v)
	}
}
func TestEqualEvidenceDoesNotIncreaseConfidenceOrVoteCount(t *testing.T) {
	s := selector(t)
	a := claim("a", 1, 0, .5, 0)
	b := claim("b", 1, 0, .5, 0)
	v := update(t, s, 0, b, a)
	if v.Selected != "a" || v.UniqueEvidence != 1 {
		t.Fatal(v)
	}
	var h fusion.Hazard
	json.Unmarshal(v.Claim.Payload, &h)
	if h.Confidence != .8 {
		t.Fatal("recursive confidence")
	}
}
func TestBatchAtomicityScopeClockAndAncestry(t *testing.T) {
	mutations := map[string]func(*Report){
		"scope": func(r *Report) { r.Claim.Key.Site = "other" }, "publisher": func(r *Report) { r.Claim.Key.Source.Device = "other" }, "clock": func(r *Report) { r.Claim.Timing.ClockDomain = "physical" }, "unknown error": func(r *Report) { r.Claim.Timing.ErrorKnown = false }, "ancestry": func(r *Report) { r.Claim.Provenance.Inputs[0].ID = "unrelated" }, "extended ttl": func(r *Report) { r.Claim.TTL += time.Second }, "recursive": func(r *Report) { r.Claim.Provenance.Inputs[0].Origin = world.Inferred }, "future": func(r *Report) { r.EvaluatedAt = anchor.Add(time.Second) },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			s := selector(t)
			r := claim("b", 1, 0, .5, 0)
			mutate(&r)
			if _, e := s.Update(anchor, []Report{claim("a", 1, 0, 1, 0), r}); e == nil {
				t.Fatal("accepted")
			}
			if v := update(t, s, 0); v.State != "unavailable" {
				t.Fatal("partial batch committed")
			}
		})
	}
	s := selector(t)
	update(t, s, time.Second)
	if _, e := s.Update(anchor, nil); e == nil {
		t.Fatal("clock regression accepted")
	}
}
func TestConflictingVersionAndSameDigestMutation(t *testing.T) {
	s := selector(t)
	update(t, s, 0, claim("a", 1, 0, 1, 0))
	for _, seq := range []uint64{1, 2} {
		r := claim("a", seq, 0, .5, 0)
		if _, e := s.Update(anchor, []Report{r}); e == nil {
			t.Fatal("changed same evidence accepted")
		}
	}
}
func TestConcurrentReadsAndWrites(t *testing.T) {
	s := selector(t)
	r := claim("a", 1, 0, .5, 0)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, e := s.Update(anchor, []Report{r}); e != nil {
					t.Error(e)
				}
			}
		}()
	}
	wg.Wait()
}
