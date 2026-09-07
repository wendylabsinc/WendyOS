package services

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"go.uber.org/zap"
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
