package liteenroll

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

func TestDiscoverLiteDeviceCAChainReturnsChainedCAsOnly(t *testing.T) {
	issue := func(name string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		c := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
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
	intermediate, intermediateKey, intermediatePEM := issue("tenant intermediate", nil, nil)
	_, _, litePEM := issue("Lite Device CA", intermediate, intermediateKey)
	full := append(append([]byte(nil), litePEM...), intermediatePEM...)
	for _, tc := range []struct {
		name   string
		body   []byte
		status int
		ok     bool
	}{
		{"full chain", full, http.StatusOK, true},
		{"twin only from a PKI without full_chain", testTrustPEM(t, true, time.Now().Add(time.Hour)), http.StatusOK, false},
		{"leaf", testTrustPEM(t, false, time.Now().Add(time.Hour)), http.StatusOK, false},
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
			got, err := DiscoverLiteDeviceCAChain(context.Background(), testHTTP{srv.Client()}, testInspector{}, srv.URL)
			if (err == nil) != tc.ok || tc.ok && got != string(full) {
				t.Fatalf("chain = %q, err = %v; want ok=%v", got, err, tc.ok)
			}
		})
	}
}
