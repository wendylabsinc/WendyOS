package commands

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func testEnrollmentToken(t *testing.T, payload string) string {
	t.Helper()
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
}

func TestEnrollmentTokenIdentity_UserEnrollment(t *testing.T) {
	token := testEnrollmentToken(t, `{"type":"user_enrollment","org_id":1,"user_id":"user-123"}`)

	cn, uris, err := enrollmentTokenIdentity(token)
	if err != nil {
		t.Fatalf("enrollmentTokenIdentity() error = %v", err)
	}
	if cn != "wendy/user/user-123" {
		t.Fatalf("enrollmentTokenIdentity() cn = %q, want %q", cn, "wendy/user/user-123")
	}
	if len(uris) != 1 || uris[0] != "urn:wendy:org:1:user:user-123" {
		t.Fatalf("enrollmentTokenIdentity() uris = %q, want [urn:wendy:org:1:user:user-123]", uris)
	}
}

func TestEnrollmentTokenIdentity_UserEnrollmentMissingOrg(t *testing.T) {
	token := testEnrollmentToken(t, `{"type":"user_enrollment","user_id":"user-123"}`)

	cn, uris, err := enrollmentTokenIdentity(token)
	if err != nil {
		t.Fatalf("enrollmentTokenIdentity() error = %v, want nil (legacy org-less token should keep login working)", err)
	}
	if cn != "wendy/user/user-123" {
		t.Fatalf("enrollmentTokenIdentity() cn = %q, want %q", cn, "wendy/user/user-123")
	}
	if len(uris) != 0 {
		t.Fatalf("enrollmentTokenIdentity() uris = %q, want none (CN-only for org-less token)", uris)
	}
}

func TestEnrollmentTokenIdentity_UserEnrollmentUserIDContainsColon(t *testing.T) {
	token := testEnrollmentToken(t, `{"type":"user_enrollment","org_id":1,"user_id":"user:123"}`)

	if _, _, err := enrollmentTokenIdentity(token); err == nil {
		t.Fatal("expected error for user_id containing ':'")
	}
}

func TestEnrollmentTokenIdentity_AssetEnrollment(t *testing.T) {
	token := testEnrollmentToken(t, `{"type":"asset_enrollment","org_id":7,"asset_id":42}`)

	cn, uris, err := enrollmentTokenIdentity(token)
	if err != nil {
		t.Fatalf("enrollmentTokenIdentity() error = %v", err)
	}
	if cn != "wendy/7/42" {
		t.Fatalf("enrollmentTokenIdentity() cn = %q, want %q", cn, "wendy/7/42")
	}
	if len(uris) != 1 || uris[0] != "urn:wendy:org:7:asset:42" {
		t.Fatalf("enrollmentTokenIdentity() uris = %q, want [urn:wendy:org:7:asset:42]", uris)
	}
}

func TestEnrollmentTokenIdentity_InvalidToken(t *testing.T) {
	if _, _, err := enrollmentTokenIdentity("not-a-jwt"); err == nil {
		t.Fatal("expected invalid token error")
	}
}

func TestStoredCertIdentityURN_User(t *testing.T) {
	cert := config.CertificateInfo{OrganizationID: 1, UserID: "user-123"}
	if urn := storedCertIdentityURN(cert); urn != "urn:wendy:org:1:user:user-123" {
		t.Fatalf("storedCertIdentityURN() = %q, want %q", urn, "urn:wendy:org:1:user:user-123")
	}
}

func TestStoredCertIdentityURN_UserIDContainsColon(t *testing.T) {
	cert := config.CertificateInfo{OrganizationID: 1, UserID: "user:123"}
	if urn := storedCertIdentityURN(cert); urn != "" {
		t.Fatalf("storedCertIdentityURN() = %q, want empty (CN-only refresh for unparseable user ID)", urn)
	}
}

func TestStoredCertIdentityURN_Asset(t *testing.T) {
	cert := config.CertificateInfo{OrganizationID: 7, AssetID: 42}
	if urn := storedCertIdentityURN(cert); urn != "urn:wendy:org:7:asset:42" {
		t.Fatalf("storedCertIdentityURN() = %q, want %q", urn, "urn:wendy:org:7:asset:42")
	}
}

func selfSignedCertPEM(t *testing.T, cn string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating certificate: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestCertCommonName(t *testing.T) {
	const wantCN = "wendy/user/3VBQnKRlcFMOFjnjyw8ca7Rk6jR2"
	certPEM := selfSignedCertPEM(t, wantCN)

	got, err := certCommonName(certPEM)
	if err != nil {
		t.Fatalf("certCommonName() error = %v", err)
	}
	if got != wantCN {
		t.Fatalf("certCommonName() = %q, want %q", got, wantCN)
	}
}

func TestCertCommonName_InvalidPEM(t *testing.T) {
	if _, err := certCommonName("not-a-pem"); err == nil {
		t.Fatal("expected error for invalid PEM")
	}
}
