package mesh

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const (
	tenantA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	tenantB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

func mustTenant(t *testing.T, text string) meshid.Tenant {
	t.Helper()
	tenant, err := meshid.ParseTenant(text)
	if err != nil {
		t.Fatalf("ParseTenant(%s): %v", text, err)
	}
	return tenant
}

// deviceForIndex builds a deterministic canonical device UUID under tenant.
func deviceForIndex(t *testing.T, tenant meshid.Tenant, i int) meshid.Device {
	t.Helper()
	text := fmt.Sprintf("%08x-4000-8000-0000-%012x", i, i)
	device, err := meshid.ParseDevice(tenant, text)
	if err != nil {
		t.Fatalf("ParseDevice(%s): %v", text, err)
	}
	return device
}

// newVIPTable opens a fresh table over the production CIDR in a temp dir.
func newVIPTable(t *testing.T) (*VIPTable, string) {
	t.Helper()
	return newVIPTableCIDR(t, DefaultServiceCIDR)
}

// newVIPTableCIDR opens a fresh table over an arbitrary CIDR. Tests use a
// small one to reach exhaustion without staging 65k allocations.
func newVIPTableCIDR(t *testing.T, cidr string) (*VIPTable, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "vips.json")
	table, err := openVIPTable(path, netip.MustParsePrefix(cidr))
	if err != nil {
		t.Fatalf("openVIPTable(%s, %s): %v", path, cidr, err)
	}
	return table, path
}

func mustAllocate(t *testing.T, table *VIPTable, device meshid.Device) netip.Addr {
	t.Helper()
	vip, err := table.Allocate(device)
	if err != nil {
		t.Fatalf("Allocate(%s): %v", device.UUID(), err)
	}
	return vip
}

func TestVIPAllocateIsIdempotentAndHostWide(t *testing.T) {
	table, _ := newVIPTable(t)
	a, b := mustTenant(t, tenantA), mustTenant(t, tenantB)
	devA, devB := deviceForIndex(t, a, 1), deviceForIndex(t, a, 2)

	vipA := mustAllocate(t, table, devA)
	if vipA.String() != "10.99.0.1" {
		t.Fatalf("first VIP = %s, want 10.99.0.1 (the network address is not a host)", vipA)
	}
	if again := mustAllocate(t, table, devA); again != vipA {
		t.Fatalf("re-Allocate returned %s, want the pinned %s", again, vipA)
	}
	if got, ok := table.Lookup(devA); !ok || got != vipA {
		t.Fatalf("Lookup = (%s, %v), want (%s, true)", got, ok, vipA)
	}

	vipB := mustAllocate(t, table, devB)
	if vipB == vipA {
		t.Fatalf("two devices share VIP %s", vipB)
	}

	// Uniqueness is host-wide: a same-UUID lookup under a tenant that never
	// allocated it is a miss, not a re-key.
	if _, ok := table.Lookup(deviceForIndex(t, b, 1)); ok {
		t.Fatal("Lookup found a VIP for a device under a different tenant")
	}

	back, err := table.Device(vipA)
	if err != nil {
		t.Fatalf("Device(%s): %v", vipA, err)
	}
	if back.Principal() != devA.Principal() {
		t.Fatalf("Device(%s) = %s, want %s", vipA, back.Principal(), devA.Principal())
	}

	stats := table.Stats()
	if stats.Allocated != 2 || stats.Retired != 0 || stats.Capacity != 65534 {
		t.Fatalf("Stats = %+v, want Allocated 2, Retired 0, Capacity 65534", stats)
	}
	if got := table.CIDR().String(); got != DefaultServiceCIDR {
		t.Fatalf("CIDR() = %s, want %s", got, DefaultServiceCIDR)
	}
}

func TestVIPRejectsNonHostOrUnallocatedAddresses(t *testing.T) {
	table, _ := newVIPTable(t)
	cases := []struct {
		vip string
		why string
	}{
		{"10.99.0.0", "network address"},
		{"10.99.255.255", "broadcast address"},
		{"10.98.0.5", "outside CIDR"},
		{"192.168.1.1", "outside CIDR"},
		{"fd45:b891:567f::1", "not IPv4"},
		{"10.99.5.5", "inside CIDR but unallocated"},
	}
	for _, c := range cases {
		if _, err := table.Device(netip.MustParseAddr(c.vip)); err == nil {
			t.Fatalf("Device(%s) (%s): expected an error", c.vip, c.why)
		}
	}
}

// TestVIPPersistenceAcrossRestart re-opens the same file as a fresh table,
// which is what an agent restart does.
func TestVIPPersistenceAcrossRestart(t *testing.T) {
	table, path := newVIPTable(t)
	a := mustTenant(t, tenantA)
	devA, devB := deviceForIndex(t, a, 1), deviceForIndex(t, a, 2)

	vipA := mustAllocate(t, table, devA)
	vipB := mustAllocate(t, table, devB)

	// Restart 1: the pinned VIPs come back unchanged.
	reopened, err := OpenVIPTable(path)
	if err != nil {
		t.Fatalf("OpenVIPTable after restart: %v", err)
	}
	if got, ok := reopened.Lookup(devA); !ok || got != vipA {
		t.Fatalf("after restart Lookup(devA) = (%s, %v), want (%s, true)", got, ok, vipA)
	}
	if again := mustAllocate(t, reopened, devA); again != vipA {
		t.Fatalf("after restart Allocate(devA) = %s, want the pinned %s", again, vipA)
	}

	// A device first seen after the restart still gets a fresh VIP, and the
	// allocation it triggers survives a second restart.
	vipC := mustAllocate(t, reopened, deviceForIndex(t, a, 3))
	if vipC == vipA || vipC == vipB {
		t.Fatalf("new device reused an existing VIP %s", vipC)
	}
	again, err := OpenVIPTable(path)
	if err != nil {
		t.Fatalf("OpenVIPTable after second restart: %v", err)
	}
	if got, ok := again.Lookup(devB); !ok || got != vipB {
		t.Fatalf("after second restart Lookup(devB) = (%s, %v), want (%s, true)", got, ok, vipB)
	}
	if again.Stats() != reopened.Stats() {
		t.Fatalf("Stats after restart = %+v, want %+v", again.Stats(), reopened.Stats())
	}

	// The file itself round-trips and stays human-readable.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f vipTableFile
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatalf("persisted table is not valid JSON: %v\n%s", err, b)
	}
	if f.Version != vipTableVersion || f.CIDR != DefaultServiceCIDR {
		t.Fatalf("persisted header = version %d cidr %s, want version %d cidr %s",
			f.Version, f.CIDR, vipTableVersion, DefaultServiceCIDR)
	}
	if f.Cursor != 4 || len(f.Entries) != 3 {
		t.Fatalf("persisted cursor/entries = %d / %d, want 4 / 3", f.Cursor, len(f.Entries))
	}
	for _, rec := range f.Entries {
		if rec.Tenant == "" || rec.Device == "" || rec.VIP == "" {
			t.Fatalf("persisted entry %+v is missing tenant/device/vip", rec)
		}
	}
	if !json.Valid(b) || !strings.Contains(string(b), "\n  \"cursor\"") {
		t.Fatalf("persisted table is not indented JSON:\n%s", b)
	}
}

func TestVIPExhaustionReportsErrExhaustedAndNeverRecycles(t *testing.T) {
	// A /24 has 254 usable hosts, so the pool is fillable in a test.
	table, _ := newVIPTableCIDR(t, "10.99.0.0/24")
	a := mustTenant(t, tenantA)

	seen := map[netip.Addr]bool{}
	for i := 1; i <= 254; i++ {
		vip := mustAllocate(t, table, deviceForIndex(t, a, i))
		if seen[vip] {
			t.Fatalf("device %d reused VIP %s", i, vip)
		}
		seen[vip] = true
	}
	if stats := table.Stats(); stats.Allocated != 254 || stats.Capacity != 254 {
		t.Fatalf("Stats = %+v, want 254 allocated of 254", stats)
	}

	_, err := table.Allocate(deviceForIndex(t, a, 255))
	if !errors.Is(err, ErrExhausted) {
		t.Fatalf("Allocate on a full pool: err = %v, want an ErrExhausted wrap", err)
	}

	// Retiring every entry frees nothing: a retired VIP is never handed out
	// again for the life of the table.
	other := mustTenant(t, tenantB)
	if err := table.RetireTenant(other); err != nil {
		t.Fatalf("RetireTenant: %v", err)
	}
	if stats := table.Stats(); stats.Allocated != 0 || stats.Retired != 254 {
		t.Fatalf("Stats after retire = %+v, want 0 allocated, 254 retired", stats)
	}
	if _, err := table.Allocate(deviceForIndex(t, a, 256)); !errors.Is(err, ErrExhausted) {
		t.Fatalf("Allocate after retiring the pool: err = %v, want an ErrExhausted wrap", err)
	}
	for vip := range seen {
		if _, err := table.Device(vip); err == nil {
			t.Fatalf("Device(%s) resolved a retired VIP", vip)
		}
	}
}

func TestVIPRetireTenantDropsForeignTenantsAndSkipsTheirVIPs(t *testing.T) {
	table, _ := newVIPTableCIDR(t, "10.99.0.0/24")
	a, b := mustTenant(t, tenantA), mustTenant(t, tenantB)

	vipA := mustAllocate(t, table, deviceForIndex(t, a, 1))
	vipB := mustAllocate(t, table, deviceForIndex(t, a, 2))
	vipForeign := mustAllocate(t, table, deviceForIndex(t, b, 3))

	// Retiring tenant A keeps A's entries and drops everything else.
	if err := table.RetireTenant(a); err != nil {
		t.Fatalf("RetireTenant: %v", err)
	}
	if _, ok := table.Lookup(deviceForIndex(t, a, 1)); !ok {
		t.Fatal("RetireTenant dropped an entry of the retained tenant")
	}
	if _, ok := table.Lookup(deviceForIndex(t, b, 3)); ok {
		t.Fatal("RetireTenant kept a foreign tenant's entry")
	}
	if _, err := table.Device(vipForeign); err == nil {
		t.Fatalf("Device(%s) still resolves a retired VIP", vipForeign)
	}

	// The next allocation walks past the retired VIP instead of recycling it.
	vipNext := mustAllocate(t, table, deviceForIndex(t, a, 4))
	if vipNext == vipForeign {
		t.Fatalf("Allocate recycled retired VIP %s", vipForeign)
	}
	if vipNext.String() != "10.99.0.4" {
		t.Fatalf("Allocate = %s, want 10.99.0.4 (10.99.0.3 is retired)", vipNext)
	}
	if stats := table.Stats(); stats.Allocated != 3 || stats.Retired != 1 {
		t.Fatalf("Stats = %+v, want 3 allocated, 1 retired", stats)
	}

	// Retirement is persisted: a restart must not resurrect the retired VIP.
	reopened, err := openVIPTable(table.path, netip.MustParsePrefix("10.99.0.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Stats() != table.Stats() {
		t.Fatalf("Stats after restart = %+v, want %+v", reopened.Stats(), table.Stats())
	}
	if got, ok := reopened.Lookup(deviceForIndex(t, a, 1)); !ok || got != vipA {
		t.Fatalf("after restart Lookup = (%s, %v), want (%s, true)", got, ok, vipA)
	}
	if got, ok := reopened.Lookup(deviceForIndex(t, a, 2)); !ok || got != vipB {
		t.Fatalf("after restart Lookup = (%s, %v), want (%s, true)", got, ok, vipB)
	}
}

func TestVIPTenantChangeRetiresOldVIPAndRekeys(t *testing.T) {
	table, path := newVIPTable(t)
	a, b := mustTenant(t, tenantA), mustTenant(t, tenantB)
	devA := deviceForIndex(t, a, 1)

	old := mustAllocate(t, table, devA)
	rekeyed := deviceForIndex(t, b, 1) // same device UUID, different tenant
	fresh := mustAllocate(t, table, rekeyed)
	if fresh == old {
		t.Fatalf("tenant change kept VIP %s; the old principal's address must be retired", old)
	}
	if _, err := table.Device(old); err == nil {
		t.Fatalf("Device(%s) still resolves the retired principal", old)
	}
	back, err := table.Device(fresh)
	if err != nil {
		t.Fatalf("Device(%s): %v", fresh, err)
	}
	if back.Principal() != rekeyed.Principal() {
		t.Fatalf("Device(%s) = %s, want %s", fresh, back.Principal(), rekeyed.Principal())
	}
	if _, ok := table.Lookup(devA); ok {
		t.Fatal("Lookup still finds the retired tenant's principal")
	}
	if got, ok := table.Lookup(rekeyed); !ok || got != fresh {
		t.Fatalf("Lookup(rekeyed) = (%s, %v), want (%s, true)", got, ok, fresh)
	}
	if stats := table.Stats(); stats.Allocated != 1 || stats.Retired != 1 {
		t.Fatalf("Stats = %+v, want 1 allocated, 1 retired", stats)
	}

	// The retired VIP is not reusable, so the next device takes the next slot.
	if next := mustAllocate(t, table, deviceForIndex(t, b, 2)); next == old {
		t.Fatalf("Allocate recycled retired VIP %s", old)
	}

	reopened, err := OpenVIPTable(path)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Stats() != table.Stats() {
		t.Fatalf("Stats after restart = %+v, want %+v", reopened.Stats(), table.Stats())
	}
	if _, err := reopened.Device(old); err == nil {
		t.Fatalf("after restart Device(%s) resurrected a retired VIP", old)
	}
}

func TestVIPCorruptFileIsAnError(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"not JSON", "vip table, probably"},
		{"wrong version", `{"version":2,"cidr":"10.99.0.0/16","cursor":1,"entries":[],"retired":[]}`},
		{"wrong cidr", `{"version":1,"cidr":"10.100.0.0/16","cursor":1,"entries":[],"retired":[]}`},
		{"unparsable cidr", `{"version":1,"cidr":"10.99.0.0","cursor":1,"entries":[],"retired":[]}`},
		{"bad device uuid", `{"version":1,"cidr":"10.99.0.0/16","cursor":1,"entries":[{"tenant":"` + tenantA + `","device":"not-a-uuid","vip":"10.99.0.1"}],"retired":[]}`},
		{"vip outside cidr", `{"version":1,"cidr":"10.99.0.0/16","cursor":2,"entries":[{"tenant":"` + tenantA + `","device":"00000001-4000-8000-0000-000000000001","vip":"192.168.1.1"}],"retired":[]}`},
		{"network address as vip", `{"version":1,"cidr":"10.99.0.0/16","cursor":1,"entries":[{"tenant":"` + tenantA + `","device":"00000001-4000-8000-0000-000000000001","vip":"10.99.0.0"}],"retired":[]}`},
		{"duplicate vip", `{"version":1,"cidr":"10.99.0.0/16","cursor":3,"entries":[{"tenant":"` + tenantA + `","device":"00000001-4000-8000-0000-000000000001","vip":"10.99.0.1"},{"tenant":"` + tenantA + `","device":"00000002-4000-8000-0000-000000000002","vip":"10.99.0.1"}],"retired":[]}`},
		{"duplicate device", `{"version":1,"cidr":"10.99.0.0/16","cursor":3,"entries":[{"tenant":"` + tenantA + `","device":"00000001-4000-8000-0000-000000000001","vip":"10.99.0.1"},{"tenant":"` + tenantA + `","device":"00000001-4000-8000-0000-000000000001","vip":"10.99.0.2"}],"retired":[]}`},
		{"allocated and retired", `{"version":1,"cidr":"10.99.0.0/16","cursor":2,"entries":[{"tenant":"` + tenantA + `","device":"00000001-4000-8000-0000-000000000001","vip":"10.99.0.1"}],"retired":["10.99.0.1"]}`},
		{"cursor past capacity", `{"version":1,"cidr":"10.99.0.0/24","cursor":99,"entries":[],"retired":[]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "vips.json")
			if err := os.WriteFile(path, []byte(c.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := openVIPTable(path, netip.MustParsePrefix(DefaultServiceCIDR)); err == nil {
				t.Fatalf("openVIPTable accepted a corrupt table (%s)", c.name)
			}
		})
	}
}

func TestVIPTableRequiresExistingParentDir(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing", "vips.json")
	if _, err := OpenVIPTable(path); err == nil {
		t.Fatal("OpenVIPTable succeeded with a missing parent directory")
	}
}

func TestVIPTableRejectsCIDRWithoutHosts(t *testing.T) {
	if _, err := openVIPTable(filepath.Join(t.TempDir(), "vips.json"), netip.MustParsePrefix("10.99.0.1/32")); err == nil {
		t.Fatal("openVIPTable accepted a CIDR with no usable host addresses")
	}
}

func TestVIPConcurrentAllocateAndRead(t *testing.T) {
	table, path := newVIPTable(t)
	a := mustTenant(t, tenantA)

	const n = 64
	var wg sync.WaitGroup
	vips := make([]netip.Addr, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			vip, err := table.Allocate(deviceForIndex(t, a, i+1))
			if err != nil {
				t.Errorf("Allocate(%d): %v", i+1, err)
				return
			}
			vips[i] = vip
			// Concurrent readers must never observe a torn table.
			if _, ok := table.Lookup(deviceForIndex(t, a, i+1)); !ok {
				t.Errorf("Lookup(%d) missed its own allocation", i+1)
			}
			_ = table.Stats()
		}(i)
	}
	wg.Wait()

	seen := map[netip.Addr]bool{}
	for i, vip := range vips {
		if seen[vip] {
			t.Fatalf("device %d got VIP %s twice", i+1, vip)
		}
		seen[vip] = true
	}
	if stats := table.Stats(); stats.Allocated != n {
		t.Fatalf("Stats = %+v, want %d allocated", stats, n)
	}
	reopened, err := OpenVIPTable(path)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Stats() != table.Stats() {
		t.Fatalf("Stats after restart = %+v, want %+v", reopened.Stats(), table.Stats())
	}
}

// A failed parent-directory sync happens after the new filename is visible.
// Rolling back memory and continuing could contradict the surviving disk state.
func TestVIPPostRenameFailureStopsPublicationAndReuse(t *testing.T) {
	table, path := newVIPTable(t)
	tenant := mustTenant(t, tenantA)
	first := deviceForIndex(t, tenant, 1)
	oldVIP := mustAllocate(t, table, first)
	second := deviceForIndex(t, tenant, 2)
	table.syncDirectory = func(string) error { return errors.New("injected directory sync failure") }
	if _, err := table.Allocate(second); err == nil {
		t.Fatal("allocation succeeded without durable rename")
	}
	if _, ok := table.Lookup(second); ok {
		t.Fatal("lookup published an allocation with uncertain durability")
	}
	if _, err := table.Device(oldVIP); err == nil {
		t.Fatal("poisoned table continued to route")
	}
	if _, err := table.Allocate(first); err == nil {
		t.Fatal("cached allocation bypassed failed persistence")
	}
	if err := table.RetireTenant(mustTenant(t, tenantB)); err == nil {
		t.Fatal("retirement overwrote uncertain persisted state")
	}
	reopened, err := OpenVIPTable(path)
	if err != nil {
		t.Fatal(err)
	}
	secondVIP, ok := reopened.Lookup(second)
	if !ok {
		t.Fatal("visible committed table was rolled back")
	}
	thirdVIP := mustAllocate(t, reopened, deviceForIndex(t, tenant, 3))
	if thirdVIP == oldVIP || thirdVIP == secondVIP {
		t.Fatal("reopen reused a persisted VIP")
	}
}

func TestVIPRejectsAbsentIdentityButAllowsNilUUID(t *testing.T) {
	table, _ := newVIPTable(t)
	if _, err := table.Allocate(meshid.Device{}); err == nil {
		t.Fatal("allocated a VIP for an absent device")
	}
	tenant := mustTenant(t, "00000000-0000-0000-0000-000000000000")
	device, err := meshid.ParseDevice(tenant, "00000000-0000-0000-0000-000000000000")
	if err != nil {
		t.Fatal(err)
	}
	vip := mustAllocate(t, table, device)
	if got, err := table.Device(vip); err != nil || got != device {
		t.Fatalf("explicit nil UUID mapping: %s, %v", got, err)
	}
	if err := table.RetireTenant(meshid.Tenant{}); err == nil {
		t.Fatal("retired VIPs without a tenant")
	}
}

func TestVIPRetirementFailureStopsOldTenantResolution(t *testing.T) {
	table, path := newVIPTable(t)
	oldDevice := deviceForIndex(t, mustTenant(t, tenantA), 1)
	oldVIP := mustAllocate(t, table, oldDevice)
	// Fail before rename, when the old table is still the authoritative disk file.
	table.path = filepath.Join(t.TempDir(), "missing-directory", "vips.json")
	if err := table.RetireTenant(mustTenant(t, tenantB)); err == nil {
		t.Fatal("retirement unexpectedly succeeded")
	}
	if _, err := table.Device(oldVIP); err == nil {
		t.Fatal("failed enrollment retirement kept resolving an old tenant")
	}
	table.path = path
	if _, err := table.Allocate(oldDevice); err == nil {
		t.Fatal("failed retirement allowed later allocations")
	}
	// A subsequent startup retries retirement before handing the table to proxies.
	reopened, err := OpenVIPTable(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.RetireTenant(mustTenant(t, tenantB)); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.Device(oldVIP); err == nil {
		t.Fatal("restart resurrected retired tenant")
	}
}
