//go:build linux

package bleprovider

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestAdvertisementFreshnessBoundsChurnAndExpiresBeforeAdmission(t *testing.T) {
	serviceUUID := ServiceUUID(testTenant)
	adv, _ := NewAdvertisement(lowDevice, "field", DefaultPSM)
	raw, _ := adv.MarshalBinary()
	tracker := newAdvertisementFreshness("/org/bluez/hci0", serviceUUID, "field", selfFixture)
	base := time.Unix(1_000, 0)
	for i := range 4096 {
		path := dbus.ObjectPath(fmt.Sprintf("/org/bluez/hci0/dev_%012x", i))
		tracker.observe(path, meshDeviceProps(serviceUUID, raw), base)
	}
	if len(tracker.seen) > 256 {
		t.Fatalf("discovery history retained %d radio addresses", len(tracker.seen))
	}
	path := dbus.ObjectPath("/org/bluez/hci0/dev_NEW")
	tracker.observe(path, meshDeviceProps(serviceUUID, raw), base.Add(advertisementFreshnessTTL))
	if _, ok := tracker.seen[path]; !ok || len(tracker.seen) != 1 {
		t.Fatalf("expired entries prevented fresh admission or remained retained: %d", len(tracker.seen))
	}
}

func meshDeviceProps(uuid string, raw []byte) map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"Address": dbus.MakeVariant("AA:BB:CC:DD:EE:01"), "AddressType": dbus.MakeVariant("public"),
		"ServiceData": dbus.MakeVariant(map[string]dbus.Variant{uuid: dbus.MakeVariant(raw)}),
	}
}

func TestFreshAdvertisementHonorsCurrentBlueZBlockPolicy(t *testing.T) {
	serviceUUID := ServiceUUID(testTenant)
	adv, _ := NewAdvertisement(lowDevice, "field", DefaultPSM)
	raw, _ := adv.MarshalBinary()
	path := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_01")
	props := meshDeviceProps(serviceUUID, raw)
	props["Blocked"] = dbus.MakeVariant(false)
	objects := managedObjects{path: {deviceInterface: props}}
	tracker := newAdvertisementFreshness("/org/bluez/hci0", serviceUUID, "field", selfFixture)
	base := time.Unix(1_000, 0)
	tracker.observe(path, props, base)
	if peers, _ := tracker.candidates(objects, base); len(peers) != 1 {
		t.Fatal("unblocked live advertisement was not eligible")
	}
	// A later local policy change overrides the still-fresh radio signal.
	// Advertisements can keep arriving from a blocked device.
	props["Blocked"] = dbus.MakeVariant(true)
	tracker.observe(path, meshDeviceProps(serviceUUID, raw), base.Add(time.Second))
	if peers, updates := tracker.candidates(objects, base.Add(time.Second)); len(peers) != 0 || len(updates) != 0 {
		t.Fatalf("blocked radio consumed discovery/dial capacity: peers=%v updates=%v", peers, updates)
	}
	props["Blocked"] = dbus.MakeVariant(false)
	if peers, _ := tracker.candidates(objects, base.Add(2*time.Second)); len(peers) != 1 {
		t.Fatal("unblocking discarded a recent valid advertisement")
	}
	props["Blocked"] = dbus.MakeVariant("false")
	if peers, _ := tracker.candidates(objects, base.Add(2*time.Second)); len(peers) != 0 {
		t.Fatal("malformed local policy admitted a radio")
	}
}

func TestBlueZCachedServiceDataNeedsLiveAdvertisement(t *testing.T) {
	serviceUUID := ServiceUUID(testTenant)
	adv, _ := NewAdvertisement(lowDevice, "field", DefaultPSM)
	raw, _ := adv.MarshalBinary()
	path := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_01")
	props := meshDeviceProps(serviceUUID, raw)
	objects := managedObjects{path: {deviceInterface: props}}
	tracker := newAdvertisementFreshness("/org/bluez/hci0", serviceUUID, "field", selfFixture)
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
	if len(peers) != 1 || len(updates) != 1 || updates[0] != path {
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
	serviceUUID := ServiceUUID(testTenant)
	adv, _ := NewAdvertisement(lowDevice, "field", DefaultPSM)
	raw, _ := adv.MarshalBinary()
	path := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_01")
	props := meshDeviceProps(serviceUUID, raw)
	objects := managedObjects{path: {deviceInterface: props}}
	tracker := newAdvertisementFreshness("/org/bluez/hci0", serviceUUID, "field", selfFixture)
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
	// another advertisement cannot replace that signal's device.
	low2 := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	other, _ := NewAdvertisement(low2, "field", DefaultPSM)
	otherRaw, _ := other.MarshalBinary()
	objects[path][deviceInterface] = meshDeviceProps(serviceUUID, otherRaw)
	if got, _ := tracker.candidates(objects, base); len(got) != 1 || got[0].hint != deviceHint(lowDevice) {
		t.Fatalf("cached payload replaced live identity: %+v", got)
	}
	changed(path, meshDeviceProps(serviceUUID, otherRaw))
	if got, _ := tracker.candidates(objects, base); len(got) != 1 || got[0].hint != deviceHint(low2) {
		t.Fatalf("new live identity rejected: %+v", got)
	}
	legacy := dbus.MakeVariant(map[string]dbus.Variant{"0000180f-0000-1000-8000-00805f9b34fb": dbus.MakeVariant([]byte{1})})
	objects[path][deviceInterface]["ServiceData"] = legacy
	tracker.handle(&dbus.Signal{Name: "org.freedesktop.DBus.Properties.PropertiesChanged", Path: path, Body: []any{deviceInterface, map[string]dbus.Variant{"ServiceData": legacy}, []string{}}}, base)
	tracker.handle(&dbus.Signal{Name: "org.freedesktop.DBus.Properties.PropertiesChanged", Path: path, Body: []any{deviceInterface, map[string]dbus.Variant{}, []string{"ServiceData"}}}, base)
	if got, _ := tracker.candidates(objects, base); len(got) != 1 || got[0].hint != deviceHint(low2) {
		t.Fatalf("concurrent legacy advertisement hid recent mesh advert: %+v", got)
	}
	if got, _ := tracker.candidates(objects, base.Add(advertisementFreshnessTTL)); len(got) != 0 {
		t.Fatalf("mesh advert did not expire despite legacy advertisements: %+v", got)
	}
	changed(path, props)
	wrongMesh, _ := NewAdvertisement(lowDevice, "other-mesh", DefaultPSM)
	wrongRaw, _ := wrongMesh.MarshalBinary()
	changed(path, meshDeviceProps(serviceUUID, wrongRaw))
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
	uuid := ServiceUUID(testTenant)
	adv, _ := NewAdvertisement(lowDevice, "field", DefaultPSM)
	raw, _ := adv.MarshalBinary()
	path := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_01")
	props := meshDeviceProps(uuid, raw)
	objects := managedObjects{path: {deviceInterface: props}}
	selection := localmesh.NewPeerSelection(func() localmesh.NodeSnapshot {
		return localmesh.NodeSnapshot{Links: []localmesh.PeerLink{{Device: peerFixture, Cost: 512}}}
	})
	r := &runtime{cfg: Config{NetworkName: "field", Credentials: &localmesh.Credentials{Device: selfFixture}, Selection: selection}, uuid: uuid, freshness: newAdvertisementFreshness("/org/bluez/hci0", uuid, "field", selfFixture)}
	base := time.Now()
	if got := r.discoveredCandidates(objects, base); len(got) != 0 {
		t.Fatalf("cached peer before live advertisement: %+v", got)
	}
	if !r.discovery.lastMeshSignal.IsZero() {
		t.Fatal("cached service data refreshed discovery liveness")
	}
	if !selection.AllowRadio(peerFixture, localmesh.RadioBLE) {
		t.Fatal("stale cached BLE peer suppressed an otherwise useful second radio")
	}
	r.freshness.observe(path, map[string]dbus.Variant{"ServiceData": props["ServiceData"]}, base)
	if got := r.discoveredCandidates(objects, base); len(got) != 1 {
		t.Fatalf("fresh BLE peer rejected: %+v", got)
	}
	if !r.discovery.lastMeshSignal.Equal(base) {
		t.Fatal("live service data did not refresh discovery liveness")
	}
	if !selection.AllowRadio(peerFixture, localmesh.RadioBLE) {
		t.Fatal("unverified hint influenced identity-based radio selection")
	}
	if got := r.discoveredCandidates(objects, base.Add(advertisementFreshnessTTL)); len(got) != 0 {
		t.Fatalf("expired BLE peer retained: %+v", got)
	}
	if !r.discovery.lastMeshSignal.Equal(base) {
		t.Fatal("polling expired BlueZ cache refreshed discovery liveness")
	}
}

func TestAdvertisementSignalsAndScansConcurrent(t *testing.T) {
	serviceUUID := ServiceUUID(testTenant)
	adv, _ := NewAdvertisement(lowDevice, "field", DefaultPSM)
	raw, _ := adv.MarshalBinary()
	path := dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_01")
	props := meshDeviceProps(serviceUUID, raw)
	objects := managedObjects{path: {deviceInterface: props}}
	tracker := newAdvertisementFreshness("/org/bluez/hci0", serviceUUID, "field", selfFixture)
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
	if got, _ := tracker.candidates(objects, at); len(got) != 1 || got[0].hint != deviceHint(lowDevice) {
		t.Fatalf("concurrent advertisement lost: %+v", got)
	}
}
