//go:build go1.27

package liteenroll

import (
	"context"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	litepb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/lite/pb"
	"google.golang.org/protobuf/proto"
)

func TestDiscoverTrustRejectsMLDSABeforePublishingConfiguration(t *testing.T) {
	root := testMLDSATrustPEM(t)
	block, _ := pem.Decode(root)
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
	cms, err := asn1.Marshal(struct {
		Type    asn1.ObjectIdentifier
		Content asn1.RawValue
	}{asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}, asn1.RawValue{Class: 2, Tag: 0, IsCompound: true, Bytes: sd}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, media string
		body        []byte
	}{
		{"PEM", "application/x-pem-file", root},
		{"EST CMS", "application/pkcs7-mime", []byte(base64.StdEncoding.EncodeToString(cms))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			requests := 0
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodGet || r.URL.Path != "/cacerts" {
					t.Errorf("continued enrollment discovery after incompatible device trust: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", tc.media)
				w.Write(tc.body)
			}))
			defer srv.Close()
			cfg := &litepb.WendyConfEnrollment{CsrUrl: srv.URL + "/csr", TimeUrl: DefaultTimeURL, Token: "do-not-print-test-credential"}
			before := proto.Clone(cfg)
			err := discoverTrust(context.Background(), srv.Client(), cfg, srv.URL+"/cacerts")
			if err == nil || !strings.Contains(err.Error(), "ML-DSA") || !strings.Contains(err.Error(), "no cloud asset was reserved") {
				t.Fatalf("expected actionable pre-mint incompatibility error: %v", err)
			}
			if strings.Contains(err.Error(), cfg.Token) {
				t.Fatal("error exposed credential")
			}
			if requests != 1 || !proto.Equal(cfg, before) {
				t.Fatal("failed discovery continued or changed enrollment configuration")
			}
		})
	}
}
