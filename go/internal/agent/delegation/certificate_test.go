package delegation

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"net/url"
	"testing"
)

const owner = "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/user"
const device = "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/robot"
const method = "/wendy.agent.services.v2.WendyContainerService/StopContainer"

func stringsDER(values ...string) []asn1.RawValue {
	out := []asn1.RawValue{}
	for _, v := range values {
		out = append(out, asn1.RawValue{Tag: asn1.TagUTF8String, Bytes: []byte(v)})
	}
	return out
}
func fixture(t *testing.T) (*x509.Certificate, wireScope) {
	t.Helper()
	u, _ := url.Parse(owner)
	w := wireScope{1, "22222222-2222-4222-8222-222222222222", owner, stringsDER(device), stringsDER("demo")}
	leaf := &x509.Certificate{URIs: []*url.URL{u}}
	setScope(t, leaf, w)
	der, _ := asn1.Marshal(stringsDER("entitlement:wendy.agent.services.v2.WendyContainerService:StopContainer:allow"))
	leaf.Extensions = append(leaf.Extensions, pkix.Extension{Id: EntitlementsOID, Value: der})
	return leaf, w
}
func setScope(t *testing.T, leaf *x509.Certificate, w wireScope) {
	t.Helper()
	der, err := asn1.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	if len(leaf.Extensions) == 0 {
		leaf.Extensions = append(leaf.Extensions, pkix.Extension{})
	}
	leaf.Extensions[0] = pkix.Extension{Id: ScopeOID, Critical: true, Value: der}
}
func TestScopeAuthorization(t *testing.T) {
	leaf, _ := fixture(t)
	s, err := Parse(leaf)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, device, method, app string
		allow                     bool
	}{
		{"selected app", device, method, "demo", true},
		{"other app", device, method, "other", false},
		{"empty app", device, method, "", false},
		{"other device", device + "-other", method, "demo", false},
		{"other method", device, "/wendy.agent.services.v2.WendyContainerService/StartContainer", "demo", false},
		{"tunnel", device, "/wendy.agent.services.v2.WendyTunnelService/Tunnel", "demo", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := s.Authorize(tc.device, tc.method, tc.app); (got == nil) != tc.allow {
				t.Fatalf("Authorize()=%v", got)
			}
		})
	}
	s.rules = append(s.rules, "entitlement:wendy.agent.services.v2.WendyContainerService:StopContainer:deny")
	if s.Authorize(device, method, "demo") == nil {
		t.Fatal("deny must override allow")
	}
	s.rules = append(s.rules, "entitlement:wendy.agent.services.v2.WendyTunnelService:Tunnel:allow")
	if s.Authorize(device, "/wendy.agent.services.v2.WendyTunnelService/Tunnel", "") == nil {
		t.Fatal("unreviewed route must be denied even with entitlement")
	}
}
func TestInvalidScope(t *testing.T) {
	tests := map[string]func(*x509.Certificate, *wireScope){
		"version":          func(_ *x509.Certificate, w *wireScope) { w.Version = 2 },
		"owner":            func(_ *x509.Certificate, w *wireScope) { w.OwnerPrincipal = owner + "-other" },
		"no devices":       func(_ *x509.Certificate, w *wireScope) { w.DevicePrincipals = nil },
		"duplicate device": func(_ *x509.Certificate, w *wireScope) { w.DevicePrincipals = stringsDER(device, device) },
		"cross tenant": func(_ *x509.Certificate, w *wireScope) {
			w.DevicePrincipals = stringsDER("spiffe://wendy.sh/tenant/33333333-3333-4333-8333-333333333333/device/robot")
		},
		"wildcard app":   func(_ *x509.Certificate, w *wireScope) { w.AppIDs = stringsDER("*") },
		"path app":       func(_ *x509.Certificate, w *wireScope) { w.AppIDs = stringsDER("../demo") },
		"malformed UUID": func(_ *x509.Certificate, w *wireScope) { w.DelegationID = "not-a-uuid" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			leaf, w := fixture(t)
			mutate(leaf, &w)
			setScope(t, leaf, w)
			if _, err := Parse(leaf); err == nil {
				t.Fatal("accepted invalid scope")
			}
		})
	}
	for _, name := range []string{"noncritical", "duplicate", "trailing DER", "missing entitlements", "malformed entitlements"} {
		t.Run(name, func(t *testing.T) {
			leaf, _ := fixture(t)
			switch name {
			case "noncritical":
				leaf.Extensions[0].Critical = false
			case "duplicate":
				leaf.Extensions = append(leaf.Extensions, leaf.Extensions[0])
			case "trailing DER":
				leaf.Extensions[0].Value = append(leaf.Extensions[0].Value, 0)
			case "missing entitlements":
				leaf.Extensions = leaf.Extensions[:1]
			case "malformed entitlements":
				leaf.Extensions[1].Value = []byte{0}
			}
			if _, err := Parse(leaf); err == nil {
				t.Fatal("accepted invalid extension")
			}
		})
	}
}
func TestOrdinaryCertificate(t *testing.T) {
	if s, err := Parse(&x509.Certificate{}); s != nil || err != nil {
		t.Fatalf("ordinary cert changed: %v %v", s, err)
	}
}
func TestEmptyAppsDenyAppAccess(t *testing.T) {
	leaf, w := fixture(t)
	w.AppIDs = nil
	setScope(t, leaf, w)
	s, err := Parse(leaf)
	if err != nil {
		t.Fatal(err)
	}
	if s.Authorize(device, method, "demo") == nil {
		t.Fatal("empty apps must deny")
	}
}

func TestScopeRejectsUnknownSequenceFields(t *testing.T) {
	leaf, _ := fixture(t)
	var fields []asn1.RawValue
	if _, err := asn1.Unmarshal(leaf.Extensions[0].Value, &fields); err != nil {
		t.Fatal(err)
	}
	fields = append(fields, asn1.RawValue{Tag: asn1.TagUTF8String, Bytes: []byte("ignored constraint")})
	leaf.Extensions[0].Value, _ = asn1.Marshal(fields)
	if _, err := Parse(leaf); err == nil {
		t.Fatal("unknown trailing sequence field accepted")
	}
}
