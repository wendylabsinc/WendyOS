package liteenroll

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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
			got, err := discoverLiteDeviceCAChain(context.Background(), srv.Client(), srv.URL)
			if (err == nil) != tc.ok || tc.ok && got != string(full) {
				t.Fatalf("chain = %q, err = %v; want ok=%v", got, err, tc.ok)
			}
		})
	}
}

func TestDiscoverLiteDeviceCAChainValidatesEndpointAndTenant(t *testing.T) {
	for _, tc := range []struct{ endpoint, tenant string }{
		{"http://identity.example", tenant},
		{"https://other.example", tenant},
		{"https://user@identity.example", tenant},
		{"https://identity.example", "../other"},
		{"https://identity.example", "00000000-0000-0000-0000-000000000000"},
	} {
		if _, err := DiscoverLiteDeviceCAChain(context.Background(), tc.endpoint, tc.tenant); err == nil {
			t.Fatal("accepted invalid session discovery configuration")
		}
	}
}

func fetchedLiteDeviceCAChain(chain string, err error, at time.Time) *liteDeviceCAChainFetch {
	f := &liteDeviceCAChainFetch{done: make(chan struct{}), chain: chain, err: err, fetched: at}
	close(f.done)
	return f
}

// stubLiteDeviceCAChainFetch counts fetches for one key and restores the real
// fetch and an empty cache afterwards.
func stubLiteDeviceCAChainFetch(t *testing.T, fetch func() (string, error)) (key string, calls *atomic.Int32) {
	t.Helper()
	const endpoint = "https://identity.invalid"
	key = endpoint + " " + tenant
	calls = new(atomic.Int32)
	real := fetchLiteDeviceCAChain
	fetchLiteDeviceCAChain = func(_ context.Context, url string) (string, error) {
		if want := "https://est.invalid/.well-known/est/" + tenant + "/cacerts?chain_algorithm=ECDSA_P256&full_chain=true"; url != want {
			t.Errorf("discovery URL = %q, want %q", url, want)
		}
		calls.Add(1)
		return fetch()
	}
	t.Cleanup(func() {
		fetchLiteDeviceCAChain = real
		liteDeviceCAChains.Lock()
		delete(liteDeviceCAChains.byKey, key)
		liteDeviceCAChains.Unlock()
	})
	return key, calls
}

// Each relay dial and its renew retry ask again; within the TTL they must not
// refetch, and after it they must. A failure is reused only briefly.
func TestDiscoverLiteDeviceCAChainCachesAnswersAndFailures(t *testing.T) {
	key, calls := stubLiteDeviceCAChainFetch(t, func() (string, error) { return "fresh", nil })
	offline := errors.New("offline")
	for _, tc := range []struct {
		name    string
		entry   *liteDeviceCAChainFetch
		want    string
		wantErr error
		fetches int32
	}{
		{"answer within TTL", fetchedLiteDeviceCAChain("cached", nil, time.Now()), "cached", nil, 0},
		{"answer after TTL", fetchedLiteDeviceCAChain("cached", nil, time.Now().Add(-liteDeviceCAChainTTL)), "fresh", nil, 1},
		{"failure within retry", fetchedLiteDeviceCAChain("", offline, time.Now()), "", offline, 0},
		{"failure after retry", fetchedLiteDeviceCAChain("", offline, time.Now().Add(-liteDeviceCAChainRetry)), "fresh", nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls.Store(0)
			liteDeviceCAChains.Lock()
			liteDeviceCAChains.byKey[key] = tc.entry
			liteDeviceCAChains.Unlock()
			got, err := DiscoverLiteDeviceCAChain(context.Background(), "https://identity.invalid", tenant)
			if got != tc.want || !errors.Is(err, tc.wantErr) || calls.Load() != tc.fetches {
				t.Fatalf("got %q, %v after %d fetches; want %q, %v after %d", got, err, calls.Load(), tc.want, tc.wantErr, tc.fetches)
			}
		})
	}
}

// Concurrent connects share one in-flight fetch, and a caller that gives up
// returns at once without cancelling it for the others.
func TestDiscoverLiteDeviceCAChainSharesInFlightFetch(t *testing.T) {
	release := make(chan struct{})
	_, calls := stubLiteDeviceCAChainFetch(t, func() (string, error) {
		<-release
		return "shared", nil
	})
	gone, giveUp := context.WithCancel(context.Background())
	giveUp()
	if _, err := DiscoverLiteDeviceCAChain(gone, "https://identity.invalid", tenant); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled caller = %v, want context.Canceled", err)
	}
	results := make(chan string, 3)
	for range 3 {
		go func() {
			got, _ := DiscoverLiteDeviceCAChain(context.Background(), "https://identity.invalid", tenant)
			results <- got
		}()
	}
	close(release)
	for range 3 {
		if got := <-results; got != "shared" {
			t.Fatalf("waiter got %q, want the shared answer", got)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("fetches = %d, want 1 shared", calls.Load())
	}
}
