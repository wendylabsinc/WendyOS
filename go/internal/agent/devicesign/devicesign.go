// Package devicesign signs the Cloud RPCs marked
// option (wendy.device_signed_request) — KeyDirectoryService.
// RegisterDeviceEncryptionKey and DevicePostboxService.AckHead — with the
// device's own leaf, which must also be the connection's mTLS peer
// (service-protos conformance/reqsig, WDY-3926).
//
// The envelope is the operator one (internal/cli/cloudrequest): a SignedRequest
// whose signature is an ML-DSA-65 JWS over the JCS claims, naming the leaf by
// x5c only. A device that still holds a P-256 key is refused, never signed
// with ES256: it switches to ML-DSA-65 at its next certificate renewal.
package devicesign

import (
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

const (
	brokerAudience = "https://cloud.wendy.sh/broker"
	signatureTTL   = 30 * time.Second
)

// Signer holds one device identity. It is safe for concurrent use.
type Signer struct {
	key      *mldsa.PrivateKey
	leafDER  []byte
	tenant   string
	resource string
	audience string
	now      func() time.Time
	random   io.Reader
}

// New loads the device identity that signs. It refuses any key other than
// ML-DSA-65 and any leaf that is not a pki-core device principal.
func New(certPEM, keyPEM string) (*Signer, error) {
	pair, err := certs.TLSKeyPair(certPEM, "", keyPEM)
	if err != nil {
		return nil, fmt.Errorf("loading device signing identity: %w", err)
	}
	key, ok := pair.PrivateKey.(*mldsa.PrivateKey)
	if !ok || key.PublicKey().Parameters() != mldsa.MLDSA65() {
		return nil, fmt.Errorf("device request signing requires an ML-DSA-65 device key, but this device holds %T; it switches at its next certificate renewal", pair.PrivateKey)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0]) // normalized by TLSKeyPair
	if err != nil {
		return nil, fmt.Errorf("parsing device leaf: %w", err)
	}
	principal, _ := certs.TenantPrincipalFromCert(leaf)
	id, err := certs.ParsePrincipal(principal)
	if err != nil || principal != certs.DeviceSPIFFEURI(id.TenantUUID, id.EntityID) {
		return nil, fmt.Errorf("device signing leaf names %q, not a device principal", principal)
	}
	return &Signer{
		key: key, leafDER: pair.Certificate[0], tenant: id.TenantUUID, resource: principal,
		audience: brokerAudience, now: time.Now, random: rand.Reader,
	}, nil
}

// Sign wraps req for method (e.g. "/wendycloud.v2.DevicePostboxService/AckHead").
// target.resource is the leaf's own SPIFFE URI: a device only ever signs for
// itself (service-protos conformance/reqsig device profile).
func (s *Signer) Sign(method string, req proto.Message, correlationID string) (*cloudpbv2.SignedRequest, error) {
	payload, err := proto.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling %s: %w", method, err)
	}
	nonce := make([]byte, 32)
	if _, err := io.ReadFull(s.random, nonce); err != nil {
		return nil, fmt.Errorf("generating request nonce: %w", err)
	}
	sum := sha256.Sum256(payload)
	now := s.now().Unix()
	claims, err := canonicalJSON(map[string]any{
		"aud":            s.audience,
		"body_sha256":    base64.RawURLEncoding.EncodeToString(sum[:]),
		"correlation_id": correlationID,
		"expiry":         now + int64(signatureTTL/time.Second),
		"iat":            now,
		"nonce":          base64.RawURLEncoding.EncodeToString(nonce),
		"operation":      strings.TrimPrefix(method, "/"),
		"target":         map[string]any{"resource": s.resource, "tenant": s.tenant},
	})
	if err != nil {
		return nil, err
	}
	header, err := canonicalJSON(map[string]any{
		"alg": "ML-DSA-65",
		"x5c": []string{base64.StdEncoding.EncodeToString(s.leafDER)},
	})
	if err != nil {
		return nil, err
	}
	input := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	// Raw FIPS 204 over the signing input, empty context: what pki-core and
	// cloud verify (same as the operator signer).
	sig, err := s.key.Sign(s.random, []byte(input), &mldsa.Options{})
	if err != nil {
		return nil, fmt.Errorf("signing %s: %w", method, err)
	}
	return &cloudpbv2.SignedRequest{
		Payload:     payload,
		PayloadType: string(req.ProtoReflect().Descriptor().FullName()),
		Signature:   []byte(input + "." + base64.RawURLEncoding.EncodeToString(sig)),
	}, nil
}

// canonicalJSON covers the claims' narrow RFC 8785 subset (string-keyed
// objects, strings, arrays, integers), as in internal/cli/cloudrequest.
func canonicalJSON(value any) ([]byte, error) {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return nil, err
	}
	return []byte(strings.TrimSuffix(b.String(), "\n")), nil
}
