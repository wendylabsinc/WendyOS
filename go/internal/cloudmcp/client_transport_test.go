package cloudmcp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestConnectorTransport(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test connector CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	config, protect, err := ClientTransport(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), "connector.example")
	if err != nil {
		t.Fatal(err)
	}
	makeLeaf := func(name string, eku x509.ExtKeyUsage, expired bool) *x509.Certificate {
		t.Helper()
		end := time.Now().Add(time.Hour)
		if expired {
			end = time.Now().Add(-time.Minute)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-time.Hour), NotAfter: end, DNSNames: []string{name}, ExtKeyUsage: []x509.ExtKeyUsage{eku}, KeyUsage: x509.KeyUsageDigitalSignature}
		raw, e := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
		if e != nil {
			t.Fatal(e)
		}
		parsed, e := x509.ParseCertificate(raw)
		if e != nil {
			t.Fatal(e)
		}
		return parsed
	}
	for _, tc := range []struct {
		name string
		leaf *x509.Certificate
		ok   bool
	}{
		{"valid", makeLeaf("connector.example", x509.ExtKeyUsageClientAuth, false), true},
		{"wrong identity", makeLeaf("attacker.example", x509.ExtKeyUsageClientAuth, false), false},
		{"wildcard identity", makeLeaf("*.example", x509.ExtKeyUsageClientAuth, false), false},
		{"server only", makeLeaf("connector.example", x509.ExtKeyUsageServerAuth, false), false},
		{"expired", makeLeaf("connector.example", x509.ExtKeyUsageClientAuth, true), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{tc.leaf}}
			if got := config.VerifyConnection(state) == nil; got != tc.ok {
				t.Fatalf("accepted=%v want %v", got, tc.ok)
			}
			r := httptest.NewRequest("POST", "https://mcp.example/orgs/org/mcp", nil)
			r.TLS = &state
			w := httptest.NewRecorder()
			protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) })).ServeHTTP(w, r)
			if got := w.Code == 204; got != tc.ok {
				t.Fatalf("status %d", w.Code)
			}
		})
	}
	handler := protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	for _, path := range []string{"/healthz", "/.well-known/oauth-protected-resource/orgs/org/mcp", "/orgs/org/mcp"} {
		r := httptest.NewRequest("GET", "https://mcp.example"+path, nil)
		r.TLS = nil
		r.Header.Set("X-Forwarded-Client-Cert", "forged")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		want := 204
		if path == "/orgs/org/mcp" {
			want = 401
		}
		if w.Code != want {
			t.Fatalf("%s: %d", path, w.Code)
		}
	}
}

func TestConnectorTransportConfiguration(t *testing.T) {
	if _, _, err := ClientTransport(nil, ""); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		pem  []byte
		name string
	}{{nil, "x"}, {[]byte("invalid"), "x"}, {openAIConnectorCA, "*.example"}, {openAIConnectorCA, ""}} {
		if _, _, err := ClientTransport(tc.pem, tc.name); err == nil {
			t.Fatal("accepted invalid configuration")
		}
	}
}
