package certs

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
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

	csrPEM, err := GenerateCSR([]byte(keyPEM), "test-device.example.com", nil)
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

	// An empty identityURN must not add any URI SAN.
	if len(csr.URIs) != 0 {
		t.Errorf("CSR with empty identityURN has %d URI SANs, want 0", len(csr.URIs))
	}
}

// TestGenerateCSRIncludesIdentityURN verifies both entity flavours the callers
// use (user for the CLI, asset for the agent) land in the CSR as a single URI
// SAN that IdentityFromCert reads back as the authoritative identity.
func TestGenerateCSRIncludesIdentityURN(t *testing.T) {
	cases := []struct {
		name   string
		urn    string
		wantID WendyIdentity
	}{
		{"user", UserURN(1, "user-123"), WendyIdentity{OrgID: 1, EntityType: "user", EntityID: "user-123"}},
		{"asset", AssetURN(7, 42), WendyIdentity{OrgID: 7, EntityType: "asset", EntityID: "42"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keyPEM, err := GenerateKeyPair()
			if err != nil {
				t.Fatalf("GenerateKeyPair() error = %v", err)
			}

			csrPEM, err := GenerateCSR([]byte(keyPEM), "sh/wendy/legacy", []string{tc.urn})
			if err != nil {
				t.Fatalf("GenerateCSR() error = %v", err)
			}

			block, _ := pem.Decode([]byte(csrPEM))
			csr, err := x509.ParseCertificateRequest(block.Bytes)
			if err != nil {
				t.Fatalf("parsing generated CSR: %v", err)
			}
			if err := csr.CheckSignature(); err != nil {
				t.Fatalf("CSR signature invalid: %v", err)
			}

			if len(csr.URIs) != 1 {
				t.Fatalf("CSR has %d URI SANs, want 1", len(csr.URIs))
			}
			if got := csr.URIs[0].String(); got != tc.urn {
				t.Errorf("CSR URI SAN = %q, want %q", got, tc.urn)
			}

			// The SAN must be what IdentityFromCert resolves. A CertificateRequest
			// and a Certificate share the URIs field, so read it via a synthetic
			// leaf carrying the same SANs.
			leaf := &x509.Certificate{Subject: csr.Subject, URIs: csr.URIs}
			id, ok, err := IdentityFromCert(leaf)
			if err != nil {
				t.Fatalf("IdentityFromCert() error = %v", err)
			}
			if !ok {
				t.Fatal("IdentityFromCert() found no identity in a URN-bearing cert")
			}
			if id != tc.wantID {
				t.Errorf("IdentityFromCert() = %+v, want %+v", id, tc.wantID)
			}
		})
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

func TestLeafCertificatePEM(t *testing.T) {
	leafPEM, _ := selfSignedCert(t)
	chainPEM, _ := selfSignedCert(t)

	got, err := LeafCertificatePEM(leafPEM + "\n" + chainPEM)
	if err != nil {
		t.Fatalf("LeafCertificatePEM() error = %v", err)
	}
	if got != leafPEM {
		t.Errorf("LeafCertificatePEM() = %q; want %q", got, leafPEM)
	}
}

func TestLeafCertificatePEM_StripsTrailingASN1Data(t *testing.T) {
	leafPEM, _ := selfSignedCert(t)

	block, _ := pem.Decode([]byte(leafPEM))
	if block == nil {
		t.Fatal("selfSignedCert produced invalid PEM")
	}
	wantDER := append([]byte(nil), block.Bytes...)
	block.Bytes = append(block.Bytes, 0xde, 0xad, 0xbe, 0xef)

	got, err := LeafCertificatePEM(string(pem.EncodeToMemory(block)))
	if err != nil {
		t.Fatalf("LeafCertificatePEM() error = %v", err)
	}

	gotBlock, _ := pem.Decode([]byte(got))
	if gotBlock == nil {
		t.Fatal("LeafCertificatePEM() produced invalid PEM")
	}
	if !bytes.Equal(gotBlock.Bytes, wantDER) {
		t.Fatalf("LeafCertificatePEM() kept trailing bytes; got DER len %d, want %d", len(gotBlock.Bytes), len(wantDER))
	}
	if _, err := x509.ParseCertificate(gotBlock.Bytes); err != nil {
		t.Fatalf("parsing stripped certificate: %v", err)
	}
}

func TestLeafCertificatePEM_NoCertificate(t *testing.T) {
	_, err := LeafCertificatePEM("not-a-cert")
	if err == nil {
		t.Fatal("LeafCertificatePEM() expected error for missing certificate")
	}
}

func TestGenerateAndCSR_RoundTrip(t *testing.T) {
	// Generate a key pair.
	keyPEM, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair() error = %v", err)
	}

	// Generate a CSR with the key.
	csrPEM, err := GenerateCSR([]byte(keyPEM), "roundtrip.example.com", nil)
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

func TestGenerateCSRRequestsClientAuthUsage(t *testing.T) {
	keyPEM, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair() error = %v", err)
	}

	csrPEM, err := GenerateCSR([]byte(keyPEM), "wendy/user/abc", nil)
	if err != nil {
		t.Fatalf("GenerateCSR() error = %v", err)
	}

	block, _ := pem.Decode([]byte(csrPEM))
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("parsing generated CSR: %v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("CSR signature invalid: %v", err)
	}

	var keyUsage asn1.BitString
	var ekus []asn1.ObjectIdentifier
	for _, ext := range csr.Extensions {
		switch {
		case ext.Id.Equal(asn1.ObjectIdentifier{2, 5, 29, 15}):
			if _, err := asn1.Unmarshal(ext.Value, &keyUsage); err != nil {
				t.Fatalf("unmarshaling key usage: %v", err)
			}
		case ext.Id.Equal(asn1.ObjectIdentifier{2, 5, 29, 37}):
			if _, err := asn1.Unmarshal(ext.Value, &ekus); err != nil {
				t.Fatalf("unmarshaling extended key usage: %v", err)
			}
		}
	}

	// digitalSignature is bit 0 of the KeyUsage BIT STRING.
	if keyUsage.BitLength == 0 || keyUsage.At(0) != 1 {
		t.Errorf("CSR key usage missing digitalSignature, got %v", keyUsage)
	}

	clientAuthOID := asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 2}
	found := false
	for _, oid := range ekus {
		if oid.Equal(clientAuthOID) {
			found = true
		}
	}
	if !found {
		t.Errorf("CSR extended key usage missing clientAuth, got %v", ekus)
	}

	// The default (no explicit usages) must remain clientAuth-only so user/CLI
	// certs stay scoped to client authentication.
	serverAuthOID := asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 1}
	for _, oid := range ekus {
		if oid.Equal(serverAuthOID) {
			t.Errorf("default CSR extended key usage unexpectedly includes serverAuth, got %v", ekus)
		}
	}
}

func TestGenerateCSRRequestsClientAndServerAuthUsage(t *testing.T) {
	keyPEM, err := GenerateKeyPair()
	if err != nil {
		t.Fatalf("GenerateKeyPair() error = %v", err)
	}

	csrPEM, err := GenerateCSR([]byte(keyPEM), "sh/wendy/1/2", []string{AssetURN(1, 2)},
		x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth)
	if err != nil {
		t.Fatalf("GenerateCSR() error = %v", err)
	}

	block, _ := pem.Decode([]byte(csrPEM))
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("parsing generated CSR: %v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		t.Fatalf("CSR signature invalid: %v", err)
	}

	var ekus []asn1.ObjectIdentifier
	for _, ext := range csr.Extensions {
		if ext.Id.Equal(asn1.ObjectIdentifier{2, 5, 29, 37}) {
			if _, err := asn1.Unmarshal(ext.Value, &ekus); err != nil {
				t.Fatalf("unmarshaling extended key usage: %v", err)
			}
		}
	}

	clientAuthOID := asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 2}
	serverAuthOID := asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 1}
	var hasClient, hasServer bool
	for _, oid := range ekus {
		if oid.Equal(clientAuthOID) {
			hasClient = true
		}
		if oid.Equal(serverAuthOID) {
			hasServer = true
		}
	}
	if !hasClient {
		t.Errorf("CSR extended key usage missing clientAuth, got %v", ekus)
	}
	if !hasServer {
		t.Errorf("CSR extended key usage missing serverAuth, got %v", ekus)
	}
}
