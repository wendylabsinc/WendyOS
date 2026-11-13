// Package mesh implements the WendyOS mesh data plane: the persistent
// device-principal↔VIP table that links mesh names to host addresses, the
// per-app DNS server that answers device-<uuid>.mesh.wendy.internal names,
// and the transparent TCP proxy that carries mesh VIP connections to peer
// devices.
package mesh

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// DefaultServiceCIDR is the mesh service CIDR v1 operates on. The wendy.json
// schema accepts other CIDRs (the route/ACCEPT plumbing honors them), but DNS
// answers and VIP→device resolution assume this network.
const DefaultServiceCIDR = "10.99.0.0/16"

var meshPrefix = netip.MustParsePrefix(DefaultServiceCIDR)

// ErrExhausted reports that the table has no unallocated, unretired host
// address left in its CIDR. Callers wrap it with the table's own context.
var ErrExhausted = errors.New("wendy net: VIP pool exhausted")

// vipTableVersion is the on-disk schema version of the VIP table file.
const vipTableVersion = 1

// VIPTable pins mesh VIPs to device principals. A VIP is allocated on a
// device's first use and stays pinned to that principal for the life of the
// table: uniqueness is host-wide, not per-tenant, a VIP is never handed to a
// second device, and a retired VIP is never re-allocated. Every mutation is
// persisted to the table's file, so the mapping survives agent restarts and a
// container's cached VIP keeps pointing at the same peer across them.
type VIPTable struct {
	mu       sync.Mutex
	path     string
	prefix   netip.Prefix
	capacity int                    // usable host addresses in prefix (network/broadcast excluded)
	entries  map[uuid.UUID]vipEntry // device UUID -> its pinned allocation
	owner    map[netip.Addr]uuid.UUID
	retired  map[netip.Addr]bool
	cursor   int // next usable host index to try, 1-based
	// A failed directory sync after rename leaves the committed filename's
	// durability uncertain. Fail closed until reopen; rolling back only memory
	// would let a later mutation reuse an address still present on disk.
	persistenceErr error
	syncDirectory  func(string) error // test seam; nil uses syncVIPDirectory
}

// vipEntry is one pinned allocation.
type vipEntry struct {
	tenant meshid.Tenant
	id     uuid.UUID
	vip    netip.Addr
}

// OpenVIPTable loads the table persisted at path, creating it when the file
// does not exist. Parent dir must already exist; a corrupt file is an error.
func OpenVIPTable(path string) (*VIPTable, error) {
	return openVIPTable(path, meshPrefix)
}

// openVIPTable is the seam tests use to exercise a smaller CIDR than the
// production one without waiting on 65k allocations.
func openVIPTable(path string, prefix netip.Prefix) (*VIPTable, error) {
	capacity := usableHosts(prefix)
	if capacity < 1 {
		return nil, fmt.Errorf("wendy net: service CIDR %s has no usable host addresses", prefix)
	}
	t := &VIPTable{
		path:     path,
		prefix:   prefix,
		capacity: capacity,
		entries:  make(map[uuid.UUID]vipEntry),
		owner:    make(map[netip.Addr]uuid.UUID),
		retired:  make(map[netip.Addr]bool),
		cursor:   1,
	}
	if err := t.load(); err != nil {
		return nil, err
	}
	// This also makes a visible rename durable when reopening after a previous
	// post-rename error, before existing allocations can be returned again.
	if err := syncVIPDirectory(filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("wendy net: VIP table directory sync: %w", err)
	}
	return t, nil
}

// CIDR returns the network the table hands VIPs out of.
func (t *VIPTable) CIDR() netip.Prefix { return t.prefix }

// Allocate returns the VIP pinned to device's principal, allocating on first
// use; repeated calls are idempotent. If the device already holds a VIP under
// a DIFFERENT tenant, that entry is retired (its VIP is never reused) and a
// fresh VIP is allocated. Errors wrap ErrExhausted when the pool is full.
func (t *VIPTable) Allocate(device meshid.Device) (netip.Addr, error) {
	if !device.IsValid() {
		return netip.Addr{}, errors.New("wendy net VIP allocation requires a device principal")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.persistenceErr != nil {
		return netip.Addr{}, t.persistenceErr
	}
	if !device.IsValid() {
		return netip.Addr{}, errors.New("net: VIP allocation requires a device identity")
	}

	id := device.UUID()
	tenant := device.Tenant()
	prev, rekeyed := t.entries[id]
	if rekeyed && prev.tenant.UUID() == tenant.UUID() {
		return prev.vip, nil
	}

	// Allocate before retiring the stale principal, so the VIP it still owns
	// is never picked up by the scan below.
	previousCursor := t.cursor
	vip, err := t.nextVIPLocked()
	if err != nil {
		return netip.Addr{}, err
	}
	if rekeyed {
		delete(t.entries, id)
		delete(t.owner, prev.vip)
		t.retired[prev.vip] = true
	}
	t.entries[id] = vipEntry{tenant: tenant, id: id, vip: vip}
	t.owner[vip] = id

	if err := t.persist(); err != nil {
		if t.persistenceErr != nil {
			return netip.Addr{}, err
		}
		// Keep memory and disk identical: a VIP the agent cannot prove it
		// persisted is a VIP it must not hand out.
		t.cursor = previousCursor
		delete(t.entries, id)
		delete(t.owner, vip)
		if rekeyed {
			t.entries[id] = prev
			t.owner[prev.vip] = id
			delete(t.retired, prev.vip)
		}
		return netip.Addr{}, err
	}
	return vip, nil
}

// Lookup returns an already-allocated VIP without allocating.
func (t *VIPTable) Lookup(device meshid.Device) (netip.Addr, bool) {
	if !device.IsValid() {
		return netip.Addr{}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.persistenceErr != nil || !device.IsValid() {
		return netip.Addr{}, false
	}
	e, ok := t.entries[device.UUID()]
	if !ok || e.tenant.UUID() != device.Tenant().UUID() {
		return netip.Addr{}, false
	}
	return e.vip, true
}

// Device returns the principal that owns vip (error if vip is outside the
// CIDR or unallocated).
func (t *VIPTable) Device(vip netip.Addr) (meshid.Device, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.persistenceErr != nil {
		return meshid.Device{}, t.persistenceErr
	}
	if _, ok := t.indexOfVIP(vip); !ok {
		return meshid.Device{}, fmt.Errorf("wendy net: %s is outside the wendy net service CIDR %s", vip, t.prefix)
	}
	id, ok := t.owner[vip]
	if !ok {
		return meshid.Device{}, fmt.Errorf("wendy net: %s is not an allocated wendy net VIP", vip)
	}
	e := t.entries[id]
	// The entry was validated on load/insert, so this cannot fail.
	return meshid.ParseDevice(e.tenant, id.String())
}

// RetireTenant drops every entry whose tenant differs from tenant; retired
// VIPs are never re-allocated.
func (t *VIPTable) RetireTenant(tenant meshid.Tenant) error {
	if !tenant.IsValid() {
		return errors.New("wendy net VIP retirement requires a tenant identity")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.persistenceErr != nil {
		return t.persistenceErr
	}
	if !tenant.IsValid() {
		return errors.New("net: VIP retirement requires a tenant identity")
	}

	var dropped []vipEntry
	for _, e := range t.entries {
		if e.tenant.UUID() != tenant.UUID() {
			dropped = append(dropped, e)
		}
	}
	if len(dropped) == 0 {
		return nil
	}
	for _, e := range dropped {
		delete(t.entries, e.id)
		delete(t.owner, e.vip)
		t.retired[e.vip] = true
	}
	if err := t.persist(); err != nil {
		// Tenant retirement is an admission change. Even a pre-rename failure
		// must stop old VIPs resolving under the new enrollment.
		if t.persistenceErr == nil {
			t.persistenceErr = fmt.Errorf("net: VIP tenant retirement failed: %w; reopen table before further use", err)
		}
		return t.persistenceErr
	}
	return nil
}

// VIPStats reports the table's occupancy.
type VIPStats struct {
	Allocated int
	Retired   int
	Capacity  int
}

// Stats returns a snapshot of the table's occupancy.
func (t *VIPTable) Stats() VIPStats {
	t.mu.Lock()
	defer t.mu.Unlock()
	return VIPStats{Allocated: len(t.entries), Retired: len(t.retired), Capacity: t.capacity}
}

// nextVIPLocked returns the first usable VIP at or after the cursor, advancing
// the cursor past it. Allocated and retired VIPs are skipped, never recycled.
func (t *VIPTable) nextVIPLocked() (netip.Addr, error) {
	for idx := t.cursor; idx <= t.capacity; idx++ {
		vip := t.vipForIndex(idx)
		if _, held := t.owner[vip]; held {
			continue
		}
		if t.retired[vip] {
			continue
		}
		t.cursor = idx + 1
		return vip, nil
	}
	return netip.Addr{}, fmt.Errorf("%w: %s has %d usable VIPs, all allocated or retired",
		ErrExhausted, t.prefix, t.capacity)
}

// usableHosts counts the host addresses of an IPv4 CIDR, excluding the network
// and broadcast addresses (10.99.0.0/16 -> 65534).
func usableHosts(p netip.Prefix) int {
	if !p.IsValid() || !p.Addr().Is4() {
		return 0
	}
	return (1 << (32 - p.Bits())) - 2
}

// vipForIndex converts a 1-based usable-host index to its address.
func (t *VIPTable) vipForIndex(idx int) netip.Addr {
	base := binary.BigEndian.Uint32(t.prefix.Addr().AsSlice())
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], base+uint32(idx))
	return netip.AddrFrom4(b)
}

// indexOfVIP returns the usable-host index of vip, or false if vip is not a
// usable host address of the table's CIDR.
func (t *VIPTable) indexOfVIP(vip netip.Addr) (int, bool) {
	if !vip.Is4() || vip.Zone() != "" || !t.prefix.Contains(vip) {
		return 0, false
	}
	base := binary.BigEndian.Uint32(t.prefix.Addr().AsSlice())
	idx := int(binary.BigEndian.Uint32(vip.AsSlice()) - base)
	if idx < 1 || idx > t.capacity {
		return 0, false
	}
	return idx, true
}

// vipTableFile is the human-readable on-disk form of the table.
type vipTableFile struct {
	Version int         `json:"version"`
	CIDR    string      `json:"cidr"`
	Cursor  int         `json:"cursor"`
	Entries []vipRecord `json:"entries"`
	Retired []string    `json:"retired"`
}

// vipRecord is one pinned allocation in the on-disk form.
type vipRecord struct {
	Tenant string `json:"tenant"`
	Device string `json:"device"`
	VIP    string `json:"vip"`
}

// load reads the persisted table. A missing file is a fresh table; anything
// unparseable, inconsistent, or out of range is an error rather than a silent
// reset, because a half-read table would hand out VIPs the network already
// believes belong to someone else.
func (t *VIPTable) load() error {
	b, err := os.ReadFile(t.path)
	if errors.Is(err, os.ErrNotExist) {
		if _, statErr := os.Stat(filepath.Dir(t.path)); statErr != nil {
			return fmt.Errorf("wendy net: VIP table directory %s: %w", filepath.Dir(t.path), statErr)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("wendy net: VIP table %s: %w", t.path, err)
	}

	var f vipTableFile
	if err := json.Unmarshal(b, &f); err != nil {
		return t.corrupt("unreadable JSON: %v", err)
	}
	if f.Version != vipTableVersion {
		return t.corrupt("unsupported version %d", f.Version)
	}
	prefix, err := netip.ParsePrefix(f.CIDR)
	if err != nil || prefix != t.prefix {
		return t.corrupt("cidr %q does not match %s", f.CIDR, t.prefix)
	}
	for _, rec := range f.Entries {
		tenant, err := meshid.ParseTenant(rec.Tenant)
		if err != nil {
			return t.corrupt("entry %q: %v", rec.Device, err)
		}
		device, err := meshid.ParseDevice(tenant, rec.Device)
		if err != nil {
			return t.corrupt("entry %q: %v", rec.Device, err)
		}
		vip, err := netip.ParseAddr(rec.VIP)
		if err != nil {
			return t.corrupt("entry %q: vip %q: %v", rec.Device, rec.VIP, err)
		}
		if _, ok := t.indexOfVIP(vip); !ok {
			return t.corrupt("entry %q: vip %s is not a usable host of %s", rec.Device, vip, t.prefix)
		}
		if _, dup := t.entries[device.UUID()]; dup {
			return t.corrupt("device %s is allocated twice", device.UUID())
		}
		if _, dup := t.owner[vip]; dup {
			return t.corrupt("vip %s is allocated to two devices", vip)
		}
		t.entries[device.UUID()] = vipEntry{tenant: tenant, id: device.UUID(), vip: vip}
		t.owner[vip] = device.UUID()
	}
	for _, text := range f.Retired {
		vip, err := netip.ParseAddr(text)
		if err != nil {
			return t.corrupt("retired vip %q: %v", text, err)
		}
		if _, ok := t.indexOfVIP(vip); !ok {
			return t.corrupt("retired vip %s is not a usable host of %s", vip, t.prefix)
		}
		if _, held := t.owner[vip]; held {
			return t.corrupt("vip %s is both allocated and retired", vip)
		}
		t.retired[vip] = true
	}
	if f.Cursor < 1 || f.Cursor > t.capacity+1 {
		return t.corrupt("cursor %d is outside [1, %d]", f.Cursor, t.capacity+1)
	}
	t.cursor = f.Cursor
	return nil
}

func (t *VIPTable) corrupt(format string, args ...any) error {
	return fmt.Errorf("wendy net: VIP table %s is corrupt: "+format, append([]any{t.path}, args...)...)
}

// persist writes the whole table atomically: a temp file in the same directory
// is fsynced, then renamed over path, so a crash mid-write leaves either the
// old table or the new one, never a truncated file. The parent directory is
// synced before any allocation can be published. Entries are sorted for
// determinism. A post-rename error poisons the table until it is reopened.
func (t *VIPTable) persist() error {
	f := vipTableFile{
		Version: vipTableVersion,
		CIDR:    t.prefix.String(),
		Cursor:  t.cursor,
		Entries: make([]vipRecord, 0, len(t.entries)),
		Retired: make([]string, 0, len(t.retired)),
	}
	for _, e := range t.entries {
		f.Entries = append(f.Entries, vipRecord{
			Tenant: e.tenant.String(),
			Device: e.id.String(),
			VIP:    e.vip.String(),
		})
	}
	sort.Slice(f.Entries, func(i, j int) bool {
		a, b := netip.MustParseAddr(f.Entries[i].VIP), netip.MustParseAddr(f.Entries[j].VIP)
		if c := a.Compare(b); c != 0 {
			return c < 0
		}
		return f.Entries[i].Device < f.Entries[j].Device
	})
	retired := make([]netip.Addr, 0, len(t.retired))
	for vip := range t.retired {
		retired = append(retired, vip)
	}
	sort.Slice(retired, func(i, j int) bool { return retired[i].Compare(retired[j]) < 0 })
	for _, vip := range retired {
		f.Retired = append(f.Retired, vip.String())
	}

	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return fmt.Errorf("wendy net: VIP table %s: encode: %w", t.path, err)
	}
	b = append(b, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(t.path), ".mesh-vips-*")
	if err != nil {
		return fmt.Errorf("wendy net: VIP table %s: %w", t.path, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once the rename has landed
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("wendy net: VIP table %s: write: %w", t.path, err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("wendy net: VIP table %s: sync: %w", t.path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("wendy net: VIP table %s: close: %w", t.path, err)
	}
	if err := os.Rename(tmpName, t.path); err != nil {
		return fmt.Errorf("wendy net: VIP table %s: rename: %w", t.path, err)
	}
	syncDirectory := t.syncDirectory
	if syncDirectory == nil {
		syncDirectory = syncVIPDirectory
	}
	if err := syncDirectory(filepath.Dir(t.path)); err != nil {
		t.persistenceErr = fmt.Errorf("wendy net: VIP table %s: directory sync after rename: %w; reopen table before further use", t.path, err)
		return t.persistenceErr
	}
	return nil
}

func syncVIPDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil // NTFS replacement follows the shared atomic writer contract.
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
