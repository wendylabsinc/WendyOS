package legacycertproof

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

func TestMetadataSignsMethodBoundProof(t *testing.T) {
	certificatePEM, privateKeyPEM, certificate, key := testIdentity(t)
	signer, err := New("urn:wendy:org:7:user:user-a", certificatePEM, privateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	signer.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	signer.random = bytes.NewReader(bytes.Repeat([]byte{0x42}, 128))

	proof, err := signer.Metadata("/wendycloud.v1.TunnelBrokerService/ClientTunnel")
	if err != nil {
		t.Fatal(err)
	}
	serial := hex.EncodeToString(certificate.SerialNumber.Bytes())
	if got := proof.Get(IdentityHeader); len(got) != 1 || got[0] != "urn:wendy:org:7:user:user-a" {
		t.Fatalf("unexpected identity metadata: %v", got)
	}
	if got := proof.Get(CertificateSerialHeader); len(got) != 1 || got[0] != serial {
		t.Fatalf("unexpected certificate serial metadata: %v", got)
	}
	if got := proof.Get(TimestampHeader); len(got) != 1 || got[0] != "1700000000" {
		t.Fatalf("unexpected timestamp metadata: %v", got)
	}
	nonce := proof.Get(NonceHeader)[0]
	canonical, err := canonicalBytes(
		"wendycloud.v1.TunnelBrokerService/ClientTunnel",
		"urn:wendy:org:7:user:user-a",
		serial,
		"1700000000",
		nonce,
	)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(canonical)
	signature, err := base64.RawURLEncoding.DecodeString(proof.Get(SignatureHeader)[0])
	if err != nil {
		t.Fatal(err)
	}
	if !ecdsa.VerifyASN1(&key.PublicKey, digest[:], signature) {
		t.Fatal("proof signature did not verify")
	}

	wrongCanonical, err := canonicalBytes(
		"wendycloud.v1.TunnelBrokerService/RegisterPresence",
		"urn:wendy:org:7:user:user-a",
		serial,
		"1700000000",
		nonce,
	)
	if err != nil {
		t.Fatal(err)
	}
	wrongDigest := sha256.Sum256(wrongCanonical)
	if ecdsa.VerifyASN1(&key.PublicKey, wrongDigest[:], signature) {
		t.Fatal("proof signature verified for a different method")
	}
}

func TestNewRejectsMismatchedKey(t *testing.T) {
	certificatePEM, _, _, _ := testIdentity(t)
	_, otherKeyPEM, _, _ := testIdentity(t)
	if _, err := New("urn:wendy:org:1:asset:2", certificatePEM, otherKeyPEM); err == nil {
		t.Fatal("expected mismatched certificate and key to fail")
	}
}

func testIdentity(t *testing.T) (string, string, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(0x1234),
		Subject:      pkix.Name{CommonName: "proof test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	privateDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	privateKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateDER})
	return string(certificatePEM), string(privateKeyPEM), certificate, key
}
