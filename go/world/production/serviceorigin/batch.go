package serviceorigin

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/mldsa"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

const (
	MaxBatchRecords = 16
	// MaxBatchBytes bounds the sum of original record, delegation, signature
	// and certificate bytes, before any input is copied or decoded. It is not
	// a transport-envelope limit; callers must bound that encoding separately.
	MaxBatchBytes = 4 << 20
)

// BatchAuthorization contains cryptographically verified originals in input
// order. Each invocation owns its slices and mutable claims. Changing them
// cannot alter verification or a later invocation.
type BatchAuthorization struct {
	Claims  []DelegationClaims
	Devices []wendycert.DeviceIdentity
}

// AuthorizeBatch must check current commissioned grants and revocation for the
// entire exact set on each invocation. Enrollment is not a site grant. The
// callback must be read-only, honor cancellation, and must not reenter the same
// BatchVerifier. It must not treat a prior invocation as a cached authority lease.
type AuthorizeBatch func(context.Context, BatchAuthorization) error

type BatchVerifierConfig struct {
	Tenant, Site, ExecutionDomain string
	Certificates                  *wendycert.DeviceVerifier
	Clock                         trustedtime.Provider
	Authorize                     AuthorizeBatch
	// Zero selects the hard maximum. Positive values may only lower it.
	MaxRecords int
	MaxBytes   int
}

// BatchVerifier serializes checks to preserve its physical-clock continuity.
// A clock fault is sticky; a policy refusal, invalid input or cancellation is
// not. It provides no reusable authorization lease or durable admission.
type BatchVerifier struct {
	mu      sync.Mutex
	cfg     BatchVerifierConfig
	last    trustedtime.Interval
	faulted bool
}

func NewBatchVerifier(cfg BatchVerifierConfig) (*BatchVerifier, error) {
	if !canonicalUUID(cfg.Tenant) || !bounded(cfg.Site, 256) || cfg.ExecutionDomain != "physical" && cfg.ExecutionDomain != "simulation" || cfg.Certificates == nil || cfg.Clock == nil || cfg.Authorize == nil || cfg.MaxRecords < 0 || cfg.MaxRecords > MaxBatchRecords || cfg.MaxBytes < 0 || cfg.MaxBytes > MaxBatchBytes {
		return nil, ErrDenied
	}
	if cfg.MaxRecords == 0 {
		cfg.MaxRecords = MaxBatchRecords
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = MaxBatchBytes
	}
	return &BatchVerifier{cfg: cfg}, nil
}

func (v *BatchVerifier) now() (trustedtime.Interval, error) {
	if v.faulted {
		return trustedtime.Interval{}, ErrClock
	}
	w, err := v.cfg.Clock.Now()
	if err != nil || w.Validate() != nil || !v.last.Earliest.IsZero() && !forward(v.last, w) {
		v.faulted = true
		return trustedtime.Interval{}, ErrClock
	}
	v.last = w
	return w, nil
}

type batchLogicalVersion struct {
	key     world.Key
	version world.Version
}

// VerifyBatch checks all original signatures before the first authorization
// callback, then checks the entire set after each of two fresh authorization
// callbacks. Three authenticated physical intervals are shared by the set.
// It returns owned verified records in input order, or nil and an error; no
// partially trusted records escape. Duplicate full logical keys and versions
// are refused, including byte-identical duplicates. Distinct-source cardinality
// and any application-specific whole-set policy belong to the caller.
//
// Signed formats and Verifier.Verify are unchanged. This method does not check
// sample-clock freshness, schema, media, historical proof retention or replica
// high-water marks. Callers must recheck authority and original deadlines at
// later I/O and persistence boundaries, without substituting newer records.
func (v *BatchVerifier) VerifyBatch(ctx context.Context, signed []SignedRecord) ([]VerifiedRecord, error) {
	if v == nil || ctx == nil {
		return nil, ErrDenied
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !v.bounds(signed) {
		return nil, ErrDenied
	}
	owned := make([]SignedRecord, len(signed))
	for i, s := range signed {
		owned[i] = SignedRecord{Delegation: cloneDelegation(s.Delegation), Record: bytes.Clone(s.Record), Signature: bytes.Clone(s.Signature)}
	}
	w0, err := v.now()
	if err != nil {
		return nil, err
	}
	verified := make([]VerifiedRecord, len(owned))
	seen := make(map[batchLogicalVersion]bool, len(owned))
	for i, s := range owned {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var claims DelegationClaims
		if jws.DecodeStrict(s.Delegation.Claims, &claims) != nil || validateClaims(claims) != nil || claims.Scope.Tenant != v.cfg.Tenant || claims.Scope.Site != v.cfg.Site || claims.Scope.ExecutionDomain != v.cfg.ExecutionDomain {
			return nil, ErrDenied
		}
		if !within(w0, claims.IssuedAtUnixMS, claims.ExpiresAtUnixMS) {
			return nil, ErrExpired
		}
		id, err := v.cfg.Certificates.Verify(s.Delegation.ChainDER, w0.Earliest, w0.Latest)
		if err != nil || id.Principal != claims.Scope.Device || id.Tenant != claims.Scope.Tenant || id.VerifySignature(append([]byte(delegationPrefix), s.Delegation.Claims...), s.Delegation.Signature) != nil {
			return nil, ErrDenied
		}
		// As in Verify, one certificate path must cover the entire claimed
		// delegation window, not just the current observation interval.
		if _, err = v.cfg.Certificates.Verify(s.Delegation.ChainDER, time.UnixMilli(claims.IssuedAtUnixMS), time.UnixMilli(claims.ExpiresAtUnixMS)); err != nil {
			return nil, ErrDenied
		}
		if !ed25519.Verify(claims.PublicKey, append([]byte(recordPrefix), s.Record...), s.Signature) {
			return nil, ErrDenied
		}
		r, err := decodeRecord(s.Record, claims, s.Delegation.Claims)
		if err != nil {
			return nil, err
		}
		if !within(w0, r.IssuedAtUnixMS, r.ExpiresAtUnixMS) {
			return nil, ErrExpired
		}
		key := batchLogicalVersion{key: world.Key{Tenant: r.Scope.Tenant, Site: r.Scope.Site, Entity: r.Publication.Entity, Type: r.Publication.ComponentType, Source: world.Source{Device: r.Scope.Device, App: r.Scope.App, Service: r.Scope.Service, Stream: r.Publication.Stream}}, version: world.Version{Epoch: r.Epoch, Sequence: r.Sequence}}
		if seen[key] {
			return nil, ErrDenied
		}
		seen[key] = true
		verified[i] = VerifiedRecord{Record: r, Bytes: s.Record, Delegation: claims, Device: id}
	}
	// No authorizer sees even one input until every original has passed its
	// immutable cryptographic and structural checks.
	for pass := 0; pass < 2; pass++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := v.cfg.Authorize(ctx, batchAuthorization(verified)); err != nil {
			return nil, ErrDenied
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		w, err := v.now()
		if err != nil {
			return nil, err
		}
		if err := v.recheck(ctx, owned, verified, w); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return verified, nil
}

func (v *BatchVerifier) bounds(records []SignedRecord) bool {
	if len(records) == 0 || len(records) > v.cfg.MaxRecords {
		return false
	}
	remaining := v.cfg.MaxBytes
	for _, s := range records {
		if len(s.Record) == 0 || len(s.Record) > MaxRecordBytes || len(s.Signature) != ed25519.SignatureSize || len(s.Delegation.Claims) == 0 || len(s.Delegation.Claims) > MaxClaimsBytes || len(s.Delegation.Signature) != mldsa.MLDSA65SignatureSize || !chainBounds(s.Delegation.ChainDER) {
			return false
		}
		for _, b := range [][]byte{s.Record, s.Signature, s.Delegation.Claims, s.Delegation.Signature} {
			if len(b) > remaining {
				return false
			}
			remaining -= len(b)
		}
		for _, der := range s.Delegation.ChainDER {
			if len(der) > remaining {
				return false
			}
			remaining -= len(der)
		}
	}
	return true
}

func batchAuthorization(records []VerifiedRecord) BatchAuthorization {
	a := BatchAuthorization{Claims: make([]DelegationClaims, len(records)), Devices: make([]wendycert.DeviceIdentity, len(records))}
	for i, r := range records {
		a.Claims[i] = cloneClaims(r.Delegation)
		a.Devices[i] = r.Device
	}
	return a
}

func (v *BatchVerifier) recheck(ctx context.Context, owned []SignedRecord, records []VerifiedRecord, w trustedtime.Interval) error {
	for i := range records {
		if err := ctx.Err(); err != nil {
			return err
		}
		r := &records[i]
		if !within(w, r.Delegation.IssuedAtUnixMS, r.Delegation.ExpiresAtUnixMS) || !within(w, r.Record.IssuedAtUnixMS, r.Record.ExpiresAtUnixMS) {
			return ErrExpired
		}
		id, err := v.cfg.Certificates.Verify(owned[i].Delegation.ChainDER, w.Earliest, w.Latest)
		if err != nil || id.Principal != r.Device.Principal || id.Tenant != r.Device.Tenant || id.SigningKeySHA256 != r.Device.SigningKeySHA256 || id.CertificateSHA256 != r.Device.CertificateSHA256 {
			return ErrDenied
		}
		r.Device = id
	}
	return nil
}
