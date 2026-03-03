package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestGenerateKeyPair(t *testing.T) {
	keyPEM, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair() error = %v", err)
	}

	if !strings.Contains(keyPEM, "EC PRIVATE KEY") {
		t.Error("GenerateKeyPair() result does not contain EC PRIVATE KEY header")
	}

	// Should be parseable.
	block, _ := pem.Decode([]byte(keyPEM))
	if block == nil {
		t.Fatal("GenerateKeyPair() produced invalid PEM")
	}

	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parsing generated key: %v", err)
	}

	if key.Curve != elliptic.P256() {
		t.Errorf("key curve = %v, want P-256", key.Curve.Params().Name)
	}
}

func TestGenerateCSR(t *testing.T) {
	keyPEM, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair() error = %v", err)
	}

	csrPEM, err := GenerateCSR(keyPEM, "test-device.example.com")
	if err != nil {
		t.Fatalf("GenerateCSR() error = %v", err)
	}

	if !strings.Contains(csrPEM, "CERTIFICATE REQUEST") {
		t.Error("GenerateCSR() result does not contain CERTIFICATE REQUEST header")
	}

	// Parse and verify the CSR.
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil {
		t.Fatal("GenerateCSR() produced invalid PEM")
	}

	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("parsing generated CSR: %v", err)
	}

	if csr.Subject.CommonName != "test-device.example.com" {
		t.Errorf("CSR CommonName = %q, want %q", csr.Subject.CommonName, "test-device.example.com")
	}
}

func TestExtractPublicKey(t *testing.T) {
	keyPEM, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair() error = %v", err)
	}

	pubPEM, err := ExtractPublicKey(keyPEM)
	if err != nil {
		t.Fatalf("ExtractPublicKey() error = %v", err)
	}

	if !strings.Contains(pubPEM, "PUBLIC KEY") {
		t.Error("ExtractPublicKey() result does not contain PUBLIC KEY header")
	}

	// Parse and verify it is an EC public key.
	block, _ := pem.Decode([]byte(pubPEM))
	if block == nil {
		t.Fatal("ExtractPublicKey() produced invalid PEM")
	}

	pub, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatalf("parsing public key: %v", err)
	}

	ecPub, ok := pub.(*ecdsa.PublicKey)
	if !ok {
		t.Fatal("extracted key is not an ECDSA public key")
	}

	if ecPub.Curve != elliptic.P256() {
		t.Errorf("public key curve = %v, want P-256", ecPub.Curve.Params().Name)
	}
}

// selfSignedCert generates a self-signed certificate and its private key for testing.
func selfSignedCert(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}

	certBlock := &pem.Block{Type: "CERTIFICATE", Bytes: certDER}
	certStr := string(pem.EncodeToMemory(certBlock))

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshaling key: %v", err)
	}
	keyBlock := &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}
	keyStr := string(pem.EncodeToMemory(keyBlock))

	return certStr, keyStr
}

func TestLoadTLSConfig(t *testing.T) {
	certPEM, keyPEM := selfSignedCert(t)

	tlsCfg, err := LoadTLSConfig(certPEM, "", keyPEM, "")
	if err != nil {
		t.Fatalf("LoadTLSConfig() error = %v", err)
	}

	if len(tlsCfg.Certificates) != 1 {
		t.Errorf("Certificates count = %d, want 1", len(tlsCfg.Certificates))
	}

	if tlsCfg.MinVersion != 0x0303 { // tls.VersionTLS12
		t.Errorf("MinVersion = %x, want TLS 1.2 (0x0303)", tlsCfg.MinVersion)
	}
}

func TestLoadTLSConfig_InvalidCert(t *testing.T) {
	_, err := LoadTLSConfig("not-a-cert", "", "not-a-key", "")
	if err == nil {
		t.Fatal("LoadTLSConfig() expected error for invalid cert, got nil")
	}
}

func TestLoadTLSConfig_WithCABundle(t *testing.T) {
	certPEM, keyPEM := selfSignedCert(t)

	// Use the self-signed cert as the CA bundle too.
	tlsCfg, err := LoadTLSConfig(certPEM, "", keyPEM, certPEM)
	if err != nil {
		t.Fatalf("LoadTLSConfig() error = %v", err)
	}

	if tlsCfg.RootCAs == nil {
		t.Error("RootCAs is nil, expected CA pool")
	}
}

func TestGenerateAndCSR_RoundTrip(t *testing.T) {
	// Generate a key pair.
	keyPEM, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair() error = %v", err)
	}

	// Generate a CSR with the key.
	csrPEM, err := GenerateCSR(keyPEM, "roundtrip.example.com")
	if err != nil {
		t.Fatalf("GenerateCSR() error = %v", err)
	}

	// Extract the public key from the private key.
	pubPEM, err := ExtractPublicKey(keyPEM)
	if err != nil {
		t.Fatalf("ExtractPublicKey() error = %v", err)
	}

	// Parse the CSR and verify its public key matches the extracted one.
	csrBlock, _ := pem.Decode([]byte(csrPEM))
	csr, err := x509.ParseCertificateRequest(csrBlock.Bytes)
	if err != nil {
		t.Fatalf("parsing CSR: %v", err)
	}

	pubBlock, _ := pem.Decode([]byte(pubPEM))
	extractedPub, err := x509.ParsePKIXPublicKey(pubBlock.Bytes)
	if err != nil {
		t.Fatalf("parsing public key: %v", err)
	}

	csrPub := csr.PublicKey.(*ecdsa.PublicKey)
	extPub := extractedPub.(*ecdsa.PublicKey)

	if csrPub.X.Cmp(extPub.X) != 0 || csrPub.Y.Cmp(extPub.Y) != 0 {
		t.Error("CSR public key does not match extracted public key")
	}
}
