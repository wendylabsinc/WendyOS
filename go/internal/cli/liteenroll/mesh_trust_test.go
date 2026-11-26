package liteenroll

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
)

func TestMeshTrustPinsDeviceIdentityAcrossRotation(t *testing.T) {
	now := time.Now()
	type issued struct {
		cert *x509.Certificate
		key  *ecdsa.PrivateKey
	}
	var serial int64
	mint := func(name string, key *ecdsa.PrivateKey, parent issued, limit int) issued {
		t.Helper()
		if key == nil {
			var err error
			key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
		}
		serial++
		cert := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
			NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
			IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
			MaxPathLen: limit, MaxPathLenZero: limit == 0,
		}
		if parent.cert == nil {
			parent = issued{cert, key}
		}
		der, err := x509.CreateCertificate(rand.Reader, cert, parent.cert, key.Public(), parent.key)
		if err != nil {
			t.Fatal(err)
		}
		cert, err = x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return issued{cert, key}
	}
	root := mint("Root", nil, issued{}, 2)
	oldParent := mint("Tenant Intermediate CA", nil, root, 1)
	newParent := mint("Tenant Intermediate CA", nil, root, 1)
	oldDevice := mint("Tenant Identity CA", nil, oldParent, 0)
	newDevice := mint("Tenant Device Identity CA", nil, newParent, 0)
	operator := mint("Tenant Operator Authority CA", nil, newParent, 0)
	otherParent := mint("Unrelated Intermediate CA", nil, root, 1)
	wrongTenant := mint("Tenant Device Identity CA", nil, otherParent, 0)
	liteName := tenant + " Lite Device CA"
	twin := mint(liteName, nil, issued{}, 0)
	signedLite := mint(liteName, twin.key, newParent, 0)
	path := []*x509.Certificate{signedLite.cert, newParent.cert}
	for _, includePredecessor := range []bool{false, true} {
		regular := []*x509.Certificate{root.cert, newParent.cert, newDevice.cert, operator.cert, otherParent.cert, wrongTenant.cert}
		if includePredecessor {
			regular = append(regular, oldParent.cert, oldDevice.cert)
		}
		bundle, err := meshTrustBundle(path, []*x509.Certificate{twin.cert}, regular, tenant, now)
		if err != nil {
			t.Fatal(err)
		}
		pins := make(map[string]bool)
		for len(bundle) != 0 {
			var block *pem.Block
			block, bundle = pem.Decode(bundle)
			if block == nil {
				t.Fatal("invalid mesh pins PEM")
			}
			pins[string(block.Bytes)] = true
		}
		if !pins[string(twin.cert.Raw)] || !pins[string(newDevice.cert.Raw)] || !pins[string(operator.cert.Raw)] ||
			pins[string(oldDevice.cert.Raw)] != includePredecessor || pins[string(wrongTenant.cert.Raw)] {
			t.Fatalf("incorrect pins with predecessor present: %t", includePredecessor)
		}
	}
}
