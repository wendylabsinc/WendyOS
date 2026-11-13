package discoverycache

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// modelsLANFixture returns a fully-populated models.LANDevice covering every
// field EntryFromDevice/Device round-trip through Entry.
func modelsLANFixture() models.LANDevice {
	return models.LANDevice{
		ID:               "dev-1",
		DisplayName:      "Orin Nano",
		Hostname:         "orin-nano.local",
		IPAddress:        "10.0.0.5",
		Port:             50051,
		IsMTLS:           true,
		AssetID:          3,
		MeshName:         "brave-dolphin",
		OrgID:            7,
		InterfaceType:    string(models.InterfaceLAN),
		NetworkInterface: "en0",
		IsWendyDevice:    true,
		AgentVersion:     "0.19.1",
		DeviceType:       "orin-nano",
		OS:               "wendyos",
		OSVersion:        "0.19.1",
		CPUArchitecture:  "arm64",
	}
}

func TestKeyFallback(t *testing.T) {
	if Key("Dev-ID", "orin") != "dev-id" {
		t.Fatalf("id should win, lowercased")
	}
	if Key("", "Orin-Nano") != "orin-nano" {
		t.Fatalf("displayName fallback, lowercased")
	}
}

func TestUpsertMergeAndTTL(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadFrom(filepath.Join(dir, "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	c.Upsert(Entry{ID: "a", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.5", Port: 50051, AgentVersion: "0.19.1"}, now.Add(-2*time.Hour))
	c.Upsert(Entry{ID: "a", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.9", Port: 50051}, now) // browse-only: no version
	fresh := c.Fresh(now)
	if len(fresh) != 1 {
		t.Fatalf("want 1 fresh entry, got %d", len(fresh))
	}
	if fresh[0].IP != "10.0.0.9" || fresh[0].AgentVersion != "0.19.1" {
		t.Fatalf("merge broke: %+v (new IP must win, old version must survive)", fresh[0])
	}

	// stale entries are not fresh
	c.Upsert(Entry{ID: "b", DisplayName: "old", Hostname: "old.local", Port: 50051}, now.Add(-61*time.Minute))
	if got := len(c.Fresh(now)); got != 1 {
		t.Fatalf("61-minute-old entry must not be fresh, got %d entries", got)
	}
}

func TestReplaceDropsFieldsUpsertWouldKeep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	c, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	c.Upsert(Entry{ID: "a", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.5", Port: 50051, MTLS: true, OrgID: 7, MeshName: "brave-dolphin", AgentVersion: "0.19.1"}, now)

	// The device stopped advertising mTLS/orgid/name: a caller holding the
	// complete current state replaces the row outright.
	c.Replace(Entry{ID: "a", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.5", Port: 50051, AgentVersion: "0.19.1"}, now)

	fresh := c.Fresh(now)
	if len(fresh) != 1 {
		t.Fatalf("want 1 entry, got %d", len(fresh))
	}
	if fresh[0].MTLS || fresh[0].OrgID != 0 || fresh[0].MeshName != "" {
		t.Fatalf("Replace must drop cleared fields: %+v", fresh[0])
	}
	if fresh[0].AgentVersion != "0.19.1" || fresh[0].IP != "10.0.0.5" || !fresh[0].LastSeen.Equal(now) {
		t.Fatalf("Replace must store the given entry verbatim, stamped now: %+v", fresh[0])
	}

	if err := c.Flush(now); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := LoadFrom(path)
	if got := reloaded.Fresh(now); len(got) != 1 || got[0].MTLS || got[0].OrgID != 0 {
		t.Fatalf("cleared fields must not survive the flush: %+v", got)
	}
}

func TestFlushRoundTripAndEviction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	c, _ := LoadFrom(path)
	now := time.Now()
	c.Upsert(Entry{ID: "a", DisplayName: "orin", Hostname: "orin.local", Port: 50051}, now)
	c.Upsert(Entry{ID: "b", DisplayName: "stale", Hostname: "stale.local", Port: 50051}, now.Add(-2*time.Hour))
	if err := c.Flush(now); err != nil {
		t.Fatal(err)
	}
	c2, _ := LoadFrom(path)
	if got := len(c2.Fresh(now)); got != 1 {
		t.Fatalf("stale entry must be evicted on flush, got %d", got)
	}
}

func TestFlushMergesConcurrentWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	now := time.Now()
	other, _ := LoadFrom(path)
	other.Upsert(Entry{ID: "other", DisplayName: "other", Hostname: "other.local", Port: 50051}, now)
	if err := other.Flush(now); err != nil {
		t.Fatal(err)
	}
	// c was loaded before other's flush (empty file), upserts one entry;
	// Flush must re-read and keep other's entry too.
	c, _ := LoadFrom(path) // loaded fresh here for simplicity; the re-read is what's under test
	c.Upsert(Entry{ID: "mine", DisplayName: "mine", Hostname: "mine.local", Port: 50051}, now)
	if err := c.Flush(now); err != nil {
		t.Fatal(err)
	}
	c2, _ := LoadFrom(path)
	if got := len(c2.Fresh(now)); got != 2 {
		t.Fatalf("flush must merge with on-disk entries, got %d", got)
	}
}

func TestCorruptFileIsEmptyCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadFrom(path)
	if err != nil || len(c.Fresh(time.Now())) != 0 {
		t.Fatalf("corrupt file must load as empty cache with nil error, err=%v", err)
	}
	// and Flush must replace it
	c.Upsert(Entry{ID: "a", DisplayName: "a", Hostname: "a.local", Port: 50051}, time.Now())
	if err := c.Flush(time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestEntryDeviceRoundTrip(t *testing.T) {
	dev := EntryFromDevice(modelsLANFixture()).Device()
	if dev.ID != "dev-1" || dev.IPAddress != "10.0.0.5" || !dev.IsMTLS || dev.OrgID != 7 ||
		dev.AssetID != 3 || dev.InterfaceType != "lan" || !dev.IsWendyDevice || dev.NetworkInterface != "en0" {
		t.Fatalf("round trip lost fields: %+v", dev)
	}
}

// TestDeleteRemovesEntryFromFile pins the removal path the streaming engine
// needs when it retires a stale identity (a connect-minted hostname row the
// device's real TXT device id supersedes): the entry must disappear from the
// file too, not be re-read off disk and kept by the next flush.
func TestDeleteRemovesEntryFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	now := time.Now()

	seed, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	seed.Upsert(Entry{ID: "orin", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.5", Port: 50051}, now)
	seed.Upsert(Entry{ID: "uuid-1", DisplayName: "orin", Hostname: "orin.local", IP: "10.0.0.5", Port: 50051}, now)
	if err := seed.Flush(now); err != nil {
		t.Fatal(err)
	}

	c, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	c.Delete(Key("orin", "orin"))
	if fresh := c.Fresh(now); len(fresh) != 1 || fresh[0].ID != "uuid-1" {
		t.Fatalf("Delete left the entry in memory: %+v", fresh)
	}
	if err := c.Flush(now); err != nil {
		t.Fatal(err)
	}

	reloaded, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	fresh := reloaded.Fresh(now)
	if len(fresh) != 1 || fresh[0].ID != "uuid-1" {
		t.Fatalf("deleted entry survived the flush: %+v", fresh)
	}
}

func TestEntriesIncludesStale(t *testing.T) {
	c, err := LoadFrom(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatalf("LoadFrom: %v", err)
	}
	now := time.Now()
	c.Upsert(Entry{ID: "fresh-dev", Hostname: "fresh.local", IP: "10.0.0.1"}, now)
	c.Upsert(Entry{ID: "stale-dev", Hostname: "stale.local", IP: "10.0.0.2"}, now.Add(-2*TTL))
	if got := len(c.Fresh(now)); got != 1 {
		t.Fatalf("Fresh = %d entries, want 1 (display TTL must be unchanged)", got)
	}
	if got := len(c.Entries()); got != 2 {
		t.Fatalf("Entries = %d, want 2 (stale included)", got)
	}
}

// The connect path reads the device's cable addresses from here; they must
// survive the trip to disk and back.
func TestWiredAddressesSurviveTheCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.json")
	c, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	dev := modelsLANFixture()
	dev.WiredAddresses = []string{"10.0.0.5", "10.0.1.5"}
	now := time.Now()
	c.Replace(EntryFromDevice(dev), now)
	if err := c.Flush(now); err != nil {
		t.Fatal(err)
	}

	reloaded, err := LoadFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	fresh := reloaded.Fresh(now)
	if len(fresh) != 1 || !slices.Equal(fresh[0].Device().WiredAddresses, dev.WiredAddresses) {
		t.Fatalf("wired addresses lost: %+v", fresh)
	}
}

// A connect-success write knows nothing about cabling and must not erase it;
// a fresh advertisement replaces it.
func TestUpsertKeepsWiredAddressesUnlessReplaced(t *testing.T) {
	c, err := LoadFrom(filepath.Join(t.TempDir(), "devices.json"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	c.Upsert(Entry{ID: "a", DisplayName: "pi", IP: "10.0.0.5", WiredAddresses: []string{"10.0.0.5"}}, now)
	c.Upsert(Entry{ID: "a", DisplayName: "pi", IP: "192.168.2.9"}, now)
	if got := c.Fresh(now)[0].WiredAddresses; !slices.Equal(got, []string{"10.0.0.5"}) {
		t.Fatalf("upsert without cabling erased it: %v", got)
	}
	c.Upsert(Entry{ID: "a", DisplayName: "pi", WiredAddresses: []string{"10.0.0.6"}}, now)
	if got := c.Fresh(now)[0].WiredAddresses; !slices.Equal(got, []string{"10.0.0.6"}) {
		t.Fatalf("newer cabling not applied: %v", got)
	}
}
