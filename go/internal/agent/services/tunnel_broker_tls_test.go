package services

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// ecdsaCertKeyPEM returns a valid self-signed ECDSA leaf cert + key in PEM form.
func ecdsaCertKeyPEM(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-device"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
	if err != nil {
		t.Fatalf("creating cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(priv)
	if err != nil {
		t.Fatalf("marshaling key: %v", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return certPEM, keyPEM
}

// A valid ECDSA leaf/key is presented as the client certificate.
func TestBrokerTLSConfig_PresentsValidClientCert(t *testing.T) {
	certPEM, keyPEM := ecdsaCertKeyPEM(t)
	cfg, err := brokerTLSConfig(zap.NewNop(), certPEM, keyPEM, "")
	if err != nil {
		t.Fatalf("brokerTLSConfig returned error: %v", err)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("expected 1 client certificate, got %d", len(cfg.Certificates))
	}
}

// A malformed key must NOT fail the dial: the broker authenticates on the XFCC
// header today, so a cert-load failure falls back to presenting no client cert
// (no error), rather than blocking the connection.
func TestBrokerTLSConfig_MalformedKeyFallsBackToNoClientCert(t *testing.T) {
	certPEM, _ := ecdsaCertKeyPEM(t)
	cfg, err := brokerTLSConfig(zap.NewNop(), certPEM, "-----BEGIN EC PRIVATE KEY-----\nnot a key\n-----END EC PRIVATE KEY-----\n", "")
	if err != nil {
		t.Fatalf("cert-load failure must be non-fatal, got error: %v", err)
	}
	if len(cfg.Certificates) != 0 {
		t.Fatalf("expected no client certificate on load failure, got %d", len(cfg.Certificates))
	}
}

// With no cert/key material at all, no client cert is presented and no error.
func TestBrokerTLSConfig_EmptyCertKeyPresentsNoClientCert(t *testing.T) {
	cfg, err := brokerTLSConfig(zap.NewNop(), "", "", "")
	if err != nil {
		t.Fatalf("empty cert/key must be non-fatal, got error: %v", err)
	}
	if len(cfg.Certificates) != 0 {
		t.Fatalf("expected no client certificate, got %d", len(cfg.Certificates))
	}
}

func TestBrokerDialOptsAddsFreshCertificateProof(t *testing.T) {
	certPEM, keyPEM := ecdsaCertKeyPEM(t)
	_, requestMetadata, err := brokerDialOpts(zap.NewNop(), 7, 42, certPEM, keyPEM, "")
	if err != nil {
		t.Fatal(err)
	}
	first, err := requestMetadata("/wendycloud.v1.TunnelBrokerService/RegisterPresence")
	if err != nil {
		t.Fatal(err)
	}
	second, err := requestMetadata("/wendycloud.v1.TunnelBrokerService/RegisterPresence")
	if err != nil {
		t.Fatal(err)
	}
	if got := first.Get("x-wendy-certificate-uri"); len(got) != 1 || got[0] != "urn:wendy:org:7:asset:42" {
		t.Fatalf("unexpected proof identity: %v", got)
	}
	if got := first.Get("x-wendy-certificate-serial"); len(got) != 1 || got[0] != "01" {
		t.Fatalf("unexpected proof certificate serial: %v", got)
	}
	if first.Get("x-wendy-certificate-signature")[0] == "" {
		t.Fatal("proof signature is empty")
	}
	if first.Get("x-wendy-certificate-nonce")[0] == second.Get("x-wendy-certificate-nonce")[0] {
		t.Fatal("successive RPC proofs reused a nonce")
	}
}

// A malformed CA chain is a hard error (the broker's server cert can't be
// validated without it) — unlike the client-cert fallback.
func TestBrokerTLSConfig_MalformedChainErrors(t *testing.T) {
	if _, err := brokerTLSConfig(zap.NewNop(), "", "", "not-a-pem-chain"); err == nil {
		t.Fatal("expected error for a malformed CA chain, got nil")
	}
}

// TestBrokerTLSConfig_TrailingBytesChainAccepted is the regression guard for the
// pki-core enrollment chain (WDY-2339). Those certificates carry trailing bytes
// after the outer ASN.1 SEQUENCE, so x509.CertPool.AppendCertsFromPEM drops them
// and reports false. brokerTLSConfig read that false as fatal, which meant a
// device enrolled against pki-core came up provisioned and unable to build a
// broker connection at all — the chain was not merely untrusted, the dial never
// happened.
func TestBrokerTLSConfig_TrailingBytesChainAccepted(t *testing.T) {
	certPEM, keyPEM := ecdsaCertKeyPEM(t)

	block, _ := pem.Decode([]byte(certPEM))
	if block == nil {
		t.Fatal("generated cert is not valid PEM")
	}
	der := append(append([]byte{}, block.Bytes...), 0x00, 0x00)
	chainPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if x509.NewCertPool().AppendCertsFromPEM(chainPEM) {
		t.Fatal("AppendCertsFromPEM accepted a trailing-bytes chain; this guard no longer tests anything")
	}

	cfg, err := brokerTLSConfig(zap.NewNop(), certPEM, keyPEM, string(chainPEM))
	if err != nil {
		t.Fatalf("brokerTLSConfig rejected a pki-core chain: %v", err)
	}
	if cfg.VerifyConnection == nil {
		t.Fatal("expected a VerifyConnection callback pinning the chain")
	}
}

// Each broker auth-strength downgrade is an audit-trail event (the agent's
// audit trail is its structured journal log): it names what was lost, and the
// connection's actual strength matches it. RULINGS 2026-10-04: no silent
// downgrade.
func TestBrokerDialOpts_DowngradesAreAudited(t *testing.T) {
	p256Cert, _ := ecdsaCertKeyPEM(t)
	p384Key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &p384Key.PublicKey, p384Key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(p384Key)
	if err != nil {
		t.Fatal(err)
	}
	p384Cert := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	p384KeyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	validCert, validKey := ecdsaCertKeyPEM(t)

	for _, tc := range []struct {
		name, certPEM, keyPEM string
		want                  []string // auth_downgrade values, in order
	}{
		{"none", validCert, validKey, nil},
		// tls.X509KeyPair accepts P-384; the P-256-only proof does not.
		{"proof only", p384Cert, p384KeyPEM, []string{"signed_proof->unsigned_headers"}},
		{"mtls and proof", p256Cert, "-----BEGIN EC PRIVATE KEY-----\nnot a key\n-----END EC PRIVATE KEY-----\n",
			[]string{"mtls_client_cert->none", "signed_proof->unsigned_headers"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			_, requestMetadata, err := brokerDialOpts(zap.New(core), 7, 42, tc.certPEM, tc.keyPEM, "")
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, e := range logs.All() {
				if v, ok := e.ContextMap()["auth_downgrade"].(string); ok {
					got = append(got, v)
				}
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("audited downgrades = %v, want %v", got, tc.want)
			}
			md, err := requestMetadata("/wendycloud.v1.TunnelBrokerService/RegisterPresence")
			if err != nil {
				t.Fatal(err)
			}
			signed := len(md.Get("x-wendy-certificate-signature")) == 1
			if wantSigned := !slices.Contains(tc.want, "signed_proof->unsigned_headers"); signed != wantSigned {
				t.Fatalf("request carries signed proof = %v, want %v", signed, wantSigned)
			}
		})
	}
}
