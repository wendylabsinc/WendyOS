package operatormesh

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/url"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestLiteIssuersReopenOfflineWithLastValidatedPath(t *testing.T) {
	now := time.Now()
	const tenant = "11111111-1111-4111-8111-111111111111"
	const origin = "https://identity.pki.test"
	type authority struct {
		cert *x509.Certificate
		key  *ecdsa.PrivateKey
		pem  string
	}
	mint := func(serial int64, name string, principal *url.URL, parent *authority) authority {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		cert := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name},
			NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
			BasicConstraintsValid: true, IsCA: principal == nil, MaxPathLenZero: name == tenant+" Lite Device CA",
		}
		if cert.IsCA {
			cert.KeyUsage = x509.KeyUsageCertSign
		} else {
			cert.URIs = []*url.URL{principal}
		}
		issuer, signer := cert, key
		if parent != nil {
			issuer, signer = parent.cert, parent.key
		}
		der, err := x509.CreateCertificate(rand.Reader, cert, issuer, key.Public(), signer)
		if err != nil {
			t.Fatal(err)
		}
		cert, err = x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return authority{cert, key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
	}
	root := mint(1, "root", nil, nil)
	lite := mint(2, tenant+" Lite Device CA", nil, &root)
	principal, _ := url.Parse("spiffe://wendy.sh/tenant/" + tenant + "/operator/fixture")
	operator := mint(3, "operator", principal, &root)
	unrelated := mint(4, "unrelated root", nil, nil)
	certificate := config.CertificateInfo{PemCertificate: operator.pem, PemCertificateChain: root.pem}
	directory := t.TempDir()
	invoke := func(endpoint string, identity config.CertificateInfo, response string, fetchErr error) (string, error) {
		return loadLiteIssuers(t.Context(), directory, endpoint, identity, func(context.Context, string, string) (string, error) {
			return response, fetchErr
		})
	}
	if got, err := invoke(origin, certificate, lite.pem, nil); err != nil || got != lite.pem {
		t.Fatalf("saving validated issuer: %v", err)
	}
	if got, err := invoke(origin, certificate, unrelated.pem, nil); err != nil || got != lite.pem {
		t.Fatalf("bad refresh displaced the last valid issuer: %v", err)
	}
	if got, err := invoke(origin, certificate, "", io.EOF); err != nil || got != lite.pem {
		t.Fatalf("offline reopen did not load last valid issuer: %v", err)
	}
	if _, err := invoke(origin+"/other", certificate, "", io.EOF); err == nil {
		t.Fatal("offline reopen reused a cache for another endpoint")
	}
	certificate.PemCertificateChain = unrelated.pem
	if _, err := invoke(origin, certificate, "", io.EOF); err == nil {
		t.Fatal("offline reopen accepted a path outside the current enrollment trust")
	}
}
