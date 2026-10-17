package liteenroll

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDiscoverP256DeviceRootsRequiresAuthenticatedP256Authority(t *testing.T) {
	root := testTrustPEM(t, true, time.Now().Add(time.Hour))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(chainAlgorithmHeader, "ECDSA_P256")
		w.Write(root)
	}))
	defer srv.Close()
	got, err := discoverP256DeviceRoots(context.Background(), srv.Client(), srv.URL)
	if err != nil || !bytes.Equal(got, root) {
		t.Fatalf("verified discovery: %v", err)
	}
	if _, err := discoverP256DeviceRoots(context.Background(), http.DefaultClient, srv.URL); err == nil {
		t.Fatal("trusted an unauthenticated HTTPS response")
	}
	transport := discoveryRoundTripper(func(req *http.Request) (*http.Response, error) {
		resp, err := srv.Client().Transport.RoundTrip(req)
		if err == nil {
			state := *resp.TLS
			state.VerifiedChains = nil
			resp.TLS = &state
		}
		return resp, err
	})
	if _, err := discoverP256DeviceRoots(context.Background(), &http.Client{Transport: transport}, srv.URL); err == nil {
		t.Fatal("trusted unverified peer certificates")
	}
}

func TestDiscoverP256DeviceRootsRejectsInvalidResponses(t *testing.T) {
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	otherP256, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		body   []byte
		status int
	}{
		{"ML-DSA", testMLDSATrustPEM(t), http.StatusOK},
		{"P384", testAlgorithmTrustPEM(t, p384.Public(), p384, x509.ECDSAWithSHA384), http.StatusOK},
		{"invalid self signature", testAlgorithmTrustPEM(t, p256.Public(), otherP256, x509.ECDSAWithSHA256), http.StatusOK},
		{"expired", testTrustPEM(t, true, time.Now().Add(-time.Minute)), http.StatusOK},
		{"leaf", testTrustPEM(t, false, time.Now().Add(time.Hour)), http.StatusOK},
		{"oversize", []byte(strings.Repeat("x", 128*1024+1)), http.StatusOK},
		{"unavailable", nil, http.StatusServiceUnavailable},
		{"redirect", nil, http.StatusFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set(chainAlgorithmHeader, "ECDSA_P256")
				if r.URL.Path == "/redirected" {
					t.Error("followed CA discovery redirect")
				}
				w.Header().Set("Location", "/redirected")
				w.WriteHeader(tc.status)
				w.Write(tc.body)
			}))
			defer srv.Close()
			if _, err := discoverP256DeviceRoots(context.Background(), srv.Client(), srv.URL); err == nil {
				t.Fatal("accepted invalid P-256 authority response")
			}
		})
	}
}

func TestDiscoverP256DeviceRootsValidatesEndpointAndTenant(t *testing.T) {
	for _, tc := range []struct{ endpoint, tenant string }{
		{"http://identity.example", tenant},
		{"https://other.example", tenant},
		{"https://user@identity.example", tenant},
		{"https://identity.example", "../other"},
		{"https://identity.example", "00000000-0000-0000-0000-000000000000"},
	} {
		if _, err := DiscoverP256DeviceRoots(context.Background(), tc.endpoint, tc.tenant); err == nil {
			t.Fatal("accepted invalid session discovery configuration")
		}
	}
}
