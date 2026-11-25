// Package viewerbridge projects current native result reads into the existing
// HTTP World component shape. It does not publish, sign, or persist records.
package viewerbridge

import (
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/appclock"
	"github.com/wendylabsinc/wendy/go/world/production/fusioncapture"
	"github.com/wendylabsinc/wendy/go/world/production/hazardreader"
	"github.com/wendylabsinc/wendy/go/world/production/internal/derivedreader"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/peoplereader"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trackcapture"
)

var ErrUnavailable = errors.New("complete current native viewer read unavailable")

type Config struct {
	Version    int                 `json:"version"`
	Continuous bool                `json:"continuous,omitempty"`
	People     peoplereader.Config `json:"people"`
	Hazards    hazardreader.Config `json:"hazards"`
}

// Normalized requires one commissioned reader socket identity and two exact
// result sources. HTTP authorization is supplied separately by the node handler.
func (c Config) Normalized() (Config, error) {
	p, err := derivedreader.Config(c.People).Normalized()
	if err != nil {
		return c, err
	}
	h, err := derivedreader.Config(c.Hazards).Normalized()
	if err != nil {
		return c, err
	}
	pc, hc := p, h
	pc.Source, hc.Source = derivedreader.Source{}, derivedreader.Source{}
	pc.MissionFloorY, hc.MissionFloorY = nil, nil
	if c.Version != 1 || !reflect.DeepEqual(pc, hc) || p.Source == h.Source || p.MissionFloorY != nil || h.MissionFloorY != nil {
		return c, ErrUnavailable
	}
	c.People, c.Hazards = peoplereader.Config(p), hazardreader.Config(h)
	return c, nil
}

type Session struct {
	mu      sync.Mutex
	cfg     Config
	people  *peoplereader.Session
	hazards *hazardreader.Session
	clock   *appclock.Provider
	api     appv1.WorldAppServiceClient
	closed  bool
}

func New(config Config, api appv1.WorldAppServiceClient) (*Session, error) {
	c, err := config.Normalized()
	if err != nil {
		return nil, err
	}
	s := &Session{cfg: c, api: api}
	newPeople, newHazards := peoplereader.New, hazardreader.New
	if c.Continuous {
		newPeople, newHazards = peoplereader.NewContinuous, hazardreader.NewContinuous
	}
	s.people, err = newPeople(c.People, api)
	if err != nil {
		return nil, err
	}
	s.hazards, err = newHazards(c.Hazards, api)
	if err != nil {
		s.people.Close()
		return nil, err
	}
	p := c.People
	s.clock, err = appclock.New(api, appclock.Config{Tenant: p.Tenant, Site: p.Site, Device: p.Device, App: p.App, Service: p.Service, ExpectedGeneration: p.ExpectedGeneration, ExecutionDomain: "simulation", SampleClockDomain: "simulation:" + p.RunID, MaxRoundTrip: min(time.Duration(p.RequestTimeoutMS)*time.Millisecond, appv1.MaxClockResponseAge), MaxWidth: time.Duration(p.ClockMaxWidthMS) * time.Millisecond, DriftPPM: 100})
	if err != nil {
		s.people.Close()
		s.hazards.Close()
		return nil, err
	}
	return s, nil
}

func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return errors.Join(s.people.Close(), s.hazards.Close(), s.clock.Close())
}

// Query starts no background polling and keeps no last-good snapshot. A failed
// or startup-waiting layer makes this complete read unavailable, not empty.
// Both readers retain their existing high-water and source-incarnation fences.
func (s *Session) Query(ctx context.Context) ([]world.Component, error) {
	if s == nil || ctx == nil {
		return nil, ErrUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var p peoplereader.Result
	var h hazardreader.Result
	var pe, he error
	var wait sync.WaitGroup
	wait.Add(2)
	go func() { defer wait.Done(); p, pe = s.people.Step(ctx) }()
	go func() { defer wait.Done(); h, he = s.hazards.Step(ctx) }()
	wait.Wait()
	if pe != nil || he != nil || p.Status != "ready" || h.Status != "ready" || p.Clock == nil || h.Clock == nil || !p.OriginAttestedInference || !h.OriginAttestedInference {
		return nil, ErrUnavailable
	}
	// One existing appclock observation closes the gap between the two complete
	// reads. It does not replace either original sample or physical deadline.
	now, err := s.clock.Observe(ctx)
	if err != nil {
		return nil, err
	}
	now, err = combineClocks(now, p.Clock, h.Clock)
	if err != nil {
		return nil, err
	}
	out := []world.Component{}
	for _, item := range p.Tracks {
		c, live, e := project(item.EnvelopeJSON, item.EnvelopeSHA256, item.RecordSHA256, item.Version, now, trackcapture.ResultSchema)
		if e != nil {
			return nil, e
		}
		if live {
			out = append(out, c)
		}
	}
	for _, item := range h.Hazards {
		c, live, e := project(item.EnvelopeJSON, item.EnvelopeSHA256, item.RecordSHA256, item.Version, now, "wendy.hazard.v1")
		if e != nil {
			return nil, e
		}
		if live {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key.Entity < out[j].Key.Entity })
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func digest(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }

func combineClocks(now appclock.Observation, clocks ...*derivedreader.Clock) (appclock.Observation, error) {
	latest := now.Physical.Latest
	for _, c := range clocks {
		// Compare each read with the untouched final observation. Widening its
		// upper bound first could hide a later reader's disjoint interval.
		if c == nil || c.ServerBootID != now.ServerBootID || c.DeploymentGeneration != now.DeploymentGeneration || c.SampleClockDomain != now.SampleClockDomain || c.SampleTime.After(now.SampleTime) || now.Physical.Latest.Before(c.PhysicalEarliest) {
			return now, ErrUnavailable
		}
		if c.PhysicalLatest.After(latest) {
			latest = c.PhysicalLatest
		}
	}
	// A narrower final interval cannot undo an upper bound already used by a
	// reader. This only shortens the originals' remaining physical validity.
	now.Physical.Latest = latest
	return now, nil
}

// project is only called with an original returned by the existing verified
// readers. It checks byte correspondence and final bounds; it does not infer a
// VerifiedRecord from untrusted HTTP data or redo the readers' signature work.
func project(raw []byte, envelopeHash, recordHash string, version world.Version, now appclock.Observation, schema string) (world.Component, bool, error) {
	var signed serviceorigin.SignedRecord
	var record serviceorigin.Record
	var delegation serviceorigin.DelegationClaims
	if len(raw) == 0 || len(raw) > 256<<10 || digest(raw) != envelopeHash || authority.DecodeStrictLimit(raw, &signed, 256<<10) != nil || digest(signed.Record) != recordHash || jws.DecodeStrict(signed.Record, &record) != nil || jws.DecodeStrict(signed.Delegation.Claims, &delegation) != nil || record.Epoch != version.Epoch || record.Sequence != version.Sequence || record.DelegationSHA256 != digest(signed.Delegation.Claims) || len(signed.Delegation.ChainDER) == 0 {
		return world.Component{}, false, ErrUnavailable
	}
	leaf, err := x509.ParseCertificate(signed.Delegation.ChainDER[0])
	if err != nil {
		return world.Component{}, false, ErrUnavailable
	}
	p := record.Publication
	var sampleUntil, physicalUntil time.Time
	switch schema {
	case trackcapture.ResultSchema:
		v, e := trackcapture.ValidatePublication(p)
		if e != nil {
			return world.Component{}, false, e
		}
		sampleUntil, physicalUntil = v.ValidUntil, v.PhysicalValidUntil()
	case "wendy.hazard.v1":
		v, e := fusioncapture.ValidateReplicaPublication(p)
		if e != nil {
			return world.Component{}, false, e
		}
		sampleUntil, physicalUntil = v.SampleValidUntil, v.PhysicalValidUntil
	default:
		return world.Component{}, false, ErrUnavailable
	}
	if p.Timing.ClockDomain != now.SampleClockDomain || p.Timing.SampleTime.After(now.SampleTime) || now.Physical.Earliest.UnixMilli() < record.IssuedAtUnixMS || now.Physical.Earliest.UnixMilli() < delegation.IssuedAtUnixMS || now.Physical.Earliest.Before(leaf.NotBefore) {
		return world.Component{}, false, ErrUnavailable
	}
	until := p.Timing.SampleTime.Add(p.TTL - max(p.Timing.ErrorBound, p.Timing.AgeHint))
	if !now.SampleTime.Before(until) || !now.SampleTime.Before(sampleUntil) || !now.Physical.Latest.Before(physicalUntil) || now.Physical.Latest.UnixMilli() >= record.ExpiresAtUnixMS || now.Physical.Latest.UnixMilli() >= delegation.ExpiresAtUnixMS || !now.Physical.Latest.Before(leaf.NotAfter) {
		return world.Component{}, false, nil
	}
	// This is the same owned projection as VerifiedRecord.Component, after the
	// reader has performed that record's current verification. Payload bytes and
	// all original timing/source/version/provenance fields remain unchanged.
	c := serviceorigin.VerifiedRecord{Record: record}.Component()
	return c, true, nil
}
