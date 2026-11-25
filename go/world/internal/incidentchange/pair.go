package incidentchange

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/gaschange"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const PairType = "wendy.gas.spatial-change.v1"
const MaxPairBytes = 56 << 10

type Pair struct {
	Schema   string   `json:"schema"`
	Contract string   `json:"contract_sha256"`
	Gateway  string   `json:"gateway"`
	Hashes   []string `json:"member_sha256"`
	Members  [][]byte `json:"members"`
}
type PairSupport struct {
	Hash       string        `json:"sha256"`
	Source     world.Source  `json:"source"`
	Version    world.Version `json:"version"`
	GasHash    string        `json:"gas_sha256"`
	ChangeHash string        `json:"change_sha256"`
	ValidUntil time.Time     `json:"valid_until"`
}

// PairVerifier authenticates both intact origin records against the existing
// pinned World root and current revocation policy. Domain validation is separate.
type PairVerifier func([]authority.SignedRecord) error

func NewWithPairVerifier(cfg Config, verify PairVerifier) (*Session, error) {
	if verify == nil {
		return nil, errors.New("paired records require origin verification")
	}
	s, err := New(cfg)
	if err == nil {
		s.verifyPair = verify
	}
	return s, err
}
func MakePair(cfg Config, gateway string, version world.Version, members [][]byte, now time.Time, verify PairVerifier) (world.Component, error) {
	if err := cfg.Validate(); err != nil {
		return world.Component{}, err
	}
	if verify == nil || len(members) != 2 {
		return world.Component{}, errors.New("two signed originals and verifier required")
	}
	source, ok := cfg.Spatial.Gas.Gateways[gateway]
	if !ok || version.Epoch == 0 || version.Sequence == 0 {
		return world.Component{}, errors.New("commissioned paired publisher required")
	}
	records := make([]authority.SignedRecord, 2)
	for i, raw := range members {
		if err := authority.DecodeStrictLimit(raw, &records[i], 48<<10); err != nil {
			return world.Component{}, err
		}
	}
	if err := verify(records); err != nil {
		return world.Component{}, err
	}
	gas, change := records[0].Component, records[1].Component
	if gas.Key.Source != source || change.Key.Source != source || gas.Version != version || change.Version != version || gas.Key.Type != gasreplica.ClaimType || change.Key.Type != gaschange.ClaimType {
		return world.Component{}, errors.New("ordered same-source same-version pair required")
	}
	if gasreplica.Digest(records[0].Grant) != gasreplica.Digest(records[1].Grant) {
		return world.Component{}, errors.New("paired origin grants differ")
	}
	if _, err := gasreplica.ValidateClaim(cfg.Spatial.Gas, gas, now); err != nil {
		return world.Component{}, err
	}
	if _, err := gaschange.ValidateClaim(cfg.Temporal, change, now); err != nil {
		return world.Component{}, err
	}
	oldest := gas.Timing.SampleTime
	if change.Timing.SampleTime.Before(oldest) {
		oldest = change.Timing.SampleTime
	}
	until := gas.Timing.SampleTime.Add(gas.TTL)
	if bound := change.Timing.SampleTime.Add(change.TTL); bound.Before(until) {
		until = bound
	}
	for _, member := range records {
		if bound := time.UnixMilli(member.ExpiresAtUnixMS); bound.Before(until) {
			until = bound
		}
	}
	if !now.Before(until) {
		return world.Component{}, errors.New("paired original support expired")
	}
	p := Pair{PairType, gasreplica.Digest(cfg), gateway, []string{memberHash(members[0]), memberHash(members[1])}, own(members)}
	raw, err := json.Marshal(p)
	if err != nil || len(raw) > MaxPairBytes {
		return world.Component{}, errors.New("paired payload exceeds bound")
	}
	leaves := map[string]world.Input{}
	for _, c := range []world.Component{gas, change} {
		for _, leaf := range c.Provenance.Inputs {
			if prior, exists := leaves[leaf.ID]; exists && gasreplica.Digest(prior) != gasreplica.Digest(leaf) {
				return world.Component{}, errors.New("paired original ancestry conflicts")
			}
			leaves[leaf.ID] = own(leaf)
		}
	}
	keys := make([]string, 0, len(leaves))
	for id := range leaves {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	ancestry := make([]world.Input, 0, len(keys))
	for _, id := range keys {
		ancestry = append(ancestry, leaves[id])
	}
	return world.Component{Key: world.Key{Tenant: cfg.Spatial.Gas.Tenant, Site: cfg.Spatial.Gas.Field.Site, Entity: cfg.Spatial.Gas.Field.Simulation.RunID + "/spatial-change", Type: PairType, Source: source}, Version: version, Frame: cfg.Spatial.Gas.Field.Frame, Timing: world.Timing{SampleTime: oldest, ClockDomain: cfg.Spatial.Gas.Field.Simulation.ClockDomain, ErrorKnown: true}, TTL: until.Sub(oldest), Provenance: world.Provenance{Origin: world.Inferred, AppVersion: PairType, ModelID: "intact-signed-gas-pair", ModelVersion: "1", Inputs: ancestry}, Payload: raw}, nil
}
func ValidatePair(cfg Config, c world.Component, now time.Time, verify PairVerifier) (Pair, error) {
	var p Pair
	if c.Tombstone || c.Timing.AgeHint < 0 || gasreplica.Remaining(c, now) <= 0 {
		return p, errors.New("paired support unavailable")
	}
	if err := authority.DecodeStrictLimit(c.Payload, &p, MaxPairBytes); err != nil {
		return p, err
	}
	if p.Schema != PairType || p.Contract != gasreplica.Digest(cfg) || len(p.Hashes) != 2 || len(p.Members) != 2 {
		return p, errors.New("paired contract differs")
	}
	expected, err := MakePair(cfg, p.Gateway, c.Version, p.Members, now, verify)
	if err != nil {
		return p, err
	}
	if gasreplica.ComponentHash(expected) != gasreplica.ComponentHash(c) {
		return p, errors.New("paired member bytes, order, binding or original lifetime differs")
	}
	return own(p), nil
}

func memberHash(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
func (p Pair) Components() ([]world.Component, error) {
	if len(p.Members) != 2 {
		return nil, errors.New("two paired members required")
	}
	out := make([]world.Component, 2)
	for i, raw := range p.Members {
		var signed authority.SignedRecord
		if err := authority.DecodeStrictLimit(raw, &signed, 48<<10); err != nil {
			return nil, err
		}
		out[i] = signed.Component
	}
	return out, nil
}

// expandPairs admits complete pairs only. A known newer invalid carrier blocks
// its generation; old separate members cannot silently restore it afterward.
func (s *Session) expandPairs(records []world.Component, now time.Time) ([]world.Component, map[string]PairSupport) {
	pairs := map[string][]world.Component{}
	for _, c := range records {
		if c.Key.Type != PairType || c.Key.Tenant != s.cfg.Spatial.Gas.Tenant || c.Key.Site != s.cfg.Spatial.Gas.Field.Site || c.Key.Entity != s.cfg.Spatial.Gas.Field.Simulation.RunID+"/spatial-change" {
			continue
		}
		for name, source := range s.cfg.Spatial.Gas.Gateways {
			if source == c.Key.Source {
				pairs[name] = append(pairs[name], c)
			}
		}
	}
	projected := map[string][]world.Component{}
	supports := map[string]PairSupport{}
	for name, found := range pairs {
		sort.Slice(found, func(i, j int) bool { return found[i].Version.Compare(found[j].Version) < 0 })
		m := s.pairSeen[name]
		for _, c := range found {
			hash := gasreplica.ComponentHash(c)
			cmp := c.Version.Compare(m.version)
			if cmp < 0 {
				continue
			}
			if cmp == 0 && (m.blocked || m.hash != "" && m.hash != hash) {
				m.blocked = true
				m.until = now
				delete(projected, name)
				delete(supports, name)
				continue
			}
			if cmp > 0 {
				m = mark{version: c.Version, hash: hash}
				delete(projected, name)
				delete(supports, name)
			}
			p, err := ValidatePair(s.cfg, c, now, s.verifyPair)
			if err != nil {
				m.blocked = true
				m.until = now
				delete(projected, name)
				delete(supports, name)
				continue
			}
			until := now.Add(gasreplica.Remaining(c, now))
			if m.until.IsZero() || until.Before(m.until) {
				m.until = until
			}
			if m.blocked || !now.Before(m.until) {
				continue
			}
			inner, err := p.Components()
			if err != nil {
				m.blocked = true
				m.until = now
				continue
			}
			for i := range inner {
				remaining := m.until.Sub(now)
				age := inner[i].TTL - remaining
				if age > inner[i].Timing.AgeHint {
					inner[i].Timing.AgeHint = age
				}
			}
			projected[name] = inner
			s.pairMembers[name] = [2]string{gasreplica.ComponentHash(inner[0]), gasreplica.ComponentHash(inner[1])}
			supports[name] = PairSupport{hash, c.Key.Source, c.Version, gasreplica.ComponentHash(inner[0]), gasreplica.ComponentHash(inner[1]), m.until}
		}
		s.pairSeen[name] = m
	}
	// A separately delivered same-version member cannot contradict an atomic
	// carrier and be silently ignored. Latch that publisher generation closed.
	for _, c := range records {
		if c.Key.Tenant != s.cfg.Spatial.Gas.Tenant || c.Key.Site != s.cfg.Spatial.Gas.Field.Site {
			continue
		}
		index := -1
		if c.Key.Type == gasreplica.ClaimType && c.Key.Entity == s.cfg.Spatial.Gas.Field.Simulation.RunID+"/gas-source" {
			index = 0
		}
		if c.Key.Type == gaschange.ClaimType && c.Key.Entity == s.cfg.Spatial.Gas.Field.Simulation.RunID+"/gas-change" {
			index = 1
		}
		if index < 0 {
			continue
		}
		for name, source := range s.cfg.Spatial.Gas.Gateways {
			m := s.pairSeen[name]
			hashes := s.pairMembers[name]
			if c.Key.Source == source && m.version.Epoch > 0 && c.Version == m.version && hashes[index] != "" && gasreplica.ComponentHash(c) != hashes[index] {
				m.blocked = true
				m.until = now
				s.pairSeen[name] = m
				delete(projected, name)
				delete(supports, name)
			}
		}
	}
	out := make([]world.Component, 0, len(records)+4)
	for _, c := range records {
		if c.Key.Type == PairType {
			continue
		}
		superseded := false
		if c.Key.Type == gasreplica.ClaimType || c.Key.Type == gaschange.ClaimType {
			for name, source := range s.cfg.Spatial.Gas.Gateways {
				if c.Key.Source == source && s.pairSeen[name].version.Epoch > 0 && c.Version.Compare(s.pairSeen[name].version) <= 0 {
					superseded = true
				}
			}
		}
		if !superseded {
			out = append(out, c)
		}
	}
	for _, members := range projected {
		out = append(out, members...)
	}
	return out, supports
}
