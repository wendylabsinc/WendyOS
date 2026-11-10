package builddelivery

import (
	"crypto/x509"
	"crypto/x509/pkix"
	_ "embed"
	"encoding/asn1"
	"encoding/json"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// These public grammar/policy vectors are also consumed by the Swift verifier.
//
//go:embed testdata/entitlements.json
var entitlementVectors []byte

func TestEntitlementConformance(t *testing.T) {
	var vectors struct {
		Device string `json:"device"`
		Cases  []struct {
			Name         string   `json:"name"`
			Entitlements []string `json:"entitlements"`
			Valid        bool     `json:"valid"`
			Allowed      bool     `json:"allowed"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(entitlementVectors, &vectors); err != nil {
		t.Fatal(err)
	}
	target, err := meshid.DeviceFromPrincipal("spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/" + vectors.Device)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range vectors.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			values := make([]asn1.RawValue, len(tc.Entitlements))
			for i, text := range tc.Entitlements {
				values[i] = asn1.RawValue{Tag: asn1.TagUTF8String, Bytes: []byte(text)}
			}
			encoded, err := asn1.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
			leaf := &x509.Certificate{Extensions: []pkix.Extension{{Id: entitlementOID, Value: encoded}}}
			rules, err := certificateEntitlements(leaf)
			if (err == nil) != tc.Valid {
				t.Fatalf("parse=%v; valid=%v", err, tc.Valid)
			}
			if err := authorizeEntitlements(leaf, target); (err == nil) != tc.Allowed {
				t.Fatalf("authorize=%v; allowed=%v", err, tc.Allowed)
			}
			if tc.Valid && (authorizeDeployment(rules, vectors.Device) == nil) != tc.Allowed {
				t.Fatal("parsed deployment policy disagrees with certificate authorization")
			}
		})
	}
}

func TestEntitlementTargetRetainsColons(t *testing.T) {
	rule, err := parseEntitlement("entitlement:deployment:create:tag:env:prod:deny")
	if err != nil {
		t.Fatal(err)
	}
	if rule.target.kind != targetTag || rule.target.value != "env:prod" || rule.effect != entitlementDeny {
		t.Fatalf("lost target/effect: %+v", rule)
	}
}

func TestEntitlementExtensionRejectsMalformedDER(t *testing.T) {
	valid, err := asn1.Marshal([]asn1.RawValue{{Tag: asn1.TagUTF8String, Bytes: []byte("entitlement:deployment:create:allow")}})
	if err != nil {
		t.Fatal(err)
	}
	ext := func(value []byte) pkix.Extension { return pkix.Extension{Id: entitlementOID, Value: value} }
	for _, tc := range []struct {
		name       string
		extensions []pkix.Extension
	}{
		{"absent", nil},
		{"empty", []pkix.Extension{ext(nil)}},
		{"duplicate", []pkix.Extension{ext(valid), ext(valid)}},
		{"empty then duplicate", []pkix.Extension{ext(nil), ext(valid)}},
		{"oversized", []pkix.Extension{ext(make([]byte, 2049))}},
		{"trailing DER", []pkix.Extension{ext(append(append([]byte{}, valid...), 0))}},
		{"non-sequence", []pkix.Extension{ext([]byte{0x0c, 0})}},
		{"wrong string type", []pkix.Extension{ext([]byte{0x30, 2, 0x04, 0})}},
		{"constructed UTF8", []pkix.Extension{ext([]byte{0x30, 2, 0x2c, 0})}},
		{"invalid UTF8", []pkix.Extension{ext([]byte{0x30, 3, 0x0c, 1, 0xff})}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := certificateEntitlements(&x509.Certificate{Extensions: tc.extensions}); err == nil {
				t.Fatal("malformed certificate entitlements accepted")
			}
		})
	}
}
