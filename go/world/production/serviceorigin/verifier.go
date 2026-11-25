package serviceorigin

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/mldsa"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

type Verifier struct {
	mu      sync.Mutex
	cfg     VerifierConfig
	last    trustedtime.Interval
	faulted bool
}

func NewVerifier(cfg VerifierConfig) (*Verifier, error) {
	if !canonicalUUID(cfg.Tenant) || !bounded(cfg.Site, 256) || cfg.ExecutionDomain != "physical" && cfg.ExecutionDomain != "simulation" || cfg.Certificates == nil || cfg.Clock == nil || cfg.Authorize == nil {
		return nil, ErrDenied
	}
	return &Verifier{cfg: cfg}, nil
}
func (v *Verifier) now() (trustedtime.Interval, error) {
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

// Verify authenticates exact immutable bytes, then applies current local
// authorization before and after verification. This is not durable admission:
// consumers still enforce original source freshness and replica high-water
// marks, and must revalidate authority at any later persistence boundary.
func (v *Verifier) Verify(ctx context.Context, signed SignedRecord) (VerifiedRecord, error) {
	if v == nil || ctx == nil {
		return VerifiedRecord{}, ErrDenied
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return VerifiedRecord{}, err
	}
	if len(signed.Record) == 0 || len(signed.Record) > MaxRecordBytes || len(signed.Signature) != ed25519.SignatureSize || len(signed.Delegation.Claims) == 0 || len(signed.Delegation.Claims) > MaxClaimsBytes || len(signed.Delegation.Signature) != mldsa.MLDSA65SignatureSize || !chainBounds(signed.Delegation.ChainDER) {
		return VerifiedRecord{}, ErrDenied
	}
	signed = SignedRecord{Delegation: cloneDelegation(signed.Delegation), Record: bytes.Clone(signed.Record), Signature: bytes.Clone(signed.Signature)}
	before, err := v.now()
	if err != nil {
		return VerifiedRecord{}, err
	}
	var claims DelegationClaims
	if jws.DecodeStrict(signed.Delegation.Claims, &claims) != nil || validateClaims(claims) != nil || claims.Scope.Tenant != v.cfg.Tenant || claims.Scope.Site != v.cfg.Site || claims.Scope.ExecutionDomain != v.cfg.ExecutionDomain {
		return VerifiedRecord{}, ErrDenied
	}
	if !within(before, claims.IssuedAtUnixMS, claims.ExpiresAtUnixMS) {
		return VerifiedRecord{}, ErrExpired
	}
	id, err := v.cfg.Certificates.Verify(signed.Delegation.ChainDER, before.Earliest, before.Latest)
	if err != nil || id.Principal != claims.Scope.Device || id.Tenant != claims.Scope.Tenant || id.VerifySignature(append([]byte(delegationPrefix), signed.Delegation.Claims...), signed.Delegation.Signature) != nil {
		return VerifiedRecord{}, ErrDenied
	}
	// Verify the whole claimed window against one path, preventing an issuer
	// from delegating before a leaf or intermediate certificate became valid.
	if _, err = v.cfg.Certificates.Verify(signed.Delegation.ChainDER, time.UnixMilli(claims.IssuedAtUnixMS), time.UnixMilli(claims.ExpiresAtUnixMS)); err != nil {
		return VerifiedRecord{}, ErrDenied
	}
	if !ed25519.Verify(claims.PublicKey, append([]byte(recordPrefix), signed.Record...), signed.Signature) {
		return VerifiedRecord{}, ErrDenied
	}
	record, err := decodeRecord(signed.Record, claims, signed.Delegation.Claims)
	if err != nil {
		return VerifiedRecord{}, err
	}
	if !within(before, record.IssuedAtUnixMS, record.ExpiresAtUnixMS) {
		return VerifiedRecord{}, ErrExpired
	}
	if err = v.cfg.Authorize(ctx, cloneClaims(claims), id); err != nil {
		return VerifiedRecord{}, ErrDenied
	}
	if err = ctx.Err(); err != nil {
		return VerifiedRecord{}, err
	}
	after, err := v.now()
	if err != nil {
		return VerifiedRecord{}, err
	}
	if !within(after, record.IssuedAtUnixMS, record.ExpiresAtUnixMS) {
		return VerifiedRecord{}, ErrExpired
	}
	id, err = v.cfg.Certificates.Verify(signed.Delegation.ChainDER, after.Earliest, after.Latest)
	if err != nil {
		return VerifiedRecord{}, ErrDenied
	}
	if err = v.cfg.Authorize(ctx, cloneClaims(claims), id); err != nil {
		return VerifiedRecord{}, ErrDenied
	}
	if err = ctx.Err(); err != nil {
		return VerifiedRecord{}, err
	}
	// Authorization may wait on local policy I/O. Check time again after it.
	final, err := v.now()
	if err != nil {
		return VerifiedRecord{}, err
	}
	if !within(final, record.IssuedAtUnixMS, record.ExpiresAtUnixMS) {
		return VerifiedRecord{}, ErrExpired
	}
	return VerifiedRecord{Record: record, Bytes: bytes.Clone(signed.Record), Delegation: cloneClaims(claims), Device: id}, nil
}

func chainBounds(chain [][]byte) bool {
	if len(chain) == 0 || len(chain) > wendycert.MaxCertificates {
		return false
	}
	total := 0
	seen := map[string]bool{}
	for _, der := range chain {
		if len(der) == 0 || len(der) > wendycert.MaxCertificateBytes-total || seen[string(der)] {
			return false
		}
		total += len(der)
		seen[string(der)] = true
	}
	return true
}
