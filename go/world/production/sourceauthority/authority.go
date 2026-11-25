package sourceauthority

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/mldsa"
	"encoding/json"
	"math"
	"reflect"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

func (s *Store) leaseAllowed(c LeaseClaims, raw []byte) error {
	r := s.state.Sources[key(SourceOf(c.Scope))]
	if c.AuthorityRevision <= r.ThroughRevision || c.Epoch < r.MinimumEpoch || c.Epoch < r.Epoch || c.AuthorityRevision < r.LeaseRevision {
		return ErrReplay
	}
	if c.Epoch == r.Epoch && r.Epoch != 0 && binding(c) != r.BindingSHA256 {
		return ErrReplay
	}
	if c.AuthorityRevision == r.LeaseRevision && !bytes.Equal(raw, r.Lease) {
		return ErrReplay
	}
	return nil
}
func (s *Store) verifyLease(ctx context.Context, raw []byte) (LeaseClaims, error) {
	w, e := s.now(ctx)
	if e != nil {
		return LeaseClaims{}, e
	}
	_, c, id, e := s.leaseCrypto(raw, w)
	if e != nil {
		return c, e
	}
	if e = s.leaseAllowed(c, raw); e != nil {
		return c, e
	}
	for i := 0; i < 2; i++ {
		if e = s.cfg.AuthorizeLease(ctx, cloneLease(c), id); e != nil {
			return c, ErrDenied
		}
		w, e = s.now(ctx)
		if e != nil {
			return c, e
		}
		if !within(w, c.NotBeforeUnixMS, c.NotAfterUnixMS) {
			return c, ErrExpired
		}
	}
	return c, nil
}
func (s *Store) verifyFloor(ctx context.Context, raw []byte) (FloorClaims, error) {
	w, e := s.now(ctx)
	if e != nil {
		return FloorClaims{}, e
	}
	_, c, id, e := s.floorCrypto(raw, w)
	if e != nil {
		return c, e
	}
	r := s.state.Sources[key(c.Source)]
	if c.AttestationRevision < r.FloorRevision || c.ThroughRevision < r.ThroughRevision || c.MinimumEpoch < r.MinimumEpoch || c.AttestationRevision == r.FloorRevision && !bytes.Equal(raw, r.Floor) {
		return c, ErrReplay
	}
	for i := 0; i < 2; i++ {
		if e = s.cfg.AuthorizeFloor(ctx, c, id); e != nil {
			return c, ErrDenied
		}
		w, e = s.now(ctx)
		if e != nil {
			return c, e
		}
		if !within(w, c.NotBeforeUnixMS, c.NotAfterUnixMS) {
			return c, ErrExpired
		}
	}
	return c, nil
}
func (s *Store) begin(ctx context.Context) error {
	if ctx == nil {
		return ErrDenied
	}
	if e := s.usable(); e != nil {
		return e
	}
	return ctx.Err()
}

// AcceptLease durably admits only a currently authorized original envelope.
// It does not admit any observation, extend a sample, or reserve a local epoch.
func (s *Store) AcceptLease(ctx context.Context, p SignedLease) error {
	if s == nil {
		return ErrClosed
	}
	raw, e := EncodeLease(p)
	if e != nil {
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e = s.begin(ctx); e != nil {
		return e
	}
	return s.acceptLease(ctx, raw)
}
func (s *Store) acceptLease(ctx context.Context, raw []byte) error {
	c, e := s.verifyLease(ctx, raw)
	if e != nil {
		return e
	}
	src := SourceOf(c.Scope)
	r := s.state.Sources[key(src)]
	r.Source = src
	r.Revision = max(r.Revision, c.AuthorityRevision)
	r.LeaseRevision = c.AuthorityRevision
	r.Epoch = c.Epoch
	r.BindingSHA256 = binding(c)
	r.Lease = bytes.Clone(raw)
	d := s.next()
	d.Sources[key(src)] = r
	return s.persist(ctx, d, func() error { _, e := s.verifyLease(ctx, raw); return e })
}
func (s *Store) AcceptFloor(ctx context.Context, p SignedFloor) error {
	if s == nil {
		return ErrClosed
	}
	raw, e := EncodeFloor(p)
	if e != nil {
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e = s.begin(ctx); e != nil {
		return e
	}
	return s.acceptFloor(ctx, raw)
}
func (s *Store) acceptFloor(ctx context.Context, raw []byte) error {
	c, e := s.verifyFloor(ctx, raw)
	if e != nil {
		return e
	}
	r := s.state.Sources[key(c.Source)]
	r.Source = c.Source
	r.Revision = max(r.Revision, c.AttestationRevision)
	r.FloorRevision = c.AttestationRevision
	r.ThroughRevision = c.ThroughRevision
	r.MinimumEpoch = c.MinimumEpoch
	r.Floor = bytes.Clone(raw)
	d := s.next()
	d.Sources[key(c.Source)] = r
	return s.persist(ctx, d, func() error { _, e := s.verifyFloor(ctx, raw); return e })
}

func (s *Store) reserve(ctx context.Context, src Source) (uint64, error) {
	if src.Validate() != nil || src.Tenant != s.cfg.Tenant {
		return 0, ErrDenied
	}
	r := s.state.Sources[key(src)]
	if r.Revision == math.MaxUint64 {
		return 0, ErrCapacity
	}
	r.Source = src
	r.Revision++
	d := s.next()
	d.Sources[key(src)] = r
	if e := s.persist(ctx, d, nil); e != nil {
		return 0, e
	}
	return r.Revision, nil
}
func ownSigner(in Signer) (Signer, error) {
	if in.DeviceKey == nil || in.DeviceKey.PublicKey().Parameters() != mldsa.MLDSA65() || !chainBounds(in.ChainDER) {
		return Signer{}, ErrDenied
	}
	out := in
	out.ChainDER = make([][]byte, len(in.ChainDER))
	for i, b := range in.ChainDER {
		out.ChainDER[i] = bytes.Clone(b)
	}
	return out, nil
}
func (s *Store) signer(in Signer, src Source, w trustedtime.Interval) (wendycert.DeviceIdentity, error) {
	id, e := s.cfg.Certificates.Verify(in.ChainDER, w.Earliest, w.Latest)
	if e != nil || id.Principal != src.Device || id.Tenant != src.Tenant {
		return id, ErrDenied
	}
	return id, nil
}
func (s *Store) delegation(p serviceorigin.SignedDelegation, w trustedtime.Interval) (serviceorigin.DelegationClaims, error) {
	var c serviceorigin.DelegationClaims
	if canonical(p.Claims, &c, serviceorigin.MaxClaimsBytes) != nil || c.Version != serviceorigin.Version || c.AuthorityDomain != "physical" || c.Scope.Validate() != nil || len(c.Scope.Publish) == 0 || c.Epoch == 0 || len(c.PublicKey) != ed25519.PublicKeySize || c.ExpiresAtUnixMS <= c.IssuedAtUnixMS || c.ExpiresAtUnixMS-c.IssuedAtUnixMS > serviceorigin.MaxLifetime.Milliseconds() || !within(w, c.IssuedAtUnixMS, c.ExpiresAtUnixMS) || !chainBounds(p.ChainDER) || len(p.Signature) != mldsa.MLDSA65SignatureSize {
		return c, ErrDenied
	}
	if c.Scope.Tenant != s.cfg.Tenant || c.Scope.Site != s.cfg.Site || c.Scope.ExecutionDomain != s.cfg.ExecutionDomain {
		return c, ErrDenied
	}
	id, e := s.cfg.Certificates.Verify(p.ChainDER, time.UnixMilli(c.IssuedAtUnixMS), time.UnixMilli(c.ExpiresAtUnixMS))
	if e != nil || id.Principal != c.Scope.Device || id.Tenant != c.Scope.Tenant || id.VerifySignature(append([]byte(delegationPurpose), p.Claims...), p.Signature) != nil {
		return c, ErrDenied
	}
	// Both delegation and lease certificate paths are presented to the same
	// current policy callback, so renewing one cannot hide a denied old key.
	return c, nil
}

// IssueLease reserves its durable revision before signing. The host must hold
// the origin identity and exact service lifecycle fence through this call.
// A consumed revision may leave a gap; it is never reused after failure.
func (s *Store) IssueLease(ctx context.Context, signer Signer, req LeaseRequest) (SignedLease, error) {
	if s == nil {
		return SignedLease{}, ErrClosed
	}
	signer, e := ownSigner(signer)
	if e != nil {
		return SignedLease{}, e
	}
	if req.Lifetime < time.Millisecond || req.Lifetime > MaxLifetime {
		return SignedLease{}, ErrDenied
	}
	b, e := json.Marshal(req.Delegation)
	if e != nil {
		return SignedLease{}, ErrDenied
	}
	var delegation serviceorigin.SignedDelegation
	if canonical(b, &delegation, MaxEnvelopeBytes) != nil {
		return SignedLease{}, ErrDenied
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e = s.begin(ctx); e != nil {
		return SignedLease{}, e
	}
	w, e := s.now(ctx)
	if e != nil {
		return SignedLease{}, e
	}
	c, e := s.delegation(delegation, w)
	if e != nil {
		return SignedLease{}, e
	}
	src := SourceOf(c.Scope)
	if _, e = s.signer(signer, src, w); e != nil {
		return SignedLease{}, e
	}
	revision, e := s.reserve(ctx, src)
	if e != nil {
		return SignedLease{}, e
	}
	w, e = s.now(ctx)
	if e != nil {
		return SignedLease{}, e
	}
	lease := LeaseClaims{Version: Version, AuthorityDomain: "physical", Scope: c.Scope, SampleClockDomain: s.cfg.SampleClockDomain, Epoch: c.Epoch, DelegationSHA256: hash(delegation.Claims), AuthorityRevision: revision, NotBeforeUnixMS: w.Earliest.UnixMilli(), NotAfterUnixMS: min(w.Earliest.Add(req.Lifetime).UnixMilli(), c.ExpiresAtUnixMS)}
	claims, e := encode(lease, MaxClaimsBytes)
	if e != nil || lease.Validate() != nil {
		return SignedLease{}, ErrDenied
	}
	sig, e := signer.DeviceKey.Sign(nil, append([]byte(leasePurpose), claims...), &mldsa.Options{})
	if e != nil {
		return SignedLease{}, ErrDenied
	}
	p := SignedLease{claims, signer.ChainDER, sig}
	raw, e := EncodeLease(p)
	if e != nil {
		return SignedLease{}, e
	}
	// The final and transactional checks revalidate the original delegation,
	// including its own device path and policy, not merely its decoded claims.
	check := func() error {
		lc, e := s.verifyLease(ctx, raw)
		if e != nil {
			return e
		}
		dc, e := s.delegation(delegation, s.observed)
		if e != nil {
			return e
		}
		id, e := s.cfg.Certificates.Verify(delegation.ChainDER, time.UnixMilli(dc.IssuedAtUnixMS), time.UnixMilli(dc.ExpiresAtUnixMS))
		if e != nil {
			return ErrDenied
		}
		if e = s.cfg.AuthorizeLease(ctx, cloneLease(lc), id); e != nil {
			return ErrDenied
		}
		w, e := s.now(ctx)
		if e != nil {
			return e
		}
		if !within(w, lc.NotBeforeUnixMS, lc.NotAfterUnixMS) || !within(w, dc.IssuedAtUnixMS, dc.ExpiresAtUnixMS) {
			return ErrExpired
		}
		return nil
	}
	if e = check(); e != nil {
		return SignedLease{}, e
	}
	r := s.state.Sources[key(src)]
	r.LeaseRevision = revision
	r.Epoch = c.Epoch
	r.BindingSHA256 = binding(lease)
	r.Lease = raw
	d := s.next()
	d.Sources[key(src)] = r
	if e = s.persist(ctx, d, check); e != nil {
		return SignedLease{}, e
	}
	return p, nil
}

func (s *Store) IssueFloor(ctx context.Context, signer Signer, src Source, minimumEpoch uint64) (SignedFloor, error) {
	return s.issueFloor(ctx, signer, src, minimumEpoch, false)
}

// RefreshFloor signs a new attestation without changing the denied revision or
// minimum epoch. It never revokes a newer active lease as a side effect of export.
func (s *Store) RefreshFloor(ctx context.Context, signer Signer, src Source) (SignedFloor, error) {
	return s.issueFloor(ctx, signer, src, 0, true)
}
func (s *Store) issueFloor(ctx context.Context, signer Signer, src Source, minimumEpoch uint64, refresh bool) (SignedFloor, error) {
	if s == nil {
		return SignedFloor{}, ErrClosed
	}
	signer, e := ownSigner(signer)
	if e != nil {
		return SignedFloor{}, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e = s.begin(ctx); e != nil {
		return SignedFloor{}, e
	}
	if src.Validate() != nil || src.Tenant != s.cfg.Tenant {
		return SignedFloor{}, ErrDenied
	}
	prior := s.state.Sources[key(src)]
	if refresh {
		if prior.ThroughRevision == 0 {
			return SignedFloor{}, ErrDenied
		}
		minimumEpoch = prior.MinimumEpoch
	} else if minimumEpoch == 0 || minimumEpoch < prior.MinimumEpoch || minimumEpoch < prior.Epoch {
		return SignedFloor{}, ErrReplay
	}
	w, e := s.now(ctx)
	if e != nil {
		return SignedFloor{}, e
	}
	id, e := s.signer(signer, src, w)
	if e != nil {
		return SignedFloor{}, e
	}
	rev, e := s.reserve(ctx, src)
	if e != nil {
		return SignedFloor{}, e
	}
	w, e = s.now(ctx)
	if e != nil {
		return SignedFloor{}, e
	}
	through := rev
	if refresh {
		through = prior.ThroughRevision
	}
	c := FloorClaims{Version: Version, AuthorityDomain: "physical", Source: src, Site: s.cfg.Site, ExecutionDomain: s.cfg.ExecutionDomain, SampleClockDomain: s.cfg.SampleClockDomain, ThroughRevision: through, AttestationRevision: rev, MinimumEpoch: minimumEpoch, NotBeforeUnixMS: w.Earliest.UnixMilli(), NotAfterUnixMS: min(w.Earliest.Add(MaxLifetime).UnixMilli(), id.ValidUntil.UnixMilli())}
	claims, e := encode(c, MaxClaimsBytes)
	if e != nil || c.Validate() != nil {
		return SignedFloor{}, ErrDenied
	}
	sig, e := signer.DeviceKey.Sign(nil, append([]byte(floorPurpose), claims...), &mldsa.Options{})
	if e != nil {
		return SignedFloor{}, ErrDenied
	}
	p := SignedFloor{claims, signer.ChainDER, sig}
	raw, e := EncodeFloor(p)
	if e != nil {
		return SignedFloor{}, e
	}
	if e = s.acceptFloor(ctx, raw); e != nil {
		return SignedFloor{}, e
	}
	return p, nil
}

// CurrentLease returns the exact latest admitted envelope after fresh current
// authority checks and a durable physical floor update. It is not a sample.
func (s *Store) CurrentLease(ctx context.Context, src Source) (SignedLease, error) {
	if s == nil {
		return SignedLease{}, ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.begin(ctx); e != nil {
		return SignedLease{}, e
	}
	r := s.state.Sources[key(src)]
	if len(r.Lease) == 0 {
		return SignedLease{}, ErrDenied
	}
	check := func() error { _, e := s.verifyLease(ctx, r.Lease); return e }
	if e := check(); e != nil {
		return SignedLease{}, e
	}
	if e := s.persist(ctx, s.next(), check); e != nil {
		return SignedLease{}, e
	}
	p, _, e := DecodeLease(r.Lease)
	return p, e
}

func (s *Store) delegationCurrent(ctx context.Context, c LeaseClaims, claims serviceorigin.DelegationClaims, id wendycert.DeviceIdentity) error {
	b, e := json.Marshal(claims)
	if e != nil || !reflect.DeepEqual(claims.Scope, c.Scope) || claims.Version != serviceorigin.Version || claims.AuthorityDomain != "physical" || claims.Epoch != c.Epoch || hash(b) != c.DelegationSHA256 || id.Principal != c.Scope.Device || id.Tenant != c.Scope.Tenant {
		return ErrDenied
	}
	if e = s.cfg.AuthorizeLease(ctx, cloneLease(c), id); e != nil {
		return ErrDenied
	}
	w, e := s.now(ctx)
	if e != nil {
		return e
	}
	if !within(w, c.NotBeforeUnixMS, c.NotAfterUnixMS) || !within(w, claims.IssuedAtUnixMS, claims.ExpiresAtUnixMS) || !w.Latest.Before(id.ValidUntil) {
		return ErrExpired
	}
	return nil
}

// AuthorizeDelegation is the remote serviceorigin.Verifier.Authorize hook.
// It receives the already authenticated original delegation identity, and adds
// exact retained lease/floor and current policy checks. It does not verify a
// record's sample or signature; the enclosing verifier performs cryptography.
func (s *Store) AuthorizeDelegation(ctx context.Context, claims serviceorigin.DelegationClaims, id wendycert.DeviceIdentity) error {
	if s == nil {
		return ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.begin(ctx); e != nil {
		return e
	}
	r := s.state.Sources[key(SourceOf(claims.Scope))]
	if len(r.Lease) == 0 {
		return ErrDenied
	}
	check := func() error {
		c, e := s.verifyLease(ctx, r.Lease)
		if e != nil {
			return e
		}
		return s.delegationCurrent(ctx, c, claims, id)
	}
	if e := check(); e != nil {
		return e
	}
	return s.persist(ctx, s.next(), check)
}

// AuthorizeRecord is an additional remote-authority check on an already
// cryptographically verified record. Callers still enforce its original sample
// freshness/schema/frame and recheck this at their durable/read boundary.
func (s *Store) AuthorizeRecord(ctx context.Context, p SignedLease, v serviceorigin.VerifiedRecord) error {
	if s == nil {
		return ErrClosed
	}
	raw, e := EncodeLease(p)
	if e != nil {
		return e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e = s.begin(ctx); e != nil {
		return e
	}
	check := func() error {
		c, e := s.verifyLease(ctx, raw)
		if e != nil {
			return e
		}
		r := s.state.Sources[key(SourceOf(c.Scope))]
		if !bytes.Equal(r.Lease, raw) {
			return ErrReplay
		}
		record := v.Record
		canonicalRecord, e := json.Marshal(record)
		if e != nil || !bytes.Equal(canonicalRecord, v.Bytes) || !reflect.DeepEqual(record.Scope, c.Scope) || record.Epoch != c.Epoch || record.DelegationSHA256 != c.DelegationSHA256 || record.Publication.Timing.ClockDomain != c.SampleClockDomain {
			return ErrDenied
		}
		if e = s.delegationCurrent(ctx, c, v.Delegation, v.Device); e != nil {
			return e
		}
		if !within(s.observed, record.IssuedAtUnixMS, record.ExpiresAtUnixMS) || !within(s.observed, v.Delegation.IssuedAtUnixMS, v.Delegation.ExpiresAtUnixMS) {
			return ErrExpired
		}
		return nil
	}
	if e = check(); e != nil {
		return e
	}
	return s.persist(ctx, s.next(), check)
}

// Snapshot exports owned historical metadata for host-authorized repair. It
// makes no claim that a retained lease or floor signature is currently valid.
// The host must authorize the requesting peer before exposing any entry.
func (s *Store) Snapshot(ctx context.Context) ([]State, error) {
	if s == nil {
		return nil, ErrClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.begin(ctx); e != nil {
		return nil, e
	}
	if e := s.persist(ctx, s.next(), nil); e != nil {
		return nil, e
	}
	out := make([]State, 0, len(s.state.Sources))
	for _, r := range s.state.Sources {
		r.Lease = bytes.Clone(r.Lease)
		r.Floor = bytes.Clone(r.Floor)
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return key(out[i].Source) < key(out[j].Source) })
	return out, nil
}
func (s *Store) ListFloors(ctx context.Context) ([]SignedFloor, error) {
	rows, e := s.Snapshot(ctx)
	if e != nil {
		return nil, e
	}
	out := []SignedFloor{}
	for _, r := range rows {
		if len(r.Floor) > 0 {
			p, _, e := DecodeFloor(r.Floor)
			if e != nil {
				return nil, ErrCorrupt
			}
			out = append(out, p)
		}
	}
	return out, nil
}
