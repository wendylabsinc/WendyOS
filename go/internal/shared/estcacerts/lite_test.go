package estcacerts

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

func TestLitePathUsesOnlyInstalledAnchors(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	const tenant = "22f35004-d638-4b5c-869f-b815974d6eae"
	serial := int64(0)
	makeCA := func(name string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, issuer bool) (*x509.Certificate, *ecdsa.PrivateKey) {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		serial++
		template := &x509.Certificate{
			Subject: pkix.Name{CommonName: name}, SerialNumber: big.NewInt(serial),
			NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
			IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
			MaxPathLenZero: issuer,
		}
		if parent == nil {
			parent, parentKey = template, key
		}
		der, err := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, parentKey)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return cert, key
	}
	root, rootKey := makeCA("Platform Root", nil, nil, false)
	customer, customerKey := makeCA("Customer", root, rootKey, false)
	organization, organizationKey := makeCA("Tenant Intermediate", customer, customerKey, false)
	lite, _ := makeCA(tenant+" Lite Device CA", organization, organizationKey, true)
	device, _ := makeCA("Tenant Identity CA", organization, organizationKey, true)
	bundle := []*x509.Certificate{lite, organization, customer, root}

	path, _, err := LitePath(bundle, PEM([]*x509.Certificate{root}), tenant, now)
	if err != nil || len(path) != 3 || !path[0].Equal(lite) || !path[2].Equal(customer) {
		t.Fatalf("full platform path = %v, %v", path, err)
	}
	enrolled := PEM([]*x509.Certificate{device, organization, customer})
	if _, _, err := LitePath(bundle, enrolled, tenant, now); err == nil {
		t.Fatal("strict verification accepted a platform root supplied by the server")
	}
	path, _, err = LitePathForEnrolledPeer(bundle, enrolled, tenant, now)
	if err != nil || len(path) < 1 || !path[0].Equal(lite) {
		t.Fatalf("path to existing ACME trust = %v, %v", path, err)
	}
	if _, _, err := LitePathForEnrolledPeer(bundle, PEM([]*x509.Certificate{device}), tenant, now); err == nil {
		t.Fatal("a sibling device issuer accepted a path outside its authority")
	}
	if _, _, err := LitePathForEnrolledPeer(bundle, enrolled, "other", now); err == nil {
		t.Fatal("another tenant's issuer was accepted")
	}
}
