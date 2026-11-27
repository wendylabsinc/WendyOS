package main

import (
	"github.com/wendylabsinc/wendy/go/world/internal/gaschange"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"sort"
	"time"
)

type changeCandidate struct {
	Gateway   string            `json:"gateway"`
	State     string            `json:"state"`
	Reason    string            `json:"reason"`
	Hash      string            `json:"sha256,omitempty"`
	Remaining float64           `json:"remaining_s"`
	Window    *gaschange.Window `json:"window,omitempty"`
	History   []gaschange.Event `json:"history"`
}
type changeSelection struct {
	State      string            `json:"state"`
	Contract   string            `json:"contract_sha256"`
	Candidates []changeCandidate `json:"candidates"`
}
type observed struct {
	hash                   string
	version                world.Version
	until                  time.Time
	state, digest, episode string
	originalUntil          float64
	history                []gaschange.Event
}
type changeReader struct {
	cfg  gaschange.Config
	seen map[string]*observed
}

func newChangeReader(c gaschange.Config) *changeReader {
	return &changeReader{c, map[string]*observed{}}
}
func (r *changeReader) step(components []world.Component, now time.Time) (changeSelection, error) {
	out := changeSelection{State: "unknown", Contract: gasreplica.Digest(r.cfg), Candidates: []changeCandidate{}}
	names := []string{}
	for n := range r.cfg.Public.Gateways {
		names = append(names, n)
	}
	sort.Strings(names)
	states := map[string]bool{}
	for _, name := range names {
		seen := r.seen[name]
		if seen == nil {
			seen = &observed{history: []gaschange.Event{}}
			r.seen[name] = seen
		}
		candidate := changeCandidate{Gateway: name, State: "unknown", Reason: "No current complete original history from this publisher.", History: seen.history}
		for _, c := range components {
			if c.Key.Type != gaschange.ClaimType || c.Key.Source != r.cfg.Public.Gateways[name] {
				continue
			}
			p, e := gaschange.ValidateClaim(r.cfg, c, now)
			if e != nil {
				candidate.Reason = e.Error()
				knownKey := c.Key.Tenant == r.cfg.Public.Tenant && c.Key.Site == r.cfg.Public.Field.Site && c.Key.Entity == r.cfg.Public.Field.Simulation.RunID+"/gas-change"
				hash := gasreplica.ComponentHash(c)
				newer := c.Version.Epoch > seen.version.Epoch || c.Version.Epoch == seen.version.Epoch && c.Version.Sequence > seen.version.Sequence
				if knownKey && (newer || c.Version == seen.version && hash != seen.hash) {
					seen.version, seen.hash, seen.until = c.Version, hash, now
				}
				continue
			}
			if c.Version.Epoch < seen.version.Epoch || c.Version.Epoch == seen.version.Epoch && c.Version.Sequence < seen.version.Sequence {
				continue
			}
			hash := gasreplica.ComponentHash(c)
			deadline := now.Add(gasreplica.Remaining(c, now))
			if c.Version == seen.version {
				if hash != seen.hash {
					candidate.Reason = "Conflicting publisher version"
					if now.Before(seen.until) {
						seen.until = now
					}
					continue
				}
				if deadline.Before(seen.until) {
					seen.until = deadline
				}
			} else {
				seen.hash, seen.version, seen.until = hash, c.Version, deadline
			}
			if !now.Before(seen.until) {
				continue
			}
			candidate.State, candidate.Hash, candidate.Remaining, candidate.Window = p.Window.State, hash, seen.until.Sub(now).Seconds(), &p.Window
			candidate.Reason = p.Window.Reason
		}
		state := candidate.State
		digest := ""
		at := r.cfg.Public.Seconds(now)
		until := at
		if candidate.Window != nil {
			digest = candidate.Window.InputDigest
			until = candidate.Window.ValidUntil
			at = 0
			for _, t := range candidate.Window.Traces {
				for _, p := range t.Points {
					at = max(at, p.Captured)
				}
			}
		}
		if state != seen.state && (digest != seen.digest || state == "unknown") {
			if state == "rising" && seen.episode == "" {
				seen.episode = gasreplica.Digest([]string{out.Contract, name, digest})
			}
			if seen.episode != "" {
				eventState := state
				if state == "unknown" || state == "mixed" {
					eventState = "interrupted"
					digest = seen.digest
					until = seen.originalUntil
				}
				seen.history = append(seen.history, gaschange.Event{ID: seen.episode, State: eventState, At: at, Witness: digest, OriginalUntil: until})
				if len(seen.history) > gaschange.MaxHistory {
					seen.history = seen.history[len(seen.history)-gaschange.MaxHistory:]
				}
				if state != "rising" {
					seen.episode = ""
				}
			}
		}
		seen.state, seen.digest = state, digest
		if candidate.Window != nil {
			seen.originalUntil = candidate.Window.ValidUntil
		}
		candidate.History = append([]gaschange.Event{}, seen.history...)
		out.Candidates = append(out.Candidates, candidate)
		if candidate.Window != nil {
			states[state] = true
		}
	}
	if len(states) == 1 {
		for state := range states {
			out.State = state
		}
	} else if len(states) > 1 {
		out.State = "mixed"
	}
	return out, nil
}
