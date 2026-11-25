// Package gaschange detects commissioned, synthetic multi-sensor changes.
// It never estimates a leak source, exposure limit, clearance or motion authority.
package gaschange

import (
	"encoding/json"
	"errors"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const Model = "bounded-gas-change-v1"
const ClaimType = "wendy.gas.change.v1"
const MaxSamples = 3
const MaxHistory = 8

type Config struct {
	Public  gasreplica.Config `json:"public"`
	Sensors []string          `json:"sensors"`
	MinSpan float64           `json:"min_span_s"`
	MaxGap  float64           `json:"max_gap_s"`
	Rate    float64           `json:"rate_ppm_s"`
}

func (c Config) Validate() error {
	if e := c.Public.Validate(); e != nil {
		return e
	}
	if len(c.Sensors) != 4 || !finite(c.MinSpan) || !finite(c.MaxGap) || !finite(c.Rate) || c.MinSpan < .2 || c.MinSpan > 2 || c.MaxGap < .2 || c.MaxGap > 1.5 || c.MinSpan > 2*c.MaxGap || c.Rate < .1 || c.Rate > 1000 {
		return errors.New("exact four-sensor catalog and bounded commissioning thresholds required")
	}
	catalog := map[string]bool{}
	for _, s := range c.Public.Field.Sensors {
		catalog[s.ID] = true
	}
	seen := map[string]bool{}
	for _, id := range c.Sensors {
		if !catalog[id] || seen[id] {
			return errors.New("commissioned sensor differs or repeats")
		}
		seen[id] = true
	}
	return nil
}
func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func clone[T any](v T) T    { b, _ := json.Marshal(v); var out T; _ = json.Unmarshal(b, &out); return out }

type Point struct {
	Captured    float64 `json:"captured_s"`
	Value       float64 `json:"value_ppm"`
	Uncertainty float64 `json:"uncertainty_ppm"`
	Hash        string  `json:"sha256"`
	ValidUntil  float64 `json:"valid_until_s"`
}
type Trace struct {
	Sensor  string  `json:"sensor"`
	State   string  `json:"state"`
	Points  []Point `json:"points"`
	MinRate float64 `json:"min_rate_ppm_s"`
	MaxRate float64 `json:"max_rate_ppm_s"`
}
type Window struct {
	State       string  `json:"state"`
	Reason      string  `json:"reason"`
	Evaluated   float64 `json:"evaluated_s"`
	ValidUntil  float64 `json:"valid_until_s"`
	Traces      []Trace `json:"traces"`
	InputDigest string  `json:"input_digest"`
}
type Event struct {
	ID            string  `json:"id"`
	State         string  `json:"state"`
	At            float64 `json:"at_s"`
	Witness       string  `json:"witness_sha256"`
	OriginalUntil float64 `json:"original_valid_until_s"`
}
type Result struct {
	Window   Window            `json:"window"`
	History  []Event           `json:"history"`
	Rejected []string          `json:"rejected"`
	Inputs   []world.Component `json:"-"`
}
type retained struct {
	component world.Component
	until     time.Time
	capture   float64
}
type mark struct {
	version world.Version
	capture float64
	hash    string
}
type Session struct {
	mu                             sync.Mutex
	cfg                            Config
	now                            time.Time
	history                        map[string][]retained
	marks                          map[string]mark
	events                         []Event
	lastState, lastDigest, episode string
	lastUntil                      float64
}

func New(cfg Config) (*Session, error) {
	if e := cfg.Validate(); e != nil {
		return nil, e
	}
	return &Session{cfg: clone(cfg), history: map[string][]retained{}, marks: map[string]mark{}, events: []Event{}}, nil
}
func newer(a, b world.Version) bool {
	return a.Epoch > b.Epoch || a.Epoch == b.Epoch && a.Sequence > b.Sequence
}

// Step consumes a latest-value snapshot. Only genuinely newer original captures
// extend a sequence; missing intermediate versions are not reconstructed.
func (s *Session) Step(components []world.Component, now time.Time) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(components) > 256 || now.Before(s.cfg.Public.Anchor) || !s.now.IsZero() && now.Before(s.now) {
		return Result{}, errors.New("bounded snapshot and monotonic evaluation clock required")
	}
	s.now = now
	rejected := []string{}
	wanted := map[string]bool{}
	for _, id := range s.cfg.Sensors {
		wanted[id] = true
	}
	batch := map[string]world.Component{}
	for _, c := range components {
		if c.Key.Type != gasreplica.ObservationType {
			continue
		}
		o, e := gasreplica.DecodeObservation(s.cfg.Public, c, now)
		if e != nil {
			rejected = append(rejected, c.Key.Entity+": "+e.Error())
			// A known original superseded by invalid calibration/run data or
			// conflicting bytes is no longer usable. Unrelated keys and old
			// reordered packets cannot erase a commissioned source history.
			for _, sensor := range s.cfg.Public.Field.Sensors {
				if !wanted[sensor.ID] || c.Key.Source != sensor.Source || c.Key.Tenant != s.cfg.Public.Tenant || c.Key.Site != s.cfg.Public.Field.Site || c.Key.Entity != s.cfg.Public.Field.Simulation.RunID+"/sensor/"+sensor.ID {
					continue
				}
				m, seen := s.marks[sensor.ID]
				hash := gasreplica.ComponentHash(c)
				if seen && (newer(c.Version, m.version) || c.Version == m.version && hash != m.hash) {
					s.history[sensor.ID] = nil
					s.marks[sensor.ID] = mark{c.Version, m.capture, hash}
				}
			}
			continue
		}
		if !wanted[o.Sensor] {
			continue
		}
		if _, exists := batch[o.Sensor]; exists {
			return Result{}, errors.New("snapshot repeats a commissioned sensor")
		}
		batch[o.Sensor] = clone(c)
	}
	for _, id := range s.cfg.Sensors {
		h := s.history[id]
		live := h[:0]
		for _, x := range h {
			if now.Before(x.until) {
				live = append(live, x)
			}
		}
		h = live
		if c, ok := batch[id]; ok {
			o, _ := gasreplica.DecodeObservation(s.cfg.Public, c, now)
			hash := gasreplica.ComponentHash(c)
			m, seen := s.marks[id]
			until := now.Add(gasreplica.Remaining(c, now))
			if seen && c.Version == m.version {
				if hash != m.hash {
					rejected = append(rejected, id+": conflicting original version")
					h = nil
				} else {
					for i := range h {
						if gasreplica.ComponentHash(h[i].component) == hash && until.Before(h[i].until) {
							h[i].until = until
						}
					}
				}
			} else if !seen || newer(c.Version, m.version) && o.Captured > m.capture {
				if len(h) > 0 && o.Captured-h[len(h)-1].capture > s.cfg.MaxGap {
					h = nil
				}
				h = append(h, retained{c, until, o.Captured})
				if len(h) > MaxSamples {
					h = h[len(h)-MaxSamples:]
				}
				s.marks[id] = mark{c.Version, o.Captured, hash}
			} else {
				rejected = append(rejected, id+": reordered original ignored")
			}
		}
		// Delivery staleness makes Evaluate unknown, but does not erase
		// unexpired originals. A delayed genuinely newer capture can prove a
		// valid capture interval. Actual capture gaps reset above; duplicates
		// cannot change the latest original timestamp or restore freshness.
		s.history[id] = h
	}
	inputs := []world.Component{}
	limit := math.Inf(1)
	for _, id := range s.cfg.Sensors {
		for _, x := range s.history[id] {
			inputs = append(inputs, clone(x.component))
			limit = min(limit, s.cfg.Public.Seconds(x.until))
		}
	}
	w, e := Evaluate(s.cfg, inputs, now)
	if e != nil {
		return Result{}, e
	}
	if finite(limit) && w.ValidUntil > limit {
		w.ValidUntil = limit
	}
	// History is explicitly local and historical. Silence interrupts an episode;
	// it cannot resolve one or establish a safe concentration.
	if w.State != s.lastState && (w.InputDigest != s.lastDigest || w.State == "unknown" || w.State == "warming") {
		state := w.State
		if state == "rising" {
			if s.episode == "" {
				s.episode = gasreplica.Digest([]string{s.cfg.Public.ContractDigest(), w.InputDigest})
			}
		}
		if s.episode != "" {
			if state == "unknown" || state == "warming" || state == "mixed" {
				state = "interrupted"
			}
			at := w.Evaluated
			if state != "interrupted" {
				at = 0
				for _, tr := range w.Traces {
					for _, p := range tr.Points {
						at = max(at, p.Captured)
					}
				}
			}
			witness, until := w.InputDigest, w.ValidUntil
			if state == "interrupted" {
				witness, until = s.lastDigest, s.lastUntil
			}
			s.events = append(s.events, Event{s.episode, state, at, witness, until})
			if len(s.events) > MaxHistory {
				s.events = s.events[len(s.events)-MaxHistory:]
			}
			if state != "rising" {
				s.episode = ""
			}
		}
	}
	s.lastState, s.lastDigest = w.State, w.InputDigest
	s.lastUntil = w.ValidUntil
	return clone(Result{Window: w, History: s.events, Rejected: rejected, Inputs: inputs}).withInputs(inputs), nil
}

// Inputs are never serialized into operator history; claim creation owns copies.
func (r Result) withInputs(x []world.Component) Result { r.Inputs = clone(x); return r }

// Evaluate reproduces one bounded window from originals, independent of reader
// arrival order. It does not reconstruct past episodes from a latest snapshot.
func Evaluate(cfg Config, inputs []world.Component, now time.Time) (Window, error) {
	if e := cfg.Validate(); e != nil {
		return Window{}, e
	}
	if len(inputs) > 4*MaxSamples || now.Before(cfg.Public.Anchor) {
		return Window{}, errors.New("window exceeds original-history bound")
	}
	w := Window{State: "warming", Reason: "Three distinct original captures spanning the commissioned window are required for every sensor.", Evaluated: cfg.Public.Seconds(now), Traces: []Trace{}, ValidUntil: cfg.Public.Seconds(now)}
	by := map[string][]Point{}
	versions := map[string]world.Version{}
	identities := map[struct {
		sensor  string
		version world.Version
	}]bool{}
	hashes := []string{}
	seen := map[string]bool{}
	wanted := map[string]bool{}
	for _, id := range cfg.Sensors {
		wanted[id] = true
	}
	for _, c := range inputs {
		o, e := gasreplica.DecodeObservation(cfg.Public, c, now)
		if e != nil {
			return w, e
		}
		if !wanted[o.Sensor] {
			return w, errors.New("uncommissioned history sensor")
		}
		hash := gasreplica.ComponentHash(c)
		identity := struct {
			sensor  string
			version world.Version
		}{o.Sensor, c.Version}
		if identities[identity] {
			return w, errors.New("one original version cannot supply multiple captures")
		}
		identities[identity] = true
		versions[hash] = c.Version
		if seen[hash] {
			return w, errors.New("duplicate original history")
		}
		seen[hash] = true
		by[o.Sensor] = append(by[o.Sensor], Point{o.Captured, o.Value, o.Uncertainty, hash, cfg.Public.Seconds(now) + gasreplica.Remaining(c, now).Seconds()})
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	w.InputDigest = gasreplica.Digest(hashes)
	until := math.Inf(1)
	counts := map[string]int{}
	missing := false
	warming := false
	ids := append([]string{}, cfg.Sensors...)
	sort.Strings(ids)
	for _, id := range ids {
		points := by[id]
		sort.Slice(points, func(i, j int) bool { return points[i].Captured < points[j].Captured })
		if len(points) > MaxSamples {
			return w, errors.New("per-sensor history exceeds bound")
		}
		tr := Trace{Sensor: id, State: "warming", Points: points}
		if tr.Points == nil {
			tr.Points = []Point{}
		}
		if len(points) == 0 {
			tr.State = "unknown"
			missing = true
		} else {
			for _, p := range points {
				until = min(until, p.ValidUntil)
			}
			until = min(until, points[len(points)-1].Captured+cfg.MaxGap)
			valid := len(points) >= 3 && points[len(points)-1].Captured-points[0].Captured >= cfg.MinSpan
			lo, hi := math.Inf(1), math.Inf(-1)
			allRise, allFall, allSteady := true, true, true
			for i := 1; i < len(points); i++ {
				a, b := points[i-1], points[i]
				dt := b.Captured - a.Captured
				if !newer(versions[b.Hash], versions[a.Hash]) {
					return w, errors.New("original version order differs from capture order")
				}
				if dt <= 0 {
					return w, errors.New("capture times must be distinct")
				}
				if dt > cfg.MaxGap {
					valid = false
				}
				l := (b.Value - b.Uncertainty - a.Value - a.Uncertainty) / dt
				h := (b.Value + b.Uncertainty - a.Value + a.Uncertainty) / dt
				lo = min(lo, l)
				hi = max(hi, h)
				allRise = allRise && l >= cfg.Rate
				allFall = allFall && h <= -cfg.Rate
				allSteady = allSteady && l < cfg.Rate && h > -cfg.Rate
			}
			if len(points) > 1 {
				tr.MinRate = lo
				tr.MaxRate = hi
			}
			if cfg.Public.Seconds(now) > points[len(points)-1].Captured+cfg.MaxGap {
				tr.State = "unknown"
				missing = true
			} else if valid {
				switch {
				case allRise:
					tr.State = "rising"
				case allFall:
					tr.State = "falling"
				case allSteady:
					tr.State = "steady"
				default:
					tr.State = "mixed"
				}
			} else {
				warming = true
			}
		}
		counts[tr.State]++
		w.Traces = append(w.Traces, tr)
	}
	if missing {
		w.State = "unknown"
		w.Reason = "A commissioned sensor is missing or its sample gap expired; silence does not resolve a change."
	} else if !warming {
		switch {
		case counts["rising"] >= 3 && counts["falling"] == 0 && counts["mixed"] == 0:
			w.State = "rising"
		case counts["falling"] >= 3 && counts["rising"] == 0 && counts["mixed"] == 0:
			w.State = "falling"
		case counts["steady"] == 4:
			w.State = "steady"
		default:
			w.State = "mixed"
		}
		w.Reason = "Conservative per-interval changes across the commissioned sensor catalog; this is not a safety or leak-cause determination."
	}
	if finite(until) && until > w.Evaluated {
		w.ValidUntil = until
	}
	return w, nil
}
