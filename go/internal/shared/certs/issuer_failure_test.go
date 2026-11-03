package certs

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

func TestIssuerFailureClassification(t *testing.T) {
	now := time.Now()
	for _, kind := range []string{"missing", "wrong-key", "expired-leaf", "future-leaf", "expired-issuer", "future-issuer", "expired-root", "future-root", "wrong-usage", "unrelated-expired"} {
		t.Run(kind, func(t *testing.T) {
			mint := func(name string, serial int64, ca bool, parent *x509.Certificate, key ed25519.PrivateKey) (*x509.Certificate, ed25519.PrivateKey) {
				pub, own, e := ed25519.GenerateKey(rand.Reader)
				if e != nil {
					t.Fatal(e)
				}
				c := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), BasicConstraintsValid: true, IsCA: ca, KeyUsage: x509.KeyUsageDigitalSignature}
				if ca {
					c.KeyUsage |= x509.KeyUsageCertSign
				}
				if kind == "expired-"+name {
					c.NotAfter = now.Add(-time.Second)
				}
				if kind == "future-"+name {
					c.NotBefore = now.Add(time.Second)
				}
				if name == "leaf" && kind == "wrong-usage" {
					c.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning}
				}
				if parent == nil {
					parent = c
					key = own
				}
				der, e := x509.CreateCertificate(rand.Reader, c, parent, pub, key)
				if e != nil {
					t.Fatal(e)
				}
				parsed, e := x509.ParseCertificate(der)
				if e != nil {
					t.Fatal(e)
				}
				return parsed, own
			}
			root, rk := mint("root", 1, true, nil, nil)
			issuer, ik := mint("issuer", 2, true, root, rk)
			leaf, _ := mint("leaf", 3, false, issuer, ik)
			var peers []*x509.Certificate
			if kind != "missing" && kind != "unrelated-expired" {
				peers = append(peers, issuer)
			}
			if kind == "wrong-key" {
				wrong, _ := mint("issuer", 4, true, root, rk)
				wrong.SubjectKeyId = issuer.SubjectKeyId
				peers = []*x509.Certificate{wrong}
			}
			if kind == "unrelated-expired" {
				other, _ := mint("other", 5, true, root, rk)
				other.NotAfter = now.Add(-time.Second)
				peers = append(peers, other)
			}
			want := kind == "missing" || kind == "wrong-key" || kind == "unrelated-expired"
			pool, inter := x509.NewCertPool(), x509.NewCertPool()
			pool.AddCert(root)
			for _, p := range peers {
				inter.AddCert(p)
			}
			_, stdErr := leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: inter, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
			err := ClassifyIssuerFailure(stdErr, leaf, peers, []*x509.Certificate{root}, x509.ExtKeyUsageServerAuth, now, now)
			if err == nil || IsIssuerPathError(err) != want {
				t.Fatalf("standard verifier: err=%v path=%v want=%v", err, IsIssuerPathError(err), want)
			}
			err = VerifyPeerCertificateChain(leaf, peers, []*x509.Certificate{root}, x509.ExtKeyUsageServerAuth, now, now)
			if err == nil || IsIssuerPathError(err) != want {
				t.Fatalf("shared verifier: err=%v path=%v want=%v", err, IsIssuerPathError(err), want)
			}
		})
	}
}
