package scan

import (
	"testing"
	"time"
)

func TestBoundedStoreOwnsRawMetadataAndKeepsPayloadFreshness(t *testing.T) {
	now := time.Now()
	store := newDeviceStore()
	store.limit = 2
	raw := []byte{0, 255, 1, 0}
	store.merge(BLEDeviceInfo{Address: "one", LastSeen: now, ServiceDataSeen: now, ServiceData: map[string][]byte{"service": raw}})
	raw[0] = 42
	snapshot := store.snapshot()
	if snapshot[0].ServiceData["service"][0] != 0 {
		t.Fatal("store retained caller buffer")
	}
	snapshot[0].ServiceData["service"][0] = 43
	if store.snapshot()[0].ServiceData["service"][0] != 0 {
		t.Fatal("snapshot aliases store bytes")
	}
	store.merge(BLEDeviceInfo{Address: "one", LastSeen: now.Add(time.Second)})
	if store.snapshot()[0].ServiceDataSeen != now {
		t.Fatal("RSSI-only snapshot refreshed payload timestamp")
	}
	store.merge(BLEDeviceInfo{Address: "two", LastSeen: now.Add(2 * time.Second)})
	store.merge(BLEDeviceInfo{Address: "three", LastSeen: now.Add(3 * time.Second)})
	if len(store.byAddress) != 2 || store.byAddress["one"].Address != "" {
		t.Fatal("background scan did not evict oldest observation")
	}
}
