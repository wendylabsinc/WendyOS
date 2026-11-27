// Package request verifies one narrowly bound World read proof. A verified
// Proof is not admission: callers must still apply site policy, revocation,
// authenticated-time freshness and durable replay protection before serving data.
package request

import (
	"crypto/mldsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
)

const (
	Type          = "wendy.world.request.v1"
	OperationRead = "world.read"
	Method        = "POST"
	Path          = "/v1/world/query"
	MaxBodyBytes  = 64 << 10
	MaxLifetime   = time.Minute
)

type Descriptor struct {
	Version         int    `json:"version"`
	Domain          string `json:"domain"`
	Tenant          string `json:"tenant"`
	Site            string `json:"site"`
	Audience        string `json:"audience"`
	Principal       string `json:"principal"`
	Operation       string `json:"operation"`
	Method          string `json:"method"`
	Path            string `json:"path"`
	BodySHA256      string `json:"body_sha256"`
	PolicyRevision  uint64 `json:"policy_revision"`
	IssuedAtUnixMS  int64  `json:"issued_at_unix_ms"`
	ExpiresAtUnixMS int64  `json:"expires_at_unix_ms"`
	Nonce           string `json:"nonce"`
}

// Expected comes from commissioned service configuration and the selected
// server route, never from the unverified proof itself.
type Expected struct {
	Tenant, Site, Audience, Operation, Method, Path string
	PolicyRevision                                  uint64
}
type Proof struct {
	Descriptor Descriptor
	Identity   wendycert.Identity
}

// BodyDigest hashes exact bytes. It does not parse or canonicalize a query body.
func BodyDigest(body []byte) (string, error) {
	if len(body) > MaxBodyBytes {
		return "", errors.New("read body exceeds bound")
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// Verify uses the same caller-supplied trusted interval for the certificate path
// and the whole request validity window. Expiry is exclusive. No clock is read.
func Verify(compact string, expected Expected, body []byte, verifier *wendycert.Verifier, earliest, latest time.Time) (Proof, error) {
	var empty Proof
	if err := validateExpected(expected); err != nil {
		return empty, err
	}
	digest, err := BodyDigest(body)
	if err != nil {
		return empty, err
	}
	payload, identity, err := jws.Verify(compact, Type, verifier, earliest, latest)
	if err != nil {
		return empty, err
	}
	var d Descriptor
	if jws.DecodeStrict(payload, &d) != nil {
		return empty, errors.New("invalid World request descriptor")
	}
	if err = validateDescriptor(d); err != nil {
		return empty, err
	}
	if d.Tenant != expected.Tenant || d.Site != expected.Site || d.Audience != expected.Audience || d.Operation != expected.Operation || d.Method != expected.Method || d.Path != expected.Path || d.PolicyRevision != expected.PolicyRevision || d.BodySHA256 != digest || d.Principal != identity.Principal || d.Tenant != identity.Tenant {
		return empty, errors.New("request does not match caller, body or commissioned target")
	}
	if earliest.Before(time.UnixMilli(d.IssuedAtUnixMS)) || !latest.Before(time.UnixMilli(d.ExpiresAtUnixMS)) {
		return empty, errors.New("request is not valid throughout the trusted time interval")
	}
	return Proof{Descriptor: d, Identity: identity}, nil
}

// Sign validates an explicit descriptor and raw body, then signs with the supplied
// key. It performs no credential lookup, enrollment, renewal, policy decision or
// clock read. Callers generate a fresh random 24-byte nonce for every request.
func Sign(d Descriptor, body []byte, chain [][]byte, key *mldsa.PrivateKey) (string, error) {
	if err := validateDescriptor(d); err != nil {
		return "", err
	}
	digest, err := BodyDigest(body)
	if err != nil {
		return "", err
	}
	if digest != d.BodySHA256 {
		return "", errors.New("request body digest mismatch")
	}
	payload, err := json.Marshal(d)
	if err != nil {
		return "", errors.New("cannot encode World request")
	}
	return jws.Sign(Type, payload, chain, key)
}
func validateExpected(e Expected) error {
	if !bounded(e.Tenant, 256) || !bounded(e.Site, 256) || !bounded(e.Audience, 512) || e.Operation != OperationRead || e.Method != Method || e.Path != Path || e.PolicyRevision == 0 {
		return errors.New("explicit commissioned World read target required")
	}
	return nil
}
func validateDescriptor(d Descriptor) error {
	if d.Version != 1 || d.Domain != "physical" || !bounded(d.Principal, 1024) {
		return errors.New("unsupported request version, domain or principal")
	}
	if err := validateExpected(Expected{Tenant: d.Tenant, Site: d.Site, Audience: d.Audience, Operation: d.Operation, Method: d.Method, Path: d.Path, PolicyRevision: d.PolicyRevision}); err != nil {
		return err
	}
	if len(d.BodySHA256) != 64 {
		return errors.New("SHA-256 body digest required")
	}
	digest, err := hex.DecodeString(d.BodySHA256)
	if err != nil || hex.EncodeToString(digest) != d.BodySHA256 {
		return errors.New("canonical SHA-256 body digest required")
	}
	nonce, err := base64.RawURLEncoding.Strict().DecodeString(d.Nonce)
	if err != nil || len(nonce) != 24 || base64.RawURLEncoding.EncodeToString(nonce) != d.Nonce {
		return errors.New("canonical 24-byte request nonce required")
	}
	if d.IssuedAtUnixMS <= 0 || d.ExpiresAtUnixMS <= d.IssuedAtUnixMS || d.ExpiresAtUnixMS-d.IssuedAtUnixMS > MaxLifetime.Milliseconds() {
		return errors.New("request lifetime must be positive and at most 60 seconds")
	}
	return nil
}
func bounded(s string, max int) bool {
	if len(s) == 0 || len(s) > max || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
