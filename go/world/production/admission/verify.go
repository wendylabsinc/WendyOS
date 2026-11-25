package admission

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
var operatorPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

func digest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func canonicalPrincipal(principal, tenant string) bool {
	prefix := "spiffe://wendy.sh/tenant/" + tenant + "/operator/"
	return strings.HasPrefix(principal, prefix) && operatorPattern.MatchString(strings.TrimPrefix(principal, prefix))
}
func intervalWithin(issued, expires int64, max time.Duration, w trustedtime.Interval) bool {
	return issued > 0 && expires > issued && expires-issued <= max.Milliseconds() && !w.Earliest.Before(time.UnixMilli(issued)) && w.Latest.Before(time.UnixMilli(expires))
}
func identityPinned(id wendycert.Identity, pin Pin, tenant string) bool {
	return id.Tenant == tenant && id.Principal == pin.Principal && id.SigningKeySHA256 == pin.SigningKeySHA256
}

func (s *Store) verifyPolicy(proof string, w trustedtime.Interval) (Policy, wendycert.Identity, string, error) {
	var p Policy
	data, id, err := jws.Verify(proof, PolicyTyp, s.cfg.Certificates, w.Earliest, w.Latest)
	if err != nil {
		return p, id, "", err
	}
	if err = jws.DecodeStrict(data, &p); err != nil {
		return p, id, "", err
	}
	if !identityPinned(id, s.cfg.PolicyIssuer, s.cfg.Tenant) || p.Version != 1 || p.Domain != "physical" || p.Tenant != s.cfg.Tenant || p.Site != s.cfg.Site || p.Audience != s.cfg.Audience || p.IssuerPrincipal != id.Principal || p.IssuerKeySHA256 != id.SigningKeySHA256 || p.Revision == 0 || !tokenPattern.MatchString(p.PolicyID) || !intervalWithin(p.IssuedAtUnixMS, p.ExpiresAtUnixMS, s.cfg.MaxPolicyLifetime, w) || len(p.Entries) == 0 || len(p.Entries) > s.cfg.MaxPolicyEntries {
		return p, id, "", ErrDenied
	}
	seen := map[string]bool{}
	for _, entry := range p.Entries {
		if !canonicalPrincipal(entry.Principal, s.cfg.Tenant) || seen[entry.Principal] || len(entry.Scopes) != 1 || entry.Scopes[0] != ReadScope {
			return p, id, "", ErrDenied
		}
		seen[entry.Principal] = true
	}
	return p, id, digest(data), nil
}

func unique(values []string, max int, valid func(string) bool) bool {
	if values == nil || len(values) > max {
		return false
	}
	seen := map[string]bool{}
	for _, value := range values {
		if !valid(value) || seen[value] {
			return false
		}
		seen[value] = true
	}
	return true
}
func (s *Store) verifyRevocations(proof string, w trustedtime.Interval) (Revocations, wendycert.Identity, string, error) {
	var r Revocations
	data, id, err := jws.Verify(proof, RevocationsTyp, s.cfg.Certificates, w.Earliest, w.Latest)
	if err != nil {
		return r, id, "", err
	}
	if err = jws.DecodeStrict(data, &r); err != nil {
		return r, id, "", err
	}
	if !identityPinned(id, s.cfg.RevocationIssuer, s.cfg.Tenant) || r.Version != 1 || r.Domain != "physical" || r.Tenant != s.cfg.Tenant || r.Site != s.cfg.Site || r.Audience != s.cfg.Audience || r.IssuerPrincipal != id.Principal || r.IssuerKeySHA256 != id.SigningKeySHA256 || r.Revision == 0 || !intervalWithin(r.IssuedAtUnixMS, r.ExpiresAtUnixMS, s.cfg.MaxRevocationAge, w) {
		return r, id, "", ErrDenied
	}
	if !unique(r.RevokedPrincipals, s.cfg.MaxRevoked, func(p string) bool { return canonicalPrincipal(p, s.cfg.Tenant) }) || !unique(r.RevokedKeySHA256, s.cfg.MaxRevoked, digestPattern.MatchString) || !unique(r.RevokedCertificateSHA256, s.cfg.MaxRevoked, digestPattern.MatchString) || !unique(r.RevokedPolicyIDs, s.cfg.MaxRevoked, tokenPattern.MatchString) || len(r.RevokedPrincipals)+len(r.RevokedKeySHA256)+len(r.RevokedCertificateSHA256)+len(r.RevokedPolicyIDs) > s.cfg.MaxRevoked {
		return r, id, "", ErrDenied
	}
	return r, id, digest(data), nil
}
func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
func revoked(r Revocations, id wendycert.Identity) bool {
	if contains(r.RevokedPrincipals, id.Principal) || contains(r.RevokedKeySHA256, id.SigningKeySHA256) || contains(r.RevokedCertificateSHA256, id.CertificateSHA256) {
		return true
	}
	for _, key := range id.CandidateSigningKeySHA256() {
		if contains(r.RevokedKeySHA256, key) {
			return true
		}
	}
	for _, cert := range id.CandidateCertificateSHA256() {
		if contains(r.RevokedCertificateSHA256, cert) {
			return true
		}
	}
	return false
}
func validNonce(n string) bool {
	b, err := base64.RawURLEncoding.Strict().DecodeString(n)
	return err == nil && len(b) == 24 && base64.RawURLEncoding.EncodeToString(b) == n
}

func (s *Store) checkAuthority(req Request, w trustedtime.Interval) error {
	if s.state.Policy.Revision == 0 || s.state.Revocations.Revision == 0 {
		return ErrDenied
	}
	p, issuer, _, err := s.verifyPolicy(s.state.Policy.Proof, w)
	if err != nil {
		return err
	}
	r, revIssuer, _, err := s.verifyRevocations(s.state.Revocations.Proof, w)
	if err != nil {
		return err
	}
	id := req.Identity
	if req.Tenant != s.cfg.Tenant || req.Site != s.cfg.Site || req.Audience != s.cfg.Audience || req.Principal != id.Principal || id.Tenant != s.cfg.Tenant || !canonicalPrincipal(id.Principal, s.cfg.Tenant) || !digestPattern.MatchString(id.SigningKeySHA256) || !digestPattern.MatchString(id.CertificateSHA256) || req.Operation != ReadScope || req.PolicyRevision != p.Revision || !validNonce(req.Nonce) || !intervalWithin(req.IssuedAtUnixMS, req.ExpiresAtUnixMS, s.cfg.MaxRequestLifetime, w) {
		return ErrDenied
	}
	if id.ValidFrom.IsZero() || id.ValidUntil.IsZero() || w.Earliest.Before(id.ValidFrom) || !w.Latest.Before(id.ValidUntil) {
		return ErrDenied
	}
	if revoked(r, id) || revoked(r, issuer) || revoked(r, revIssuer) || contains(r.RevokedPolicyIDs, p.PolicyID) {
		return ErrDenied
	}
	for _, entry := range p.Entries {
		if entry.Principal == id.Principal && entry.Scopes[0] == ReadScope {
			return nil
		}
	}
	return ErrDenied
}
