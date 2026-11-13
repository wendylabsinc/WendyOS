package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/revocation"
	"github.com/wendylabsinc/wendy/go/internal/testutil/crltest"
)

func TestCLIRevocationOnFullAndResumedVerification(t *testing.T) {
	p := crltest.New(t, true)
	leaf := p.Issue(t, 42, "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/22222222-2222-4222-8222-222222222222")
	previous := revocation.Default
	revocation.Default = revocation.New(p.Client)
	defer func() { revocation.Default = previous }()
	expected, found, err := IdentityFromCert(leaf.Cert)
	if err != nil || !found {
		t.Fatalf("fixture identity: found=%v err=%v", found, err)
	}
	verify, err := BuildServerVerifyConnection(ServerVerifyOpts{ChainPEM: p.PEM, ExpectedIdentity: &expected})
	if err != nil {
		t.Fatal(err)
	}
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf.Cert}}
	if err := verify(state); err != nil {
		t.Fatal(err)
	}
	p.Publish(t, 2, 42)
	revocation.Default.Refresh(context.Background())
	for _, resume := range []bool{false, true} {
		state.DidResume = resume
		if err := verify(state); !errors.Is(err, revocation.ErrRevoked) {
			t.Fatalf("resume=%v accepted revoked peer: %v", resume, err)
		}
	}
	// Historical receipts continue to authenticate signatures at their recorded
	// time. They must not become invalid when the signing leaf is later revoked.
	if err := VerifyPeerCertificateChain(leaf.Cert, nil, []*x509.Certificate{p.CA}, x509.ExtKeyUsageServerAuth, time.Now(), time.Now()); err != nil {
		t.Fatalf("historical verifier changed: %v", err)
	}
	if _, err := VerifyLivePeerCertificateChainExpiry(leaf.Cert, nil, []*x509.Certificate{p.CA}, x509.ExtKeyUsageServerAuth, time.Now(), time.Now()); !errors.Is(err, revocation.ErrRevoked) {
		t.Fatalf("live verifier accepted revoked peer: %v", err)
	}
}

func TestLiveVerifierRejectsRevokedIntermediate(t *testing.T) {
	p := crltest.New(t, true)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: "intermediate"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, CRLDistributionPoints: []string{crltest.Endpoint}}
	raw, err := x509.CreateCertificate(rand.Reader, template, p.CA, key.Public(), p.Key)
	if err != nil {
		t.Fatal(err)
	}
	intermediate, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{SerialNumber: big.NewInt(42), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	raw, err = x509.CreateCertificate(rand.Reader, leafTemplate, intermediate, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	previous := revocation.Default
	revocation.Default = revocation.New(p.Client)
	defer func() { revocation.Default = previous }()
	verify := func() error {
		return VerifyLivePeerCertificateChain(leaf, []*x509.Certificate{intermediate}, []*x509.Certificate{p.CA}, x509.ExtKeyUsageClientAuth, now, now)
	}
	if err := verify(); err != nil {
		t.Fatal(err)
	}
	p.Publish(t, 2, 7)
	revocation.Default.Refresh(context.Background())
	if err := verify(); !errors.Is(err, revocation.ErrRevoked) {
		t.Fatalf("revoked intermediate accepted: %v", err)
	}
}
