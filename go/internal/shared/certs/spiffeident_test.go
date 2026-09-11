package certs

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"net/url"
	"testing"
)

const testTenant = "6f1b7d3c-6b7e-4a2f-9c1e-2b4a8d5e0f31"

func leafWithURIs(t *testing.T, cn string, uris ...string) *x509.Certificate {
	t.Helper()
	leaf := &x509.Certificate{Subject: pkix.Name{CommonName: cn}}
	for _, raw := range uris {
		u, err := url.Parse(raw)
		if err != nil {
			t.Fatalf("parsing test URI %q: %v", raw, err)
		}
		leaf.URIs = append(leaf.URIs, u)
	}
	return leaf
}

// TestParsePrincipal pins every principal kind pki-core mints. A kind the
// parser refuses is an actor no gate can see, so the list is the contract.
func TestParsePrincipal(t *testing.T) {
	base := "spiffe://wendy.sh/tenant/" + testTenant
	for _, tc := range []struct {
		principal  string
		entityType string
		entityID   string
	}{
		{base + "/operator/auth0|abc", EntityUser, "auth0|abc"},
		{base + "/service/user-5", EntityUser, "5"},
		{base + "/service/asset-42", EntityAsset, "42"},
		{base + "/service/ci-runner", EntityUser, "ci-runner"},
		{base + "/device/box-01", EntityAsset, "box-01"},
		{base + "/device/fleet-a/box-01", EntityAsset, "fleet-a/box-01"},
		{base + "/signer/release", EntitySigner, "release"},
	} {
		got, err := ParsePrincipal(tc.principal)
		if err != nil {
			t.Fatalf("ParsePrincipal(%q): %v", tc.principal, err)
		}
		if got.EntityType != tc.entityType || got.EntityID != tc.entityID {
			t.Errorf("ParsePrincipal(%q) = %s/%s, want %s/%s",
				tc.principal, got.EntityType, got.EntityID, tc.entityType, tc.entityID)
		}
		if got.TenantUUID != testTenant {
			t.Errorf("ParsePrincipal(%q) tenant = %q, want %q", tc.principal, got.TenantUUID, testTenant)
		}
		if got.Principal != tc.principal {
			t.Errorf("ParsePrincipal(%q) did not round-trip the principal: %q", tc.principal, got.Principal)
		}
		if got.OrgID != 0 {
			t.Errorf("ParsePrincipal(%q) invented org %d", tc.principal, got.OrgID)
		}
	}
}

func TestParsePrincipalRejects(t *testing.T) {
	base := "spiffe://wendy.sh/tenant/" + testTenant
	for _, principal := range []string{
		"spiffe://wendy.sh/tenant/not-a-uuid/operator/x",
		// Non-canonical (upper-case) UUID: pki-core routes by a canonical
		// tenant, so two spellings of one tenant must not both parse or they
		// would fail to match each other downstream.
		"spiffe://wendy.sh/tenant/6F1B7D3C-6B7E-4A2F-9C1E-2B4A8D5E0F31/operator/x",
		base + "/operator/",
		base + "/wizard/x",
		base + "/service/user-",
		base,
		"spiffe://other.example/tenant/" + testTenant + "/operator/x",
		"urn:wendy:org:7:user:5",
		"",
	} {
		if id, err := ParsePrincipal(principal); err == nil {
			t.Errorf("ParsePrincipal(%q) accepted, got %+v", principal, id)
		}
	}
}

// TestIdentityFromCertPrefersPrincipal is the WDY-2968 core: a pki-core-renewed
// leaf carries no urn:wendy SAN, and it must still be a recognised identity.
func TestIdentityFromCertPrefersPrincipal(t *testing.T) {
	principal := "spiffe://wendy.sh/tenant/" + testTenant + "/device/box-01"

	// SPIFFE only — what every pki-core renewal and ACME enrollment produces.
	id, ok, err := IdentityFromCert(leafWithURIs(t, "", principal))
	if err != nil || !ok {
		t.Fatalf("SPIFFE-only leaf: ok=%v err=%v, want a recognised identity", ok, err)
	}
	if id.EntityType != EntityAsset || id.EntityID != "box-01" || id.OrgID != 0 {
		t.Errorf("SPIFFE-only leaf = %+v", id)
	}
	if id.IdentityKey() != principal {
		t.Errorf("IdentityKey() = %q, want the principal", id.IdentityKey())
	}
	if !id.Scope().Known() {
		t.Error("a leaf with a tenant principal must have a known scope; an unknown one disarms enforcement fleet-wide")
	}

	// Transitional leaf: both SANs, agreeing. The principal decides the entity,
	// the URN lends its org so an old-chain peer can still compare.
	both := leafWithURIs(t, "",
		"spiffe://wendy.sh/tenant/"+testTenant+"/service/asset-42",
		"urn:wendy:org:7:asset:42")
	id, ok, err = IdentityFromCert(both)
	if err != nil || !ok {
		t.Fatalf("transitional leaf: ok=%v err=%v", ok, err)
	}
	if id.TenantUUID != testTenant || id.OrgID != 7 || id.EntityID != "42" {
		t.Errorf("transitional leaf = %+v", id)
	}
	if id.LegacyURN() != "urn:wendy:org:7:asset:42" {
		t.Errorf("LegacyURN() = %q", id.LegacyURN())
	}

	// Contradictory SANs: the principal is authoritative and the legacy claim
	// contributes nothing, rather than lending its org to another entity.
	conflict := leafWithURIs(t, "",
		"spiffe://wendy.sh/tenant/"+testTenant+"/service/asset-42",
		"urn:wendy:org:7:asset:99")
	id, ok, err = IdentityFromCert(conflict)
	if err != nil || !ok {
		t.Fatalf("conflicting leaf: ok=%v err=%v", ok, err)
	}
	if id.EntityID != "42" || id.OrgID != 0 {
		t.Errorf("conflicting leaf = %+v, want entity 42 and no inherited org", id)
	}

	// Two principals is refused for the same reason pki-core refuses it.
	if _, _, err := IdentityFromCert(leafWithURIs(t, "",
		"spiffe://wendy.sh/tenant/"+testTenant+"/device/a",
		"spiffe://wendy.sh/tenant/"+testTenant+"/device/b")); err == nil {
		t.Error("two tenant principals accepted; want an error")
	}
}

func TestScopeMatching(t *testing.T) {
	tenantA := Scope{TenantUUID: testTenant}
	tenantB := Scope{TenantUUID: "00000000-0000-4000-8000-000000000000"}
	org7 := Scope{OrgID: 7}
	org9 := Scope{OrgID: 9}
	both := Scope{TenantUUID: testTenant, OrgID: 7}

	for _, tc := range []struct {
		name              string
		a, b              Scope
		match, comparable bool
	}{
		{"same tenant", tenantA, tenantA, true, true},
		{"different tenant", tenantA, tenantB, false, true},
		{"same org", org7, org7, true, true},
		{"different org", org7, org9, false, true},
		{"tenant wins when both carry one", both, tenantA, true, true},
		// The transition state: unprovable, deliberately not a match, and
		// distinguishable from "different tenant" so grace can forgive it.
		{"no shared vocabulary", tenantA, org7, false, false},
		{"nothing known", Scope{}, org7, false, false},
	} {
		if got := tc.a.Matches(tc.b); got != tc.match {
			t.Errorf("%s: Matches = %v, want %v", tc.name, got, tc.match)
		}
		if got := tc.a.Comparable(tc.b); got != tc.comparable {
			t.Errorf("%s: Comparable = %v, want %v", tc.name, got, tc.comparable)
		}
	}

	if (Scope{}).Known() {
		t.Error("an empty scope must not read as known")
	}
}

func TestSameEntity(t *testing.T) {
	principal := "spiffe://wendy.sh/tenant/" + testTenant + "/device/box-01"
	spiffe, err := ParsePrincipal(principal)
	if err != nil {
		t.Fatal(err)
	}
	other, err := ParsePrincipal("spiffe://wendy.sh/tenant/" + testTenant + "/device/box-02")
	if err != nil {
		t.Fatal(err)
	}
	legacy := WendyIdentity{OrgID: 7, EntityType: EntityAsset, EntityID: "42"}

	if !spiffe.SameEntity(spiffe) {
		t.Error("a principal must match itself")
	}
	if spiffe.SameEntity(other) {
		t.Error("different device principals must not match")
	}
	if !legacy.SameEntity(legacy) {
		t.Error("a legacy identity must match itself")
	}
	if legacy.SameEntity(WendyIdentity{OrgID: 9, EntityType: EntityAsset, EntityID: "42"}) {
		t.Error("same asset id in another org must not match")
	}
	// No shared vocabulary: a pin written before the cutover cannot vouch for a
	// principal, and saying otherwise is what a substituted device would need.
	if spiffe.SameEntity(legacy) {
		t.Error("a principal must not match a legacy identity")
	}
}

func TestParseIdentityURNAcceptsBothForms(t *testing.T) {
	principal := "spiffe://wendy.sh/tenant/" + testTenant + "/device/box-01"
	id, err := ParseIdentityURN("  " + principal + "  ")
	if err != nil || id.Principal != principal {
		t.Fatalf("ParseIdentityURN(principal) = %+v, %v", id, err)
	}
	id, err = ParseIdentityURN("urn:wendy:org:7:asset:42")
	if err != nil || id.OrgID != 7 || id.EntityID != "42" {
		t.Fatalf("ParseIdentityURN(urn) = %+v, %v", id, err)
	}
}
