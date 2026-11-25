package gaschange

import (
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const MaxClaimBytes = 40 << 10

type Claim struct {
	Schema   string            `json:"schema"`
	Contract string            `json:"contract_sha256"`
	Gateway  string            `json:"gateway"`
	Window   Window            `json:"window"`
	Inputs   []world.Component `json:"inputs"`
}

func MakeClaim(cfg Config, gateway string, version world.Version, r Result) (world.Component, error) {
	source, ok := cfg.Public.Gateways[gateway]
	if !ok || version.Epoch == 0 || version.Sequence == 0 {
		return world.Component{}, errors.New("commissioned publisher and durable version required")
	}
	w := r.Window
	if w.State == "unknown" || w.State == "warming" || w.ValidUntil <= w.Evaluated {
		return world.Component{}, errors.New("incomplete history cannot publish a current change claim")
	}
	reproduced, e := Evaluate(cfg, r.Inputs, cfg.Public.Instant(w.Evaluated))
	if e != nil {
		return world.Component{}, e
	}
	if w.ValidUntil > reproduced.ValidUntil {
		return world.Component{}, errors.New("claim exceeds original support")
	}
	reproduced.ValidUntil = w.ValidUntil
	if gasreplica.Digest(reproduced) != gasreplica.Digest(w) {
		return world.Component{}, errors.New("window differs from originals")
	}
	inputs := clone(r.Inputs)
	sort.Slice(inputs, func(i, j int) bool {
		a, b := inputs[i], inputs[j]
		if a.Key.Entity != b.Key.Entity {
			return a.Key.Entity < b.Key.Entity
		}
		return a.Timing.SampleTime.Before(b.Timing.SampleTime)
	})
	oldest := cfg.Public.Instant(w.Evaluated)
	ancestry := []world.Input{}
	for _, c := range inputs {
		if c.Timing.SampleTime.Before(oldest) {
			oldest = c.Timing.SampleTime
		}
		sim := cfg.Public.Field.Simulation
		ancestry = append(ancestry, world.Input{ID: "sha256:" + gasreplica.ComponentHash(c), Origin: world.Simulated, Simulation: &sim})
	}
	p := Claim{ClaimType, gasreplica.Digest(cfg), gateway, w, inputs}
	raw, e := json.Marshal(p)
	if e != nil || len(raw) > MaxClaimBytes {
		return world.Component{}, errors.New("bounded claim exceeds 40 KiB payload; complete signed envelope must also pass transport bound")
	}
	return world.Component{Key: world.Key{Tenant: cfg.Public.Tenant, Site: cfg.Public.Field.Site, Entity: cfg.Public.Field.Simulation.RunID + "/gas-change", Type: ClaimType, Source: source}, Version: version, Frame: cfg.Public.Field.Frame, Timing: world.Timing{SampleTime: oldest, ClockDomain: cfg.Public.Field.Simulation.ClockDomain, ErrorKnown: true}, TTL: cfg.Public.Instant(w.ValidUntil).Sub(oldest), Provenance: world.Provenance{Origin: world.Inferred, AppVersion: Model, ModelID: Model, ModelVersion: "1", Inputs: ancestry}, Payload: raw}, nil
}
func ValidateClaim(cfg Config, c world.Component, now time.Time) (Claim, error) {
	var p Claim
	if c.Tombstone || c.Key.Type != ClaimType || gasreplica.Remaining(c, now) <= 0 || c.Timing.AgeHint < 0 {
		return p, errors.New("change claim unavailable or expired")
	}
	if e := authority.DecodeStrictLimit(c.Payload, &p, MaxClaimBytes); e != nil {
		return p, e
	}
	if p.Schema != ClaimType || p.Contract != gasreplica.Digest(cfg) || !finite(p.Window.Evaluated) || !finite(p.Window.ValidUntil) || p.Window.Evaluated > cfg.Public.Seconds(now) || p.Window.ValidUntil <= cfg.Public.Seconds(now) {
		return p, errors.New("change contract or original lifetime differs")
	}
	want, e := MakeClaim(cfg, p.Gateway, c.Version, Result{Window: p.Window, Inputs: p.Inputs})
	if e != nil {
		return p, e
	}
	if gasreplica.ComponentHash(want) != gasreplica.ComponentHash(c) {
		return p, errors.New("claim metadata, provenance or original window differs")
	}
	return clone(p), nil
}
