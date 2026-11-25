// Package gasreplica adapts the simulation gas model to ordinary signed World
// components. Domain validation remains outside the replica and transport.
package gasreplica

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/gascontract"
	"github.com/wendylabsinc/wendy/go/world/internal/gasfield"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const ObservationType = "wendy.gas.observation.v1"
const ClaimType = "wendy.gas.source-region.v1"
const MaxClaimBytes = 48 << 10 // leaves room for the actual 64 KiB signed envelope

// Config retains the public API while the signed contract has no authority dependency.
type Config = gascontract.Config

func Digest(value any) string {
	raw, _ := json.Marshal(value)
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

// ComponentHash excludes unsigned transit age, as the existing replica does.
func ComponentHash(c world.Component) string { c.Timing.AgeHint = 0; return Digest(c) }
func Remaining(c world.Component, now time.Time) time.Duration {
	return min(c.Timing.SampleTime.Add(c.TTL).Sub(now), c.TTL-c.Timing.AgeHint) - c.Timing.ErrorBound
}

func ObservationComponent(cfg Config, o gasfield.Observation) (world.Component, error) {
	// The model's validation checks the exact source, calibration and simulation
	// catalog before this payload can become an ordinary World component.
	if _, err := new(gasfield.Session).Step(gasfield.Request{Config: &cfg.Field, Now: o.Captured, Assumptions: cfg.Assumptions, Observations: []gasfield.Observation{o}}); err != nil {
		return world.Component{}, err
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return world.Component{}, err
	}
	return world.Component{Key: world.Key{Tenant: cfg.Tenant, Site: cfg.Field.Site, Entity: cfg.Field.Simulation.RunID + "/sensor/" + o.Sensor, Type: ObservationType, Source: o.Source}, Version: o.Version, Frame: o.Frame, Timing: world.Timing{SampleTime: cfg.Instant(o.Captured), ClockDomain: cfg.Field.Simulation.ClockDomain, ErrorKnown: true}, TTL: cfg.Instant(o.ValidUntil).Sub(cfg.Instant(o.Captured)), Provenance: o.Provenance, Payload: raw}, nil
}
func DecodeObservation(cfg Config, c world.Component, now time.Time) (gasfield.Observation, error) {
	var o gasfield.Observation
	if c.Tombstone || c.Key.Tenant != cfg.Tenant || c.Key.Site != cfg.Field.Site || c.Key.Type != ObservationType || c.Frame != cfg.Field.Frame || c.Timing.ClockDomain != cfg.Field.Simulation.ClockDomain || !c.Timing.ErrorKnown || c.Timing.ErrorBound != 0 || c.Timing.AgeHint < 0 || Remaining(c, now) <= 0 || c.Timing.SampleTime.After(now) {
		return o, errors.New("raw observation scope or freshness differs")
	}
	if err := authority.DecodeStrictLimit(c.Payload, &o, 8<<10); err != nil {
		return o, err
	}
	want, err := ObservationComponent(cfg, o)
	if err != nil {
		return o, err
	}
	if ComponentHash(want) != ComponentHash(c) {
		return o, errors.New("observation metadata does not bind its original payload")
	}
	return o, nil
}

// CompactRegion stores topology with one bit per grid cell, not thousands of
// repeated JSON coordinates. Per-cell residuals are reconstructed by validation.
type CompactRegion struct {
	Mask       []byte     `json:"mask"`
	Position   [2]float64 `json:"position_xz"`
	Radius     float64    `json:"radius_m"`
	Strength   float64    `json:"strength_ppm"`
	Residual   float64    `json:"normalized_rms"`
	ValidUntil float64    `json:"valid_until_s"`
}
type Claim struct {
	Schema      string            `json:"schema"`
	Contract    string            `json:"contract_sha256"`
	Gateway     string            `json:"gateway"`
	Evaluated   float64           `json:"evaluated_s"`
	State       string            `json:"state"`
	Region      CompactRegion     `json:"region"`
	Inputs      []world.Component `json:"inputs"`
	InputDigest string            `json:"input_digest"`
}

func compact(cfg Config, r gasfield.Result) (CompactRegion, error) {
	if r.Estimate == nil {
		return CompactRegion{}, errors.New("a source region is required")
	}
	nx := int(math.Floor((cfg.Field.Max[0]-cfg.Field.Min[0])/cfg.Field.Step)) + 1
	nz := int(math.Floor((cfg.Field.Max[1]-cfg.Field.Min[1])/cfg.Field.Step)) + 1
	if nx*nz > 4096 {
		return CompactRegion{}, errors.New("grid exceeds bound")
	}
	e := r.Estimate
	out := CompactRegion{Mask: make([]byte, (nx*nz+7)/8), Position: e.Position, Radius: e.Radius, Strength: e.Strength, Residual: e.Score, ValidUntil: e.ValidUntil}
	for _, cell := range e.Cells {
		ix := int(math.Round((cell.Position[0] - cfg.Field.Min[0]) / cfg.Field.Step))
		iz := int(math.Round((cell.Position[1] - cfg.Field.Min[1]) / cfg.Field.Step))
		if ix < 0 || ix >= nx || iz < 0 || iz >= nz {
			return out, errors.New("cell outside grid")
		}
		i := iz*nx + ix
		out.Mask[i/8] |= 1 << uint(i%8)
	}
	return out, nil
}

// Evaluate uses only the supplied replica snapshot. Invalid sources are held out
// with explicit diagnostics; they never become valid observations by coercion.
func Evaluate(cfg Config, components []world.Component, now time.Time) (gasfield.Result, []world.Component, []string, error) {
	if err := cfg.Validate(); err != nil {
		return gasfield.Result{}, nil, nil, err
	}
	if len(components) > 256 || cfg.Seconds(now) < 0 {
		return gasfield.Result{}, nil, nil, errors.New("snapshot or clock outside bounds")
	}
	observations := []gasfield.Observation{}
	accepted := []world.Component{}
	rejected := []string{}
	seen := map[string]bool{}
	for _, c := range components {
		if c.Key.Type != ObservationType {
			continue
		}
		o, err := DecodeObservation(cfg, c, now)
		if err != nil {
			rejected = append(rejected, c.Key.Entity+": "+err.Error())
			continue
		}
		if seen[o.Sensor] {
			return gasfield.Result{}, nil, nil, errors.New("multiple current records for one configured sensor")
		}
		seen[o.Sensor] = true
		observations = append(observations, o)
		accepted = append(accepted, c)
	}
	r, err := new(gasfield.Session).Step(gasfield.Request{Config: &cfg.Field, Now: cfg.Seconds(now), Assumptions: cfg.Assumptions, Observations: observations})
	if err != nil {
		return r, nil, rejected, err
	}
	used := map[string]bool{}
	for _, rd := range r.Readings {
		if rd.State == "live" {
			used[rd.Sensor.ID] = true
		}
	}
	inputs := []world.Component{}
	for _, c := range accepted {
		var o gasfield.Observation
		_ = json.Unmarshal(c.Payload, &o)
		if used[o.Sensor] {
			inputs = append(inputs, c)
			if r.Estimate != nil {
				r.Estimate.ValidUntil = min(r.Estimate.ValidUntil, cfg.Seconds(now)+Remaining(c, now).Seconds())
			}
		}
	}
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Key.Entity < inputs[j].Key.Entity })
	return r, inputs, rejected, nil
}
func MakeClaim(cfg Config, gateway string, version world.Version, r gasfield.Result, inputs []world.Component) (world.Component, error) {
	source, ok := cfg.Gateways[gateway]
	if !ok || version.Epoch == 0 || version.Sequence == 0 {
		return world.Component{}, errors.New("unconfigured claim publisher")
	}
	region, err := compact(cfg, r)
	if err != nil {
		return world.Component{}, err
	}
	if len(inputs) < 4 || len(inputs) > 16 {
		return world.Component{}, errors.New("claim requires bounded original support")
	}
	canonical := make([]world.Component, len(inputs))
	copy(canonical, inputs)
	hashes := []string{}
	ancestry := []world.Input{}
	oldest := math.Inf(1)
	for _, c := range canonical {
		o, err := DecodeObservation(cfg, c, cfg.Instant(r.Now))
		if err != nil {
			return world.Component{}, err
		}
		oldest = min(oldest, o.Captured)
		h := ComponentHash(c)
		hashes = append(hashes, h)
		sim := cfg.Field.Simulation
		ancestry = append(ancestry, world.Input{ID: "sha256:" + h, Origin: world.Simulated, Simulation: &sim})
	}
	p := Claim{Schema: ClaimType, Contract: cfg.ContractDigest(), Gateway: gateway, Evaluated: r.Now, State: r.State, Region: region, Inputs: canonical, InputDigest: Digest(hashes)}
	raw, err := json.Marshal(p)
	if err != nil || len(raw) > MaxClaimBytes {
		return world.Component{}, errors.New("claim payload exceeds bounded representation")
	}
	return world.Component{Key: world.Key{Tenant: cfg.Tenant, Site: cfg.Field.Site, Entity: cfg.Field.Simulation.RunID + "/gas-source", Type: ClaimType, Source: source}, Version: version, Frame: cfg.Field.Frame, Timing: world.Timing{SampleTime: cfg.Instant(oldest), ClockDomain: cfg.Field.Simulation.ClockDomain, ErrorKnown: true}, TTL: cfg.Instant(region.ValidUntil).Sub(cfg.Instant(oldest)), Provenance: world.Provenance{Origin: world.Inferred, AppVersion: "gas-replica-v1", ModelID: gasfield.Model, ModelVersion: "1", Inputs: ancestry}, Payload: raw}, nil
}

type Validated struct {
	Claim      Claim           `json:"claim"`
	Component  world.Component `json:"component"`
	Result     gasfield.Result `json:"result"`
	Hash       string          `json:"sha256"`
	RemainingS float64         `json:"remaining_s"`
}

// ValidateClaim independently checks the signed publisher's bounded result
// against its retained original components. This validates model correspondence,
// not a physical model or independent sensor attestation inside the derived claim.
func ValidateClaim(cfg Config, c world.Component, now time.Time) (Validated, error) {
	var p Claim
	empty := Validated{}
	if c.Tombstone || c.Key.Type != ClaimType || c.Key.Tenant != cfg.Tenant || c.Key.Site != cfg.Field.Site || c.Frame != cfg.Field.Frame || !c.Timing.ErrorKnown || c.Timing.ErrorBound != 0 || c.Timing.AgeHint < 0 || Remaining(c, now) <= 0 || c.Timing.SampleTime.After(now) {
		return empty, errors.New("claim scope or expiry differs")
	}
	if err := authority.DecodeStrictLimit(c.Payload, &p, MaxClaimBytes); err != nil {
		return empty, err
	}
	if p.Schema != ClaimType || p.Contract != cfg.ContractDigest() || p.Evaluated < 0 || math.IsNaN(p.Evaluated) || cfg.Instant(p.Evaluated).After(now) || p.Region.ValidUntil <= cfg.Seconds(now) || p.Region.ValidUntil <= p.Evaluated || cfg.Gateways[p.Gateway] != c.Key.Source {
		return empty, errors.New("incompatible model, publisher, assumptions or evaluation clock")
	}
	r, inputs, rejected, err := Evaluate(cfg, p.Inputs, cfg.Instant(p.Evaluated))
	if err != nil || len(rejected) != 0 || len(inputs) != len(p.Inputs) || r.Estimate == nil {
		return empty, errors.New("claim original support cannot reproduce a region")
	}
	// A derived lifetime may be shorter than its support, never longer.
	if p.Region.ValidUntil > r.Estimate.ValidUntil+1e-8 {
		return empty, errors.New("derived region outlives original support")
	}
	r.Estimate.ValidUntil = p.Region.ValidUntil
	want, err := MakeClaim(cfg, p.Gateway, c.Version, r, inputs)
	if err != nil {
		return empty, err
	}
	if ComponentHash(want) != ComponentHash(c) {
		return empty, errors.New("claim metadata, ancestry or compact region differs from model result")
	}
	r.Now = cfg.Seconds(now)
	r.Estimate.ValidUntil = min(r.Estimate.ValidUntil, r.Now+Remaining(c, now).Seconds())
	return Validated{p, c, r, ComponentHash(c), Remaining(c, now).Seconds()}, nil
}
func CheckSignedSize(record authority.SignedRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if len(raw) > authority.MaxEnvelopeBytes {
		return fmt.Errorf("complete signed envelope %d exceeds %d", len(raw), authority.MaxEnvelopeBytes)
	}
	return nil
}
