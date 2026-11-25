// Package fusionview selects one existing hazard claim. It never republishes or
// recursively combines inferred confidence. Its caller authenticates publishers.
package fusionview

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type Config struct {
	Tenant      string                  `json:"tenant"`
	Site        string                  `json:"site"`
	RunID       string                  `json:"run_id"`
	ClockDomain string                  `json:"clock_domain"`
	Frame       world.FrameRef          `json:"frame"`
	Gateways    map[string]world.Source `json:"gateways"`
	Hold        time.Duration           `json:"hold_ns"`
	Improvement float64                 `json:"improvement_fraction"`
}

// Report is a gateway's ordered evaluation, including explicit negative results.
// Absence of delivery is not a negative result. Sequence is allocated by the
// adapter within its fixed run and durable publisher epoch. A restarted
// consumer starts with no claims or recovered monotonic lifetime.
type Report struct {
	Gateway     string           `json:"gateway"`
	Version     world.Version    `json:"version"`
	EvaluatedAt time.Time        `json:"evaluated_at"`
	State       string           `json:"state"`
	Claim       *world.Component `json:"claim,omitempty"`
}
type Candidate struct {
	Gateway      string     `json:"gateway"`
	State        string     `json:"state"`
	UncertaintyM *float64   `json:"uncertainty_m,omitempty"`
	ValidUntil   *time.Time `json:"valid_until,omitempty"`
	InputDigest  string     `json:"input_digest,omitempty"`
}
type Event struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"`
	From   string    `json:"from,omitempty"`
	To     string    `json:"to,omitempty"`
	Reason string    `json:"reason"`
}
type View struct {
	State          string           `json:"state"`
	Reason         string           `json:"reason"`
	Selected       string           `json:"selected,omitempty"`
	Claim          *world.Component `json:"claim,omitempty"`
	Candidates     []Candidate      `json:"candidates"`
	Challenger     string           `json:"challenger,omitempty"`
	HoldProgress   float64          `json:"hold_progress"`
	UniqueEvidence int              `json:"unique_evidence"`
	Events         []Event          `json:"events"`
}
type entry struct {
	report Report
	hash   [32]byte
	hazard fusion.Hazard
}
type Selector struct {
	mu                                            sync.Mutex
	cfg                                           Config
	entries                                       map[string]entry
	last                                          time.Time
	incumbent, challenger                         string
	challengeStart, challengeLast, challengeUntil time.Time
	events                                        []Event
	state                                         string
}

func New(cfg Config) (*Selector, error) {
	if cfg.Tenant == "" || cfg.Site == "" || cfg.RunID == "" || cfg.ClockDomain == "" || cfg.Frame.Validate() != nil || len(cfg.Gateways) < 1 || len(cfg.Gateways) > 16 || cfg.Hold < 100*time.Millisecond || cfg.Hold > 30*time.Second || !finite(cfg.Improvement) || cfg.Improvement <= 0 || cfg.Improvement >= 1 {
		return nil, errors.New("invalid selector configuration")
	}
	sources := map[world.Source]bool{}
	copyGateways := map[string]world.Source{}
	for id, source := range cfg.Gateways {
		if len(id) == 0 || len(id) > 80 || source.Validate() != nil || sources[source] {
			return nil, errors.New("invalid or duplicate gateway")
		}
		sources[source] = true
		copyGateways[id] = source
	}
	cfg.Gateways = copyGateways
	return &Selector{cfg: cfg, entries: map[string]entry{}}, nil
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func (s *Selector) validate(now time.Time, r Report) (entry, error) {
	source, ok := s.cfg.Gateways[r.Gateway]
	if !ok || (r.Version.Epoch == 0 || r.Version.Sequence == 0) || r.EvaluatedAt.IsZero() || r.EvaluatedAt.After(now) {
		return entry{}, errors.New("invalid report identity or time")
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > 1<<20 {
		return entry{}, errors.New("report exceeds bound")
	}
	// Own all memory; callers cannot mutate retained claims or returned views.
	var owned Report
	if err = json.Unmarshal(raw, &owned); err != nil {
		return entry{}, err
	}
	r = owned
	e := entry{report: r, hash: sha256.Sum256(raw)}
	if r.State != fusion.HazardDetected {
		if (r.State != fusion.NotDetected && r.State != fusion.InsufficientEvidence) || r.Claim != nil {
			return entry{}, errors.New("invalid negative report")
		}
		return e, nil
	}
	c := r.Claim
	if c == nil || c.Key != (world.Key{Tenant: s.cfg.Tenant, Site: s.cfg.Site, Entity: s.cfg.RunID + "/possible-gas-hazard", Type: fusion.HazardType, Source: source}) || c.Frame != s.cfg.Frame || c.Version != r.Version || c.Tombstone || c.Timing.Validate() != nil || !c.Timing.ErrorKnown || c.Timing.ErrorBound != 0 || c.Timing.AgeHint != 0 || c.Timing.ClockDomain != s.cfg.ClockDomain || c.Timing.SampleTime.After(r.EvaluatedAt) || c.TTL <= 0 || c.TTL > 30*time.Second || c.Provenance.Validate() != nil || c.Provenance.Origin != world.Inferred || c.Provenance.ModelID != fusion.ModelID || c.Provenance.ModelVersion != fusion.ModelVersion || c.Provenance.AppVersion != fusion.AppVersion {
		return entry{}, errors.New("claim differs from configured scope, publisher, model or clock")
	}
	if err = json.Unmarshal(c.Payload, &e.hazard); err != nil {
		return entry{}, err
	}
	h := e.hazard
	digest, err := hex.DecodeString(h.InputDigest)
	if err != nil || len(digest) != 32 || h.Label == "" || !finite(h.UncertaintyM) || h.UncertaintyM <= 0 || !finite(h.Confidence) || h.Confidence < 0 || h.Confidence > 1 || !h.ValidUntil.Equal(c.Timing.SampleTime.Add(c.TTL)) || len(h.Inputs) < 4 || len(h.Inputs) > 32 || len(h.Inputs) != len(c.Provenance.Inputs) {
		return entry{}, errors.New("invalid hazard or input ancestry")
	}
	for _, p := range h.PositionM {
		if !finite(p) {
			return entry{}, errors.New("nonfinite position")
		}
	}
	ancestry := map[string]world.Input{}
	var simulation *world.SimulationContext
	for _, in := range c.Provenance.Inputs {
		if in.Origin != world.Simulated || in.Simulation == nil || in.Simulation.RunID != s.cfg.RunID || in.Simulation.ClockDomain != s.cfg.ClockDomain {
			return entry{}, errors.New("input requires the configured simulation context")
		}
		if simulation != nil && *in.Simulation != *simulation {
			return entry{}, errors.New("input simulation scenarios or seeds differ")
		}
		simulation = in.Simulation
		ancestry[in.ID] = in
	}
	seen := map[string]bool{}
	oldest := r.EvaluatedAt
	for _, in := range h.Inputs {
		if seen[in.SampleID] || in.SampleID == "" || in.Source.Validate() != nil || in.SampleTime.After(r.EvaluatedAt) || in.SampleTime.IsZero() || h.ValidUntil.After(in.ValidUntil) || !in.ValidUntil.After(in.SampleTime) {
			return entry{}, errors.New("invalid or extended input lifetime")
		}
		sourceBytes, _ := json.Marshal(in.Source)
		sourceHash := sha256.Sum256(sourceBytes)
		ancestryID := fmt.Sprintf("%x/%d/%s/%d", sourceHash, in.Version.Epoch, s.cfg.RunID+"/"+in.SensorID, in.Version.Sequence)
		if ancestry[ancestryID].ID == "" || in.Replay != nil || in.Capture != nil {
			return entry{}, errors.New("unsupported or unbound input ancestry")
		}
		// Consume each leaf once. Equal slice lengths alone do not establish
		// coverage: multiple distinct sample IDs could otherwise reuse one
		// source/entity/version and leave unrelated leaves unreferenced.
		delete(ancestry, ancestryID)
		seen[in.SampleID] = true
		captureStart := in.SampleTime
		if in.Sample.Audio != nil {
			captureStart = captureStart.Add(-time.Duration(in.Sample.Audio.DurationNS))
		}
		if !in.CaptureStart.Equal(captureStart) {
			return entry{}, errors.New("input capture interval differs")
		}
		if captureStart.Before(oldest) {
			oldest = captureStart
		}
	}
	if len(ancestry) != 0 {
		return entry{}, errors.New("unreferenced input ancestry")
	}
	if !oldest.Equal(c.Timing.SampleTime) {
		return entry{}, errors.New("claim age differs from oldest input")
	}
	return e, nil
}

// Update atomically applies a bounded delivered batch and returns a selection.
// Calling with no reports advances expiry but cannot complete a quality hold.
func (s *Selector) Update(now time.Time, reports []Report) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if now.IsZero() || (!s.last.IsZero() && now.Before(s.last)) || len(reports) > 32 {
		return View{}, errors.New("invalid selector clock or batch")
	}
	next := make(map[string]entry, len(s.entries))
	for id, e := range s.entries {
		next[id] = e
	}
	for _, r := range reports {
		e, err := s.validate(now, r)
		if err != nil {
			return View{}, fmt.Errorf("gateway %q: %w", r.Gateway, err)
		}
		old, ok := next[r.Gateway]
		if ok {
			if r.Version.Compare(old.report.Version) < 0 {
				continue
			}
			if r.Version == old.report.Version {
				if old.hash != e.hash {
					return View{}, errors.New("same gateway sequence has conflicting content")
				}
				continue
			}
			if r.EvaluatedAt.Before(old.report.EvaluatedAt) {
				return View{}, errors.New("gateway evaluation time regressed")
			}
			if r.Claim != nil && old.report.Claim != nil && e.hazard.InputDigest == old.hazard.InputDigest && !bytes.Equal(r.Claim.Payload, old.report.Claim.Payload) {
				return View{}, errors.New("same evidence digest changed hazard")
			}
		}
		next[r.Gateway] = e
	}
	s.entries = next
	s.last = now
	ids := make([]string, 0, len(s.cfg.Gateways))
	for id := range s.cfg.Gateways {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	v := View{State: "unavailable", Reason: "No delivered, fresh hazard claim", Candidates: []Candidate{}, Events: []Event{}}
	live := map[string]entry{}
	unique := map[string]bool{}
	for _, id := range ids {
		c := Candidate{Gateway: id, State: "not_received"}
		if e, ok := s.entries[id]; ok {
			c.State = e.report.State
			if e.report.Claim != nil {
				h := e.hazard
				c.ValidUntil = &h.ValidUntil
				c.InputDigest = h.InputDigest
				if !now.Before(h.ValidUntil) {
					c.State = "expired"
				} else {
					u := h.UncertaintyM
					c.UncertaintyM = &u
					live[id] = e
					unique[h.InputDigest] = true
				}
			} else if now.Sub(e.report.EvaluatedAt) > 30*time.Second {
				c.State = "stale_report"
			}
		}
		v.Candidates = append(v.Candidates, c)
	}
	v.UniqueEvidence = len(unique)
	conflict := false
	for i, a := range ids {
		ea, ok := live[a]
		if !ok {
			continue
		}
		for _, b := range ids[i+1:] {
			eb, ok := live[b]
			if !ok {
				continue
			}
			d := 0.
			for k := range ea.hazard.PositionM {
				x := ea.hazard.PositionM[k] - eb.hazard.PositionM[k]
				d += x * x
			}
			if math.Sqrt(d) > ea.hazard.UncertaintyM+eb.hazard.UncertaintyM {
				conflict = true
			}
		}
	}
	if conflict {
		s.challenger = ""
		v.State = "disputed"
		v.Reason = "Fresh gateway uncertainty regions disagree; no unified location"
	} else if len(live) > 0 {
		best := ""
		for _, id := range ids {
			e, ok := live[id]
			if ok && (best == "" || e.hazard.UncertaintyM < live[best].hazard.UncertaintyM) {
				best = id
			}
		}
		if _, ok := live[s.incumbent]; !ok {
			s.switchTo(now, best, "Incumbent unavailable; selected lowest-uncertainty eligible alternative")
		} else if best != s.incumbent && live[best].hazard.UncertaintyM <= live[s.incumbent].hazard.UncertaintyM*(1-s.cfg.Improvement) {
			at := live[best].report.Claim.Timing.SampleTime
			if s.challenger != best || now.After(s.challengeUntil) {
				s.challenger = best
				s.challengeStart = at
				s.challengeLast = at
			} else if at.After(s.challengeLast) {
				s.challengeLast = at
			}
			s.challengeUntil = live[best].hazard.ValidUntil
			// Evidence must advance for the full hold, without a delivery gap longer
			// than its lifetime. A duplicate or polling timer never supplies support.
			if s.challengeLast.Sub(s.challengeStart) >= s.cfg.Hold {
				s.switchTo(now, best, "Sustained lower uncertainty on newer evidence")
			}
		} else {
			s.challenger = ""
		}
		v.State = "selected"
		v.Reason = "Retaining selected gateway until a sustained material improvement"
		v.Selected = s.incumbent
		v.Claim = live[s.incumbent].report.Claim
	} else {
		s.incumbent = ""
		s.challenger = ""
	}
	if v.State != s.state {
		s.event(Event{Time: now, Kind: v.State, To: v.Selected, Reason: v.Reason})
		s.state = v.State
	}
	v.Challenger = s.challenger
	if s.challenger != "" {
		v.HoldProgress = math.Min(1, float64(s.challengeLast.Sub(s.challengeStart))/float64(s.cfg.Hold))
	}
	v.Events = append(v.Events, s.events...)
	raw, _ := json.Marshal(v)
	var owned View
	_ = json.Unmarshal(raw, &owned)
	return owned, nil
}
func (s *Selector) switchTo(now time.Time, id, reason string) {
	old := s.incumbent
	s.incumbent = id
	s.challenger = ""
	s.event(Event{Time: now, Kind: "handoff", From: old, To: id, Reason: reason})
}
func (s *Selector) event(e Event) {
	s.events = append(s.events, e)
	if len(s.events) > 32 {
		s.events = append([]Event(nil), s.events[len(s.events)-32:]...)
	}
}
