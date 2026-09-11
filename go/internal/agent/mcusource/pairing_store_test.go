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
