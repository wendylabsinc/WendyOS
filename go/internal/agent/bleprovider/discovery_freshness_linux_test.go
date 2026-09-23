//go:build linux

package bleprovider

import (
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func meshDeviceProps(uuid string, raw []byte) map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"Address": dbus.MakeVariant("AA:BB:CC:DD:EE:01"), "AddressType": dbus.MakeVariant("public"),
		"ServiceData": dbus.MakeVariant(map[string]dbus.Variant{uuid: dbus.MakeVariant(raw)}),
	}
}

func TestBlueZCachedServiceDataNeedsLiveAdvertisement(t *testing.T) {
	uuid, _ := ServiceUUID(64)
	adv, _ := NewAdvertisement(12, "field", DefaultPSM)
	raw, _ := adv.MarshalBinary()
	path := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_01")
	props := meshDeviceProps(uuid, raw)
	objects := managedObjects{path: {deviceInterface: props}}
	tracker := newAdvertisementFreshness("/org/bluez/hci0", uuid, "field", 20)
	base := time.Unix(1_000, 0)
	check := func(at time.Time, want int) {
		t.Helper()
		got, _ := tracker.candidates(objects, at)
		if len(got) != want {
			t.Fatalf("at %v candidates=%+v, want %d", at.Sub(base), got, want)
		}
	}

	check(base, 0) // GetManagedObjects alone can return stale ServiceData.
	tracker.handle(&dbus.Signal{Name: "org.freedesktop.DBus.Properties.PropertiesChanged", Path: path, Body: []any{deviceInterface, map[string]dbus.Variant{"RSSI": dbus.MakeVariant(int16(-50))}, []string{}}}, base)
	check(base, 0) // A legacy advertisement or connection can update RSSI.
	tracker.handle(&dbus.Signal{Name: "org.freedesktop.DBus.Properties.PropertiesChanged", Path: path, Body: []any{deviceInterface, map[string]dbus.Variant{"ServiceData": props["ServiceData"]}, []string{}}}, base)
	peers, updates := tracker.candidates(objects, base)
	if len(peers) != 1 || len(updates) != 1 || updates[0] != 12 {
		t.Fatalf("live advertisement peers=%+v seen updates=%v", peers, updates)
	}
	peers, updates = tracker.candidates(objects, base.Add(time.Second))
	if len(peers) != 1 || len(updates) != 0 {
		t.Fatalf("cache poll refreshed radio selection: peers=%+v seen updates=%v", peers, updates)
	}
	check(base.Add(advertisementFreshnessTTL-time.Nanosecond), 1)
	check(base.Add(advertisementFreshnessTTL), 0)
	// Polling the unchanged BlueZ cache must not extend the expiry.
	check(base.Add(2*advertisementFreshnessTTL), 0)
}

func TestAdvertisementFreshnessTracksLiveMeshAmongOtherAdvertisements(t *testing.T) {
	uuid, _ := ServiceUUID(64)
	adv, _ := NewAdvertisement(12, "field", DefaultPSM)
	raw, _ := adv.MarshalBinary()
	path := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_01")
	props := meshDeviceProps(uuid, raw)
	objects := managedObjects{path: {deviceInterface: props}}
	tracker := newAdvertisementFreshness("/org/bluez/hci0", uuid, "field", 20)
	base := time.Unix(1_000, 0)
	changed := func(p dbus.ObjectPath, pmap map[string]dbus.Variant) {
		tracker.handle(&dbus.Signal{Name: "org.freedesktop.DBus.Properties.PropertiesChanged", Path: p, Body: []any{deviceInterface, map[string]dbus.Variant{"ServiceData": pmap["ServiceData"]}, []string{}}}, base)
	}
	tracker.handle(&dbus.Signal{Name: "org.freedesktop.DBus.ObjectManager.InterfacesAdded", Body: []any{path, map[string]map[string]dbus.Variant{deviceInterface: props}}}, base)
	if got, _ := tracker.candidates(objects, base); len(got) != 0 {
		t.Fatalf("cached Device1 creation accepted as live advertisement: %+v", got)
	}
	changed("/org/bluez/hci1/dev_AA", props)
	if got, _ := tracker.candidates(objects, base); len(got) != 0 {
		t.Fatalf("other adapter accepted: %+v", got)
	}
	changed(path, props)
	if got, _ := tracker.candidates(objects, base); len(got) != 1 {
		t.Fatalf("live advertisement rejected: %+v", got)
	}
	// Only the live signal changes the peer identity. A cached value from
	// another advertisement cannot replace that signal's asset.
	other, _ := NewAdvertisement(13, "field", DefaultPSM)
	otherRaw, _ := other.MarshalBinary()
	objects[path][deviceInterface] = meshDeviceProps(uuid, otherRaw)
	if got, _ := tracker.candidates(objects, base); len(got) != 1 || got[0].asset != 12 {
		t.Fatalf("cached payload replaced live identity: %+v", got)
	}
	changed(path, meshDeviceProps(uuid, otherRaw))
	if got, _ := tracker.candidates(objects, base); len(got) != 1 || got[0].asset != 13 {
		t.Fatalf("new live identity rejected: %+v", got)
	}
	legacy := dbus.MakeVariant(map[string]dbus.Variant{"0000180f-0000-1000-8000-00805f9b34fb": dbus.MakeVariant([]byte{1})})
	objects[path][deviceInterface]["ServiceData"] = legacy
	tracker.handle(&dbus.Signal{Name: "org.freedesktop.DBus.Properties.PropertiesChanged", Path: path, Body: []any{deviceInterface, map[string]dbus.Variant{"ServiceData": legacy}, []string{}}}, base)
	tracker.handle(&dbus.Signal{Name: "org.freedesktop.DBus.Properties.PropertiesChanged", Path: path, Body: []any{deviceInterface, map[string]dbus.Variant{}, []string{"ServiceData"}}}, base)
	if got, _ := tracker.candidates(objects, base); len(got) != 1 || got[0].asset != 13 {
		t.Fatalf("concurrent legacy advertisement hid recent mesh advert: %+v", got)
	}
	if got, _ := tracker.candidates(objects, base.Add(advertisementFreshnessTTL)); len(got) != 0 {
		t.Fatalf("mesh advert did not expire despite legacy advertisements: %+v", got)
	}
	changed(path, props)
	wrongMesh, _ := NewAdvertisement(12, "other-mesh", DefaultPSM)
	wrongRaw, _ := wrongMesh.MarshalBinary()
	changed(path, meshDeviceProps(uuid, wrongRaw))
	if got, _ := tracker.candidates(objects, base); len(got) != 0 {
		t.Fatalf("same UUID with wrong mesh retained previous hint: %+v", got)
	}
	changed(path, props)
	tracker.handle(&dbus.Signal{Name: "org.freedesktop.DBus.ObjectManager.InterfacesRemoved", Body: []any{path, []string{deviceInterface}}}, base)
	if got, _ := tracker.candidates(objects, base); len(got) != 0 {
		t.Fatalf("removed Device1 accepted: %+v", got)
	}
}

func TestStaleBlueZCacheCannotRefreshRadioSelection(t *testing.T) {
	uuid, _ := ServiceUUID(64)
	adv, _ := NewAdvertisement(12, "field", DefaultPSM)
	raw, _ := adv.MarshalBinary()
	path := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_01")
	props := meshDeviceProps(uuid, raw)
	objects := managedObjects{path: {deviceInterface: props}}
	selection := localmesh.NewPeerSelection(func() localmesh.NodeSnapshot {
		return localmesh.NodeSnapshot{Links: []localmesh.PeerLink{{Asset: 15, Cost: 512}}}
	})
	r := &runtime{cfg: Config{MeshName: "field", Credentials: &localmesh.Credentials{Asset: 20}, Selection: selection}, uuid: uuid, freshness: newAdvertisementFreshness("/org/bluez/hci0", uuid, "field", 20)}
	base := time.Now()
	if got := r.discoveredCandidates(objects, base); len(got) != 0 {
		t.Fatalf("cached peer before live advertisement: %+v", got)
	}
	if !selection.AllowRadio(15, localmesh.RadioBLE) {
		t.Fatal("stale cached BLE peer suppressed an otherwise useful second radio")
	}
	r.freshness.observe(path, map[string]dbus.Variant{"ServiceData": props["ServiceData"]}, base)
	if got := r.discoveredCandidates(objects, base); len(got) != 1 {
		t.Fatalf("fresh BLE peer rejected: %+v", got)
	}
	if selection.AllowRadio(15, localmesh.RadioBLE) {
		t.Fatal("live diverse BLE peer did not influence radio selection")
	}
	if got := r.discoveredCandidates(objects, base.Add(advertisementFreshnessTTL)); len(got) != 0 {
		t.Fatalf("expired BLE peer retained: %+v", got)
	}
}

func TestAdvertisementSignalsAndScansConcurrent(t *testing.T) {
	uuid, _ := ServiceUUID(64)
	adv, _ := NewAdvertisement(12, "field", DefaultPSM)
	raw, _ := adv.MarshalBinary()
	path := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_01")
	props := meshDeviceProps(uuid, raw)
	objects := managedObjects{path: {deviceInterface: props}}
	tracker := newAdvertisementFreshness("/org/bluez/hci0", uuid, "field", 20)
	at := time.Unix(1_000, 0)
	signal := &dbus.Signal{Name: "org.freedesktop.DBus.Properties.PropertiesChanged", Path: path, Body: []any{deviceInterface, map[string]dbus.Variant{"ServiceData": props["ServiceData"]}, []string{}}}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			tracker.handle(signal, at)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			tracker.candidates(objects, at)
		}
	}()
	wg.Wait()
	if got, _ := tracker.candidates(objects, at); len(got) != 1 || got[0].asset != 12 {
		t.Fatalf("concurrent advertisement lost: %+v", got)
	}
}
