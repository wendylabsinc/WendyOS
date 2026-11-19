package literelay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testTenant = "5c9e14bb-49c0-4178-9cc5-493a151f9885"

func issueTestCA(t *testing.T, name string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, isCA bool) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: isCA, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	if parent == nil {
		parent, parentKey = c, key
	}
	der, err := x509.CreateCertificate(rand.Reader, c, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func TestFetchLiteDeviceCAChainReturnsChainedCAsOnly(t *testing.T) {
	intermediate, intermediateKey, intermediatePEM := issueTestCA(t, "tenant intermediate", nil, nil, true)
	_, _, litePEM := issueTestCA(t, "Lite Device CA", intermediate, intermediateKey, true)
	_, _, twinPEM := issueTestCA(t, "Lite Device CA twin", nil, nil, true)
	_, _, leafPEM := issueTestCA(t, "leaf", intermediate, intermediateKey, false)
	full := append(append([]byte(nil), litePEM...), intermediatePEM...)
	for _, tc := range []struct {
		name   string
		body   []byte
		status int
		ok     bool
	}{
		{"full chain", full, http.StatusOK, true},
		{"twin only from a PKI without full_chain", twinPEM, http.StatusOK, false},
		{"leaf", leafPEM, http.StatusOK, false},
		{"oversize", []byte(strings.Repeat("x", 128*1024+1)), http.StatusOK, false},
		{"not yet created", nil, http.StatusNotFound, false},
		{"redirect", nil, http.StatusFound, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/redirected" {
					t.Error("followed CA discovery redirect")
				}
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(tc.status)
				w.Write(tc.body)
			}))
			defer srv.Close()
			got, err := FetchLiteDeviceCAChain(context.Background(), srv.Client(), srv.URL)
			if (err == nil) != tc.ok || tc.ok && got != string(full) {
				t.Fatalf("chain = %q, err = %v; want ok=%v", got, err, tc.ok)
			}
		})
	}
}

func TestFetchLiteDeviceCAChainRequiresHTTPS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("fetched Lite CA material over plain HTTP")
	}))
	defer srv.Close()
	if _, err := FetchLiteDeviceCAChain(context.Background(), srv.Client(), srv.URL); err == nil {
		t.Fatal("accepted a plain HTTP discovery endpoint")
	}
}

func TestLiteDeviceCAChainURL(t *testing.T) {
	got, err := LiteDeviceCAChainURL("est.wendy.dev", testTenant)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://est.wendy.dev/.well-known/est/" + testTenant + "/cacerts?chain_algorithm=ECDSA_P256&full_chain=true"
	if got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
	for _, tc := range []struct{ host, tenant string }{
		{"", testTenant},
		{"est.wendy.dev/path", testTenant},
		{"user@est.wendy.dev", testTenant},
		{"est.wendy.dev", "../other"},
		{"est.wendy.dev", "00000000-0000-0000-0000-000000000000"},
		{"est.wendy.dev", strings.ToUpper(testTenant)},
	} {
		if _, err := LiteDeviceCAChainURL(tc.host, tc.tenant); err == nil {
			t.Errorf("LiteDeviceCAChainURL(%q, %q) accepted invalid input", tc.host, tc.tenant)
		}
	}
}
