package mtls

import (
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"testing"
	"time"
)

func peerTrustPEM(certificates ...*x509.Certificate) string {
	var result []byte
	for _, certificate := range certificates {
		result = append(result, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})...)
	}
	return string(result)
}

func TestPeerTrustDuplicateLeafCannotBypassIssuerExpiry(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	for _, path := range []string{"native ECDSA", "native MLDSA", "MLDSA fallback"} {
		t.Run(path, func(t *testing.T) {
			var root, leaf *x509.Certificate
			if path == "native ECDSA" {
				issuer, key := expiryTestCertificate(t, "selected CA", now, now.Add(time.Minute), nil, nil, true)
				root = issuer
				leaf, _ = expiryTestCertificate(t, "duplicated own leaf", now, now.Add(time.Hour), root, key, false)
			} else {
				issuer, key := buildMLDSACACert(t, pkix.Name{CommonName: "selected CA"}, true)
				root = signPeerTestCertificate(t, issuer, issuer, key, func(c *tbsCertificate) { c.Validity = validity{now.Add(-time.Hour), now.Add(time.Minute)} })
				leaf = signPeerTestCertificate(t, buildMLDSALeafCert(t, root, key), root, key, func(c *tbsCertificate) { c.Validity = validity{now.Add(-time.Hour), now.Add(time.Hour)} })
			}
			trust := peerTrustPEM(leaf, root, leaf)
			verify, err := NewPeerCertificateVerifier(trust, nil, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if path == "MLDSA fallback" {
				_, roots, err := peerCertificateTrust(trust)
				if err != nil || len(roots) != 1 {
					t.Fatalf("filtered fallback trust: roots=%d err=%v", len(roots), err)
				}
				// Force the supplementary verifier on Go versions with native MLDSA.
				verify = buildPeerCertificateVerifierWithCache(x509.NewCertPool(), roots, nil, time.Time{}, newPeerPathCache())
			}
			chain := [][]byte{leaf.Raw, root.Raw}
			for _, at := range []time.Time{now, now.Add(time.Second)} {
				expires, err := verify(chain, at)
				if err != nil || !expires.Equal(root.NotAfter) {
					t.Fatalf("valid mixed bundle: expiry=%v want=%v err=%v", expires, root.NotAfter, err)
				}
			}
			if _, err := verify(chain, root.NotAfter.Add(time.Second)); err == nil {
				t.Fatal("duplicated leaf bypassed expired selected CA")
			}
			// A new verifier must also reject; this is not just cache expiry.
			fresh, err := NewPeerCertificateVerifier(trust, nil, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := fresh(chain, root.NotAfter.Add(time.Second)); err == nil {
				t.Fatal("fresh verifier accepted expired issuer via duplicated leaf")
			}
		})
	}
}

func TestPeerTrustRequiresEligibleCA(t *testing.T) {
	now := time.Now()
	root, key := expiryTestCertificate(t, "eligible CA", now, now.Add(time.Hour), nil, nil, true)
	leaf, _ := expiryTestCertificate(t, "leaf only", now, now.Add(time.Hour), root, key, false)
	for _, tc := range []struct {
		name  string
		alter func(*x509.Certificate)
	}{
		{"not a CA", func(c *x509.Certificate) { c.IsCA = false }},
		{"missing basic constraints", func(c *x509.Certificate) { c.BasicConstraintsValid = false }},
		{"missing signing permission", func(c *x509.Certificate) { c.KeyUsage = x509.KeyUsageDigitalSignature }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			template := *root
			tc.alter(&template)
			der, err := x509.CreateCertificate(rand.Reader, &template, &template, key.Public(), key)
			if err != nil {
				t.Fatal(err)
			}
			bad, err := x509.ParseCertificate(der)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewPeerCertificateVerifier(peerTrustPEM(leaf, bad), nil, time.Time{}); err == nil {
				t.Fatal("bundle without an eligible CA was accepted")
			}
			pool, roots, err := peerCertificateTrust(peerTrustPEM(leaf, bad, root))
			if err != nil || len(roots) != 1 || len(pool.Subjects()) != 1 {
				t.Fatalf("native and fallback roots were not filtered equally: roots=%d err=%v", len(roots), err)
			}
		})
	}
	if _, err := NewPeerCertificateVerifier(peerTrustPEM(leaf), nil, time.Time{}); err == nil {
		t.Fatal("leaf-only trust was accepted")
	}
}
