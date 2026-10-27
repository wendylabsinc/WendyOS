package devicesign

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/url"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

const (
	tenant    = "11111111-1111-4111-8111-111111111111"
	principal = "spiffe://wendy.sh/tenant/" + tenant + "/device/sim"
)

// leafFor self-issues a device leaf for the key in keyPEM.
func leafFor(t *testing.T, keyPEM, uri string) string {
	t.Helper()
	signer, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	u, _ := url.Parse(uri)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "sim"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), URIs: []*url.URL{u},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, signer.Public(), caKey)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestSignProducesDeviceSignedRequest(t *testing.T) {
	keyPEM, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	certPEM := leafFor(t, keyPEM, principal)
	s, err := New(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	req := &cloudpbv2.SignedRequest{Payload: []byte("inner")} // any message will do
	signed, err := s.Sign("/wendycloud.v2.DevicePostboxService/AckHead", req, "corr-1")
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := proto.Marshal(req); string(signed.Payload) != string(want) || signed.PayloadType != "wendycloud.v2.SignedRequest" {
		t.Fatal("payload or payload_type is not the request")
	}

	parts := strings.Split(string(signed.Signature), ".")
	if len(parts) != 3 {
		t.Fatalf("JWS has %d segments", len(parts))
	}
	var header struct {
		Alg string   `json:"alg"`
		X5C []string `json:"x5c"`
		Kid string   `json:"kid"`
	}
	var claims struct {
		Operation     string            `json:"operation"`
		Target        map[string]string `json:"target"`
		BodySHA256    string            `json:"body_sha256"`
		CorrelationID string            `json:"correlation_id"`
		Aud           string            `json:"aud"`
		Nonce         string            `json:"nonce"`
	}
	decode(t, parts[0], &header)
	decode(t, parts[1], &claims)

	// Algorithm in use: ML-DSA-65, the device leaf by x5c only.
	block, _ := pem.Decode([]byte(certPEM))
	if header.Alg != "ML-DSA-65" || header.Kid != "" || len(header.X5C) != 1 || header.X5C[0] != base64.StdEncoding.EncodeToString(block.Bytes) {
		t.Fatalf("header = %+v", header)
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	signer, _ := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	if err := mldsa.Verify(signer.Public().(*mldsa.PublicKey), []byte(parts[0]+"."+parts[1]), sig, &mldsa.Options{}); err != nil {
		t.Fatalf("signature does not verify: %v", err)
	}
	sum := sha256.Sum256(signed.Payload)
	if claims.Operation != "wendycloud.v2.DevicePostboxService/AckHead" ||
		claims.Target["tenant"] != tenant || claims.Target["resource"] != principal ||
		claims.BodySHA256 != base64.RawURLEncoding.EncodeToString(sum[:]) ||
		claims.CorrelationID != "corr-1" || claims.Aud != brokerAudience || claims.Nonce == "" {
		t.Fatalf("claims = %+v", claims)
	}
}

// A P-256 device is refused, never signed with ES256 (HARD rule 1).
func TestNewRefusesNonMLDSA65Key(t *testing.T) {
	keyPEM, err := certs.GenerateKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New(leafFor(t, keyPEM, principal), keyPEM); err == nil || !strings.Contains(err.Error(), "ML-DSA-65") {
		t.Fatalf("New(P-256) err = %v, want an ML-DSA-65 refusal", err)
	}
}

func TestNewRefusesNonDevicePrincipal(t *testing.T) {
	keyPEM, _ := certs.GenerateMLDSAKeyPair()
	operator := "spiffe://wendy.sh/tenant/" + tenant + "/operator/someone"
	if _, err := New(leafFor(t, keyPEM, operator), keyPEM); err == nil {
		t.Fatal("New accepted an operator leaf as a device signer")
	}
}

func decode(t *testing.T, segment string, v any) {
	t.Helper()
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}
