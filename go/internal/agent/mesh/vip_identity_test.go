package mesh

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestVIPTableRejectsMissingIdentityBeforeChangingAllocations(t *testing.T) {
	table, _ := newVIPTable(t)
	if _, err := table.Allocate(meshid.Device{}); err == nil {
		t.Fatal("allocated a VIP without a device principal")
	}
	if stats := table.Stats(); stats.Allocated != 0 {
		t.Fatalf("missing principal changed allocation state: %+v", stats)
	}
	device := deviceForIndex(t, mustTenant(t, tenantA), 1)
	vip, err := table.Allocate(device)
	if err != nil {
		t.Fatal(err)
	}
	if err := table.RetireTenant(meshid.Tenant{}); err == nil {
		t.Fatal("retired live allocations without a new tenant identity")
	}
	if got, ok := table.Lookup(device); !ok || got != vip {
		t.Fatalf("missing tenant retired an existing principal: %s, %v", got, ok)
	}
}

func TestVIPTableAcceptsExplicitNilUUIDPrincipal(t *testing.T) {
	const nilUUID = "00000000-0000-0000-0000-000000000000"
	tenant, err := meshid.ParseTenant(nilUUID)
	if err != nil {
		t.Fatal(err)
	}
	device, err := meshid.ParseDevice(tenant, nilUUID)
	if err != nil {
		t.Fatal(err)
	}
	table, path := newVIPTable(t)
	vip, err := table.Allocate(device)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := table.Lookup(meshid.Device{}); ok {
		t.Fatal("an absent identity resolved to the explicit nil-UUID principal")
	}
	reopened, err := OpenVIPTable(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := reopened.Device(vip); err != nil || got != device {
		t.Fatalf("explicit nil UUID principal lost after restart: %v, %v", got, err)
	}
}
