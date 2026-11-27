package sourceauthority

import (
	"bytes"
	"crypto/mldsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

func hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func canonical(raw []byte, out any, limit int) error {
	if len(raw) == 0 || len(raw) > limit || !utf8.Valid(raw) {
		return ErrDenied
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(out) != nil {
		return ErrDenied
	}
	if _, err := d.Token(); !errors.Is(err, io.EOF) {
		return ErrDenied
	}
	// Equality to the sole canonical encoding also refuses duplicate fields,
	// case aliases, invalid escaped Unicode and alternate number spellings.
	b, err := json.Marshal(out)
	if err != nil || !bytes.Equal(b, raw) {
		return ErrDenied
	}
	return nil
}
func encode(v any, limit int) ([]byte, error) {
	b, e := json.Marshal(v)
	if e != nil || len(b) > limit {
		return nil, ErrCapacity
	}
	return b, nil
}
func bounded(s string) bool {
	if s == "" || len(s) > 256 || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}
func domain(execution, sample string) bool {
	if !bounded(sample) {
		return false
	}
	if execution == "simulation" {
		return strings.HasPrefix(sample, "simulation:") && len(sample) > len("simulation:")
	}
	return execution == "physical" && !strings.HasPrefix(sample, "simulation:") && !strings.HasPrefix(sample, "replay:")
}
func validWindow(from, until int64) bool {
	return from > 0 && until > from && until-from <= MaxLifetime.Milliseconds() && time.UnixMilli(until).Year() <= 9999
}
func within(w trustedtime.Interval, from, until int64) bool {
	return w.Validate() == nil && !w.Earliest.Before(time.UnixMilli(from)) && w.Latest.Before(time.UnixMilli(until))
}
func (s Source) Validate() error {
	return (serviceorigin.Scope{Tenant: s.Tenant, Site: "validation", Device: s.Device, App: s.App, Service: s.Service, DeploymentGeneration: 1, ExecutionDomain: "physical"}).Validate()
}
func (c LeaseClaims) Validate() error {
	if c.Version != Version || c.AuthorityDomain != "physical" || c.Scope.Validate() != nil || len(c.Scope.Publish) == 0 || !domain(c.Scope.ExecutionDomain, c.SampleClockDomain) || c.Epoch == 0 || c.AuthorityRevision == 0 || !digestValid(c.DelegationSHA256) || !validWindow(c.NotBeforeUnixMS, c.NotAfterUnixMS) {
		return ErrDenied
	}
	return nil
}
func (c FloorClaims) Validate() error {
	if c.Version != Version || c.AuthorityDomain != "physical" || c.Source.Validate() != nil || !bounded(c.Site) || !domain(c.ExecutionDomain, c.SampleClockDomain) || c.ThroughRevision == 0 || c.AttestationRevision < c.ThroughRevision || c.MinimumEpoch == 0 || !validWindow(c.NotBeforeUnixMS, c.NotAfterUnixMS) {
		return ErrDenied
	}
	return nil
}
func digestValid(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == s
}
func chainBounds(chain [][]byte) bool {
	if len(chain) == 0 || len(chain) > wendycert.MaxCertificates {
		return false
	}
	n := 0
	seen := map[string]bool{}
	for _, b := range chain {
		if len(b) == 0 || len(b) > wendycert.MaxCertificateBytes-n || seen[string(b)] {
			return false
		}
		n += len(b)
		seen[string(b)] = true
	}
	return true
}
func DecodeLease(raw []byte) (SignedLease, LeaseClaims, error) {
	var s SignedLease
	var c LeaseClaims
	if canonical(raw, &s, MaxEnvelopeBytes) != nil || canonical(s.Claims, &c, MaxClaimsBytes) != nil || c.Validate() != nil || !chainBounds(s.ChainDER) || len(s.Signature) != mldsa.MLDSA65SignatureSize {
		return s, c, ErrDenied
	}
	return s, c, nil
}
func DecodeFloor(raw []byte) (SignedFloor, FloorClaims, error) {
	var s SignedFloor
	var c FloorClaims
	if canonical(raw, &s, MaxEnvelopeBytes) != nil || canonical(s.Claims, &c, MaxClaimsBytes) != nil || c.Validate() != nil || !chainBounds(s.ChainDER) || len(s.Signature) != mldsa.MLDSA65SignatureSize {
		return s, c, ErrDenied
	}
	return s, c, nil
}
func EncodeLease(s SignedLease) ([]byte, error) {
	b, e := encode(s, MaxEnvelopeBytes)
	if e != nil {
		return nil, e
	}
	if _, _, e = DecodeLease(b); e != nil {
		return nil, e
	}
	return b, nil
}
func EncodeFloor(s SignedFloor) ([]byte, error) {
	b, e := encode(s, MaxEnvelopeBytes)
	if e != nil {
		return nil, e
	}
	if _, _, e = DecodeFloor(b); e != nil {
		return nil, e
	}
	return b, nil
}
func key(s Source) string { b, _ := json.Marshal(s); return string(b) }
func cloneLease(c LeaseClaims) LeaseClaims {
	c.Scope.Publish = append([]serviceorigin.PublishScope(nil), c.Scope.Publish...)
	return c
}
func binding(c LeaseClaims) string {
	b, _ := json.Marshal(struct {
		Scope             serviceorigin.Scope
		Epoch             uint64
		Delegation, Clock string
	}{c.Scope, c.Epoch, c.DelegationSHA256, c.SampleClockDomain})
	return hash(b)
}

func (s *Store) leaseScope(c LeaseClaims) bool {
	return c.Scope.Tenant == s.cfg.Tenant && c.Scope.Site == s.cfg.Site && c.Scope.ExecutionDomain == s.cfg.ExecutionDomain && c.SampleClockDomain == s.cfg.SampleClockDomain
}
func (s *Store) floorScope(c FloorClaims) bool {
	return c.Source.Tenant == s.cfg.Tenant && c.Site == s.cfg.Site && c.ExecutionDomain == s.cfg.ExecutionDomain && c.SampleClockDomain == s.cfg.SampleClockDomain
}
func (s *Store) leaseCrypto(raw []byte, w trustedtime.Interval) (SignedLease, LeaseClaims, wendycert.DeviceIdentity, error) {
	p, c, e := DecodeLease(raw)
	if e != nil || !s.leaseScope(c) {
		return p, c, wendycert.DeviceIdentity{}, ErrDenied
	}
	if !within(w, c.NotBeforeUnixMS, c.NotAfterUnixMS) {
		return p, c, wendycert.DeviceIdentity{}, ErrExpired
	}
	id, e := s.cfg.Certificates.Verify(p.ChainDER, time.UnixMilli(c.NotBeforeUnixMS), time.UnixMilli(c.NotAfterUnixMS))
	if e != nil || id.Principal != c.Scope.Device || id.Tenant != c.Scope.Tenant || id.VerifySignature(append([]byte(leasePurpose), p.Claims...), p.Signature) != nil {
		return p, c, id, ErrDenied
	}
	return p, c, id, nil
}
func (s *Store) floorCrypto(raw []byte, w trustedtime.Interval) (SignedFloor, FloorClaims, wendycert.DeviceIdentity, error) {
	p, c, e := DecodeFloor(raw)
	if e != nil || !s.floorScope(c) {
		return p, c, wendycert.DeviceIdentity{}, ErrDenied
	}
	if !within(w, c.NotBeforeUnixMS, c.NotAfterUnixMS) {
		return p, c, wendycert.DeviceIdentity{}, ErrExpired
	}
	id, e := s.cfg.Certificates.Verify(p.ChainDER, time.UnixMilli(c.NotBeforeUnixMS), time.UnixMilli(c.NotAfterUnixMS))
	if e != nil || id.Principal != c.Source.Device || id.Tenant != c.Source.Tenant || id.VerifySignature(append([]byte(floorPurpose), p.Claims...), p.Signature) != nil {
		return p, c, id, ErrDenied
	}
	return p, c, id, nil
}
