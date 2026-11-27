package replicastore

import (
	"context"
	"encoding/hex"
	"reflect"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/replication"
	"github.com/wendylabsinc/wendy/go/world/production/rpctrace"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/servicejournal"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/sourceauthority"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

func digestValid(v string) bool {
	b, e := hex.DecodeString(v)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == v
}
func bounded(v string) bool {
	return v != "" && len(v) <= 256 && utf8.ValidString(v) && strings.TrimSpace(v) == v && strings.IndexFunc(v, unicode.IsControl) < 0
}
func open(cfg Config, reopen bool) (*Store, error) {
	local, e := serviceaccess.DeviceTenant(cfg.Endpoint.Device)
	peer, pe := serviceaccess.DeviceTenant(cfg.Peer.Device)
	if e != nil || pe != nil || local != cfg.Endpoint.Tenant || peer != local || cfg.Peer.Device == cfg.Endpoint.Device || !digestValid(cfg.Peer.BindingSHA256) || cfg.Generation == 0 || cfg.Local == nil || cfg.Authority == nil || cfg.Clock == nil || cfg.Certificates == nil || cfg.Guard == nil || cfg.Authorize == nil || cfg.AdmitSchema == nil || cfg.Replica.Tenant != local || cfg.Replica.Site != cfg.Endpoint.Site || cfg.Replica.ClockDomain != cfg.Endpoint.SampleClockDomain || cfg.Replica.Now == nil || len(cfg.Replica.Frames) == 0 {
		return nil, ErrDenied
	}
	if !bounded(cfg.Endpoint.Site) || !bounded(cfg.Endpoint.SampleClockDomain) || cfg.Endpoint.ExecutionDomain != "physical" && cfg.Endpoint.ExecutionDomain != "simulation" || cfg.Replica.AllowSynthetic != (cfg.Endpoint.ExecutionDomain == "simulation") || cfg.Endpoint.ExecutionDomain == "simulation" && (!strings.HasPrefix(cfg.Endpoint.SampleClockDomain, "simulation:") || cfg.Endpoint.SampleClockDomain == "simulation:") || cfg.Endpoint.ExecutionDomain == "physical" && (strings.HasPrefix(cfg.Endpoint.SampleClockDomain, "simulation:") || strings.HasPrefix(cfg.Endpoint.SampleClockDomain, "replay:")) {
		return nil, ErrDenied
	}
	cfg.Import = append([]proofref.Source(nil), cfg.Import...)
	cfg.Export = append([]proofref.Source(nil), cfg.Export...)
	cfg.Replica.Frames = append([]world.FrameRef(nil), cfg.Replica.Frames...)
	s := &Store{cfg: cfg}
	if len(cfg.Import)+len(cfg.Export) == 0 || !s.sourceList(cfg.Import, cfg.Peer.Device) || !s.sourceList(cfg.Export, cfg.Endpoint.Device) {
		return nil, ErrDenied
	}
	verifier, e := serviceorigin.NewVerifier(serviceorigin.VerifierConfig{Tenant: local, Site: cfg.Endpoint.Site, ExecutionDomain: cfg.Endpoint.ExecutionDomain, Certificates: cfg.Certificates, Clock: cfg.Clock, Authorize: func(ctx context.Context, c serviceorigin.DelegationClaims, id wendycert.DeviceIdentity) error {
		if !s.inside(ctx) || !s.scopeAllowed(c.Scope) || id.Principal != cfg.Peer.Device {
			return ErrDenied
		}
		if _, e := s.check(ctx, replication.Import, s.scopeSources(c.Scope)); e != nil {
			return e
		}
		return cfg.Authority.AuthorizeDelegation(ctx, c, id)
	}})
	if e != nil {
		return nil, e
	}
	rc := servicejournal.RemoteConfig{Path: cfg.RemotePath, LocalDevice: cfg.Endpoint.Device, Replica: cfg.Replica, Verifier: verifier, PhysicalClock: cfg.Clock, Guard: s.journalGuard, Authorize: s.authorizeRecord, AdmitSchema: s.schema, MaxEntries: cfg.MaxEntries, MaxProofBytes: cfg.MaxProofBytes}
	if reopen {
		s.remote, e = servicejournal.OpenRemote(rc)
	} else {
		s.remote, e = servicejournal.CreateRemote(rc)
	}
	if e != nil {
		return nil, e
	}
	return s, nil
}
func (s *Store) sourceList(list []proofref.Source, device string) bool {
	if len(list) > 64 {
		return false
	}
	seen := map[proofref.Source]bool{}
	for _, v := range list {
		if v.Validate() != nil || v.Tenant != s.cfg.Endpoint.Tenant || v.Site != s.cfg.Endpoint.Site || v.Device != device || !replication.SupportedComponent(v.Component) || seen[v] {
			return false
		}
		seen[v] = true
	}
	return true
}
func (s *Store) sourcesAllowed(d replication.Direction, wanted []proofref.Source) bool {
	list := s.cfg.Import
	device := s.cfg.Peer.Device
	if d == replication.Export {
		list = s.cfg.Export
		device = s.cfg.Endpoint.Device
	} else if d != replication.Import {
		return false
	}
	if len(wanted) == 0 {
		return len(list) == 0
	}
	if !s.sourceList(wanted, device) {
		return false
	}
	for _, w := range wanted {
		found := false
		for _, v := range list {
			if w == v {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func (s *Store) scopeSources(scope serviceorigin.Scope) []proofref.Source {
	out := make([]proofref.Source, 0, len(scope.Publish))
	for _, p := range scope.Publish {
		out = append(out, proofref.Source{Tenant: scope.Tenant, Site: scope.Site, Device: scope.Device, App: scope.App, Service: scope.Service, Component: p.ComponentType, Stream: p.Stream})
	}
	return out
}
func (s *Store) scopeAllowed(scope serviceorigin.Scope) bool {
	return scope.Validate() == nil && len(scope.Publish) != 0 && scope.ExecutionDomain == s.cfg.Endpoint.ExecutionDomain && s.sourcesAllowed(replication.Import, s.scopeSources(scope))
}
func (s *Store) schema(ctx context.Context, v serviceorigin.VerifiedRecord) error {
	if !s.inside(ctx) || !s.scopeAllowed(v.Record.Scope) {
		return ErrDenied
	}
	p := v.Record.Publication
	if !replication.SupportedComponent(p.ComponentType) || p.Timing.ClockDomain != s.cfg.Endpoint.SampleClockDomain || !p.Timing.ErrorKnown {
		return ErrDenied
	}
	if _, e := replication.RecordInterpretationUntil(v.Record); e != nil {
		return ErrDenied
	}
	found := false
	for _, f := range s.cfg.Replica.Frames {
		if f == p.Frame {
			found = true
			break
		}
	}
	if !found {
		return ErrDenied
	}
	if p.Tombstone {
		if len(p.Payload) != 0 {
			return ErrDenied
		}
	} else if p.ComponentType == replication.ScalarComponent {
		var scalar world.ScalarSample
		if authority.DecodeStrictLimit(p.Payload, &scalar, serviceorigin.MaxPayloadBytes) != nil {
			return ErrDenied
		}
		o := world.Observation{ID: p.Entity, Tenant: v.Record.Scope.Tenant, Site: v.Record.Scope.Site, Source: world.Source{Device: v.Record.Scope.Device, App: v.Record.Scope.App, Service: v.Record.Scope.Service, Stream: p.Stream}, Frame: p.Frame, Timing: p.Timing, Provenance: p.Provenance, Scalar: &scalar}
		if o.Validate() != nil {
			return ErrDenied
		}
	}
	return s.cfg.AdmitSchema(ctx, v)
}
func (s *Store) authorizeRecord(ctx context.Context, v serviceorigin.VerifiedRecord, w trustedtime.Interval) (servicejournal.AuthorityWindow, error) {
	if !s.inside(ctx) || !s.scopeAllowed(v.Record.Scope) {
		rpctrace.Add(ctx, rpctrace.StageRemotePeerGrantOmitted, 0)
		return servicejournal.AuthorityWindow{}, ErrDenied
	}
	grant, e := s.check(ctx, replication.Import, s.scopeSources(v.Record.Scope))
	if e != nil {
		rpctrace.Add(ctx, rpctrace.StageRemotePeerGrantOmitted, 0)
		return grant, e
	}
	lease, e := s.cfg.Authority.CurrentLease(ctx, sourceauthority.SourceOf(v.Record.Scope))
	if e != nil {
		rpctrace.Add(ctx, rpctrace.StageRemoteSourceAuthorityOmitted, 0)
		return grant, e
	}
	raw, e := sourceauthority.EncodeLease(lease)
	if e != nil {
		rpctrace.Add(ctx, rpctrace.StageRemoteSourceAuthorityOmitted, 0)
		return grant, e
	}
	_, c, e := sourceauthority.DecodeLease(raw)
	if e != nil {
		rpctrace.Add(ctx, rpctrace.StageRemoteSourceAuthorityOmitted, 0)
		return grant, e
	}
	if !reflect.DeepEqual(c.Scope, v.Record.Scope) || c.SampleClockDomain != s.cfg.Endpoint.SampleClockDomain {
		rpctrace.Add(ctx, rpctrace.StageRemoteSourceAuthorityOmitted, 0)
		return grant, ErrDenied
	}
	if e = s.cfg.Authority.AuthorizeRecord(ctx, lease, v); e != nil {
		rpctrace.Add(ctx, rpctrace.StageRemoteSourceAuthorityOmitted, 0)
		return grant, e
	}
	from, until := time.UnixMilli(c.NotBeforeUnixMS), time.UnixMilli(c.NotAfterUnixMS)
	if grant.NotBefore.Before(from) {
		grant.NotBefore = from
	}
	if until.Before(grant.NotAfter) {
		grant.NotAfter = until
	}
	var interpretationCap time.Time
	if ctx.Value(snapshotContextKey{}) == s {
		// Only live reads apply the original interpretation deadline. Repair
		// still retains expired exact originals as non-live version high-water.
		cap, e := replication.RecordInterpretationUntil(v.Record)
		if e != nil {
			rpctrace.Add(ctx, rpctrace.StageRemoteInterpretationOmitted, 0)
			return grant, ErrDenied
		}
		interpretationCap = cap
		if !cap.IsZero() && cap.Before(grant.NotAfter) {
			grant.NotAfter = cap
		}
	}
	if !windowCurrent(grant, w) {
		// Classify only this existing refusal, using its already observed
		// interval and original cap. Do not obtain another clock/authority read.
		if !interpretationCap.IsZero() && !w.Latest.Before(interpretationCap) {
			rpctrace.Add(ctx, rpctrace.StageRemoteInterpretationOmitted, 0)
		} else {
			rpctrace.Add(ctx, rpctrace.StageRemoteGrantWindowOmitted, 0)
		}
		return grant, ErrDenied
	}
	return grant, nil
}
