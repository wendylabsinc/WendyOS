package mtls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

func meshPathMLDSAFixture(t testing.TB, params mldsa.Parameters) (*x509.Certificate, [][]byte, time.Time) {
	t.Helper()
	now := time.Now()
	key, err := mldsa.GenerateKey(params)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "mesh path benchmark CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(2 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, leafKey.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return root, [][]byte{leafDER}, now
}

func BenchmarkMeshPeerPathVerification(b *testing.B) {
	for _, algorithm := range []struct {
		name   string
		params mldsa.Parameters
	}{{"MLDSA65", mldsa.MLDSA65()}, {"MLDSA87", mldsa.MLDSA87()}} {
		b.Run(algorithm.name, func(b *testing.B) {
			root, chain, now := meshPathMLDSAFixture(b, algorithm.params)
			for _, path := range []string{"native", "fallback"} {
				b.Run(path, func(b *testing.B) {
					pool := x509.NewCertPool()
					if path == "native" {
						pool.AddCert(root)
					}
					for _, mode := range []string{"uncached", "cached"} {
						b.Run(mode, func(b *testing.B) {
							var cache *peerPathCache
							if mode == "cached" {
								cache = newPeerPathCache()
								b.Cleanup(func() {
									if len(cache.entries) != 1 {
										b.Error("benchmark did not retain the verified path")
									}
								})
							}
							verify := buildPeerCertificateVerifierWithCache(pool, []*x509.Certificate{root}, nil, time.Time{}, cache)
							if _, err := verify(chain, now); err != nil {
								b.Fatal(err)
							}
							b.ReportAllocs()
							b.ResetTimer()
							for i := 0; i < b.N; i++ {
								if _, err := verify(chain, now); err != nil {
									b.Fatal(err)
								}
							}
						})
					}
				})
			}
		})
	}
}
