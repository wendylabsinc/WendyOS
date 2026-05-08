package certs

import (
	"bytes"
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

// helpers shared by sign tests

func generateTestKeyAndCert(t *testing.T) (*ecdsa.PrivateKey, *x509.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parsing certificate: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return key, cert, string(pemBytes)
}

func TestSignBytesAndVerifyBytes_RoundTrip(t *testing.T) {
	key, cert, _ := generateTestKeyAndCert(t)
	data := []byte("hello, wendy")

	sig, err := SignBytes(data, key)
	if err != nil {
		t.Fatalf("SignBytes: %v", err)
	}
	if err := VerifyBytes(data, sig, cert); err != nil {
		t.Errorf("VerifyBytes: %v", err)
	}
}

func TestVerifyBytes_WrongData(t *testing.T) {
	key, cert, _ := generateTestKeyAndCert(t)
	sig, _ := SignBytes([]byte("original"), key)
	if err := VerifyBytes([]byte("tampered"), sig, cert); err == nil {
		t.Error("expected verification failure for tampered data")
	}
}

func TestParseLeafCertificate_Valid(t *testing.T) {
	_, want, pemStr := generateTestKeyAndCert(t)
	got, err := ParseLeafCertificate(pemStr)
	if err != nil {
		t.Fatalf("ParseLeafCertificate: %v", err)
	}
	if !got.Equal(want) {
		t.Error("parsed certificate does not match original")
	}
}

func TestParseLeafCertificate_Invalid(t *testing.T) {
	if _, err := ParseLeafCertificate("not pem"); err == nil {
		t.Error("expected error for invalid PEM")
	}
}

func TestSigningPayload_Stable(t *testing.T) {
	annotations := map[string]string{
		"sh.wendy/entitlement.gpu":       "",
		"sh.wendy/entitlement.bluetooth": "",
		"sh.wendy/signed.repo":           "myapp",
		"sh.wendy/signed.at":             "2024-01-01T00:00:00Z",
		"sh.wendy/signature":             "should-be-excluded",
		"sh.wendy/signature.cert":        "should-be-excluded",
	}
	payload := SigningPayload(nil, annotations)
	s := string(payload)
	if strings.Contains(s, "should-be-excluded") {
		t.Error("payload must not contain signature annotations")
	}
	if !strings.Contains(s, "sh.wendy/entitlement.bluetooth") {
		t.Error("payload missing bluetooth entitlement key")
	}
	if !strings.Contains(s, "sh.wendy/entitlement.gpu") {
		t.Error("payload missing gpu entitlement key")
	}
	if !strings.Contains(s, "sh.wendy/signed.repo=myapp") {
		t.Error("payload missing sh.wendy/signed.repo")
	}
	if !strings.Contains(s, "sh.wendy/signed.at=2024-01-01T00:00:00Z") {
		t.Error("payload missing sh.wendy/signed.at")
	}
	// Verify ordering: bluetooth < gpu < signed.at < signed.repo alphabetically.
	btIdx := strings.Index(s, "bluetooth")
	gpuIdx := strings.Index(s, "gpu")
	if btIdx > gpuIdx {
		t.Errorf("keys not sorted: bluetooth at %d, gpu at %d", btIdx, gpuIdx)
	}
}

func TestSigningPayload_ValueEscaping(t *testing.T) {
	annotations := map[string]string{
		"sh.wendy/entitlement.weird": "val%with\nnewline\rand%percent",
	}
	payload := string(SigningPayload(nil, annotations))
	if strings.Contains(payload, "\n\n") {
		t.Error("unescaped newline in value would create spurious record boundary")
	}
	if !strings.Contains(payload, "%25") {
		t.Error("payload missing escaped percent (%25)")
	}
	if !strings.Contains(payload, "%0A") {
		t.Error("payload missing escaped newline (%0A)")
	}
	if !strings.Contains(payload, "%0D") {
		t.Error("payload missing escaped carriage return (%0D)")
	}
}

func TestSigningPayload_Empty(t *testing.T) {
	if payload := SigningPayload(nil, nil); len(payload) != 0 {
		t.Errorf("expected empty payload for nil inputs, got %q", payload)
	}
	if payload := SigningPayload(nil, map[string]string{}); len(payload) != 0 {
		t.Errorf("expected empty payload for empty annotations, got %q", payload)
	}
}

func TestSigningPayload_ContentDigestsIncluded(t *testing.T) {
	digests := []string{"sha256:bbb", "sha256:aaa"} // intentionally unsorted
	payload := string(SigningPayload(digests, nil))
	// Both digests must appear, sorted: aaa before bbb.
	if !strings.Contains(payload, "digest=sha256:aaa") {
		t.Error("payload missing sha256:aaa")
	}
	if !strings.Contains(payload, "digest=sha256:bbb") {
		t.Error("payload missing sha256:bbb")
	}
	if strings.Index(payload, "sha256:aaa") > strings.Index(payload, "sha256:bbb") {
		t.Error("digests not sorted: aaa must appear before bbb")
	}
}

func TestSigningPayload_ContentDigestsDontMatchEntitlementOrder(t *testing.T) {
	// Digests (digest=) must precede entitlement keys (sh.wendy/) in the payload.
	payload := string(SigningPayload(
		[]string{"sha256:abc"},
		map[string]string{"sh.wendy/entitlement.gpu": `{}`},
	))
	dIdx := strings.Index(payload, "digest=")
	eIdx := strings.Index(payload, "sh.wendy/")
	if dIdx < 0 || eIdx < 0 {
		t.Fatalf("missing expected content in payload: %q", payload)
	}
	if dIdx > eIdx {
		t.Errorf("digest line should precede entitlement line; got dIdx=%d eIdx=%d", dIdx, eIdx)
	}
}

func TestSignAndVerify_EntitlementAnnotations(t *testing.T) {
	key, cert, certPEM := generateTestKeyAndCert(t)
	digests := []string{"sha256:layer1", "sha256:config"}
	annotations := map[string]string{
		"sh.wendy/entitlement.gpu":       "",
		"sh.wendy/entitlement.bluetooth": "",
	}

	payload := SigningPayload(digests, annotations)
	sig, err := SignBytes(payload, key)
	if err != nil {
		t.Fatalf("SignBytes: %v", err)
	}
	annotations[AnnotationSignature] = sig
	annotations[AnnotationSignatureCert] = certPEM

	// Verify using the cert from annotations (signature keys must be excluded from payload).
	parsedCert, err := ParseLeafCertificate(annotations[AnnotationSignatureCert])
	if err != nil {
		t.Fatalf("ParseLeafCertificate: %v", err)
	}
	verifyPayload := SigningPayload(digests, annotations)
	if err := VerifyBytes(verifyPayload, annotations[AnnotationSignature], parsedCert); err != nil {
		t.Errorf("end-to-end verification failed: %v", err)
	}

	// Tamper with an entitlement value and confirm verification fails.
	annotations["sh.wendy/entitlement.gpu"] = `{"port":9999}`
	tamperedPayload := SigningPayload(digests, annotations)
	if err := VerifyBytes(tamperedPayload, sig, cert); err == nil {
		t.Error("expected verification failure after entitlement tampering")
	}

	// Restore entitlements and tamper with a layer digest.
	annotations["sh.wendy/entitlement.gpu"] = `{"port":0}`
	layerTamperedPayload := SigningPayload([]string{"sha256:layer1", "sha256:evil"}, annotations)
	if err := VerifyBytes(layerTamperedPayload, sig, cert); err == nil {
		t.Error("expected verification failure after layer digest tampering")
	}
}
