package liteenroll

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
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
		got, err := discoveryRoots([]byte(base64.StdEncoding.EncodeToString(ci)), "application/pkcs7-mime")
		if err != nil || !bytes.Equal(got, roots) {
			t.Fatalf("explicit=%v: %v", explicit, err)
		}
		if _, err := discoveryRoots([]byte(base64.StdEncoding.EncodeToString(append(ci, 0))), "application/pkcs7-mime"); err == nil {
			t.Fatal("accepted trailing DER")
		}
	}
	if _, err := discoveryRoots(testTrustPEM(t, false, time.Now().Add(time.Hour)), "application/x-pem-file"); err == nil {
		t.Fatal("accepted leaf")
	}
}

func TestDiscoverTrustUsesVerifiedHTTPS(t *testing.T) {
	root := testTrustPEM(t, true, time.Now().Add(time.Hour))
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
