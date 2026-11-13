package commands

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

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// certPEM builds a self-signed leaf whose NotAfter is `until`.
func certPEM(t *testing.T, until time.Time) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    until.Add(-24 * time.Hour),
		NotAfter:     until,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("cert: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func TestCertExpired(t *testing.T) {
	now := time.Now()
	if !certExpired(config.CertificateInfo{PemCertificate: certPEM(t, now.Add(-time.Hour))}, now) {
		t.Error("past NotAfter should be expired")
	}
	if certExpired(config.CertificateInfo{PemCertificate: certPEM(t, now.Add(time.Hour))}, now) {
		t.Error("future NotAfter should not be expired")
	}
	// Unparseable cert is attempted (not treated as expired), so the handshake
	// can surface the real error.
	if certExpired(config.CertificateInfo{PemCertificate: "garbage"}, now) {
		t.Error("unparseable cert should not be reported expired")
	}
}

func TestPreferValidCerts(t *testing.T) {
	now := time.Now()
	expired := config.CertificateInfo{OrganizationID: 1, PemCertificate: certPEM(t, now.Add(-time.Hour))}
	valid := config.CertificateInfo{OrganizationID: 2, PemCertificate: certPEM(t, now.Add(time.Hour))}

	// Drops the expired session when a valid one exists.
	got := preferValidCerts([]config.CertificateInfo{expired, valid}, now)
	if len(got) != 1 || got[0].OrganizationID != 2 {
		t.Fatalf("want only org 2, got %+v", got)
	}

	// All expired → keep them all so the handshake still errors meaningfully.
	got = preferValidCerts([]config.CertificateInfo{expired}, now)
	if len(got) != 1 || got[0].OrganizationID != 1 {
		t.Fatalf("want fallback to expired cert, got %+v", got)
	}
}
