// Package builddelivery defines operator authority for delivery of one build.
// It is independent of image signatures: the builder may choose the output
// digest, but only within the operator's target, reference and time bounds.
package builddelivery

import (
	"bytes"
	"crypto"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/distribution/reference"
	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
)

const (
	Type             = "wendy-build-delivery+jws"
	Version          = 1
	MaxContractBytes = 192 * 1024
	MaxLifetime      = 24 * time.Hour
)

// Claims is the signed payload authorizing one target-bound build delivery.
type Claims struct {
	// Version is the contract schema version; currently 1.
	Version int `json:"v"`
	// JobID is the canonical job UUID used to reject conflicting authorizations and track replay.
	JobID string `json:"jti"`
	// Operator is the full SPIFFE operator principal whose certificate signs the grant.
	Operator string `json:"iss"`
	// Target is the full SPIFFE device principal of the target receiving the image.
	Target string `json:"aud"`
	// Builder is the full SPIFFE device principal of the build host authorized to deliver it.
	Builder string `json:"builder"`
	// BuilderSPKI is unpadded base64url SHA-256 of the builder's full DER SubjectPublicKeyInfo.
	BuilderSPKI string `json:"builder_spki_sha256"`
	// Reference is the exact local repository:tag authorized for publication, without a registry host.
	Reference string `json:"reference"`
	// InputSHA256 is unpadded base64url SHA-256 of the deterministic BuildSpec, excluding grants and session IDs.
	InputSHA256 string `json:"input_sha256"`
	// ExpectedDigest is the current sha256:<hex> image digest required at Reference; empty requires an absent tag.
	ExpectedDigest string `json:"expected_digest"`
	// IssuedAt is the Unix-second creation time and earliest permitted admission time.
	IssuedAt int64 `json:"iat"`
	// AdmitBefore is the exclusive Unix-second deadline for first durable admission by the target.
	AdmitBefore int64 `json:"admit_before"`
	// ValidUntil is the exclusive Unix-second delivery deadline for an admitted grant.
	ValidUntil int64 `json:"exp"`
	// Operations must currently contain exactly ["stage", "publish"], in that order.
	// Staging uploads immutable content; publishing conditionally updates Reference.
	Operations []string `json:"operations"`
	// MaxBytes caps cumulative uploaded bytes, including retransmitted content.
	MaxBytes int64 `json:"max_bytes"`
}

type header struct {
	Algorithm string   `json:"alg"`
	Type      string   `json:"typ"`
	Chain     []string `json:"x5c"`
}

// Verified is returned only after signature, primary PKI path, embedded rights
// and target policy have passed. A JWS-supplied root never establishes trust.
type Verified struct {
	claims     Claims
	compact    string
	admittedAt time.Time
}

func (v Verified) Claims() Claims {
	c := v.claims
	c.Operations = append([]string(nil), c.Operations...)
	return c
}
func (v Verified) Compact() string       { return v.compact }
func (v Verified) AdmittedAt() time.Time { return v.admittedAt }
func Fingerprint(leaf *x509.Certificate) string {
	sum := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// VerifyBuilderCertificate resolves an authorization key independently of the
// RPC tunnel used to submit a build. The expected device must come from the
// user's existing inventory/enrollment binding, never this certificate bundle.
func VerifyBuilderCertificate(chain [][]byte, roots []*x509.Certificate, expected meshid.Device, now time.Time) (*x509.Certificate, error) {
	if !expected.IsValid() || len(chain) == 0 || len(chain) > 16 {
		return nil, errors.New("missing expected builder identity or certificate")
	}
	parsed, err := parseChain(chain)
	if err != nil {
		return nil, err
	}
	id, found, err := certs.IdentityFromCert(parsed[0])
	if err != nil || !found || id.Principal != expected.Principal() || id.EntityType != certs.EntityAsset {
		return nil, errors.New("builder certificate does not match the expected device")
	}
	if parsed[0].IsCA || parsed[0].KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		return nil, errors.New("invalid builder leaf profile")
	}
	if err = certs.VerifyPeerCertificateChain(parsed[0], parsed[1:], roots, x509.ExtKeyUsageClientAuth, now, now); err != nil {
		return nil, err
	}
	return parsed[0], nil
}

func Sign(c Claims, certificatePEM, chainPEM, keyPEM string) (string, error) {
	if err := validate(c); err != nil {
		return "", err
	}
	pair, err := certs.TLSKeyPair(certificatePEM, chainPEM, keyPEM)
	if err != nil {
		return "", err
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return "", errors.New("operator key cannot sign")
	}
	public, ok := signer.Public().(*mldsa.PublicKey)
	if !ok || public.Parameters() != mldsa.MLDSA65() {
		return "", errors.New("build delivery requires an ML-DSA-65 operator key")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return "", err
	}
	principal, ok := certs.TenantPrincipalFromCert(leaf)
	if !ok || principal != c.Operator {
		return "", errors.New("operator certificate does not match the grant issuer")
	}
	h := header{Algorithm: "ML-DSA-65", Type: Type}
	for _, der := range pair.Certificate {
		h.Chain = append(h.Chain, base64.StdEncoding.EncodeToString(der))
	}
	hb, _ := json.Marshal(h)
	payload, _ := json.Marshal(c)
	input := encode(hb) + "." + encode(payload)
	signature, err := signer.Sign(rand.Reader, []byte(input), crypto.Hash(0))
	if err != nil {
		return "", err
	}
	compact := input + "." + encode(signature)
	if len(compact) > MaxContractBytes {
		return "", errors.New("oversized delivery contract")
	}
	return compact, nil
}

// Admit verifies a new grant while the signing credential is valid. Durable
// target admission is the offline proof of timely authorization; later uses
// must load that exact admitted artifact rather than backdating a fresh grant.
func Admit(compact string, roots []*x509.Certificate, target meshid.Device, now time.Time) (Verified, error) {
	var result Verified
	if !target.IsValid() || len(compact) > MaxContractBytes {
		return result, errors.New("invalid target or oversized contract")
	}
	parts := strings.Split(compact, ".")
	if len(parts) != 3 {
		return result, errors.New("invalid compact JWS")
	}
	hb, err := decode(parts[0])
	if err != nil {
		return result, err
	}
	var h header
	if err = strictJSON(hb, &h); err != nil {
		return result, err
	}
	if h.Algorithm != "ML-DSA-65" || h.Type != Type || len(h.Chain) == 0 || len(h.Chain) > 16 {
		return result, errors.New("invalid delivery JWS header")
	}
	payload, err := decode(parts[1])
	if err != nil {
		return result, err
	}
	var c Claims
	if err = strictJSON(payload, &c); err != nil {
		return result, err
	}
	if err = validate(c); err != nil {
		return result, err
	}
	if c.Target != target.Principal() {
		return result, errors.New("delivery contract addresses another target")
	}
	if now.Unix() < c.IssuedAt || now.Unix() >= c.AdmitBefore || now.Unix() >= c.ValidUntil {
		return result, errors.New("delivery contract is outside its admission window")
	}
	chain := make([][]byte, len(h.Chain))
	for i, s := range h.Chain {
		chain[i], err = base64.StdEncoding.Strict().DecodeString(s)
		if err != nil {
			return result, err
		}
	}
	op, err := meshoperator.VerifyPrimary(chain, roots, target.Tenant(), now)
	if err != nil {
		return result, err
	}
	if op.Operator().Principal() != c.Operator {
		return result, errors.New("delivery issuer does not match the operator certificate")
	}
	leaf, err := x509.ParseCertificate(op.LeafDER())
	if err != nil {
		return result, err
	}
	if c.AdmitBefore > op.NotAfter().Unix() {
		return result, errors.New("admission window outlives operator credential")
	}
	if err = authorizeEntitlements(leaf, target); err != nil {
		return result, err
	}
	public, ok := leaf.PublicKey.(*mldsa.PublicKey)
	if !ok || public.Parameters() != mldsa.MLDSA65() {
		return result, errors.New("delivery signature requires ML-DSA-65")
	}
	signature, err := decode(parts[2])
	if err != nil {
		return result, err
	}
	if err = mldsa.Verify(public, []byte(parts[0]+"."+parts[1]), signature, nil); err != nil {
		return result, errors.New("invalid delivery signature")
	}
	return Verified{claims: c, compact: compact, admittedAt: now}, nil
}

func validate(c Claims) error {
	op, err := meshoperator.ParsePrincipal(c.Operator)
	if err != nil {
		return err
	}
	target, err := meshid.DeviceFromPrincipal(c.Target)
	if err != nil {
		return err
	}
	builder, err := meshid.DeviceFromPrincipal(c.Builder)
	if err != nil {
		return err
	}
	if op.Tenant() != target.Tenant() || builder.Tenant() != target.Tenant() {
		return errors.New("delivery principals must share one tenant")
	}
	id, err := uuid.Parse(c.JobID)
	if err != nil || id.String() != c.JobID || id == uuid.Nil {
		return errors.New("invalid delivery job ID")
	}
	if c.Version != Version || c.IssuedAt <= 0 || c.AdmitBefore <= c.IssuedAt || c.ValidUntil < c.AdmitBefore || c.ValidUntil-c.IssuedAt > int64(MaxLifetime/time.Second) {
		return errors.New("invalid delivery time bounds or version")
	}
	for _, s := range []string{c.BuilderSPKI, c.InputSHA256} {
		b, e := decode(s)
		if e != nil || len(b) != 32 {
			return errors.New("invalid SHA-256 binding")
		}
	}
	ref, err := reference.Parse(c.Reference)
	if err != nil {
		return errors.New("invalid delivery reference")
	}
	named, ok := ref.(reference.Named)
	if !ok || named.Name() != strings.Split(c.Reference, ":")[0] {
		return errors.New("delivery reference must contain only a repository and tag")
	}
	parts := strings.Split(named.Name(), "/")
	if len(parts) > 1 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		return errors.New("delivery reference cannot contain a registry host")
	}
	if _, ok = ref.(reference.Tagged); !ok {
		return errors.New("delivery reference requires an explicit tag")
	}
	if _, ok = ref.(reference.Digested); ok {
		return errors.New("delivery reference cannot be a digest")
	}
	if c.ExpectedDigest != "" {
		if len(c.ExpectedDigest) != 71 || !strings.HasPrefix(c.ExpectedDigest, "sha256:") {
			return errors.New("invalid expected image digest")
		}
		for _, ch := range c.ExpectedDigest[7:] {
			if !strings.ContainsRune("0123456789abcdef", ch) {
				return errors.New("invalid expected image digest")
			}
		}
	}
	if c.MaxBytes <= 0 || c.MaxBytes > 1<<40 {
		return errors.New("invalid delivery byte budget")
	}
	if len(c.Operations) != 2 || c.Operations[0] != "stage" || c.Operations[1] != "publish" {
		return errors.New("unsupported delivery operations")
	}
	return nil
}
func encode(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// Strict base64 decoding still ignores CR/LF; require the canonical encoding
// so all language implementations accept the same signed artifacts. Padding
// and whitespace are rejected rather than normalized before verification.
func decode(s string) ([]byte, error) {
	b, err := base64.RawURLEncoding.Strict().DecodeString(s)
	if err != nil || encode(b) != s {
		return nil, errors.New("noncanonical delivery base64url")
	}
	return b, nil
}
func parseChain(chain [][]byte) ([]*x509.Certificate, error) {
	out := make([]*x509.Certificate, len(chain))
	total := 0
	for i, der := range chain {
		total += len(der)
		if len(der) > 32768 || total > 128*1024 {
			return nil, errors.New("oversized certificate chain")
		}
		c, e := x509.ParseCertificate(der)
		if e != nil {
			return nil, e
		}
		out[i] = c
	}
	return out, nil
}

// Reject duplicate members at every depth as well as unknown fields, so Go and
// Swift cannot assign different meanings to one signed artifact.
func strictJSON(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	if err := uniqueJSON(d, 0); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing JSON")
	}
	// encoding/json otherwise accepts case-insensitive field aliases ("ISS")
	// and null strings. Signed artifacts must have exactly the same meaning in
	// every verifier, including Swift's case-sensitive Codable decoder.
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil || members == nil {
		return errors.New("expected delivery JSON object")
	}
	typ := reflect.TypeOf(out).Elem()
	for i := 0; i < typ.NumField(); i++ {
		name, options, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		value, exists := members[name]
		if !exists && options != "omitempty" {
			return fmt.Errorf("missing delivery JSON member %q", name)
		}
		if exists && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return fmt.Errorf("null delivery JSON member %q", name)
		}
		delete(members, name)
	}
	if len(members) != 0 {
		return errors.New("unknown delivery JSON member")
	}
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	return d.Decode(out)
}
func uniqueJSON(d *json.Decoder, depth int) error {
	if depth > 32 {
		return errors.New("JSON nesting exceeds delivery contract limit")
	}
	tok, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, e := d.Token()
			if e != nil {
				return e
			}
			s, ok := key.(string)
			if !ok || seen[s] {
				return errors.New("duplicate JSON member")
			}
			seen[s] = true
			if e = uniqueJSON(d, depth+1); e != nil {
				return e
			}
		}
	case '[':
		for d.More() {
			if e := uniqueJSON(d, depth+1); e != nil {
				return e
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	_, err = d.Token()
	return err
}
