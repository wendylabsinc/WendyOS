//go:build linux

package bleprovider

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/google/uuid"
)

func adField(kind byte, data []byte) []byte {
	return append([]byte{byte(len(data) + 1), kind}, data...)
}
func wireUUID(id uuid.UUID) []byte {
	wire := make([]byte, 16)
	for i := range wire {
		wire[i] = id[15-i]
	}
	return wire
}
func nameFields(device uuid.UUID, mesh string) ([]byte, []byte) {
	a, _ := NewAdvertisement(device, mesh, DefaultPSM)
	name, _ := a.MarshalLocalName()
	return adField(7, wireUUID(testTenant.UUID())), adField(9, []byte(name))
}
func reportObjects(address string) (dbus.ObjectPath, managedObjects) {
	path := dbus.ObjectPath("/org/bluez/hci0/dev_" + address)
	mac := strings.ReplaceAll(address, "_", ":")
	return path, managedObjects{path: {deviceInterface: {"Address": dbus.MakeVariant(mac), "AddressType": dbus.MakeVariant("public")}}}
}

func TestLocalNameCorrelationBothOrdersAndCacheExpiry(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint(reverse), func(t *testing.T) {
			f := newAdvertisementFreshness("/org/bluez/hci0", ServiceUUID(testTenant), "field", selfFixture)
			path, objects := reportObjects("AA_BB_CC_DD_EE_01")
			u, n := nameFields(lowDevice, "field")
			first, second := u, n
			if reverse {
				first, second = n, u
			}
			base := time.Unix(1000, 0)
			objects[path][deviceInterface]["UUIDs"] = dbus.MakeVariant([]string{ServiceUUID(testTenant)})
			objects[path][deviceInterface]["Name"] = dbus.MakeVariant(string(n[2:]))
			if got, _ := f.candidates(objects, base); len(got) != 0 {
				t.Fatal("trusted cached name/UUIDs")
			}
			f.observeReport("AA:BB:CC:DD:EE:01", "public", first, base)
			if got, _ := f.candidates(objects, base); len(got) != 0 {
				t.Fatal("accepted incomplete observation")
			}
			f.observeReport("AA:BB:CC:DD:EE:01", "public", second, base.Add(time.Second))
			peers, updates := f.candidates(objects, base.Add(time.Second))
			if len(peers) != 1 || len(updates) != 1 || peers[0].hint != deviceHint(lowDevice) {
				t.Fatalf("peers=%+v updates=%v", peers, updates)
			}
			if _, updates = f.candidates(objects, base.Add(2*time.Second)); len(updates) != 0 {
				t.Fatal("cache poll refreshed selection")
			}
			// RSSI, UUIDs and Name on Device1 are not proof of a live mesh advert.
			f.observe(path, objects[path][deviceInterface], base.Add(14*time.Second))
			if got, _ := f.candidates(objects, base.Add(advertisementFreshnessTTL)); len(got) != 0 {
				t.Fatal("cache extended expiry")
			}
		})
	}
}

func TestLocalNameRequiresRecentBothParts(t *testing.T) {
	for _, repeatName := range []bool{false, true} {
		t.Run(fmt.Sprint(repeatName), func(t *testing.T) {
			f := newAdvertisementFreshness("/org/bluez/hci0", ServiceUUID(testTenant), "field", selfFixture)
			_, objects := reportObjects("AA_BB_CC_DD_EE_01")
			u, n := nameFields(lowDevice, "field")
			base := time.Unix(1000, 0)
			f.observeReport("AA:BB:CC:DD:EE:01", "public", append(bytes.Clone(u), n...), base)
			repeat := u
			if repeatName {
				repeat = n
			}
			for i := 1; i <= 20; i++ {
				f.observeReport("AA:BB:CC:DD:EE:01", "public", repeat, base.Add(time.Duration(i)*time.Second))
			}
			if got, _ := f.candidates(objects, base.Add(20*time.Second)); len(got) != 0 {
				t.Fatal("one repeated component renewed the other")
			}
			// Repeated complete observations, including unchanged names, remain live.
			for i := 21; i < 60; i++ {
				at := base.Add(time.Duration(i) * time.Second)
				f.observeReport("AA:BB:CC:DD:EE:01", "public", append(bytes.Clone(u), n...), at)
				if got, _ := f.candidates(objects, at); len(got) != 1 {
					t.Fatalf("lost repeated observation at %v", at)
				}
			}
		})
	}
}

func TestUnrelatedNamesCannotHideOrRefreshMesh(t *testing.T) {
	f := newAdvertisementFreshness("/org/bluez/hci0", ServiceUUID(testTenant), "field", selfFixture)
	_, objects := reportObjects("AA_BB_CC_DD_EE_01")
	u, n := nameFields(lowDevice, "field")
	at := time.Unix(1000, 0)
	f.observeReport("AA:BB:CC:DD:EE:01", "public", append(bytes.Clone(u), n...), at)
	for i := 1; i < 20; i++ {
		current := at.Add(time.Duration(i) * time.Second)
		f.observeReport("AA:BB:CC:DD:EE:01", "public", adField(9, []byte("other app")), current)
		f.observeReport("AA:BB:CC:DD:EE:01", "public", append(adField(7, wireUUID(otherTenant.UUID())), n...), current)
		got, _ := f.candidates(objects, current)
		if (len(got) == 1) != (i < 15) {
			t.Fatalf("unrelated announcement changed lifetime at %d: %+v", i, got)
		}
	}
}

func TestLocalNameRejectsWrongIdentityAndMalformedBinaryFallback(t *testing.T) {
	u, n := nameFields(lowDevice, "field")
	a, _ := NewAdvertisement(lowDevice, "field", DefaultPSM)
	binary, _ := a.MarshalBinary()
	wrong, _ := NewAdvertisement(lowDevice, "elsewhere", DefaultPSM)
	wrongName, _ := wrong.MarshalLocalName()
	versioned := bytes.Clone(binary)
	versioned[0] = 2
	unknownName, _ := encodeLocalName(versioned)
	cases := map[string][]byte{
		"wrong tenant":                      append(adField(7, wireUUID(otherTenant.UUID())), n...),
		"wrong mesh":                        append(bytes.Clone(u), adField(9, []byte(wrongName))...),
		"shortened name":                    append(bytes.Clone(u), adField(8, n[2:])...),
		"truncated name":                    append(bytes.Clone(u), adField(9, n[2:len(n)-1])...),
		"unknown version":                   append(bytes.Clone(u), adField(9, []byte(unknownName))...),
		"malformed service with valid name": append(append(bytes.Clone(u), n...), adField(0x21, append(wireUUID(testTenant.UUID()), 0))...),
		"legacy service with valid name":    append(append(bytes.Clone(u), n...), adField(0x21, append(wireUUID(testTenant.UUID()), make([]byte, 10)...))...),
	}
	for label, data := range cases {
		t.Run(label, func(t *testing.T) {
			f := newAdvertisementFreshness("/org/bluez/hci0", ServiceUUID(testTenant), "field", selfFixture)
			_, objects := reportObjects("AA_BB_CC_DD_EE_01")
			at := time.Unix(1000, 0)
			f.observeReport("AA:BB:CC:DD:EE:01", "public", data, at)
			if got, _ := f.candidates(objects, at); len(got) != 0 {
				t.Fatalf("accepted %+v", got)
			}
		})
	}
}

func TestLocalNameIndependentAddressesAndEpochs(t *testing.T) {
	f := newAdvertisementFreshness("/org/bluez/hci0", ServiceUUID(testTenant), "field", selfFixture)
	p1, objects := reportObjects("AA_BB_CC_DD_EE_01")
	p2, objects2 := reportObjects("AA_BB_CC_DD_EE_02")
	objects[p2] = objects2[p2]
	u, n := nameFields(lowDevice, "field")
	at := time.Unix(1000, 0)
	f.observeReport("AA:BB:CC:DD:EE:01", "public", u, at)
	f.observeReport("AA:BB:CC:DD:EE:02", "public", n, at)
	if got, _ := f.candidates(objects, at); len(got) != 0 {
		t.Fatal("cross-address correlation")
	}
	f.forget(p1)
	f.observeReport("AA:BB:CC:DD:EE:01", "public", n, at)
	if got, _ := f.candidates(objects, at); len(got) != 0 {
		t.Fatal("removed partial observation retained")
	}
	f.observeReport("AA:BB:CC:DD:EE:01", "public", u, at)
	f.observeReport("AA:BB:CC:DD:EE:02", "public", u, at)
	if got, _ := f.candidates(objects, at); len(got) != 2 {
		t.Fatal("equal prefixes collapsed")
	}
	f.clearReports()
	if got, _ := f.candidates(objects, at); len(got) != 0 {
		t.Fatal("source epoch retained text peers")
	}
	f.observeReport("AA:BB:CC:DD:EE:01", "public", n, at)
	if got, _ := f.candidates(objects, at); len(got) != 0 {
		t.Fatal("combined packets across epochs")
	}
}

func TestLocalNameBinaryPrecedenceAndWithdrawal(t *testing.T) {
	f := newAdvertisementFreshness("/org/bluez/hci0", ServiceUUID(testTenant), "field", selfFixture)
	_, objects := reportObjects("AA_BB_CC_DD_EE_01")
	u, n := nameFields(lowDevice, "field")
	at := time.Unix(1000, 0)
	f.observeReport("AA:BB:CC:DD:EE:01", "public", append(bytes.Clone(u), n...), at)
	a, _ := NewAdvertisement(uuid.MustParse("22222222-2222-4222-8222-222222222222"), "field", 0x83)
	raw, _ := a.MarshalBinary()
	f.observeReport("AA:BB:CC:DD:EE:01", "public", adField(0x21, append(wireUUID(testTenant.UUID()), raw...)), at.Add(time.Second))
	f.observeReport("AA:BB:CC:DD:EE:01", "public", append(bytes.Clone(u), n...), at.Add(2*time.Second))
	got, _ := f.candidates(objects, at.Add(2*time.Second))
	if len(got) != 1 || got[0].hint != a.DeviceHint || got[0].psm != 0x83 {
		t.Fatalf("text overrode binary: %+v", got)
	}
	f.clearReports()
	if got, _ := f.candidates(objects, at.Add(2*time.Second)); len(got) != 1 {
		t.Fatal("report recovery removed D-Bus-compatible binary observation")
	}
	f.observeReport("AA:BB:CC:DD:EE:01", "public", adField(0x21, append(wireUUID(testTenant.UUID()), 0)), at.Add(3*time.Second))
	if got, _ := f.candidates(objects, at.Add(3*time.Second)); len(got) != 0 {
		t.Fatal("malformed same-service payload retained old advert")
	}
}

func TestLocalNameObservationBoundsAndLateDeviceObject(t *testing.T) {
	f := newAdvertisementFreshness("/org/bluez/hci0", ServiceUUID(testTenant), "field", selfFixture)
	u, n := nameFields(lowDevice, "field")
	at := time.Unix(1000, 0)
	f.observeReport("AA:BB:CC:DD:EE:01", "public", append(bytes.Clone(u), n...), at)
	if got, _ := f.candidates(nil, at); len(got) != 0 {
		t.Fatal("candidate without Device1 address")
	}
	_, objects := reportObjects("AA_BB_CC_DD_EE_01")
	if got, _ := f.candidates(objects, at); len(got) != 1 {
		t.Fatal("late object lost live observation")
	}
	for i := 0; i < maxAdvertisementObservations*2; i++ {
		addr := fmt.Sprintf("00:00:00:00:%02X:%02X", i>>8, i&255)
		f.observeReport(addr, "public", u, at)
	}
	if len(f.partial) > maxAdvertisementObservations || len(f.seen) > maxAdvertisementObservations {
		t.Fatal("unbounded observations")
	}
	for i := 0; i < maxAdvertisementObservations*2; i++ {
		addr := fmt.Sprintf("00:00:00:00:%02X:%02X", i>>8, i&255)
		f.observeReport(addr, "public", append(bytes.Clone(u), n...), at.Add(10*time.Second))
	}
	if len(f.partial) > maxAdvertisementObservations || len(f.seen) > maxAdvertisementObservations {
		t.Fatal("unbounded complete observations")
	}
}

func TestAdvertisingDataStrictStructure(t *testing.T) {
	u, n := nameFields(lowDevice, "field")
	for _, data := range [][]byte{{4, 9, 'x'}, {0, 1}, adField(7, []byte{1}), adField(0x21, []byte{1}), append(bytes.Clone(n), n...)} {
		if _, err := parseAdvertisingData(data, ServiceUUID(testTenant)); err == nil {
			t.Fatalf("accepted malformed AD %x", data)
		}
	}
	fields, err := parseAdvertisingData(append(append(bytes.Clone(u), n...), 0, 0), ServiceUUID(testTenant))
	if err != nil || !fields.tenant || !fields.namePresent {
		t.Fatalf("valid padded AD: %+v %v", fields, err)
	}
	a, _ := NewAdvertisement(lowDevice, "field", DefaultPSM)
	raw, _ := a.MarshalBinary()
	service := adField(0x21, append(wireUUID(testTenant.UUID()), raw...))
	if _, err := parseAdvertisingData(append(bytes.Clone(service), service...), ServiceUUID(testTenant)); err == nil {
		t.Fatal("duplicate service entry accepted")
	}
}

func TestKernelMergedShortAndCompleteNames(t *testing.T) {
	u, n := nameFields(lowDevice, "field")
	short := adField(8, n[2:7])
	for _, data := range [][]byte{
		append(append(append(bytes.Clone(u), short...), 0, 0), n...),
		append(append(bytes.Clone(u), n...), short...),
	} {
		f := newAdvertisementFreshness("/org/bluez/hci0", ServiceUUID(testTenant), "field", selfFixture)
		_, objects := reportObjects("AA_BB_CC_DD_EE_01")
		at := time.Unix(1000, 0)
		f.observeReport("AA:BB:CC:DD:EE:01", "public", data, at)
		if got, _ := f.candidates(objects, at); len(got) != 1 {
			t.Fatal("kernel-merged short/complete name rejected")
		}
	}
	bad := append(adField(8, []byte("wrong")), n...)
	if _, err := parseAdvertisingData(bad, ServiceUUID(testTenant)); err == nil {
		t.Fatal("conflicting names accepted")
	}
	for _, reverse := range []bool{false, true} {
		f := newAdvertisementFreshness("/org/bluez/hci0", ServiceUUID(testTenant), "field", selfFixture)
		_, objects := reportObjects("AA_BB_CC_DD_EE_01")
		at := time.Unix(1000, 0)
		first, second := append(bytes.Clone(u), short...), n
		if reverse {
			first, second = second, first
		}
		f.observeReport("AA:BB:CC:DD:EE:01", "public", first, at)
		f.observeReport("AA:BB:CC:DD:EE:01", "public", second, at.Add(time.Second))
		if got, _ := f.candidates(objects, at.Add(time.Second)); len(got) != 1 {
			t.Fatal("separate primary shortened name prevented complete scan response")
		}
	}
}

func TestLocalNameAddressTypeAndCurrentAddressBinding(t *testing.T) {
	f := newAdvertisementFreshness("/org/bluez/hci0", ServiceUUID(testTenant), "field", selfFixture)
	path, objects := reportObjects("AA_BB_CC_DD_EE_01")
	u, n := nameFields(lowDevice, "field")
	at := time.Unix(1000, 0)
	f.observeReport("AA:BB:CC:DD:EE:01", "public", u, at)
	f.observeReport("AA:BB:CC:DD:EE:01", "random", n, at)
	if got, _ := f.candidates(objects, at); len(got) != 0 {
		t.Fatal("combined public and random reports")
	}
	f.observeReport("AA:BB:CC:DD:EE:01", "public", n, at)
	if got, _ := f.candidates(objects, at); len(got) != 1 {
		t.Fatal("valid pair missing")
	}
	objects[path][deviceInterface]["Address"] = dbus.MakeVariant("AA:BB:CC:DD:EE:99")
	if got, _ := f.candidates(objects, at); len(got) != 0 {
		t.Fatal("live report used a different cached address")
	}
	objects[path][deviceInterface]["Address"] = dbus.MakeVariant("AA:BB:CC:DD:EE:01")
	objects[path][deviceInterface]["AddressType"] = dbus.MakeVariant("random")
	if got, _ := f.candidates(objects, at); len(got) != 0 {
		t.Fatal("live report used a different cached address type")
	}
}

func TestLocalNameStalePairAndConcurrentReports(t *testing.T) {
	f := newAdvertisementFreshness("/org/bluez/hci0", ServiceUUID(testTenant), "field", selfFixture)
	_, objects := reportObjects("AA_BB_CC_DD_EE_01")
	u, n := nameFields(lowDevice, "field")
	at := time.Unix(1000, 0)
	f.observeReport("AA:BB:CC:DD:EE:01", "public", u, at)
	f.observeReport("AA:BB:CC:DD:EE:01", "public", n, at.Add(scanResponseCorrelationTTL))
	if got, _ := f.candidates(objects, at.Add(scanResponseCorrelationTTL)); len(got) != 0 {
		t.Fatal("expired correlation accepted")
	}
	f.clearReports()
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		for range 1000 {
			f.observeReport("AA:BB:CC:DD:EE:01", "public", append(bytes.Clone(u), n...), at)
		}
	}()
	go func() {
		defer wg.Done()
		for range 1000 {
			f.candidates(objects, at)
		}
	}()
	go func() {
		defer wg.Done()
		for range 1000 {
			f.clearReports()
		}
	}()
	wg.Wait()
	f.observeReport("AA:BB:CC:DD:EE:01", "public", append(bytes.Clone(u), n...), at)
	if got, _ := f.candidates(objects, at); len(got) != 1 {
		t.Fatal("concurrent tracker lost final report")
	}
}

func TestBlueZIneligibleBinaryClearsTextCorrelation(t *testing.T) {
	f := newAdvertisementFreshness("/org/bluez/hci0", ServiceUUID(testTenant), "field", selfFixture)
	path, objects := reportObjects("AA_BB_CC_DD_EE_01")
	tenant, name := nameFields(lowDevice, "field")
	at := time.Unix(1000, 0)
	f.observeReport("AA:BB:CC:DD:EE:01", "public", append(bytes.Clone(tenant), name...), at)
	if peers, _ := f.candidates(objects, at); len(peers) != 1 {
		t.Fatal("missing initial text candidate")
	}
	// The newer binary payload is for another mesh. A later name alone must not
	// join the tenant component from before that binary announcement.
	adv, _ := NewAdvertisement(peerDevice, "other-field", DefaultPSM)
	raw, _ := adv.MarshalBinary()
	f.observe(path, map[string]dbus.Variant{"ServiceData": dbus.MakeVariant(map[string]dbus.Variant{ServiceUUID(testTenant): dbus.MakeVariant(raw)})}, at.Add(time.Second))
	f.observeReport("AA:BB:CC:DD:EE:01", "public", name, at.Add(2*time.Second))
	if peers, _ := f.candidates(objects, at.Add(2*time.Second)); len(peers) != 0 {
		t.Fatalf("old text tenant component survived binary withdrawal: %+v", peers)
	}
	f.observeReport("AA:BB:CC:DD:EE:01", "public", tenant, at.Add(2500*time.Millisecond))
	if peers, _ := f.candidates(objects, at.Add(2500*time.Millisecond)); len(peers) != 1 {
		t.Fatal("fresh text pair did not recover")
	}
}
