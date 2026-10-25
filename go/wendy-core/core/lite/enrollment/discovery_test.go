package liteenroll

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	litepb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/lite/pb"
	"google.golang.org/protobuf/proto"
)

func TestDiscoveryCMS(t *testing.T) {
	roots := testTrustPEM(t, true, time.Now().Add(time.Hour))
	block, _ := pem.Decode(roots)
	sd, err := asn1.Marshal(struct {
		Version int
		Digests []pkix.AlgorithmIdentifier `asn1:"set"`
		Content struct{ Type asn1.ObjectIdentifier }
		Certs   []asn1.RawValue `asn1:"tag:0"`
		Signers []asn1.RawValue `asn1:"set"`
	}{1, []pkix.AlgorithmIdentifier{}, struct{ Type asn1.ObjectIdentifier }{asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}}, []asn1.RawValue{{FullBytes: block.Bytes}}, []asn1.RawValue{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, explicit := range []bool{false, true} {
		content := asn1.RawValue{FullBytes: sd}
		if explicit {
			content = asn1.RawValue{Class: 2, Tag: 0, IsCompound: true, Bytes: sd}
		}
		ci, err := asn1.Marshal(struct {
			Type    asn1.ObjectIdentifier
			Content asn1.RawValue
		}{asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}, content})
		if err != nil {
			t.Fatal(err)
		}
		for _, raw := range []bool{false, true} {
			encode := func(data []byte) []byte {
				if raw {
					return data
				}
				return []byte(base64.StdEncoding.EncodeToString(data))
			}
			got, err := discoveryRoots(encode(ci), "application/pkcs7-mime", testInspector{})
			if err != nil || !bytes.Equal(got, roots) {
				t.Fatalf("explicit=%v raw=%v: %v", explicit, raw, err)
			}
			if _, err := discoveryRoots(encode(append(ci, 0)), "application/pkcs7-mime", testInspector{}); err == nil {
				t.Fatal("accepted trailing DER")
			}
		}

	}
	if _, err := discoveryRoots(testTrustPEM(t, false, time.Now().Add(time.Hour)), "application/x-pem-file", testInspector{}); err == nil {
		t.Fatal("accepted leaf")
	}
}

func TestDiscoverTrustUsesVerifiedHTTPS(t *testing.T) {
	root := testTrustPEM(t, true, time.Now().Add(time.Hour))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(chainAlgorithmHeader, "ECDSA_P256")
		if r.Method == http.MethodHead {
			w.WriteHeader(404)
			return
		}
		w.Write(root)
	}))
	defer srv.Close()
	cfg := &litepb.WendyConfEnrollment{CsrUrl: srv.URL, TimeUrl: "roughtime"}
	if err := discoverTrust(context.Background(), http.DefaultClient, cfg, srv.URL); err == nil {
		t.Fatal("accepted untrusted TLS")
	}
	if cfg.ProvisionTrust || len(cfg.DeviceRoots) > 0 {
		t.Fatal("partially modified config")
	}
	if err := discoverTrust(context.Background(), srv.Client(), cfg, srv.URL); err != nil {
		t.Fatal(err)
	}
	if !cfg.ProvisionTrust || !bytes.Equal(cfg.DeviceRoots, root) || len(cfg.HttpsRoots) == 0 {
		t.Fatal("missing USB trust")
	}
	// Even a transport deliberately skipping verification must not supply roots.
	unsafe := srv.Client().Transport.(*http.Transport).Clone()
	unsafe.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	cfg = &litepb.WendyConfEnrollment{CsrUrl: srv.URL, TimeUrl: "roughtime"}
	if err := discoverTrust(context.Background(), &http.Client{Transport: unsafe}, cfg, srv.URL); err == nil {
		t.Fatal("accepted unverified peer")
	}
	if cfg.ProvisionTrust {
		t.Fatal("published unverified trust")
	}
}

type discoveryRoundTripper func(*http.Request) (*http.Response, error)

func (f discoveryRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

// Rewrite the public TSA URL to a local server while retaining actual TLS
// verification and the verified certificate chains returned by net/http.
func defaultTSATestClient(t *testing.T, srv *httptest.Server, inspect func(*http.Response) error) *http.Client {
	t.Helper()
	client := srv.Client()
	transport := client.Transport
	target, err := url.Parse(srv.URL + "/time")
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Transport: discoveryRoundTripper(func(req *http.Request) (*http.Response, error) {
		isTSA := req.URL.String() == DefaultTimeURL
		if isTSA {
			if req.Method != http.MethodHead {
				t.Errorf("TSA trust discovery used %s, want HEAD", req.Method)
			}
			req = req.Clone(req.Context())
			u := *target
			req.URL, req.Host = &u, u.Host
		} else if req.URL.Host != target.Host {
			return nil, fmt.Errorf("unexpected network request to %s", req.URL)
		}
		resp, err := transport.RoundTrip(req)
		if err == nil && isTSA && inspect != nil {
			if err := inspect(resp); err != nil {
				resp.Body.Close()
				return nil, err
			}
		}
		return resp, err
	})}
}

func TestDiscoverDefaultTrustPinsTSAAndChecksItsHTTPSChain(t *testing.T) {
	device := testTrustPEM(t, true, time.Now().Add(time.Hour))
	var requests []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(chainAlgorithmHeader, "ECDSA_P256")
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodGet {
			w.Write(device)
		} else {
			// A TSA need not implement HEAD. Its verified TLS chain is sufficient.
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()
	checkedTSA := false
	client := defaultTSATestClient(t, srv, func(resp *http.Response) error {
		checkedTSA = true
		return nil
	})
	cfg := &litepb.WendyConfEnrollment{CsrUrl: srv.URL + "/csr", TimeUrl: DefaultTimeURL}
	if err := discoverTrust(context.Background(), client, cfg, srv.URL+"/cacerts"); err != nil {
		t.Fatal(err)
	}
	if !checkedTSA || fmt.Sprint(requests) != "[GET /cacerts HEAD /csr HEAD /time]" {
		t.Fatalf("missing TSA HTTPS verification: %v", requests)
	}
	wantHTTPS := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if !cfg.ProvisionTrust || !bytes.Equal(cfg.DeviceRoots, device) ||
		!bytes.Equal(cfg.TsaRoots, defaultTSARoots) || !bytes.Equal(cfg.HttpsRoots, wantHTTPS) {
		t.Fatal("wrong discovered device roots, pinned TSA root, or deduplicated HTTPS roots")
	}
}

func TestDiscoverDefaultTrustPreservesExplicitBundles(t *testing.T) {
	device := testTrustPEM(t, true, time.Now().Add(time.Hour))
	tsa := testTrustPEM(t, true, time.Now().Add(time.Hour))
	https := testTrustPEM(t, true, time.Now().Add(time.Hour))
	requests := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/time" {
			t.Errorf("requested %s despite explicit device roots", r.URL.Path)
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))
	defer srv.Close()
	client := defaultTSATestClient(t, srv, nil)
	cfg := &litepb.WendyConfEnrollment{
		CsrUrl: "https://unused.invalid/csr", TimeUrl: DefaultTimeURL,
		DeviceRoots: device, TsaRoots: tsa, HttpsRoots: https, ProvisionTrust: true,
	}
	if err := discoverTrust(context.Background(), client, cfg, ""); err != nil {
		t.Fatal(err)
	}
	verifiedHTTPS := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	wantHTTPS := append(bytes.Clone(https), verifiedHTTPS...)
	if requests != 1 || !cfg.ProvisionTrust || !bytes.Equal(cfg.DeviceRoots, device) ||
		!bytes.Equal(cfg.TsaRoots, tsa) || !bytes.Equal(cfg.HttpsRoots, wantHTTPS) {
		t.Fatal("discovery replaced explicit trust or failed to add the verified TSA HTTPS root")
	}
	if err := discoverTrust(context.Background(), client, cfg, ""); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cfg.HttpsRoots, wantHTTPS) {
		t.Fatal("repeated discovery duplicated the TSA HTTPS root")
	}
}

func TestDiscoverDefaultTrustFailureIsAtomic(t *testing.T) {
	device := testTrustPEM(t, true, time.Now().Add(time.Hour))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(chainAlgorithmHeader, "ECDSA_P256")
		w.Write(device)
	}))
	defer srv.Close()
	for _, tc := range []struct {
		name    string
		inspect func(*http.Response) error
	}{
		{"missing TLS", func(resp *http.Response) error { resp.TLS = nil; return nil }},
		{"unverified TLS", func(resp *http.Response) error { resp.TLS.VerifiedChains = nil; return nil }},
		{"TSA unavailable", func(*http.Response) error { return fmt.Errorf("TSA unavailable") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &litepb.WendyConfEnrollment{CsrUrl: srv.URL + "/csr", TimeUrl: DefaultTimeURL}
			before := proto.Clone(cfg)
			client := defaultTSATestClient(t, srv, tc.inspect)
			if err := discoverTrust(context.Background(), client, cfg, srv.URL+"/cacerts"); err == nil {
				t.Fatal("accepted failed TSA HTTPS verification")
			}
			if !proto.Equal(cfg, before) {
				t.Fatal("failed TSA verification published partially discovered trust")
			}
		})
	}
}

func TestDiscoverCustomTSARequiresExplicitRoots(t *testing.T) {
	client := &http.Client{Transport: discoveryRoundTripper(func(req *http.Request) (*http.Response, error) {
		t.Errorf("made network request before validating custom TSA trust: %s", req.URL)
		return nil, fmt.Errorf("unexpected network request")
	})}
	cfg := &litepb.WendyConfEnrollment{CsrUrl: "https://csr.example/v1/" + tenant, TimeUrl: "https://tsa.example/time"}
	before := proto.Clone(cfg)
	if err := discoverTrust(context.Background(), client, cfg, ""); err == nil {
		t.Fatal("automatically trusted a custom TSA")
	}
	if !proto.Equal(cfg, before) {
		t.Fatal("custom TSA failure modified trust")
	}
}
