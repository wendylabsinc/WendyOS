package localmesh

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"testing"
	"time"
)

type issuedCert struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  string
}

func mintIssued(t *testing.T, name, principal string, parent *issuedCert) issuedCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, big.NewInt(1<<62))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		BasicConstraintsValid: true, IsCA: principal == "", KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	if template.IsCA {
		template.KeyUsage |= x509.KeyUsageCertSign
	} else {
		u, err := url.Parse(principal)
		if err != nil {
			t.Fatal(err)
		}
		template.URIs = []*url.URL{u}
	}
	issuer, signer := template, key
	if parent != nil {
		issuer, signer = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, issuer, &key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return issuedCert{cert, key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
}

func issuedCredentials(t *testing.T, leaf issuedCert, certPEM, chainPEM string) *Credentials {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(leaf.key)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := NewCredentials(certPEM, chainPEM, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})))
	if err != nil {
		t.Fatal(err)
	}
	return credentials
}

func TestIssuerDigestNamesOnlyTrustedPresentedIssuers(t *testing.T) {
	root := mintIssued(t, "root", "", nil)
	top := mintIssued(t, "top", "", &root)
	issuing := mintIssued(t, "issuing", "", &top)
	other := mintIssued(t, "other-issuing", "", &top)
	chain := issuing.pem + top.pem + root.pem
	a := mintIssued(t, "a", testPrincipal(testTenantUUID, testDeviceAID), &issuing)
	b := mintIssued(t, "b", testPrincipal(testTenantUUID, testDeviceBID), &issuing)
	c := mintIssued(t, "c", testPrincipal(testTenantUUID, testDeviceCID), &other)

	sameA := issuedCredentials(t, a, a.pem, chain)
	sameB := issuedCredentials(t, b, b.pem, chain)
	if len(sameA.Certificate.Certificate) != 3 {
		t.Fatalf("presented %d certificates, want leaf and two issuers", len(sameA.Certificate.Certificate))
	}
	if sameA.IssuerDigest() == "" || sameA.IssuerDigest() != sameB.IssuerDigest() {
		t.Fatalf("devices with one enrolled chain disagree: %q %q", sameA.IssuerDigest(), sameB.IssuerDigest())
	}
	leaf := sameA.LeafCertificate()
	if len(leaf.Certificate) != 1 || string(leaf.Certificate[0]) != string(a.cert.Raw) || leaf.PrivateKey != sameA.Certificate.PrivateKey {
		t.Fatal("leaf identity changed")
	}
	if len(sameA.Certificate.Certificate) != 3 {
		t.Fatal("LeafCertificate modified the full identity")
	}
	if branch := issuedCredentials(t, c, c.pem, other.pem+top.pem+root.pem); branch.IssuerDigest() == "" || branch.IssuerDigest() == sameA.IssuerDigest() {
		t.Fatalf("another issuing CA has digest %q", branch.IssuerDigest())
	}
	// An issuer carried only in the certificate PEM is presented but is not a
	// local anchor. A peer with the same digest could not rely on it.
	if embedded := issuedCredentials(t, a, a.pem+issuing.pem, top.pem+root.pem); embedded.IssuerDigest() != "" {
		t.Fatal("digest names an issuer that is not a local trust anchor")
	}
	// A chain of only a provable self-signed root presents no issuers.
	direct := mintIssued(t, "direct", testPrincipal(testTenantUUID, testDeviceAID), &root)
	if rootOnly := issuedCredentials(t, direct, direct.pem, root.pem); len(rootOnly.Certificate.Certificate) != 1 || rootOnly.IssuerDigest() != "" {
		t.Fatal("root-only enrollment named presented issuers")
	}
	if (*Credentials)(nil).IssuerDigest() != "" || (&Credentials{}).IssuerDigest() != "" {
		t.Fatal("empty credentials named issuers")
	}
}

// A peer that omits its issuers is verified against the receiver's own trust
// anchors only. Omission cannot add trust: a receiver without the issuer
// rejects the bare leaf and still accepts the full chain through its root.
func TestLeafOnlyIdentityNeedsLocalIssuer(t *testing.T) {
	root := mintIssued(t, "root", "", nil)
	top := mintIssued(t, "top", "", &root)
	issuing := mintIssued(t, "issuing", "", &top)
	other := mintIssued(t, "other-issuing", "", &top)
	a := mintIssued(t, "a", testPrincipal(testTenantUUID, testDeviceAID), &issuing)
	b := mintIssued(t, "b", testPrincipal(testTenantUUID, testDeviceBID), &issuing)
	c := mintIssued(t, "c", testPrincipal(testTenantUUID, testDeviceCID), &other)
	sender := issuedCredentials(t, a, a.pem, issuing.pem+top.pem+root.pem)
	sameIssuers := issuedCredentials(t, b, b.pem, issuing.pem+top.pem+root.pem)
	otherIssuers := issuedCredentials(t, c, c.pem, other.pem+top.pem+root.pem)
	leaf := sender.LeafCertificate().Certificate

	if identity, err := sameIssuers.Verify(leaf, time.Now()); err != nil || identity.Device != sender.Device {
		t.Fatalf("receiver with the sender's issuers: %v %v", identity.Device, err)
	}
	if identity, err := otherIssuers.Verify(leaf, time.Now()); err == nil {
		t.Fatalf("receiver without the sender's issuer accepted the bare leaf as %v", identity.Device)
	}
	if identity, err := otherIssuers.Verify(sender.Certificate.Certificate, time.Now()); err != nil || identity.Device != sender.Device {
		t.Fatalf("receiver without the sender's issuer, full chain: %v %v", identity.Device, err)
	}
	if sender.IssuerDigest() == otherIssuers.IssuerDigest() {
		t.Fatal("receivers that need the issuers share the sender's digest")
	}
}
