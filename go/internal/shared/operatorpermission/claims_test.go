package operatorpermission

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"net/url"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	_ "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	_ "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"strings"
)

const tenant = "11111111-1111-4111-8111-111111111111"
const device = "22222222-2222-4222-8222-222222222222"

func testLeaf(t *testing.T, values ...string) *x509.Certificate {
	t.Helper()
	u, _ := url.Parse("spiffe://wendy.sh/tenant/" + tenant + "/operator/test")
	leaf := &x509.Certificate{URIs: []*url.URL{u}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(4 * time.Minute)}
	if values != nil {
		var data []asn1.RawValue
		for _, v := range values {
			data = append(data, asn1.RawValue{Tag: 12, Bytes: []byte(v)})
		}
		der, err := asn1.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		leaf.Extensions = []pkix.Extension{{Id: OID, Value: der}}
	}
	return leaf
}
func TestPolicy(t *testing.T) {
	cases := []struct {
		name   string
		claims []string
		perm   string
		ok     bool
	}{
		{"identity read", nil, "device:read", true},
		{"identity shell", nil, "device:shell", false},
		{"identity logs", nil, "device:logs", false},
		{"empty extension", []string{}, "device:read", false},
		{"target allow", []string{"entitlement:device:shell:device:" + device + ":allow"}, "device:shell", true},
		{"wrong target", []string{"entitlement:device:shell:device:other:allow"}, "device:shell", false},
		{"category wildcard", []string{"entitlement:device:*:device:" + device + ":allow"}, "device:shell", true},
		{"deny wins", []string{"entitlement:device:*:allow", "entitlement:device:shell:device:" + device + ":deny"}, "device:shell", false},
		{"deny first", []string{"entitlement:device:shell:deny", "entitlement:device:*:allow"}, "device:shell", false},
		{"unresolved site allow", []string{"entitlement:device:shell:site:site1:allow"}, "device:shell", false},
		{"unresolved tag deny", []string{"entitlement:device:*:allow", "entitlement:device:shell:tag:lab:deny"}, "device:shell", false},
		{"wrong permission", []string{"entitlement:device:update:allow"}, "device:software", false},
		{"unknown operation", []string{"entitlement:device:*:allow"}, "", false},
		{"malformed deny", []string{"entitlement:device:*:allow", "entitlement:device:shell:future:foo:deny"}, "device:shell", false},
	}
	p := Policy{Enforce: true, Scope: certs.Scope{TenantUUID: tenant}, DeviceUUID: device}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := p.Decide(testLeaf(t, tc.claims...), tc.perm); (err == nil) != tc.ok {
				t.Fatalf("Decide()=%v want allowed %v", err, tc.ok)
			}
		})
	}
	for _, change := range []string{"tenant", "device", "expired", "future", "lifetime"} {
		t.Run(change, func(t *testing.T) {
			leaf := testLeaf(t, "entitlement:device:shell:allow")
			switch change {
			case "tenant":
				leaf.URIs[0].Path = "/tenant/33333333-3333-4333-8333-333333333333/operator/test"
			case "device":
				leaf.URIs[0].Path = "/tenant/" + tenant + "/device/" + device
			case "expired":
				leaf.NotAfter = time.Now().Add(-time.Second)
			case "future":
				leaf.NotBefore = time.Now().Add(time.Minute)
			case "lifetime":
				leaf.NotBefore = time.Now().Add(-time.Hour)
			}
			if p.Decide(leaf, "device:shell") == nil {
				t.Fatal("accepted invalid identity or lifetime")
			}
		})
	}
}

func TestStrictDER(t *testing.T) {
	good := testLeaf(t, "entitlement:device:read:allow").Extensions[0].Value
	for name, data := range map[string][]byte{"trailing": append(append([]byte{}, good...), 0), "not sequence": {0x0c, 0}, "printable string": {0x30, 3, 0x13, 1, 'x'}, "invalid utf8": {0x30, 3, 0x0c, 1, 0xff}, "oversized": make([]byte, MaxEncodedBytes+1)} {
		t.Run(name, func(t *testing.T) {
			leaf := testLeaf(t)
			leaf.Extensions = []pkix.Extension{{Id: OID, Value: data}}
			if _, _, err := Parse(leaf); err == nil {
				t.Fatal("accepted invalid DER")
			}
		})
	}
	leaf := testLeaf(t, "entitlement:device:read:allow")
	leaf.Extensions = append(leaf.Extensions, leaf.Extensions[0])
	if _, _, err := Parse(leaf); err == nil {
		t.Fatal("duplicate accepted")
	}
}

func TestEveryRPCIsReviewed(t *testing.T) {
	seen := map[string]bool{}
	protoregistry.GlobalFiles.RangeFiles(func(file protoreflect.FileDescriptor) bool {
		if !strings.HasPrefix(string(file.Package()), "wendy.agent.services.") {
			return true
		}
		services := file.Services()
		for i := 0; i < services.Len(); i++ {
			s := services.Get(i)
			methods := s.Methods()
			for j := 0; j < methods.Len(); j++ {
				method := "/" + string(s.FullName()) + "/" + string(methods.Get(j).Name())
				seen[method] = true
				if _, ok := rpcPermissions[method]; !ok {
					t.Errorf("RPC has no reviewed permission entry: %s", method)
				}
			}
		}
		return true
	})
	for method := range rpcPermissions {
		if !seen[method] {
			t.Errorf("stale method policy: %s", method)
		}
	}
	if RPCPermission("/new.Service/Mutate") != "" {
		t.Fatal("unknown method allowed")
	}
}

func FuzzEntitlementDER(f *testing.F) {
	f.Add([]byte{0x30, 0})
	f.Add([]byte("not DER"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _, _ = Parse(&x509.Certificate{Extensions: []pkix.Extension{{Id: OID, Value: data}}})
	})
}

func TestPrivilegedAppConfiguration(t *testing.T) {
	for _, tc := range []struct {
		json string
		want string
		bad  bool
	}{
		{`{"entitlements":[{"type":"network"}]}`, "", false},
		{`{"entitlements":[{"type":"admin"}]}`, "device:shell", false},
		{`{"services":{"worker":{"entitlements":[{"type":"build"}]}}}`, "device:shell", false},
		{`invalid`, "", true},
	} {
		permission, err := RequestPermission(&appConfigRequest{data: []byte(tc.json)})
		if permission != tc.want || (err != nil) != tc.bad {
			t.Fatalf("permission=%q err=%v", permission, err)
		}
	}
}

type appConfigRequest struct{ data []byte }

func (r *appConfigRequest) GetAppConfig() []byte { return r.data }
