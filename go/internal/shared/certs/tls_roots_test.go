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
	"testing"
	"time"
)

func TestTLSKeyPairOmitsOnlyProvenSelfSignedCARoots(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	makeCert := func(name string, ca bool, parent *x509.Certificate, signer *ecdsa.PrivateKey) *x509.Certificate {
		t.Helper()
		template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: ca, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature}
		if ca {
			template.KeyUsage |= x509.KeyUsageCertSign
		}
		if parent == nil {
			parent = template
		}
		der, e := x509.CreateCertificate(rand.Reader, template, parent, &key.PublicKey, signer)
		if e != nil {
			t.Fatal(e)
		}
		cert, e := x509.ParseCertificate(der)
		if e != nil {
			t.Fatal(e)
		}
		return cert
	}
	root := makeCert("root", true, nil, key)
	leaf := makeCert("leaf", false, root, key)
	selfLeaf := makeCert("self-leaf", false, nil, key)
	intermediate := makeCert("intermediate", true, root, key)
	// Same subject and issuer do not prove self-signature (key rollover).
	selfIssued := makeCert("root", true, nil, other)
	corrupt := append([]byte(nil), root.Raw...)
	corrupt[len(corrupt)-1] ^= 1
	unsupported := bytes.ReplaceAll(root.Raw, []byte{0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x04, 0x03, 0x02}, []byte{0x06, 0x08, 0x2a, 0x86, 0x48, 0xce, 0x3d, 0x04, 0x03, 0x63})
	unknown, e := x509.ParseCertificate(unsupported)
	if e != nil {
		t.Fatal(e)
	}
	if unknown.SignatureAlgorithm != x509.UnknownSignatureAlgorithm {
		t.Fatal("negative control signature is supported")
	}
	viaIntermediate := makeCert("via-intermediate", false, intermediate, key)
	roots, issuers := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(root)
	issuers.AddCert(intermediate)
	if _, err := viaIntermediate.Verify(x509.VerifyOptions{Roots: roots, Intermediates: issuers, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err != nil {
		t.Fatal(err)
	}
	if _, err := viaIntermediate.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}); err == nil {
		t.Fatal("missing intermediate unexpectedly accepted")
	}
	kd, e := x509.MarshalECPrivateKey(key)
	if e != nil {
		t.Fatal(e)
	}
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}))
	for _, tc := range []struct {
		name   string
		leaf   []byte
		extras [][]byte
		want   int
	}{
		{"root", leaf.Raw, [][]byte{root.Raw}, 1},
		{"intermediate and root", leaf.Raw, [][]byte{intermediate.Raw, root.Raw}, 2},
		{"self-issued cross-signed", leaf.Raw, [][]byte{selfIssued.Raw}, 2},
		{"invalid self-signature", leaf.Raw, [][]byte{corrupt}, 2},
		{"unknown algorithm", leaf.Raw, [][]byte{unsupported}, 2},
		{"self-signed leaf preserved", selfLeaf.Raw, [][]byte{root.Raw}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var chain []byte
			for _, der := range tc.extras {
				chain = append(chain, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
			}
			pair, e := TLSKeyPair(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tc.leaf})), string(chain), keyPEM)
			if e != nil {
				t.Fatal(e)
			}
			if len(pair.Certificate) != tc.want || !bytes.Equal(pair.Certificate[0], tc.leaf) {
				t.Fatalf("chain count=%d want=%d or changed leaf", len(pair.Certificate), tc.want)
			}
			if tc.want == 2 && !bytes.Equal(pair.Certificate[1], tc.extras[0]) {
				t.Fatal("retained issuer changed")
			}
		})
	}
}
