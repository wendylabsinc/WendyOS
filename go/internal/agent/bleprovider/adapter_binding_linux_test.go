//go:build linux

package bleprovider

import (
	"context"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
)

type adapterBindingObjects struct{ objects managedObjects }

func (o *adapterBindingObjects) GetManagedObjects() (managedObjects, *dbus.Error) {
	return o.objects, nil
}

func TestSelectedAdapterSuppliesBothLocalSocketBindings(t *testing.T) {
	address, connect := privateAdvertisementBus(t)
	bluez, client := connect(), connect()
	objects := managedObjects{}
	for _, adapter := range []struct {
		path          dbus.ObjectPath
		address, kind string
	}{
		{"/org/bluez/hci0", "02:00:00:00:00:01", "public"},
		{"/org/bluez/hci1", "C2:11:22:33:44:55", "random"},
	} {
		objects[adapter.path] = map[string]map[string]dbus.Variant{adapterInterface: {}, advertManagerInterface: {}}
		if _, err := prop.Export(bluez, adapter.path, map[string]map[string]*prop.Prop{adapterInterface: {
			"Address": {Value: adapter.address}, "AddressType": {Value: adapter.kind},
			"Powered": {Value: false, Writable: true},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := bluez.Export(&adapterBindingObjects{objects: objects}, "/", "org.freedesktop.DBus.ObjectManager"); err != nil {
		t.Fatal(err)
	}
	if _, err := bluez.RequestName(bluezName, dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	selected, err := selectAdapter(ctx, client, "/org/bluez/hci1")
	if err != nil || selected != "/org/bluez/hci1" {
		t.Fatalf("selected %s: %v", selected, err)
	}
	local, err := adapterL2Address(ctx, client, selected, DefaultPSM)
	if err != nil {
		t.Fatal(err)
	}
	for _, psm := range []uint16{DefaultPSM, 0} {
		address, err := localL2SocketAddress(local, psm)
		if err != nil || address.Addr != ([6]byte{0xc2, 0x11, 0x22, 0x33, 0x44, 0x55}) || address.AddrType != btAddrLERandom || address.PSM != psm {
			t.Fatalf("socket PSM %d bound outside selected identity: %+v: %v", psm, address, err)
		}
	}
	for _, invalid := range []l2addr{{}, {Address: "00:00:00:00:00:00", Type: btAddrLEPublic, PSM: DefaultPSM}, {Address: local.Address, Type: 99, PSM: DefaultPSM}} {
		if _, err := localL2SocketAddress(invalid, 0); err == nil {
			t.Fatal("invalid selected identity silently fell back to any adapter")
		}
	}
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", address)
	leafLocal, err := leafAdapterAddress(ctx)
	if err != nil || leafLocal.Address != "02:00:00:00:00:01" || leafLocal.Type != btAddrLEPublic {
		t.Fatalf("leaf did not resolve the default adapter identity: %+v: %v", leafLocal, err)
	}
	var powered dbus.Variant
	if err := client.Object(bluezName, "/org/bluez/hci0").CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, adapterInterface, "Powered").Store(&powered); err != nil {
		t.Fatal(err)
	}
	if powered.Value() != true {
		t.Fatal("leaf-only setup left the selected controller powered off")
	}
}

func TestLeafAdapterPowerFailureIsSetupFailure(t *testing.T) {
	address, connect := privateAdvertisementBus(t)
	bluez := connect()
	adapter := dbus.ObjectPath("/org/bluez/hci0")
	objects := managedObjects{adapter: {adapterInterface: {}, advertManagerInterface: {}}}
	if _, err := prop.Export(bluez, adapter, map[string]map[string]*prop.Prop{adapterInterface: {
		"Address": {Value: "02:00:00:00:00:01"}, "AddressType": {Value: "public"},
		"Powered": {Value: false}, // BlueZ refuses changing this controller.
	}}); err != nil {
		t.Fatal(err)
	}
	if err := bluez.Export(&adapterBindingObjects{objects: objects}, "/", "org.freedesktop.DBus.ObjectManager"); err != nil {
		t.Fatal(err)
	}
	if _, err := bluez.RequestName(bluezName, dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", address)
	if _, err := leafAdapterAddress(context.Background()); err == nil {
		t.Fatal("leaf setup succeeded without powering the selected controller")
	}
}
