// Package incidentchange joins intact spatial and temporal claims by exact
// original witnesses. It does not infer leak causality or combine confidence.
package incidentchange

import (
	"encoding/json"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/gaschange"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/incident"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const Type = "wendy.incident.change.v1"

type Config struct {
	Spatial  incident.Config  `json:"spatial"`
	Temporal gaschange.Config `json:"temporal"`
}

func (c Config) Validate() error {
	if e := c.Spatial.Validate(); e != nil {
		return e
	}
	if e := c.Temporal.Validate(); e != nil {
		return e
	}
	if gasreplica.Digest(c.Spatial.Gas) != gasreplica.Digest(c.Temporal.Public) {
		return errors.New("spatial and temporal commissioning must be identical")
	}
	return nil
}
func own[T any](v T) T { b, _ := json.Marshal(v); var out T; _ = json.Unmarshal(b, &out); return out }

type Witness struct {
	Sensor     string    `json:"sensor"`
	Hash       string    `json:"sha256"`
	Captured   time.Time `json:"captured_at"`
	ValidUntil time.Time `json:"valid_until"`
}
type Temporal struct {
	State      string            `json:"state"`
	Reason     string            `json:"reason"`
	Gateway    string            `json:"gateway,omitempty"`
	Hash       string            `json:"sha256,omitempty"`
	Window     *gaschange.Window `json:"window,omitempty"`
	ValidUntil time.Time         `json:"valid_until"`
}
type Result struct {
	Schema      string             `json:"schema"`
	Contract    string             `json:"contract_sha256"`
	State       string             `json:"state"`
	Reason      string             `json:"reason"`
	At          time.Time          `json:"at"`
	ValidUntil  time.Time          `json:"valid_until"`
	Spatial     incident.Result    `json:"spatial"`
	Temporal    Temporal           `json:"temporal"`
	Witnesses   []Witness          `json:"witnesses"`
	Supports    []incident.Support `json:"supports"`
	InputDigest string             `json:"input_digest"`
	Carriers    []PairSupport      `json:"carriers,omitempty"`
	frozen      string
	ancestry    []world.Input
}
type mark struct {
	version world.Version
	hash    string
	until   time.Time
	blocked bool
}
type Session struct {
	mu          sync.Mutex
	cfg         Config
	spatial     *incident.Selector
	seen        map[string]mark
	last        time.Time
	pairSeen    map[string]mark
	verifyPair  PairVerifier
	pairMembers map[string][2]string
}

func New(c Config) (*Session, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	c = own(c)
	s, e := incident.New(c.Spatial)
	return &Session{cfg: c, spatial: s, seen: map[string]mark{}, pairSeen: map[string]mark{}, pairMembers: map[string][2]string{}}, e
}
func (s *Session) Step(records []world.Component, now time.Time) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(records) > 256 || now.Before(s.last) || now.Before(s.cfg.Spatial.Gas.Anchor) {
		return Result{}, errors.New("bounded snapshot and monotonic evaluation required")
	}
	s.last = now
	records, carriers := s.expandPairs(records, now)
	spatial, e := s.spatial.Step(records, now)
	if e != nil {
		return Result{}, e
	}
	r := Result{Schema: Type, Contract: gasreplica.Digest(s.cfg), State: "unavailable", Reason: "Current spatial and temporal support required", At: now, Spatial: spatial, Temporal: Temporal{State: "unavailable", Reason: "No current matching temporal publisher"}, Witnesses: []Witness{}, Supports: []incident.Support{}}
	// Observe every commissioned temporal key, even when another gas gateway is
	// selected. Invalid newer versions and same-version conflicts latch closed.
	valid := map[string]world.Component{}
	claims := map[string]gaschange.Claim{}
	for name, source := range s.cfg.Spatial.Gas.Gateways {
		found := []world.Component{}
		for _, c := range records {
			if c.Key.Type == gaschange.ClaimType && c.Key.Source == source && c.Key.Tenant == s.cfg.Spatial.Gas.Tenant && c.Key.Site == s.cfg.Spatial.Gas.Field.Site && c.Key.Entity == s.cfg.Spatial.Gas.Field.Simulation.RunID+"/gas-change" {
				found = append(found, c)
			}
		}
		m := s.seen[name]
		sort.Slice(found, func(i, j int) bool { return found[i].Version.Compare(found[j].Version) < 0 })
		for _, c := range found {
			hash := gasreplica.ComponentHash(c)
			cmp := c.Version.Compare(m.version)
			if cmp < 0 {
				continue
			}
			p, err := gaschange.ValidateClaim(s.cfg.Temporal, c, now)
			if cmp == 0 && (m.blocked || m.hash != "" && m.hash != hash) {
				m.blocked = true
				m.until = now
				continue
			}
			if cmp > 0 {
				m = mark{version: c.Version, hash: hash}
			}
			if err != nil {
				m.blocked = true
				m.until = now
				continue
			}
			until := now.Add(gasreplica.Remaining(c, now))
			original := s.cfg.Spatial.Gas.Instant(p.Window.ValidUntil)
			if original.Before(until) {
				until = original
			}
			if m.until.IsZero() || until.Before(m.until) {
				m.until = until
			}
			if !m.blocked && now.Before(m.until) {
				valid[name] = c
				claims[name] = p
			}
		}
		s.seen[name] = m
		if m.blocked || !now.Before(m.until) {
			delete(valid, name)
			delete(claims, name)
		}
	}
	gateway := spatial.Gas.Gateway
	c, ok := valid[gateway]
	if carrier, exists := carriers[gateway]; exists && carrier.GasHash == spatial.Gas.Hash && ok && carrier.ChangeHash == gasreplica.ComponentHash(c) {
		r.Carriers = []PairSupport{carrier}
	}
	if ok {
		p := claims[gateway]
		r.Temporal = Temporal{State: p.Window.State, Reason: p.Window.Reason, Gateway: gateway, Hash: gasreplica.ComponentHash(c), Window: &p.Window, ValidUntil: s.seen[gateway].until}
	}
	// Independent spatial data remains available even when no join can be made.
	if spatial.State != "consistent" {
		r.Reason = "Spatial evidence is absent, ambiguous or conflicting; no rising incident"
		if spatial.State != "unavailable" {
			r.State = "unsupported"
		}
		return s.finish(r, nil), nil
	}
	if !ok {
		return s.finish(r, nil), nil
	}
	var gas world.Component
	for _, candidate := range records {
		if gasreplica.ComponentHash(candidate) == spatial.Gas.Hash {
			gas = candidate
			break
		}
	}
	var spatialClaim gasreplica.Claim
	if e := json.Unmarshal(gas.Payload, &spatialClaim); e != nil {
		return r, e
	}
	latest := map[string]world.Component{}
	for _, sample := range claims[gateway].Inputs {
		o, e := gasreplica.DecodeObservation(s.cfg.Spatial.Gas, sample, now)
		if e != nil {
			return r, e
		}
		old, exists := latest[o.Sensor]
		if !exists || sample.Timing.SampleTime.After(old.Timing.SampleTime) {
			latest[o.Sensor] = sample
		}
	}
	matched := map[string]world.Component{}
	for _, sample := range spatialClaim.Inputs {
		o, e := gasreplica.DecodeObservation(s.cfg.Spatial.Gas, sample, now)
		if e != nil {
			return r, e
		}
		matched[o.Sensor] = sample
	}
	for _, sensor := range s.cfg.Temporal.Sensors {
		a, aok := latest[sensor]
		b, bok := matched[sensor]
		if !aok || !bok || gasreplica.ComponentHash(a) != gasreplica.ComponentHash(b) {
			r.State = "unsupported"
			r.Reason = "Temporal window and spatial region do not share every latest commissioned original"
			return s.finish(r, nil), nil
		}
		r.Witnesses = append(r.Witnesses, Witness{sensor, gasreplica.ComponentHash(a), a.Timing.SampleTime, a.Timing.SampleTime.Add(a.TTL)})
	}
	sort.Slice(r.Witnesses, func(i, j int) bool { return r.Witnesses[i].Sensor < r.Witnesses[j].Sensor })
	r.ValidUntil = spatial.ValidUntil
	if r.Temporal.ValidUntil.Before(r.ValidUntil) {
		r.ValidUntil = r.Temporal.ValidUntil
	}
	if !now.Before(r.ValidUntil) {
		r.State = "unavailable"
		r.Reason = "Original joined support expired"
		return s.finish(r, nil), nil
	}
	r.State = "non_rising"
	r.Reason = "Exact originals match, but the temporal rule does not establish a sustained rise"
	if r.Temporal.State == "rising" {
		r.State = "supported_rise"
		r.Reason = "A sustained rise shares exact original samples with the spatial gas/visual association; no leak causality"
	}
	r.Supports = append(own(spatial.Supports), incident.Support{Kind: "change", Hash: r.Temporal.Hash, Source: c.Key.Source, Version: c.Version, Captured: c.Timing.SampleTime, ValidUntil: r.Temporal.ValidUntil})
	return s.finish(r, records), nil
}
func (s *Session) finish(r Result, records []world.Component) Result {
	ids := []string{r.Contract, r.State}
	for _, carrier := range r.Carriers {
		ids = append(ids, carrier.Hash)
	}
	for _, p := range r.Supports {
		ids = append(ids, p.Hash)
	}
	if len(r.Supports) == 0 {
		ids = append(ids, r.Spatial.InputDigest, r.Temporal.Hash)
	}
	r.InputDigest = gasreplica.Digest(ids)
	leaves := map[string]world.Input{}
	for _, p := range r.Supports {
		for _, c := range records {
			if gasreplica.ComponentHash(c) == p.Hash {
				for _, leaf := range c.Provenance.Inputs {
					leaves[leaf.ID] = own(leaf)
				}
			}
		}
	}
	keys := []string{}
	for k := range leaves {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		r.ancestry = append(r.ancestry, leaves[k])
	}
	r.frozen = gasreplica.Digest(r)
	return r
}

// Component is compact: independently validated original claims remain in their
// own records, rather than recursively expanding images and temporal histories.
func Component(cfg Config, reader string, version world.Version, r Result) (world.Component, error) {
	if e := cfg.Validate(); e != nil {
		return world.Component{}, e
	}
	source, ok := cfg.Spatial.Readers[reader]
	if !ok || version.Epoch == 0 || version.Sequence == 0 || r.Contract != gasreplica.Digest(cfg) || r.frozen == "" || r.frozen != gasreplica.Digest(r) || (r.State != "supported_rise" && r.State != "non_rising") || len(r.Witnesses) != 4 || len(r.Supports) != 3 || !r.At.Before(r.ValidUntil) {
		return world.Component{}, errors.New("intact evaluated join and commissioned publisher required")
	}
	p := struct {
		Schema      string             `json:"schema"`
		Contract    string             `json:"contract_sha256"`
		State       string             `json:"state"`
		Reason      string             `json:"reason"`
		Supports    []incident.Support `json:"supports"`
		Witnesses   []Witness          `json:"witnesses"`
		ValidUntil  time.Time          `json:"valid_until"`
		InputDigest string             `json:"input_digest"`
		Carriers    []PairSupport      `json:"carriers,omitempty"`
	}{Type, r.Contract, r.State, r.Reason, r.Supports, r.Witnesses, r.ValidUntil, r.InputDigest, r.Carriers}
	raw, e := json.Marshal(p)
	if e != nil || len(raw) > 16<<10 {
		return world.Component{}, errors.New("compact join exceeds 16 KiB payload bound")
	}
	oldest := r.At
	for _, p := range r.Supports {
		if p.Captured.Before(oldest) {
			oldest = p.Captured
		}
	}
	return world.Component{Key: world.Key{Tenant: cfg.Spatial.Gas.Tenant, Site: cfg.Spatial.Gas.Field.Site, Entity: cfg.Spatial.Gas.Field.Simulation.RunID + "/rising-incident", Type: Type, Source: source}, Version: version, Frame: cfg.Spatial.Visual.Frame, Timing: world.Timing{SampleTime: oldest, ClockDomain: cfg.Spatial.Visual.Simulation.ClockDomain, ErrorKnown: true}, TTL: r.ValidUntil.Sub(oldest), Provenance: world.Provenance{Origin: world.Inferred, AppVersion: "world-incident-change-v1", ModelID: "exact-original-witness-join", ModelVersion: "1", Inputs: own(r.ancestry)}, Payload: raw}, nil
}
