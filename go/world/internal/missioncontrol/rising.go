package missioncontrol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/gasfield"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// RisingInspectionType retains an operator-reviewed association and its exact
// original temporal claim. It neither proves physical truth nor grants motion.
const RisingInspectionType = "wendy.rising-incident.inspection.v1"
const MaxRisingPayload = 20 << 10

type RisingSelection struct {
	Schema         string    `json:"schema"`
	JoinedSHA256   string    `json:"joined_sha256"`
	JoinedJSON     []byte    `json:"joined_json"`
	TemporalSHA256 string    `json:"temporal_sha256"`
	TemporalJSON   []byte    `json:"temporal_json"`
	SpatialJSON    []byte    `json:"spatial_json"`
	ValidUntil     time.Time `json:"valid_until"`
}
type RisingWitness struct {
	Sensor     string    `json:"sensor"`
	Hash       string    `json:"sha256"`
	Captured   time.Time `json:"captured_at"`
	ValidUntil time.Time `json:"valid_until"`
}
type RisingPoint struct {
	Captured    float64 `json:"captured_s"`
	Value       float64 `json:"value_ppm"`
	Uncertainty float64 `json:"uncertainty_ppm"`
	Hash        string  `json:"sha256"`
	ValidUntil  float64 `json:"valid_until_s"`
}
type RisingTrace struct {
	Sensor  string        `json:"sensor"`
	State   string        `json:"state"`
	Points  []RisingPoint `json:"points"`
	MinRate float64       `json:"min_rate_ppm_s"`
	MaxRate float64       `json:"max_rate_ppm_s"`
}
type RisingWindow struct {
	State       string        `json:"state"`
	Reason      string        `json:"reason"`
	Evaluated   float64       `json:"evaluated_s"`
	ValidUntil  float64       `json:"valid_until_s"`
	Traces      []RisingTrace `json:"traces"`
	InputDigest string        `json:"input_digest"`
}
type RisingClaim struct {
	Schema   string            `json:"schema"`
	Contract string            `json:"contract_sha256"`
	Gateway  string            `json:"gateway"`
	Window   RisingWindow      `json:"window"`
	Inputs   []world.Component `json:"inputs"`
}
type risingJoin struct {
	Schema      string            `json:"schema"`
	Contract    string            `json:"contract_sha256"`
	State       string            `json:"state"`
	Reason      string            `json:"reason"`
	Supports    []IncidentSupport `json:"supports"`
	Witnesses   []RisingWitness   `json:"witnesses"`
	ValidUntil  time.Time         `json:"valid_until"`
	InputDigest string            `json:"input_digest"`
	Carriers    []struct {
		Hash       string        `json:"sha256"`
		Source     world.Source  `json:"source"`
		Version    world.Version `json:"version"`
		GasHash    string        `json:"gas_sha256"`
		ChangeHash string        `json:"change_sha256"`
		ValidUntil time.Time     `json:"valid_until"`
	} `json:"carriers,omitempty"`
}

func risingHash(v []byte) string               { h := sha256.Sum256(v); return hex.EncodeToString(h[:]) }
func risingCanonical(c world.Component) []byte { b, _ := json.Marshal(c); return b }
func risingFinite(v float64) bool              { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func risingSameTime(a, b time.Time) bool       { return math.Abs(a.Sub(b).Seconds()) < 0.000001 }

// DecodeRisingSelection validates immutable historical evidence. Current
// authority and freshness are checked separately at preparation and signing.
// This checks the retained assertion and original consistency, not the full
// commissioned rate/span/gap rule. The domain producer revalidates that rule
// with gaschange.ValidateClaim before constructing this context.
func DecodeRisingSelection(c world.Component) (RisingSelection, world.Component, RisingClaim, error) {
	var p RisingSelection
	var spatial world.Component
	var temporal RisingClaim
	bad := errors.New("invalid retained rising-incident context")
	fail := func() (RisingSelection, world.Component, RisingClaim, error) {
		return p, spatial, temporal, bad
	}
	if c.Key.Type != RisingInspectionType || c.Tombstone || c.Timing.AgeHint != 0 || c.Timing.ErrorBound != 0 || !c.Timing.ErrorKnown || c.Timing.Validate() != nil || c.TTL <= 0 || c.TTL > 10*time.Second || c.Frame.Validate() != nil || c.Version.Epoch == 0 || c.Version.Sequence == 0 || c.Key.Source.Validate() != nil || c.Provenance.Validate() != nil || c.Provenance.Origin != world.Inferred || c.Provenance.AppVersion != "world-rising-inspection-v1" || c.Provenance.ModelID != "retained-exact-rise-selection" || c.Provenance.ModelVersion != "1" {
		return fail()
	}
	if triggerJSON(c.Payload, &p, MaxRisingPayload) != nil || p.Schema != RisingInspectionType {
		return fail()
	}
	if _, e := triggerFields(c.Payload, "schema joined_sha256 joined_json temporal_sha256 temporal_json spatial_json valid_until temporal_codec", ""); e != nil {
		return fail()
	}
	var joined, claim world.Component
	for _, item := range []struct {
		raw   []byte
		value *world.Component
	}{{p.JoinedJSON, &joined}, {p.TemporalJSON, &claim}, {p.SpatialJSON, &spatial}} {
		if triggerJSON(item.raw, item.value, 40<<10) != nil || !bytes.Equal(risingCanonical(*item.value), item.raw) || item.value.Tombstone || item.value.Timing.AgeHint != 0 || item.value.Timing.Validate() != nil || !item.value.Timing.ErrorKnown || item.value.Timing.ErrorBound != 0 || item.value.TTL <= 0 || item.value.TTL > 10*time.Second || item.value.Key.Tenant != c.Key.Tenant || item.value.Key.Site != c.Key.Site || item.value.Frame != c.Frame || item.value.Timing.ClockDomain != c.Timing.ClockDomain || item.value.Provenance.Validate() != nil || item.value.Key.Source.Validate() != nil || item.value.Version.Epoch == 0 || item.value.Version.Sequence == 0 {
			return fail()
		}
	}
	run := strings.TrimPrefix(c.Timing.ClockDomain, "simulation:")
	if run == c.Timing.ClockDomain || run == "" || c.Key.Entity != run+"/rising-inspection" || joined.Key.Type != "wendy.incident.change.v1" || joined.Key.Entity != run+"/rising-incident" || claim.Key.Type != "wendy.gas.change.v1" || claim.Key.Entity != run+"/gas-change" || p.JoinedSHA256 != risingHash(p.JoinedJSON) || p.TemporalSHA256 != risingHash(p.TemporalJSON) || c.Key.Source != joined.Key.Source || c.Version != joined.Version || spatial.Key.Source != joined.Key.Source || spatial.Version != joined.Version {
		return fail()
	}
	if joined.Provenance.AppVersion != "world-incident-change-v1" || joined.Provenance.ModelID != "exact-original-witness-join" || joined.Provenance.ModelVersion != "1" || claim.Provenance.AppVersion != "bounded-gas-change-v1" || claim.Provenance.ModelID != "bounded-gas-change-v1" || claim.Provenance.ModelVersion != "1" {
		return fail()
	}
	if _, e := IncidentTriggerTarget(spatial); e != nil {
		return fail()
	}
	var old IncidentSelection
	_ = json.Unmarshal(spatial.Payload, &old)
	var j risingJoin
	if triggerJSON(joined.Payload, &j, 16<<10) != nil || j.Schema != "wendy.incident.change.v1" || j.State != "supported_rise" || !hashPattern.MatchString(j.Contract) || !hashPattern.MatchString(j.InputDigest) || len(j.Supports) != 3 || len(j.Witnesses) != 4 || len(j.Carriers) > 1 || !j.ValidUntil.Equal(p.ValidUntil) {
		return fail()
	}
	if _, e := triggerFields(joined.Payload, "schema contract_sha256 state reason supports witnesses valid_until input_digest", "carriers"); e != nil {
		return fail()
	}
	var change IncidentSupport
	var independent []IncidentSupport
	for _, s := range j.Supports {
		if s.Kind == "change" {
			if change.Kind != "" {
				return fail()
			}
			change = s
		} else {
			independent = append(independent, s)
		}
	}
	if !reflect.DeepEqual(independent, old.Supports) || change.SHA256 != p.TemporalSHA256 || change.Source != claim.Key.Source || change.Version != claim.Version || !change.Captured.Equal(claim.Timing.SampleTime) || change.ValidUntil.After(claim.Timing.SampleTime.Add(claim.TTL)) || len(independent) != 2 {
		return fail()
	}
	var gas IncidentSupport
	for _, s := range independent {
		if s.Kind == "gas" {
			gas = s
		}
	}
	if gas.Source != claim.Key.Source || gas.Kind == "" {
		return fail()
	}
	if !p.ValidUntil.Equal(joined.Timing.SampleTime.Add(joined.TTL)) || !c.Timing.SampleTime.Equal(joined.Timing.SampleTime) || c.TTL != joined.TTL || p.ValidUntil.After(change.ValidUntil) || p.ValidUntil.After(old.ValidUntil) || !reflect.DeepEqual(c.Provenance.Inputs, joined.Provenance.Inputs) {
		return fail()
	}
	for _, pair := range j.Carriers {
		if !hashPattern.MatchString(pair.Hash) || pair.GasHash != gas.SHA256 || pair.ChangeHash != change.SHA256 || pair.Source != gas.Source || pair.Source != change.Source || pair.Version != gas.Version || pair.Version != change.Version || p.ValidUntil.After(pair.ValidUntil) {
			return fail()
		}
	}
	if triggerJSON(claim.Payload, &temporal, 40<<10) != nil || temporal.Schema != "wendy.gas.change.v1" || !hashPattern.MatchString(temporal.Contract) || temporal.Gateway == "" || temporal.Window.State != "rising" || len(temporal.Window.Traces) != 4 || len(temporal.Inputs) != 12 || !risingFinite(temporal.Window.Evaluated) || !risingFinite(temporal.Window.ValidUntil) || temporal.Window.ValidUntil <= temporal.Window.Evaluated {
		return fail()
	}
	originals := map[string]gasfield.Observation{}
	components := map[string]world.Component{}
	versions := map[string]bool{}
	var anchor time.Time
	hashes := []string{}
	for _, input := range temporal.Inputs {
		var o gasfield.Observation
		if triggerJSON(input.Payload, &o, 8<<10) != nil || input.Tombstone || input.Timing.Validate() != nil || input.Key.Source.Validate() != nil || input.Version.Epoch == 0 || input.Version.Sequence == 0 || input.Provenance.Validate() != nil || input.Key.Type != "wendy.gas.observation.v1" || input.Key.Tenant != c.Key.Tenant || input.Key.Site != c.Key.Site || input.Frame != c.Frame || input.Key.Source != o.Source || input.Version != o.Version || input.Timing.ClockDomain != c.Timing.ClockDomain || !input.Timing.ErrorKnown || input.Timing.ErrorBound != 0 || input.Timing.AgeHint < 0 || input.Timing.AgeHint >= input.TTL || input.TTL <= 0 || input.TTL > 5*time.Second || o.Units != "ppm" || o.Sensor == "" || o.Calibration == "" || o.Species == "" || o.SensorType == "" || o.Site != c.Key.Site || o.Frame != c.Frame || !reflect.DeepEqual(o.Provenance, input.Provenance) || input.Provenance.Origin != world.Simulated || input.Provenance.Simulation == nil || input.Provenance.Simulation.RunID != run || input.Provenance.Simulation.ClockDomain != c.Timing.ClockDomain || input.Key.Entity != run+"/sensor/"+o.Sensor || !risingFinite(o.Captured) || !risingFinite(o.ValidUntil) || !risingFinite(o.Value) || !risingFinite(o.Uncertainty) || o.Captured < 0 || o.ValidUntil <= o.Captured || o.Uncertainty < .1 || o.Uncertainty > 100 || o.Value < 0 || o.Value > 10000 {
			return fail()
		}
		// Existing World identity excludes only unsigned transit age. Retain
		// the original component unchanged, including its transport accounting.
		identity := input
		identity.Timing.AgeHint = 0
		h := risingHash(risingCanonical(identity))
		if _, exists := originals[h]; exists {
			return fail()
		}
		v, _ := json.Marshal([]any{o.Sensor, input.Version})
		if versions[string(v)] {
			return fail()
		}
		versions[string(v)] = true
		a := input.Timing.SampleTime.Add(-time.Duration(o.Captured * 1e9))
		if anchor.IsZero() {
			anchor = a
		}
		if !risingSameTime(anchor, a) || !risingSameTime(input.Timing.SampleTime.Add(input.TTL), anchor.Add(time.Duration(o.ValidUntil*1e9))) {
			return fail()
		}
		originals[h] = o
		components[h] = input
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)
	b, _ := json.Marshal(hashes)
	if risingHash(b) != temporal.Window.InputDigest {
		return fail()
	}
	if !risingSameTime(claim.Timing.SampleTime.Add(claim.TTL), anchor.Add(time.Duration(temporal.Window.ValidUntil*1e9))) {
		return fail()
	}
	seen := map[string]bool{}
	used := map[string]bool{}
	rising := 0
	for _, trace := range temporal.Window.Traces {
		if seen[trace.Sensor] || len(trace.Points) != 3 || (trace.State != "rising" && trace.State != "steady") || !risingFinite(trace.MinRate) || !risingFinite(trace.MaxRate) || trace.MinRate > trace.MaxRate {
			return fail()
		}
		seen[trace.Sensor] = true
		if trace.State == "rising" {
			rising++
		}
		var previous gasfield.Observation
		var pv world.Version
		lo, hi := math.Inf(1), math.Inf(-1)
		for i, point := range trace.Points {
			o, ok := originals[point.Hash]
			input := components[point.Hash]
			if !ok || used[point.Hash] || o.Sensor != trace.Sensor || point.Captured != o.Captured || point.Captured > temporal.Window.Evaluated || point.Value != o.Value || point.Uncertainty != o.Uncertainty || !risingFinite(point.ValidUntil) || point.ValidUntil > o.ValidUntil+0.000001 || temporal.Window.ValidUntil > point.ValidUntil {
				return fail()
			}
			used[point.Hash] = true
			if i > 0 {
				dt := o.Captured - previous.Captured
				if dt <= 0 || input.Version.Compare(pv) <= 0 || o.Source != previous.Source || o.Calibration != previous.Calibration || o.SensorType != previous.SensorType || o.Species != previous.Species {
					return fail()
				}
				lo = math.Min(lo, (o.Value-o.Uncertainty-previous.Value-previous.Uncertainty)/dt)
				hi = math.Max(hi, (o.Value+o.Uncertainty-previous.Value+previous.Uncertainty)/dt)
			}
			previous = o
			pv = input.Version
		}
		if math.Abs(lo-trace.MinRate) > 1e-7 || math.Abs(hi-trace.MaxRate) > 1e-7 || (trace.State == "rising" && lo <= 0) {
			return fail()
		}
		count := 0
		for _, w := range j.Witnesses {
			if w.Sensor == trace.Sensor {
				count++
				last := trace.Points[2]
				input := components[last.Hash]
				if w.Hash != last.Hash || !w.Captured.Equal(input.Timing.SampleTime) || !w.ValidUntil.Equal(input.Timing.SampleTime.Add(input.TTL)) || p.ValidUntil.After(w.ValidUntil) {
					return fail()
				}
			}
		}
		if count != 1 {
			return fail()
		}
	}
	for _, component := range []world.Component{c, joined, claim} {
		seenLeaves := map[string]bool{}
		if len(component.Provenance.Inputs) < 4 || len(component.Provenance.Inputs) > 32 {
			return fail()
		}
		for _, leaf := range component.Provenance.Inputs {
			if leaf.Origin != world.Simulated || leaf.Simulation == nil || leaf.Simulation.RunID != run || leaf.Simulation.ClockDomain != c.Timing.ClockDomain || seenLeaves[leaf.ID] {
				return fail()
			}
			seenLeaves[leaf.ID] = true
		}
		if component.Key.Type == "wendy.gas.change.v1" {
			if len(seenLeaves) != 12 {
				return fail()
			}
			for hash := range originals {
				if !seenLeaves["sha256:"+hash] {
					return fail()
				}
			}
		}
	}
	if rising < 3 || len(used) != 12 {
		return fail()
	}
	return p, spatial, temporal, nil
}
func RisingTriggerTarget(c world.Component) ([3]float64, error) {
	_, spatial, _, err := DecodeRisingSelection(c)
	if err != nil {
		return [3]float64{}, err
	}
	return IncidentTriggerTarget(spatial)
}
