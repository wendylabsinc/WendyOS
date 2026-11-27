package operator

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/admission"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/request"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

// Signer is a one-shot local helper, not a network signer or authority service.
// Commissioned trust and authenticated time are explicit dependencies.
type Signer struct {
	Certificates *wendycert.Verifier
	Clock        trustedtime.Provider
	Keychain     Keychain
}
type ReadTarget struct {
	Site, Audience string
	PolicyRevision uint64
}
type PolicyTarget struct {
	Site, Audience, PolicyID string
	Revision                 uint64
	Lifetime                 time.Duration
}
type RevocationTarget struct {
	Site, Audience, PolicyID                               string
	Revision                                               uint64
	Lifetime                                               time.Duration
	RevokeSelf, RevokeKey, RevokeCertificate, RevokePolicy bool
}

const MaxSigningUncertainty = 5 * time.Second
const IssuanceBackdate = 5 * time.Second

var targetToken = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

func validateTarget(site, audience string, revision uint64) error {
	if !targetToken.MatchString(site) || !validText(audience, 512) || revision == 0 {
		return errors.New("explicit bounded site, audience and revision required")
	}
	return nil
}
func validCommission(lifetime time.Duration) bool {
	return lifetime >= time.Second && lifetime <= 10*time.Minute
}
func (s Signer) begin(ctx context.Context, selection Selection) (*Credential, trustedtime.Interval, error) {
	var empty trustedtime.Interval
	if s.Clock == nil || s.Certificates == nil || ctx == nil || ctx.Err() != nil {
		return nil, empty, errors.New("authenticated clock, trust and live context required")
	}
	if err := validateSelection(selection); err != nil {
		return nil, empty, err
	}
	w, err := s.Clock.Now()
	if err != nil || w.Validate() != nil || w.Latest.Sub(w.Earliest) > MaxSigningUncertainty {
		return nil, empty, trustedtime.ErrUntrusted
	}
	c, err := Resolve(ctx, selection, s.Keychain, s.Certificates, w)
	if err != nil {
		return nil, empty, err
	}
	return c, w, nil
}
func validity(w trustedtime.Interval, c *Credential, lifetime time.Duration) (int64, int64, error) {
	// Independent authenticated intervals may overlap yet have different lower
	// bounds. Backdate issuance within the commissioned 5s uncertainty budget;
	// expiry stays anchored to that issuance, never renewed after signing.
	issued := w.Earliest.Add(-IssuanceBackdate).UnixMilli()
	expires := time.UnixMilli(issued).Add(lifetime).UnixMilli()
	if limit := c.identity.ValidUntil.UnixMilli(); limit < expires {
		expires = limit
	}
	if issued <= 0 || expires <= issued || !w.Latest.Before(time.UnixMilli(expires)) {
		return 0, 0, errors.New("authenticated uncertainty or certificate expiry leaves no valid signing window")
	}
	return issued, expires, nil
}
func (s Signer) finish(ctx context.Context, c *Credential, before trustedtime.Interval, compact, typ string, issued, expires int64) error {
	if ctx.Err() != nil {
		return errors.New("signing cancelled")
	}
	after, err := s.Clock.Now()
	if err != nil || after.Validate() != nil || after.Latest.Sub(after.Earliest) > MaxSigningUncertainty || after.BootID != before.BootID || after.Monotonic < before.Monotonic || after.Earliest.Before(before.Earliest) || after.Latest.Before(before.Earliest) {
		return trustedtime.ErrUntrusted
	}
	if after.Earliest.Before(time.UnixMilli(issued)) || !after.Latest.Before(time.UnixMilli(expires)) {
		return errors.New("signing window expired before output")
	}
	_, identity, err := jws.Verify(compact, typ, s.Certificates, after.Earliest, after.Latest)
	if err != nil || identity.Principal != c.identity.Principal || identity.SigningKeySHA256 != c.identity.SigningKeySHA256 {
		return errors.New("signed proof failed final identity or validity check")
	}
	return unchanged(c.selected)
}

// SignRead signs only world.read POST /v1/world/query, with a fresh random nonce.
func (s Signer) SignRead(ctx context.Context, selection Selection, target ReadTarget, body []byte) (string, error) {
	if err := validateTarget(target.Site, target.Audience, target.PolicyRevision); err != nil {
		return "", err
	}
	if len(body) > request.MaxBodyBytes {
		return "", errors.New("read body exceeds bound")
	}
	body = append([]byte(nil), body...)
	defer clear(body)
	digest, err := request.BodyDigest(body)
	if err != nil {
		return "", err
	}
	c, w, err := s.begin(ctx, selection)
	if err != nil {
		return "", err
	}
	defer func() { c.key = nil }()
	issued, expires, err := validity(w, c, request.MaxLifetime)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, 24)
	if _, err = rand.Read(nonce); err != nil {
		return "", errors.New("request nonce unavailable")
	}
	d := request.Descriptor{Version: 1, Domain: "physical", Tenant: selection.Tenant, Site: target.Site, Audience: target.Audience, Principal: selection.Principal, Operation: request.OperationRead, Method: request.Method, Path: request.Path, BodySHA256: digest, PolicyRevision: target.PolicyRevision, IssuedAtUnixMS: issued, ExpiresAtUnixMS: expires, Nonce: base64.RawURLEncoding.EncodeToString(nonce)}
	compact, err := request.Sign(d, body, c.selected.chain, c.key)
	if err != nil {
		return "", errors.New("bounded read signing failed")
	}
	if err = s.finish(ctx, c, w, compact, request.Type, issued, expires); err != nil {
		return "", err
	}
	return compact, nil
}

// SignReadPolicy is an explicit commissioning action. It grants only world.read
// to this selected operator, for at most ten minutes, at the selected site/service.
func (s Signer) SignReadPolicy(ctx context.Context, selection Selection, target PolicyTarget) (string, error) {
	if err := validateTarget(target.Site, target.Audience, target.Revision); err != nil {
		return "", err
	}
	if !targetToken.MatchString(target.PolicyID) || !validCommission(target.Lifetime) {
		return "", errors.New("explicit policy ID and lifetime from one second to ten minutes required")
	}
	c, w, err := s.begin(ctx, selection)
	if err != nil {
		return "", err
	}
	defer func() { c.key = nil }()
	issued, expires, err := validity(w, c, target.Lifetime)
	if err != nil {
		return "", err
	}
	p := admission.Policy{Version: 1, Domain: "physical", Tenant: selection.Tenant, Site: target.Site, Audience: target.Audience, IssuerPrincipal: selection.Principal, IssuerKeySHA256: c.identity.SigningKeySHA256, PolicyID: target.PolicyID, Revision: target.Revision, IssuedAtUnixMS: issued, ExpiresAtUnixMS: expires, Entries: []admission.PolicyEntry{{Principal: selection.Principal, Scopes: []string{admission.ReadScope}}}}
	return s.signCommission(ctx, c, w, admission.PolicyTyp, p, issued, expires)
}

// SignRevocations signs an explicit short-lived bootstrap or self-revocation
// snapshot. It cannot name another principal, key, certificate or arbitrary list.
func (s Signer) SignRevocations(ctx context.Context, selection Selection, target RevocationTarget) (string, error) {
	if err := validateTarget(target.Site, target.Audience, target.Revision); err != nil {
		return "", err
	}
	if !validCommission(target.Lifetime) || (target.RevokePolicy && !targetToken.MatchString(target.PolicyID)) || (!target.RevokePolicy && target.PolicyID != "") {
		return "", errors.New("bounded revocation lifetime and explicit self-policy selection required")
	}
	c, w, err := s.begin(ctx, selection)
	if err != nil {
		return "", err
	}
	defer func() { c.key = nil }()
	issued, expires, err := validity(w, c, target.Lifetime)
	if err != nil {
		return "", err
	}
	r := admission.Revocations{Version: 1, Domain: "physical", Tenant: selection.Tenant, Site: target.Site, Audience: target.Audience, IssuerPrincipal: selection.Principal, IssuerKeySHA256: c.identity.SigningKeySHA256, Revision: target.Revision, IssuedAtUnixMS: issued, ExpiresAtUnixMS: expires, RevokedPrincipals: []string{}, RevokedKeySHA256: []string{}, RevokedCertificateSHA256: []string{}, RevokedPolicyIDs: []string{}}
	if target.RevokeSelf {
		r.RevokedPrincipals = []string{selection.Principal}
	}
	if target.RevokeKey {
		r.RevokedKeySHA256 = []string{c.identity.SigningKeySHA256}
	}
	if target.RevokeCertificate {
		r.RevokedCertificateSHA256 = []string{c.identity.CertificateSHA256}
	}
	if target.RevokePolicy {
		r.RevokedPolicyIDs = []string{target.PolicyID}
	}
	return s.signCommission(ctx, c, w, admission.RevocationsTyp, r, issued, expires)
}
func (s Signer) signCommission(ctx context.Context, c *Credential, w trustedtime.Interval, typ string, payload any, issued, expires int64) (string, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return "", errors.New("commissioning payload invalid")
	}
	compact, err := jws.Sign(typ, b, c.selected.chain, c.key)
	if err != nil {
		return "", errors.New("bounded commissioning signing failed")
	}
	if err = s.finish(ctx, c, w, compact, typ, issued, expires); err != nil {
		return "", err
	}
	return compact, nil
}
