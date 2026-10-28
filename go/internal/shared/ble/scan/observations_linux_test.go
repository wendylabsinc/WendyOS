//go:build linux

package scan

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/wendylabsinc/wendy/go/internal/shared/ble/bluez"
)

func radioSignal(path string, changed map[string]dbus.Variant, invalidated ...string) *dbus.Signal {
	return &dbus.Signal{Path: dbus.ObjectPath(path), Name: bluez.PropsChangedName, Body: []any{bluez.DeviceIface, changed, invalidated}}
}
func rawService(data []byte) dbus.Variant {
	return dbus.MakeVariant(map[string]dbus.Variant{"1234": dbus.MakeVariant(data)})
}

const adapter = "/org/bluez/hci0"
const radioPath = adapter + "/dev_01_02_03_04_05_06"

func TestLinuxObservationsOnlyRealRadioFieldsRefresh(t *testing.T) {
	var observations linuxObservations
	now := time.Unix(1000, 0)
	// RSSI and name from cached property changes do not prove an advertisement.
	observations.observe(adapter, radioSignal(radioPath, map[string]dbus.Variant{"Name": dbus.MakeVariant("cached name"), "RSSI": dbus.MakeVariant(int16(-40))}), now)
	if len(observations.snapshot(8)) != 0 {
		t.Fatal("cached properties created a live radio")
	}
	payload := []byte{0, 255, 8, 0}
	signal := radioSignal(radioPath, map[string]dbus.Variant{"ServiceData": rawService(payload), "RSSI": dbus.MakeVariant(int16(-45))})
	observations.observe(adapter, signal, now)
	payload[0] = 99
	rows := observations.snapshot(8)
	key := CanonicalUUID("1234")
	if len(rows) != 1 || rows[0].ServiceData[key][0] != 0 || rows[0].Address != "01:02:03:04:05:06" || rows[0].RSSI != -45 {
		t.Fatalf("incorrect raw observation: %+v", rows)
	}
	rows[0].ServiceData[key][0] = 77
	if observations.snapshot(8)[0].ServiceData[key][0] != 0 {
		t.Fatal("snapshot aliases stored bytes")
	}
	observations.observe(adapter, radioSignal(radioPath, map[string]dbus.Variant{"RSSI": dbus.MakeVariant(int16(-30))}), now.Add(time.Second))
	observations.observe(adapter, radioSignal(radioPath, map[string]dbus.Variant{"ManufacturerData": dbus.MakeVariant(map[uint16]dbus.Variant{1: dbus.MakeVariant([]byte{1})})}), now.Add(2*time.Second))
	row := observations.snapshot(8)[0]
	if row.LastSeen != now.Add(2*time.Second) || row.ServiceDataSeen != now {
		t.Fatal("unrelated advertisement refreshed service payload")
	}
	// DuplicateData produces repeated identical ServiceData: that genuinely is fresh.
	observations.observe(adapter, radioSignal(radioPath, map[string]dbus.Variant{"ServiceData": rawService([]byte{0, 255, 8, 0})}), now.Add(3*time.Second))
	if observations.snapshot(8)[0].ServiceDataSeen != now.Add(3*time.Second) {
		t.Fatal("identical advertisement did not refresh")
	}
	observations.observe(adapter, radioSignal(radioPath, nil, "ServiceData"), now.Add(4*time.Second))
	if len(observations.snapshot(8)[0].ServiceData) != 0 {
		t.Fatal("invalidated service payload retained")
	}
}
func TestLinuxObservationsBoundAndFilter(t *testing.T) {
	var observations linuxObservations
	now := time.Unix(1000, 0)
	changes := map[string]dbus.Variant{"ServiceData": rawService([]byte{1})}
	observations.observe(adapter, radioSignal("/org/bluez/hci1/dev_01_02_03_04_05_06", changes), now)
	observations.observe(adapter, radioSignal(adapter+"/dev_bad", changes), now)
	if len(observations.snapshot(8)) != 0 {
		t.Fatal("accepted another adapter or malformed path")
	}
	for i := 0; i < 1100; i++ {
		path := fmt.Sprintf("%s/dev_01_02_03_04_%02X_%02X", adapter, i/256, i%256)
		observations.observe(adapter, radioSignal(path, changes), now.Add(time.Duration(i)*time.Second))
	}
	if len(observations.byPath) != linuxObservationLimit {
		t.Fatal("observation cache is not bounded")
	}
	rows := observations.snapshot(3)
	if len(rows) != 3 || rows[0].LastSeen != now.Add(1099*time.Second) {
		t.Fatal("snapshot did not retain newest observations")
	}
}
func TestLinuxBoundedSnapshotNeverReadsBlueZCachedRoster(t *testing.T) {
	// No D-Bus connection exists. This fixture must use only observed events.
	scanner := &linuxScanner{limit: 8}
	rows, err := scanner.Snapshot(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatalf("unexpected fresh cached devices: %v %v", rows, err)
	}
	now := time.Now()
	scanner.observations.observe(adapter, radioSignal(radioPath, map[string]dbus.Variant{"ServiceData": rawService([]byte{0, 255})}), now)
	rows, err = scanner.Snapshot(context.Background())
	if err != nil || len(rows) != 1 || rows[0].ServiceDataSeen != now {
		t.Fatalf("missing actual event: %v %v", rows, err)
	}
}

func TestLinuxAdvertisementHandlerStopsWithoutDeferredQueue(t *testing.T) {
	var observations linuxObservations
	handler := &linuxAdvertisementHandler{observations: &observations}
	handler.setAdapter(adapter)
	handler.DeliverSignal("", "", radioSignal(radioPath, map[string]dbus.Variant{"ServiceData": rawService([]byte{0, 255})}))
	before := observations.snapshot(8)
	if len(before) != 1 {
		t.Fatal("handler did not ingest observation")
	}
	handler.Terminate()
	handler.DeliverSignal("", "", radioSignal(radioPath, map[string]dbus.Variant{"ServiceData": rawService([]byte{1})}))
	if after := observations.snapshot(8); after[0].ServiceDataSeen != before[0].ServiceDataSeen {
		t.Fatal("terminated handler kept ingesting")
	}
}
