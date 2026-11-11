package registry

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/operatorpermission"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestRegistryOperatorPolicy(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const device = "22222222-2222-4222-8222-222222222222"
	for _, tc := range []struct {
		name, method, principal, claim string
		want                           int
	}{
		{"identity read", "GET", "operator/alice", "", 200},
		{"identity upload", "POST", "operator/alice", "", 403},
		{"operator upload", "POST", "operator/alice", "entitlement:deployment:create:device:" + device + ":allow", 200},
		{"wrong device", "PATCH", "operator/alice", "entitlement:deployment:create:device:other:allow", 403},
		{"wrong permission", "PUT", "operator/alice", "entitlement:device:shell:allow", 403},
		{"peer device", "PUT", "device/" + device, "entitlement:deployment:create:allow", 403},
		{"unknown verb", "CUSTOM", "operator/alice", "entitlement:deployment:*:allow", 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, _ := url.Parse("spiffe://wendy.sh/tenant/" + tenant + "/" + tc.principal)
			leaf := &x509.Certificate{URIs: []*url.URL{u}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Minute)}
			if tc.claim != "" {
				der, _ := asn1.Marshal([]asn1.RawValue{{Tag: 12, Bytes: []byte(tc.claim)}})
				leaf.Extensions = []pkix.Extension{{Id: operatorpermission.OID, Value: der}}
			}
			req := httptest.NewRequest(tc.method, "https://device/v2/app/blobs/uploads/", nil)
			req.TLS = &tls.ConnectionState{HandshakeComplete: true, PeerCertificates: []*x509.Certificate{leaf}}
			called := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(200) })
			policy := &operatorpermission.Policy{Enforce: true, Scope: certs.Scope{TenantUUID: tenant}, DeviceUUID: device}
			response := httptest.NewRecorder()
			operatorAuthorization(policy, next).ServeHTTP(response, req)
			if response.Code != tc.want || called != (tc.want == 200) {
				t.Fatalf("status=%d handler=%v", response.Code, called)
			}
		})
	}
}
