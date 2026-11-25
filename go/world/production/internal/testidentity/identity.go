// Package testidentity generates deliberately untrusted certificates for tests.
// It never reads credentials, writes keys, installs trust, or contacts an issuer.
// Runtime packages must not import this package.
package testidentity

import (
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net/url"
	"testing"
	"time"
)

const Tenant = "11111111-1111-4111-8111-111111111111"
const Principal = "spiffe://wendy.sh/tenant/" + Tenant + "/operator/test-only"

type Fixture struct {
	Tenant, Principal   string
	RootDER             []byte
	ChainDER            [][]byte
	Key                 *mldsa.PrivateKey
	NotBefore, NotAfter time.Time
	// Issuer material is available solely to test negative certificate cases.
	Root, Issuer, Leaf *x509.Certificate
	RootKey, IssuerKey *mldsa.PrivateKey
}

func New(t testing.TB, now time.Time) *Fixture { return NewWith(t, now, Principal) }

func NewWith(t testing.TB, now time.Time, principal string) *Fixture {
	t.Helper()
	f := &Fixture{Tenant: Tenant, Principal: principal, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	key := func() *mldsa.PrivateKey {
		k, err := mldsa.GenerateKey(mldsa.MLDSA65())
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	f.RootKey, f.IssuerKey, f.Key = key(), key(), key()
	ca := func(serial int64, name string) *x509.Certificate {
		return &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, NotBefore: now.Add(-24 * time.Hour), NotAfter: now.Add(24 * time.Hour), BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	}
	root := ca(1, "UNTRUSTED TEST ROOT")
	f.Root = Issue(t, root, root, f.RootKey.Public(), f.RootKey)
	f.Issuer = Issue(t, ca(2, "UNTRUSTED TEST ISSUER"), f.Root, f.IssuerKey.Public(), f.RootKey)
	u, err := url.Parse(principal)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "UNTRUSTED TEST OPERATOR"}, NotBefore: f.NotBefore, NotAfter: f.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{u}}
	f.Leaf = Issue(t, leaf, f.Issuer, f.Key.Public(), f.IssuerKey)
	f.RootDER = f.Root.Raw
	f.ChainDER = [][]byte{f.Leaf.Raw, f.Issuer.Raw}
	return f
}

func Issue(t testing.TB, template, issuer *x509.Certificate, publicKey any, signer any) *x509.Certificate {
	t.Helper()
	der, err := x509.CreateCertificate(rand.Reader, template, issuer, publicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	c, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
