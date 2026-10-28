package localmesh

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net/url"
	"testing"
	"time"
)

// Normal independently rooted device certificates. The CA signature remains
// P-256; the origin proof is selected by the enrolled device's actual key.
func TestManifestNativeMLDSAOriginPositive(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	authority := newManifestTestAuthority(t, now)
	for _, algorithm := range []string{"P256", "MLDSA65"} {
		t.Run(algorithm, func(t *testing.T) {
			var signer crypto.Signer
			var err error
			if algorithm == "MLDSA65" {
				signer, err = mldsa.GenerateKey(mldsa.MLDSA65())
			} else {
				signer, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
			}
			if err != nil {
				t.Fatal(err)
			}
			principal := testPrincipal(testTenantUUID, testDeviceAID)
			uri, err := url.Parse(principal)
			if err != nil {
				t.Fatal(err)
			}
			template := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: now.Add(-time.Minute),
				NotAfter: now.Add(time.Hour), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature,
				ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
			der, err := x509.CreateCertificate(rand.Reader, template, authority.cert, signer.Public(), authority.key)
			if err != nil {
				t.Fatal(err)
			}
			key, err := x509.MarshalPKCS8PrivateKey(signer)
			if err != nil {
				t.Fatal(err)
			}
			credentials, err := NewCredentials(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
				authority.pem, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})))
			if err != nil {
				t.Fatal(err)
			}
			cache, err := OpenIdentityCache("", DefaultCacheLimits(), credentials.Verify, now)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = cache.Put(credentials.Certificate.Certificate, now); err != nil {
				t.Fatal(err)
			}
			directory, err := NewDirectory(credentials.Device.Tenant(), 8, cache)
			if err != nil {
				t.Fatal(err)
			}
			manifest := routeManifest(credentials.Device, now)
			for revision := uint64(1); revision <= 2; revision++ {
				manifest.Revision = revision
				manifest.Route.Seqno = uint16(6 + revision)
				wire, err := SignManifest(manifest, credentials.Certificate.Certificate, credentials.Signer, now)
				if err != nil {
					t.Fatal(err)
				}
				if algorithm == "MLDSA65" && len(wire.Signature) != 3309 {
					t.Fatal("ML-DSA65 proof length", len(wire.Signature))
				}
				if algorithm == "P256" && len(wire.Signature) > 80 {
					t.Fatal("P-256 proof length", len(wire.Signature))
				}
				changed, err := directory.Accept(wire, now)
				if err != nil || !changed {
					t.Fatal("normal origin admission/replacement", changed, err)
				}
				if !directory.RouteAuthorizations(now)[*manifest.Route] {
					t.Fatal("current signed origin not authorized")
				}
			}
		})
	}
}
