package scan

import (
	"testing"
	"time"
)

func TestMeshAdvertisementMetadataSurvivesSparseUpdates(t *testing.T) {
	store := newDeviceStore()
	now := time.Now()
	data := map[string][]byte{"uuid": {1, 2, 3}}
	store.merge(BLEDeviceInfo{Address: "radio", Name: "hint", LastSeen: now, ServiceData: data})
	store.merge(BLEDeviceInfo{Address: "radio", RSSI: -40})
	got := store.snapshot()[0]
	if !got.LastSeen.Equal(now) || string(got.ServiceData["uuid"]) != string(data["uuid"]) {
		t.Fatal("lost mesh discovery metadata")
	}
	if store.merge(BLEDeviceInfo{Address: "radio", LastSeen: now, ServiceData: data}) {
		t.Fatal("unchanged advertisement emitted again")
	}
	if !store.merge(BLEDeviceInfo{Address: "radio", LastSeen: now.Add(time.Second)}) {
		t.Fatal("new observation did not refresh candidate")
	}
}
func TestMeshScannerBoundsRetainedDevices(t *testing.T) {
	store := newDeviceStore()
	store.limit = 2
	now := time.Now()
	store.merge(BLEDeviceInfo{Address: "old", LastSeen: now})
	store.merge(BLEDeviceInfo{Address: "recent", LastSeen: now.Add(time.Second)})
	store.merge(BLEDeviceInfo{Address: "new", LastSeen: now.Add(2 * time.Second)})
	if len(store.byAddress) != 2 {
		t.Fatal("unbounded scan")
	}
	if _, ok := store.byAddress["old"]; ok {
		t.Fatal("old observation retained")
	}
	if store.merge(BLEDeviceInfo{Address: "stale", LastSeen: now.Add(-time.Second)}) {
		t.Fatal("stale observation displaced fresh candidate")
	}
}
