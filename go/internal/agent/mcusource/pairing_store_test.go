package mcusource_test

import (
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
)

func TestPairingStoreRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sensor-pairings.json")
	s := mcusource.NewPairingStore(path)
	if err := s.Load(); err != nil {
		t.Fatalf("load empty: %v", err)
	}
	if err := s.Add(mcusource.SensorPairing{SourceAssetID: 12, OrgID: 3, Name: "sensor-hub"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	s2 := mcusource.NewPairingStore(path)
	if err := s2.Load(); err != nil {
		t.Fatalf("reload: %v", err)
	}
	got := s2.List()
	if len(got) != 1 || got[0].SourceAssetID != 12 || got[0].Name != "sensor-hub" {
		t.Fatalf("bad reload: %+v", got)
	}
	if err := s2.Remove(12); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if len(s2.List()) != 0 {
		t.Fatal("expected empty after remove")
	}
}

func TestPairingStorePrincipalIdentitiesStayDistinct(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pairings.json")
	s := mcusource.NewPairingStore(path)
	a := mcusource.SensorPairing{SourcePrincipalURI: "spiffe://wendy.sh/tenant/11111111-2222-4333-8444-555555555555/device/board-a", Transport: "wendycom", Name: "A"}
	b := a
	b.SourcePrincipalURI = "spiffe://wendy.sh/tenant/11111111-2222-4333-8444-555555555555/device/board-b"
	b.Name = "B"
	for _, p := range []mcusource.SensorPairing{a, b, {SourceAssetID: 7, Name: "Legacy"}} {
		if err := s.Add(p); err != nil {
			t.Fatal(err)
		}
	}
	s = mcusource.NewPairingStore(path)
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 3 {
		t.Fatal("distinct principals collided with asset zero")
	}
	if err := s.RemovePairing(a); err != nil {
		t.Fatal(err)
	}
	if len(s.List()) != 2 {
		t.Fatal("removing one principal removed another source")
	}
	if name, ok := s.NameForKey(b.Key()); !ok || name != "B" {
		t.Fatal("principal label lost")
	}
	if name, ok := s.NameFor(7); !ok || name != "Legacy" {
		t.Fatal("legacy label lost")
	}
}
