package cloudrelay

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/revocation"
	"github.com/wendylabsinc/wendy/go/internal/testutil/crltest"
)

func TestCloudCRLDownloadsOnlyWendyPaths(t *testing.T) {
	for _, pq := range []bool{false, true} {
		t.Run(map[bool]string{false: "ECDSA", true: "MLDSA"}[pq], func(t *testing.T) {
			web := crltest.New(t, false)
			private := crltest.New(t, pq)
			roots := x509.NewCertPool()
			roots.AddCert(web.CA)
			roots.AddCert(private.CA)
			issue := func(p *crltest.PKI) *x509.Certificate {
				template := &x509.Certificate{SerialNumber: big.NewInt(42), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), DNSNames: []string{"cloud.example"}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, CRLDistributionPoints: []string{crltest.Endpoint}}
				raw, err := x509.CreateCertificate(rand.Reader, template, p.CA, p.Key.Public(), p.Key)
				if err != nil {
					t.Fatal(err)
				}
				leaf, err := x509.ParseCertificate(raw)
				if err != nil {
					t.Fatal(err)
				}
				return leaf
			}
			old := revocation.Default
			defer func() { revocation.Default = old }()
			revocation.Default = revocation.New(web.Client)
			verify := cloudServerVerifier("cloud.example", roots, []*x509.Certificate{private.CA})
			for _, resume := range []bool{false, true} {
				if err := verify(tls.ConnectionState{DidResume: resume, PeerCertificates: []*x509.Certificate{issue(web), web.CA}}); err != nil {
					t.Fatal(err)
				}
				revocation.Default.Refresh(context.Background())
			}
			if web.Hits.Load() != 0 {
				t.Fatalf("public Web PKI registered CRL downloads: %d", web.Hits.Load())
			}
			if err := cloudServerVerifier("wrong.example", roots, []*x509.Certificate{private.CA})(tls.ConnectionState{PeerCertificates: []*x509.Certificate{issue(web)}}); err == nil {
				t.Fatal("public hostname validation bypassed")
			}
			if err := cloudServerVerifier("cloud.example", x509.NewCertPool(), []*x509.Certificate{private.CA})(tls.ConnectionState{PeerCertificates: []*x509.Certificate{issue(web)}}); err == nil {
				t.Fatal("untrusted public issuer accepted")
			}
			revocation.Default = revocation.New(private.Client)
			state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{issue(private), private.CA}}
			if err := verify(state); err != nil {
				t.Fatal(err)
			}
			private.Publish(t, 2, 42)
			revocation.Default.Refresh(context.Background())
			if private.Hits.Load() != 1 {
				t.Fatalf("Wendy CRL not fetched: %d", private.Hits.Load())
			}
			if err := verify(state); !errors.Is(err, revocation.ErrRevoked) {
				t.Fatalf("revoked Wendy Cloud certificate accepted: %v", err)
			}
		})
	}
}
