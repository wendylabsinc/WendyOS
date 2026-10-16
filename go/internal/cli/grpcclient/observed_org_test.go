package grpcclient

import (
	"sync/atomic"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

func TestObservedServerOrg_UnsetReturnsFalse(t *testing.T) {
	c := &AgentConnection{}
	if org, ok := c.ObservedServerOrg(); ok || org != 0 {
		t.Errorf("ObservedServerOrg() = (%d, %v), want (0, false)", org, ok)
	}
}

func TestObservedServerOrg_ReturnsStoredValue(t *testing.T) {
	c := &AgentConnection{observedServerOrg: new(atomic.Int32)}
	c.observedServerOrg.Store(7)
	if org, ok := c.ObservedServerOrg(); !ok || org != 7 {
		t.Errorf("ObservedServerOrg() = (%d, %v), want (7, true)", org, ok)
	}
}

func TestObservedServerOrg_ZeroStoredIsUnset(t *testing.T) {
	c := &AgentConnection{observedServerOrg: new(atomic.Int32)} // never stored → 0
	if org, ok := c.ObservedServerOrg(); ok || org != 0 {
		t.Errorf("ObservedServerOrg() = (%d, %v), want (0, false)", org, ok)
	}
}

func TestObservedServerIdentity_UnsetReturnsFalse(t *testing.T) {
	c := &AgentConnection{}
	if id, ok := c.ObservedServerIdentity(); ok || id.OrgID != 0 {
		t.Errorf("ObservedServerIdentity() = (%+v, %v), want (zero, false)", id, ok)
	}
	// A sink that was wired but never fired (handshake never reached a verified
	// server cert) must read as unset, not as a zero-valued identity.
	c = &AgentConnection{observedServerIdentity: new(atomic.Pointer[certs.WendyIdentity])}
	if id, ok := c.ObservedServerIdentity(); ok {
		t.Errorf("ObservedServerIdentity() = (%+v, true), want unset", id)
	}
}

func TestObservedServerIdentity_ReturnsStoredIdentity(t *testing.T) {
	c := &AgentConnection{observedServerIdentity: new(atomic.Pointer[certs.WendyIdentity])}
	verifiedIdentitySink(c.observedServerIdentity)(certs.WendyIdentity{OrgID: 7, EntityType: "asset", EntityID: "42"})

	id, ok := c.ObservedServerIdentity()
	if !ok {
		t.Fatal("ObservedServerIdentity() not ok, want the stored identity")
	}
	if id.OrgID != 7 || id.EntityType != "asset" || id.EntityID != "42" {
		t.Errorf("ObservedServerIdentity() = %+v, want org 7 asset 42", id)
	}
}

// TestVerifiedIdentitySink_IgnoresOrglessIdentity keeps a malformed identity out
// of the store: org zero without a tenant principal proves no Wendy scope.
func TestVerifiedIdentitySink_IgnoresOrglessIdentity(t *testing.T) {
	c := &AgentConnection{observedServerIdentity: new(atomic.Pointer[certs.WendyIdentity])}
	verifiedIdentitySink(c.observedServerIdentity)(certs.WendyIdentity{EntityType: "asset", EntityID: "42"})

	if id, ok := c.ObservedServerIdentity(); ok {
		t.Errorf("ObservedServerIdentity() = (%+v, true), want unset for org 0", id)
	}
}

func TestVerifiedIdentitySinkAcceptsCompleteUUIDPrincipalOnly(t *testing.T) {
	valid, err := certs.ParsePrincipal("spiffe://wendy.sh/tenant/22f35004-d638-4b5c-869f-b815974d6eae/device/03ce308b-4c00-40c2-bc64-5f66a83d4f37")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		edit func(*certs.WendyIdentity)
		want bool
	}{
		{"UUID identity", func(*certs.WendyIdentity) {}, true},
		{"missing principal", func(id *certs.WendyIdentity) { id.Principal = "" }, false},
		{"invalid principal", func(id *certs.WendyIdentity) { id.Principal = "not-a-tenant-principal" }, false},
		{"wrong tenant field", func(id *certs.WendyIdentity) { id.TenantUUID = "11111111-1111-4111-8111-111111111111" }, false},
		{"wrong entity type", func(id *certs.WendyIdentity) { id.EntityType = certs.EntityUser }, false},
		{"wrong entity ID", func(id *certs.WendyIdentity) { id.EntityID = "other-device" }, false},
		{"missing tenant field", func(id *certs.WendyIdentity) { id.TenantUUID = "" }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := valid
			tc.edit(&id)
			conn := &AgentConnection{observedServerIdentity: new(atomic.Pointer[certs.WendyIdentity])}
			verifiedIdentitySink(conn.observedServerIdentity)(id)
			got, ok := conn.ObservedServerIdentity()
			if ok != tc.want || ok && got != valid {
				t.Fatalf("observed (%+v, %v), want accepted=%v", got, ok, tc.want)
			}
		})
	}
}
