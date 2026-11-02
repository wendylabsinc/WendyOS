//go:build linux

package bluetooth

import (
	"github.com/godbus/dbus/v5"
	"testing"
)

func TestRadioStatusSelectsAdvertisingCapableAdapter(t *testing.T) {
	t.Setenv("WENDY_BT_ADAPTER", "")
	objects := managedObjects{
		"/org/bluez/hci0": {adapterIface: {}},
		"/org/bluez/hci1": {adapterIface: {}, advManagerIface: {
			"SupportedFeatures":          dbus.MakeVariant([]string{"CanSetTxPower"}),
			"SupportedCapabilities":      dbus.MakeVariant(map[string]dbus.Variant{"MinTxPower": dbus.MakeVariant(int16(-40)), "MaxTxPower": dbus.MakeVariant(int16(8))}),
			"SupportedSecondaryChannels": dbus.MakeVariant([]string{"Coded"}),
		}},
	}
	s, err := bluetoothRadioStatus(objects)
	if err != nil || s.Adapter != "/org/bluez/hci1" || !s.Available || !s.AdvertisingSetSupported || s.GetAdvertisingMaxDbm() != 8 {
		t.Fatal(s, err)
	}
	if len(s.SupportedMeshModes) != 1 || s.SupportedMeshModes[0] != "normal" || s.CodedUnavailableReason == "" {
		t.Fatal("overclaimed coded support", s)
	}
	t.Setenv("WENDY_BT_ADAPTER", "/org/bluez/hci0")
	s, err = bluetoothRadioStatus(objects)
	if err != nil || s.Available || len(s.SupportedMeshModes) != 0 || s.Error == "" {
		t.Fatal("non-advertising adapter accepted", s, err)
	}
}
func TestRadioStatusRejectsMalformedPowerCapabilities(t *testing.T) {
	t.Setenv("WENDY_BT_ADAPTER", "")
	s, err := bluetoothRadioStatus(managedObjects{"/org/bluez/hci0": {adapterIface: {}, advManagerIface: {
		"SupportedFeatures":     dbus.MakeVariant([]string{"CanSetTxPower"}),
		"SupportedCapabilities": dbus.MakeVariant(map[string]dbus.Variant{"MinTxPower": dbus.MakeVariant(int16(10)), "MaxTxPower": dbus.MakeVariant(int16(8))}),
	}}})
	if err != nil || s.AdvertisingSetSupported || s.AdvertisingMinDbm != nil || s.AdvertisingMaxDbm != nil {
		t.Fatal(s, err)
	}
}
