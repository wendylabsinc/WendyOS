// Package incident associates independently retained visual and gas evidence.
// Spatial consistency is not a gas detection or a causal conclusion.
package incident

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/fusion"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/gasview"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"math"
	"sort"
	"sync"
	"time"
)

const VisualType = "wendy.visual.fixture.v1"
const Type = "wendy.incident.association.v1"
const MaxVisualBytes = 44 << 10

type Config struct {
	Gas             gasreplica.Config       `json:"gas"`
	Visual          fusion.VisualConfig     `json:"visual"`
	VisualPublisher world.Source            `json:"visual_publisher"`
	Readers         map[string]world.Source `json:"readers"`
}

func (c Config) Validate() error {
	if e := c.Gas.Validate(); e != nil {
		return e
	}
	if e := c.Visual.Validate(); e != nil {
		return e
	}
	if c.Gas.Tenant != c.Visual.Tenant || c.Gas.Field.Site != c.Visual.Site || c.Gas.Field.Frame != c.Visual.Frame || c.Gas.Field.Simulation != c.Visual.Simulation || !c.Gas.Anchor.Equal(c.Visual.Anchor) || c.VisualPublisher.Validate() != nil || len(c.Readers) != 2 {
		return errors.New("gas and visual contracts require identical site/frame/run/clock/anchor")
	}
	seen := map[world.Source]bool{}
	for _, s := range c.Gas.Field.Sensors {
		seen[s.Source] = true
	}
	for _, s := range c.Visual.Sources {
		if seen[s] {
			return errors.New("visual and gas sensor identities overlap")
		}
		seen[s] = true
	}
	for _, s := range c.Gas.Gateways {
		if seen[s] {
			return errors.New("publisher overlaps sensor")
		}
		seen[s] = true
	}
	if seen[c.VisualPublisher] {
		return errors.New("visual publisher overlaps another source")
	}
	seen[c.VisualPublisher] = true
	for id, s := range c.Readers {
		if id == "" || len(id) > 64 || s.Validate() != nil || seen[s] {
			return errors.New("distinct association publisher catalog required")
		}
		seen[s] = true
	}
	return nil
}
func (c Config) Digest() string { return gasreplica.Digest(c) }

type VisualClaim struct {
	Schema    string              `json:"schema"`
	Contract  string              `json:"contract_sha256"`
	Evaluated time.Time           `json:"evaluated_at"`
	Result    fusion.VisualResult `json:"result"`
	Images    map[string][]byte   `json:"images"`
}
type Visual struct {
	State     string        `json:"state"`
	Reason    string        `json:"reason"`
	Hash      string        `json:"sha256,omitempty"`
	Version   world.Version `json:"version"`
	Remaining float64       `json:"remaining_s"`
	Claim     *VisualClaim  `json:"claim,omitempty"`
}

func imageLoader(images map[string][]byte) fusion.Loader {
	return func(ctx context.Context, e sensing.EvidenceRef) ([]byte, error) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		b, ok := images[e.SHA256]
		if !ok {
			return nil, errors.New("missing image evidence")
		}
		return b, nil
	}
}
func MakeVisual(cfg Config, version world.Version, records []world.Component, images map[string][]byte, now time.Time) (world.Component, error) {
	if e := cfg.Validate(); e != nil {
		return world.Component{}, e
	}
	if version.Epoch == 0 || version.Sequence == 0 || len(images) != 2 {
		return world.Component{}, errors.New("two original images and allocated version required")
	}
	size := 0
	for _, b := range images {
		size += len(b)
	}
	if size > 24<<10 {
		return world.Component{}, errors.New("inline image evidence exceeds bound")
	}
	result, e := fusion.EvaluateVisual(context.Background(), cfg.Visual, records, imageLoader(images), func() time.Time { return now })
	if e != nil {
		return world.Component{}, e
	}
	if !result.Spatial.Associated {
		return world.Component{}, errors.New(result.Spatial.Reason)
	}
	used := map[string]bool{}
	ancestry := []world.Input{}
	for _, c := range result.Inputs {
		s, _ := sensing.FromComponent(c)
		if s.Evidence != nil {
			used[s.Evidence.SHA256] = true
		}
		sim := *c.Provenance.Simulation
		ancestry = append(ancestry, world.Input{ID: "sha256:" + gasreplica.ComponentHash(c), Origin: world.Simulated, Simulation: &sim})
	}
	if len(used) != len(images) {
		return world.Component{}, errors.New("unused visual evidence")
	}
	p := VisualClaim{VisualType, cfg.Digest(), now, result, images}
	raw, e := json.Marshal(p)
	if e != nil || len(raw) > MaxVisualBytes {
		return world.Component{}, errors.New("visual claim exceeds bound")
	}
	return world.Component{Key: world.Key{Tenant: cfg.Gas.Tenant, Site: cfg.Visual.Site, Entity: cfg.Visual.Simulation.RunID + "/visual-fixture", Type: VisualType, Source: cfg.VisualPublisher}, Version: version, Frame: cfg.Visual.Frame, Timing: world.Timing{SampleTime: result.Oldest, ClockDomain: cfg.Visual.Simulation.ClockDomain, ErrorKnown: true}, TTL: result.ValidUntil.Sub(result.Oldest), Provenance: world.Provenance{Origin: world.Inferred, AppVersion: "world-incident-v1", ModelID: "two-camera-lidar-red-fixture", ModelVersion: "1", Inputs: ancestry}, Payload: raw}, nil
}
func ValidateVisual(cfg Config, c world.Component, now time.Time) (Visual, error) {
	empty := Visual{State: "unavailable", Reason: "No current compatible visual claim"}
	if c.Tombstone || gasreplica.Remaining(c, now) <= 0 || c.Timing.SampleTime.After(now) || c.Timing.AgeHint < 0 || !c.Timing.ErrorKnown || c.Timing.ErrorBound != 0 {
		return empty, errors.New("visual claim expired or invalid clock")
	}
	var p VisualClaim
	if e := authority.DecodeStrictLimit(c.Payload, &p, MaxVisualBytes); e != nil {
		return empty, e
	}
	if p.Schema != VisualType || p.Contract != cfg.Digest() || p.Evaluated.After(now) || !p.Evaluated.Before(p.Result.ValidUntil) {
		return empty, errors.New("visual model contract or evaluation time differs")
	}
	want, e := MakeVisual(cfg, c.Version, p.Result.Inputs, p.Images, p.Evaluated)
	if e != nil {
		return empty, e
	}
	if gasreplica.ComponentHash(want) != gasreplica.ComponentHash(c) {
		return empty, errors.New("visual geometry, ancestry or envelope differs from original images")
	}
	remaining := gasreplica.Remaining(c, now).Seconds()
	return Visual{State: "localized", Reason: p.Result.Spatial.Reason, Hash: gasreplica.ComponentHash(c), Version: c.Version, Remaining: remaining, Claim: &p}, nil
}

type Support struct {
	Kind       string        `json:"kind"`
	Hash       string        `json:"sha256"`
	Source     world.Source  `json:"source"`
	Version    world.Version `json:"version"`
	Captured   time.Time     `json:"captured_at"`
	ValidUntil time.Time     `json:"valid_until"`
}
type Result struct {
	frozen      string
	ancestry    []world.Input
	Schema      string            `json:"schema"`
	Contract    string            `json:"contract_sha256"`
	State       string            `json:"state"`
	Reason      string            `json:"reason"`
	At          time.Time         `json:"at"`
	ValidUntil  time.Time         `json:"valid_until"`
	Gas         gasview.Selection `json:"gas"`
	Visual      Visual            `json:"visual"`
	Supports    []Support         `json:"supports"`
	SeparationM *float64          `json:"distance_to_region_m,omitempty"`
	InputDigest string            `json:"input_digest"`
}
type Selector struct {
	mu       sync.Mutex
	cfg      Config
	gas      *gasview.Selector
	last     time.Time
	version  world.Version
	hash     string
	deadline time.Time
}

func New(cfg Config) (*Selector, error) {
	if e := cfg.Validate(); e != nil {
		return nil, e
	}
	g, e := gasview.New(cfg.Gas)
	// Configuration is immutable after construction, including map-backed catalogs.
	raw, _ := json.Marshal(cfg)
	var owned Config
	if err := json.Unmarshal(raw, &owned); err != nil {
		return nil, err
	}
	g, e = gasview.New(owned.Gas)
	return &Selector{cfg: owned, gas: g}, e
}
func (s *Selector) Step(records []world.Component, now time.Time) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Result{Schema: Type, Contract: s.cfg.Digest(), State: "unavailable", Reason: "No current compatible source evidence", At: now, Supports: []Support{}, Visual: Visual{State: "unavailable", Reason: "No current visual claim"}}
	if len(records) > 256 || now.Before(s.last) {
		return out, errors.New("bounded snapshot and monotonic clock required")
	}
	s.last = now
	gas, e := s.gas.Step(records, now)
	if e != nil {
		return out, e
	}
	out.Gas = gas
	visuals := []world.Component{}
	var gasComponent *world.Component
	for i, c := range records {
		if c.Key.Type == VisualType && c.Key.Source == s.cfg.VisualPublisher {
			visuals = append(visuals, c)
		}
		if gas.Hash != "" && gasreplica.ComponentHash(c) == gas.Hash {
			gasComponent = &records[i]
		}
	}
	if len(visuals) > 1 {
		out.Visual.Reason = "Ambiguous visual source records"
	} else if len(visuals) == 1 {
		c := visuals[0]
		v, err := ValidateVisual(s.cfg, c, now)
		if err != nil {
			out.Visual.Reason = err.Error()
		} else if c.Version.Compare(s.version) < 0 || c.Version == s.version && s.hash != "" && s.hash != v.Hash {
			out.Visual.Reason = "Reordered or conflicting visual version"
		} else {
			deadline := now.Add(time.Duration(v.Remaining * float64(time.Second)))
			if c.Version == s.version && !s.deadline.IsZero() && s.deadline.Before(deadline) {
				deadline = s.deadline
			}
			s.version = c.Version
			s.hash = v.Hash
			s.deadline = deadline
			if !now.Before(deadline) {
				out.Visual.Reason = "Original visual support expired locally"
			} else {
				v.Remaining = deadline.Sub(now).Seconds()
				out.Visual = v
				out.ancestry = append(out.ancestry, ownInputs(c.Provenance.Inputs)...)
				out.Supports = append(out.Supports, Support{"visual", v.Hash, c.Key.Source, c.Version, c.Timing.SampleTime, deadline})
			}
		}
	}
	if gasComponent != nil {
		c := *gasComponent
		out.ancestry = append(out.ancestry, ownInputs(c.Provenance.Inputs)...)
		until := s.cfg.Gas.Instant(gas.Result.Estimate.ValidUntil)
		out.Supports = append(out.Supports, Support{"gas", gas.Hash, c.Key.Source, c.Version, c.Timing.SampleTime, until})
	}
	visualOK := out.Visual.State == "localized"
	gasOK := gas.State == "localized"
	switch {
	case visualOK && gasOK:
		// Compare actual accepted grid cells, not a fitted-radius/covariance score.
		p := out.Visual.Claim.Result.Spatial.PositionM
		distance := math.Inf(1)
		for _, cell := range gas.Result.Estimate.Cells {
			dx := math.Max(0, math.Abs(p[0]-cell.Position[0])-s.cfg.Gas.Field.Step/2)
			dz := math.Max(0, math.Abs(p[2]-cell.Position[1])-s.cfg.Gas.Field.Step/2)
			distance = math.Min(distance, math.Hypot(dx, dz))
		}
		out.SeparationM = &distance
		out.State = "conflicting"
		out.Reason = "Visual fixture footprint and gas model-fit region are spatially disjoint; no joint location"
		if distance <= out.Visual.Claim.Result.Spatial.UncertaintyM {
			out.State = "consistent"
			out.Reason = "Visual fixture footprint intersects the gas model-fit region; association only, not leak causality"
		}
		lo, hi := out.Visual.Claim.Result.Oldest, out.Visual.Claim.Result.Latest
		var claim gasreplica.Claim
		_ = json.Unmarshal(gasComponent.Payload, &claim)
		for _, c := range claim.Inputs {
			if c.Timing.SampleTime.Before(lo) {
				lo = c.Timing.SampleTime
			}
			if c.Timing.SampleTime.After(hi) {
				hi = c.Timing.SampleTime
			}
		}
		if hi.Sub(lo) > 350*time.Millisecond {
			out.State = "unavailable"
			out.Reason = "Gas and visual captures exceed the declared 350 ms association window"
		}
	case gasOK:
		out.State = "gas_only"
		out.Reason = "Gas source-region evidence is current; visual support is unavailable"
	case visualOK:
		out.State = "visual_only"
		out.Reason = "Visual fixture is localized; gas region is absent, ambiguous or disputed"
	case gas.State == "ambiguous":
		out.State = "gas_only"
		out.Reason = "Ambiguous gas region retained separately; no joint localization"
	}
	for _, support := range out.Supports {
		if out.ValidUntil.IsZero() || support.ValidUntil.Before(out.ValidUntil) {
			out.ValidUntil = support.ValidUntil
		}
	}
	// Exclude receipt time and decreasing age from identity. Replayed support does
	// not create a new inference or renew its lifetime.
	ids := []string{out.State, out.Contract}
	for _, support := range out.Supports {
		ids = append(ids, support.Kind+":"+support.Hash)
	}
	sort.Strings(ids)
	out.InputDigest = gasreplica.Digest(ids)
	out.frozen = gasreplica.Digest(out)
	return out, nil
}

// Component publishes a compact association. Original independent claims remain
// separate records; their hashes and source versions bind this interpretation.
func Component(cfg Config, reader string, version world.Version, result Result) (world.Component, error) {
	source, ok := cfg.Readers[reader]
	if !ok || version.Epoch == 0 || version.Sequence == 0 || len(result.Supports) == 0 || !result.At.Before(result.ValidUntil) || result.Contract != cfg.Digest() || result.frozen == "" || result.frozen != gasreplica.Digest(result) {
		return world.Component{}, errors.New("current support and configured association source required")
	}
	oldest := result.Supports[0].Captured
	ancestry := ownInputs(result.ancestry)
	if len(ancestry) == 0 {
		return world.Component{}, errors.New("evaluated original ancestry required")
	}
	for _, support := range result.Supports {
		if support.Captured.Before(oldest) {
			oldest = support.Captured
		}
	}
	payload := struct {
		Schema      string    `json:"schema"`
		Contract    string    `json:"contract_sha256"`
		State       string    `json:"state"`
		Reason      string    `json:"reason"`
		Supports    []Support `json:"supports"`
		ValidUntil  time.Time `json:"valid_until"`
		InputDigest string    `json:"input_digest"`
	}{Type, cfg.Digest(), result.State, result.Reason, result.Supports, result.ValidUntil, result.InputDigest}
	raw, e := json.Marshal(payload)
	if e != nil {
		return world.Component{}, e
	}
	return world.Component{Key: world.Key{Tenant: cfg.Gas.Tenant, Site: cfg.Visual.Site, Entity: cfg.Visual.Simulation.RunID + "/incident", Type: Type, Source: source}, Version: version, Frame: cfg.Visual.Frame, Timing: world.Timing{SampleTime: oldest, ClockDomain: cfg.Visual.Simulation.ClockDomain, ErrorKnown: true}, TTL: result.ValidUntil.Sub(oldest), Provenance: world.Provenance{Origin: world.Inferred, AppVersion: "world-incident-v1", ModelID: "spatial-temporal-association", ModelVersion: "1", Inputs: ancestry}, Payload: raw}, nil
}

// ownInputs severs nested provenance pointers at both ownership boundaries.
// Caller-owned originals and a previously returned component cannot mutate a
// later publication of an already evaluated result.
func ownInputs(in []world.Input) []world.Input {
	out := append([]world.Input(nil), in...)
	for i := range out {
		if in[i].Simulation != nil {
			v := *in[i].Simulation
			out[i].Simulation = &v
		}
		if in[i].OriginalCaptureTime != nil {
			v := *in[i].OriginalCaptureTime
			out[i].OriginalCaptureTime = &v
		}
	}
	return out
}
