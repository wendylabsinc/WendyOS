package mtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

func expiryTestCertificate(t *testing.T, name string, now, expiry time.Time, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, ca bool) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: name}, NotBefore: now.Add(-2 * time.Hour), NotAfter: expiry, BasicConstraintsValid: true, IsCA: ca, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	if ca {
		template.KeyUsage |= x509.KeyUsageCertSign
	}
	if parent == nil {
		parent, parentKey = template, key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, parent, key.Public(), parentKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, key
}

func TestVerifiedPathExpiry(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, algorithm := range []string{"native", "MLDSA fallback"} {
		t.Run(algorithm, func(t *testing.T) {
			var root, issuer, leaf *x509.Certificate
			if algorithm == "native" {
				var rootKey, issuerKey *ecdsa.PrivateKey
				root, rootKey = expiryTestCertificate(t, "root", now, now.Add(4*time.Hour), nil, nil, true)
				issuer, issuerKey = expiryTestCertificate(t, "issuer", now, now.Add(time.Hour), root, rootKey, true)
				leaf, _ = expiryTestCertificate(t, "leaf", now, now.Add(2*time.Hour), issuer, issuerKey, false)
			} else {
				rootCert, rootKey := buildMLDSACACert(t, pkix.Name{CommonName: "root"}, true)
				root = signPeerTestCertificate(t, rootCert, rootCert, rootKey, func(c *tbsCertificate) { c.Validity = validity{now.Add(-time.Hour), now.Add(4 * time.Hour)} })
				issuerCert, issuerKey := buildMLDSACACert(t, pkix.Name{CommonName: "issuer"}, true)
				issuer = signPeerTestCertificate(t, issuerCert, root, rootKey, func(c *tbsCertificate) { c.Validity = validity{now.Add(-time.Hour), now.Add(time.Hour)} })
				leaf = signPeerTestCertificate(t, buildMLDSALeafCert(t, issuer, issuerKey), issuer, issuerKey, func(c *tbsCertificate) { c.Validity = validity{now.Add(-time.Hour), now.Add(2 * time.Hour)} })
			}
			for _, extra := range []struct {
				name    string
				root    bool
				expires time.Time
			}{
				{"expired unused root", true, now.Add(-time.Hour)},
				{"soon expiring unused root", true, now.Add(time.Minute)},
				{"expired unused issuer", false, now.Add(-time.Hour)},
				{"soon expiring unused issuer", false, now.Add(time.Minute)},
			} {
				t.Run(extra.name, func(t *testing.T) {
					unused, _ := expiryTestCertificate(t, "unrelated", now, extra.expires, nil, nil, true)
					roots, peers := []*x509.Certificate{root}, []*x509.Certificate{issuer}
					if extra.root {
						roots = append(roots, unused)
					} else {
						peers = append(peers, unused)
					}
					pool := x509.NewCertPool()
					if algorithm == "native" {
						for _, ca := range roots {
							pool.AddCert(ca)
						}
					}
					// The empty pool forces the custom path even on Go versions
					// that implement native ML-DSA certificate verification.
					verify := buildPeerCertificateVerifier(pool, roots, nil, time.Time{})
					raw := [][]byte{leaf.Raw}
					for _, ca := range peers {
						raw = append(raw, ca.Raw)
					}
					expires, err := verify(raw, now)
					if err != nil || !expires.Equal(issuer.NotAfter) {
						t.Fatalf("expiry=%v want=%v err=%v", expires, issuer.NotAfter, err)
					}
					pathExpiry, err := certs.VerifyPeerCertificateChainExpiry(leaf, peers, roots, x509.ExtKeyUsageClientAuth, now, now)
					if err != nil || !pathExpiry.Equal(issuer.NotAfter) {
						t.Fatalf("shared path expiry=%v err=%v", pathExpiry, err)
					}
					if _, err := verify(raw, issuer.NotAfter.Add(time.Second)); err == nil {
						t.Fatal("selected issuer expiry was ignored")
					}
					// Presenting the old root does not replace removed local trust.
					withoutTrust := buildPeerCertificateVerifier(x509.NewCertPool(), []*x509.Certificate{unused}, nil, time.Time{})
					if _, err := withoutTrust(append(raw, root.Raw), now); err == nil {
						t.Fatal("removed root remained trusted")
					}
				})
			}
		})
	}
}
