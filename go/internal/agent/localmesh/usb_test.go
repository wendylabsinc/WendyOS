//go:build linux || darwin

package localmesh

import (
	"net"
	"testing"
)

func TestUSBCarrierRequiresGadgetInterfaceAndWellKnownAddress(t *testing.T) {
	for _, tc := range []struct {
		name, address string
		want          bool
	}{
		{"usb0", "fe80::5741:1/64", true}, {"usb1", "fe80::5741:1/64", true},
		{"wlan0", "fe80::5741:1/64", false}, {"veth0", "fe80::5741:1/64", false},
		{"usb0", "fe80::1234/64", false}, {"usb0", "192.168.55.1/24", false},
	} {
		ip, subnet, err := net.ParseCIDR(tc.address)
		if err != nil {
			t.Fatal(err)
		}
		subnet.IP = ip
		got, ok := usbLANInterface(net.Interface{Name: tc.name}, subnet)
		if ok != tc.want {
			t.Fatalf("%s %s: got %v", tc.name, tc.address, ok)
		}
		if ok && (got.transport != "usb" || got.cost != EthernetLinkCost || !got.ip.Equal(ip)) {
			t.Fatalf("incorrect USB carrier: %+v", got)
		}
	}
}
