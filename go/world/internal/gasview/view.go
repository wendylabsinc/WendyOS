// Package gasview selects one intact gas claim. It does not merge regions,
// increase confidence, or compare gas quality with camera/LiDAR estimates.
package gasview

import (
	"errors"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/gasfield"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type Candidate struct {
	Gateway   string        `json:"gateway"`
	State     string        `json:"state"`
	Reason    string        `json:"reason"`
	Hash      string        `json:"sha256,omitempty"`
	Version   world.Version `json:"version"`
	Remaining float64       `json:"remaining_s"`
	Coverage  int           `json:"coverage"`
	Radius    float64       `json:"radius_m"`
}
type Selection struct {
	State      string          `json:"state"`
	Reason     string          `json:"reason"`
	Gateway    string          `json:"gateway,omitempty"`
	Hash       string          `json:"sha256,omitempty"`
	Version    world.Version   `json:"version"`
	Contract   string          `json:"contract_sha256"`
	Candidates []Candidate     `json:"candidates"`
	Result     gasfield.Result `json:"result"`
}
type retained struct {
	version  world.Version
	hash     string
	deadline time.Time
}
type Selector struct {
	mu             sync.Mutex
	cfg            gasreplica.Config
	latest         map[string]retained
	selected       string
	challenger     string
	challengeAt    time.Time
	challengeInput string
	last           time.Time
}

func New(cfg gasreplica.Config) (*Selector, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Selector{cfg: cfg, latest: map[string]retained{}}, nil
}
func (s *Selector) Step(components []world.Component, now time.Time) (Selection, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(components) > 256 || now.Before(s.last) {
		return Selection{}, errors.New("snapshot bound or clock regression")
	}
	s.last = now
	base, _, _, err := gasreplica.Evaluate(s.cfg, nil, now)
	if err != nil {
		return Selection{}, err
	}
	out := Selection{State: "unavailable", Reason: "No fresh compatible gateway claim", Contract: s.cfg.ContractDigest(), Candidates: []Candidate{}, Result: base}
	names := []string{}
	for name := range s.cfg.Gateways {
		names = append(names, name)
	}
	sort.Strings(names)
	valid := map[string]gasreplica.Validated{}
	for _, name := range names {
		d := Candidate{Gateway: name, State: "unavailable", Reason: "No current claim in this replica"}
		found := []world.Component{}
		for _, c := range components {
			if c.Key.Type == gasreplica.ClaimType && c.Key.Source == s.cfg.Gateways[name] {
				found = append(found, c)
			}
		}
		if len(found) > 1 {
			d.Reason = "Multiple records for the configured gateway"
		} else if len(found) == 1 {
			c := found[0]
			v, e := gasreplica.ValidateClaim(s.cfg, c, now)
			if e != nil {
				d.Reason = e.Error()
			} else {
				old, ok := s.latest[name]
				order := c.Version.Compare(old.version)
				if ok && (order < 0 || order == 0 && old.hash != v.Hash) {
					d.Reason = "Reordered or conflicting gateway version"
				} else {
					deadline := now.Add(time.Duration(v.RemainingS * float64(time.Second)))
					if ok && order == 0 && old.deadline.Before(deadline) {
						deadline = old.deadline
					}
					s.latest[name] = retained{c.Version, v.Hash, deadline}
					if !deadline.After(now) {
						d.Reason = "Original claim expired locally"
					} else {
						v.RemainingS = deadline.Sub(now).Seconds()
						v.Result.Estimate.ValidUntil = min(v.Result.Estimate.ValidUntil, v.Result.Now+v.RemainingS)
						d = Candidate{Gateway: name, State: v.Claim.State, Reason: v.Result.Reason, Hash: v.Hash, Version: c.Version, Remaining: v.RemainingS, Coverage: v.Result.Coverage, Radius: v.Result.Estimate.Radius}
						valid[name] = v
					}
				}
			}
		}
		out.Candidates = append(out.Candidates, d)
	}
	if len(valid) == 2 {
		a, b := valid[names[0]], valid[names[1]]
		if a.Claim.State == "localized" && b.Claim.State == "localized" && math.Hypot(a.Result.Estimate.Position[0]-b.Result.Estimate.Position[0], a.Result.Estimate.Position[1]-b.Result.Estimate.Position[1]) > a.Result.Estimate.Radius+b.Result.Estimate.Radius {
			out.State = "disputed"
			out.Reason = "Fresh gateway source regions disagree; neither is selected"
			out.Result.Reason = out.Reason
			s.selected = ""
			s.challenger = ""
			return out, nil
		}
	}
	better := func(a, b string) bool {
		va, vb := valid[a], valid[b]
		if va.Claim.State != vb.Claim.State {
			return va.Claim.State == "localized"
		}
		if va.Result.Estimate.Radius != vb.Result.Estimate.Radius {
			return va.Result.Estimate.Radius < vb.Result.Estimate.Radius
		}
		return a < b
	}
	best := ""
	for _, name := range names {
		if _, ok := valid[name]; ok && (best == "" || better(name, best)) {
			best = name
		}
	}
	if best == "" {
		s.selected = ""
		s.challenger = ""
		return out, nil
	}
	current, alive := valid[s.selected]
	if !alive || current.Claim.State != "localized" && valid[best].Claim.State == "localized" {
		s.selected = best
		s.challenger = ""
	} else if best != s.selected && valid[best].Result.Estimate.Radius < current.Result.Estimate.Radius*.8 {
		challenger := valid[best]
		if s.challenger != best {
			s.challenger = best
			s.challengeAt = now
			s.challengeInput = challenger.Claim.InputDigest
		} else if now.Sub(s.challengeAt) >= time.Second && challenger.Claim.InputDigest != s.challengeInput {
			s.selected = best
			s.challenger = ""
		}
	} else {
		s.challenger = ""
	}
	v := valid[s.selected]
	out.State = v.Claim.State
	out.Reason = "Selected intact compatible claim; independent claims are not fused"
	if best != s.selected {
		out.Reason = "Local incumbent retained; a smaller challenger needs sustained fresh evidence"
	}
	out.Gateway = s.selected
	out.Hash = v.Hash
	out.Version = v.Component.Version
	out.Result = v.Result
	return out, nil
}
