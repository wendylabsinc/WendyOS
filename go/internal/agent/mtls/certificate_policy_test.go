package mtls

import (
	"crypto/ecdsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

func policyPathFixture(t *testing.T, algorithm string, now, issuerNB, issuerNA, leafNB, leafNA time.Time) (*x509.Certificate, [][]byte, string) {
	t.Helper()
	var root, issuer, leaf *x509.Certificate
	if algorithm == "native" {
		var rootKey, issuerKey *ecdsa.PrivateKey
		root, rootKey = expiryTestCertificate(t, "policy root", now, now.Add(72*time.Hour), nil, nil, true)
		issuer, issuerKey = expiryTestCertificate(t, "policy issuer", issuerNB.Add(2*time.Hour), issuerNA, root, rootKey, true)
		leaf, _ = expiryTestCertificate(t, "policy leaf", leafNB.Add(2*time.Hour), leafNA, issuer, issuerKey, false)
	} else {
		rootCert, rootKey := buildMLDSACACert(t, pkix.Name{CommonName: "policy root"}, true)
		root = signPeerTestCertificate(t, rootCert, rootCert, rootKey, func(c *tbsCertificate) { c.Validity = validity{now.Add(-2 * time.Hour), now.Add(72 * time.Hour)} })
		issuerCert, issuerKey := buildMLDSACACert(t, pkix.Name{CommonName: "policy issuer"}, true)
		issuer = signPeerTestCertificate(t, issuerCert, root, rootKey, func(c *tbsCertificate) { c.Validity = validity{issuerNB, issuerNA} })
		leaf = signPeerTestCertificate(t, buildMLDSALeafCert(t, issuer, issuerKey), issuer, issuerKey, func(c *tbsCertificate) { c.Validity = validity{leafNB, leafNA} })
	}
	return leaf, [][]byte{leaf.Raw, issuer.Raw}, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
}

func TestCertificatePolicyKeepsIssuerWallTimeAndSelectedWindow(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	wall, floor := now.Add(-598*time.Second), now.Add(-30*time.Second)
	for _, algorithm := range []string{"native", "MLDSA"} {
		t.Run(algorithm, func(t *testing.T) {
			for _, mode := range []string{"valid", "issuer-lower-bound", "future-issuer", "expired-issuer", "expired-leaf", "beyond-leaf-grace", "fast-clock", "certainty-off"} {
				t.Run(mode, func(t *testing.T) {
					issuerNB, issuerNA := now.Add(-time.Hour), now.Add(time.Hour)
					leafNB, leafNA := now.Add(-5*time.Minute), now.Add(2*time.Hour)
					verifyAt := wall
					if mode == "issuer-lower-bound" {
						leafNB = wall.Add(-time.Hour)
						issuerNB = wall.Add(-time.Minute)
					}
					if mode == "future-issuer" {
						issuerNB = wall.Add(time.Second)
					}
					if mode == "expired-issuer" {
						issuerNA = wall.Add(-time.Second)
					}
					if mode == "expired-leaf" {
						leafNB = wall.Add(-time.Hour)
						leafNA = wall.Add(-time.Second)
					}
					if mode == "beyond-leaf-grace" {
						leafNB = floor.Add(24*time.Hour + time.Second)
						leafNA = leafNB.Add(time.Hour)
						issuerNA = now.Add(48 * time.Hour)
					}
					if mode == "fast-clock" {
						verifyAt = now.Add(3 * time.Hour)
					}
					leaf, chain, roots := policyPathFixture(t, algorithm, now, issuerNB, issuerNA, leafNB, leafNA)
					unused, _ := expiryTestCertificate(t, "unused future root", now.Add(48*time.Hour), now.Add(72*time.Hour), nil, nil, true)
					roots += string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: unused.Raw}))
					uncertain := mode != "certainty-off"
					policy := certs.CertificateTimePolicy{Floor: func() time.Time { return floor }, Uncertain: func() bool { return uncertain }}
					verify, err := NewPeerCertificateWindowVerifierWithPolicy(roots, nil, policy)
					if err != nil {
						t.Fatal(err)
					}
					window, err := verify(chain, verifyAt)
					if mode != "valid" && mode != "issuer-lower-bound" {
						if err == nil {
							t.Fatal("invalid path accepted", mode, window)
						}
						return
					}
					wantNB := leaf.NotBefore
					if issuerNB.After(wantNB) {
						wantNB = issuerNB
					}
					if err != nil || !window.NotBefore.Equal(wantNB) || !window.NotAfter.Equal(issuerNA) {
						t.Fatalf("selected path window=%+v err=%v", window, err)
					}
					cached, err := verify(chain, verifyAt.Add(time.Second))
					if err != nil || cached != window {
						t.Fatal("cache lost selected lower/upper path bounds", cached, err)
					}
					uncertain = false
					if _, err = verify(chain, verifyAt.Add(2*time.Second)); err == nil && mode == "valid" {
						t.Fatal("certainty transition reused stale leaf-floor proof")
					}
				})
			}
		})
	}
}
