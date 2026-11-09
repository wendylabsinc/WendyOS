package meshoperator

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestVerifyPrimaryCredentialProfile(t *testing.T) {
	now := time.Now()
	tenant, err := meshid.ParseTenant("2558fd76-afc7-466e-9613-6b715296a526")
	if err != nil {
		t.Fatal(err)
	}
	principal := "spiffe://wendy.sh/tenant/" + tenant.String() + "/operator/alice"
	uri, err := url.Parse(principal)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test primary root"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		eku     []x509.ExtKeyUsage
		unknown []asn1.ObjectIdentifier
		ca      bool
		valid   bool
	}{
		{name: "clientAuth", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, valid: true},
		{name: "clientAuth and serverAuth", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}, valid: true},
		{name: "missing EKU"},
		{name: "any EKU only", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}},
		{name: "serverAuth only", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}},
		{name: "CA with clientAuth", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, ca: true},
		{name: "Lite operator", eku: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, unknown: []asn1.ObjectIdentifier{{1, 3, 6, 1, 4, 1, 65441, 1, 9}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			leaf := &x509.Certificate{
				SerialNumber: big.NewInt(2), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute),
				BasicConstraintsValid: true, IsCA: test.ca, KeyUsage: x509.KeyUsageDigitalSignature,
				ExtKeyUsage: test.eku, UnknownExtKeyUsage: test.unknown, URIs: []*url.URL{uri},
			}
			if test.ca {
				leaf.KeyUsage |= x509.KeyUsageCertSign
			}
			der, err := x509.CreateCertificate(rand.Reader, leaf, root, key.Public(), key)
			if err != nil {
				t.Fatal(err)
			}
			verified, err := VerifyPrimary([][]byte{der}, []*x509.Certificate{root}, tenant, now)
			if (err == nil) != test.valid {
				t.Fatalf("VerifyPrimary valid=%v, want %v: %v", err == nil, test.valid, err)
			}
			if test.valid && verified.Operator().Principal() != principal {
				t.Fatalf("verified principal = %q", verified.Operator().Principal())
			}
		})
	}
}
