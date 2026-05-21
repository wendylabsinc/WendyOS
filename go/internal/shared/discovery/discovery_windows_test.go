//go:build windows

package discovery

import (
	"net"
	"testing"

	"github.com/hashicorp/mdns"
	"github.com/wendylabsinc/wendy/internal/shared/models"
)

func TestLANDeviceFromMDNSEntryPrefersIPv4AndParsesTXT(t *testing.T) {
	entry := &mdns.ServiceEntry{
		Name:       "wendyos-prudent-lark._wendyos._udp.local.",
		Host:       "wendyos-prudent-lark.local.",
		AddrV4:     net.ParseIP("169.254.249.48"),
		AddrV6:     net.ParseIP("fe80::576f:1b86:d80b:a8b9"),
		Port:       50051,
		InfoFields: []string{"id=agent-id", "displayname=Prudent Lark", "tls=true"},
	}

	dev, ok := lanDeviceFromMDNSEntry(entry, nil, windowsNetworkAdapterLookup{})
	if !ok {
		t.Fatal("lanDeviceFromMDNSEntry returned false")
	}
	if dev.ID != "agent-id" {
		t.Fatalf("ID = %q, want %q", dev.ID, "agent-id")
	}
	if dev.DisplayName != "Prudent Lark" {
		t.Fatalf("DisplayName = %q, want %q", dev.DisplayName, "Prudent Lark")
	}
	if dev.Hostname != "wendyos-prudent-lark.local" {
		t.Fatalf("Hostname = %q, want %q", dev.Hostname, "wendyos-prudent-lark.local")
	}
	if dev.IPAddress != "169.254.249.48" {
		t.Fatalf("IPAddress = %q, want %q", dev.IPAddress, "169.254.249.48")
	}
	if dev.Port != 50051 {
		t.Fatalf("Port = %d, want %d", dev.Port, 50051)
	}
	if !dev.IsMTLS {
		t.Fatal("IsMTLS = false, want true")
	}
	if dev.InterfaceType != string(models.InterfaceLAN) {
		t.Fatalf("InterfaceType = %q, want %q", dev.InterfaceType, models.InterfaceLAN)
	}
	if !dev.IsWendyDevice {
		t.Fatal("IsWendyDevice = false, want true")
	}
}

func TestLANDeviceFromMDNSEntryUsesWendyOSDeviceID(t *testing.T) {
	entry := &mdns.ServiceEntry{
		Name:       "wendyos-prudent-lark._wendyos._udp.local.",
		Host:       "wendyos-prudent-lark.local.",
		AddrV4:     net.ParseIP("169.254.249.48"),
		Port:       50051,
		InfoFields: []string{"id=display-id", "wendyosdevice=device-id"},
	}

	dev, ok := lanDeviceFromMDNSEntry(entry, nil, windowsNetworkAdapterLookup{})
	if !ok {
		t.Fatal("lanDeviceFromMDNSEntry returned false")
	}
	if dev.ID != "device-id" {
		t.Fatalf("ID = %q, want %q", dev.ID, "device-id")
	}
}

func TestLANDeviceFromMDNSEntryAddsIPv6LinkLocalZone(t *testing.T) {
	iface := &net.Interface{Name: "Ethernet"}
	entry := &mdns.ServiceEntry{
		Name:   "wendyos-prudent-lark._wendyos._udp.local.",
		Host:   "wendyos-prudent-lark.local.",
		AddrV6: net.ParseIP("fe80::576f:1b86:d80b:a8b9"),
		Port:   50051,
	}

	dev, ok := lanDeviceFromMDNSEntry(entry, iface, windowsNetworkAdapterLookup{})
	if !ok {
		t.Fatal("lanDeviceFromMDNSEntry returned false")
	}
	want := "fe80::576f:1b86:d80b:a8b9%Ethernet"
	if dev.IPAddress != want {
		t.Fatalf("IPAddress = %q, want %q", dev.IPAddress, want)
	}
}

func TestLANDeviceFromMDNSEntryDoesNotZoneGlobalIPv6(t *testing.T) {
	iface := &net.Interface{Name: "Ethernet"}
	entry := &mdns.ServiceEntry{
		Name:   "wendyos-prudent-lark._wendyos._udp.local.",
		Host:   "wendyos-prudent-lark.local.",
		AddrV6: net.ParseIP("2001:db8::1"),
		Port:   50051,
	}

	dev, ok := lanDeviceFromMDNSEntry(entry, iface, windowsNetworkAdapterLookup{})
	if !ok {
		t.Fatal("lanDeviceFromMDNSEntry returned false")
	}
	if dev.IPAddress != "2001:db8::1" {
		t.Fatalf("IPAddress = %q, want %q", dev.IPAddress, "2001:db8::1")
	}
}

func TestLANDeviceFromMDNSEntryFiltersWrongService(t *testing.T) {
	entry := &mdns.ServiceEntry{
		Name: "phone._remotepairing._tcp.local.",
		Host: "phone.local.",
		Port: 1234,
	}

	_, ok := lanDeviceFromMDNSEntry(entry, nil, windowsNetworkAdapterLookup{})
	if ok {
		t.Fatal("lanDeviceFromMDNSEntry returned true for wrong service")
	}
}

func TestDeduplicateLANDevicesPrefersIPv4(t *testing.T) {
	devices := []models.LANDevice{
		{
			ID:          "device-id",
			DisplayName: "Prudent Lark",
			Hostname:    "wendyos-prudent-lark.local",
			IPAddress:   "fe80::576f:1b86:d80b:a8b9%Ethernet",
			Port:        50051,
		},
		{
			ID:          "device-id",
			DisplayName: "Prudent Lark",
			Hostname:    "wendyos-prudent-lark.local",
			IPAddress:   "169.254.249.48",
			Port:        50051,
		},
	}

	got := deduplicateLANDevices(devices)
	if len(got) != 1 {
		t.Fatalf("deduplicateLANDevices returned %d devices, want 1", len(got))
	}
	if got[0].IPAddress != "169.254.249.48" {
		t.Fatalf("IPAddress = %q, want %q", got[0].IPAddress, "169.254.249.48")
	}
}

func TestParseNetAdapterJSONFiltersWendyByName(t *testing.T) {
	// Single entry: PowerShell emits an object, not an array, in this case.
	in := `{"Name":"Wendy USB Ethernet","InterfaceDescription":"Realtek USB GbE","MacAddress":"00-11-22-33-44-55","LinkSpeed":"1 Gbps","IPAddress":"169.254.1.5"}`

	got := parseNetAdapterJSON(in)
	if len(got) != 1 {
		t.Fatalf("got %d devices, want 1", len(got))
	}
	want := models.EthernetInterface{
		Name:          "Wendy USB Ethernet",
		DisplayName:   "Realtek USB GbE",
		MACAddress:    "00-11-22-33-44-55",
		IPAddress:     "169.254.1.5",
		LinkSpeed:     "1 Gbps",
		IsWendyDevice: true,
	}
	if got[0] != want {
		t.Fatalf("got %#v, want %#v", got[0], want)
	}
}

func TestLANDeviceFromMDNSEntryUsesWindowsAdapterMetadataForUSB(t *testing.T) {
	iface := &net.Interface{Index: 12, Name: "Ethernet 3"}
	entry := &mdns.ServiceEntry{
		Name:       "wendyos-prudent-lark._wendyos._udp.local.",
		Host:       "wendyos-prudent-lark.local.",
		AddrV4:     net.ParseIP("169.254.249.48"),
		Port:       50051,
		InfoFields: []string{"id=agent-id", "displayname=Prudent Lark"},
	}
	adapterLookup := windowsNetworkAdapterLookupFromEntries([]netAdapterEntry{{
		InterfaceIndex:       12,
		Name:                 "Ethernet 3",
		InterfaceDescription: "Remote NDIS Compatible Device",
		LinkSpeed:            "425 Mbps",
	}})

	dev, ok := lanDeviceFromMDNSEntry(entry, iface, adapterLookup)
	if !ok {
		t.Fatal("lanDeviceFromMDNSEntry returned false")
	}
	if dev.NetworkInterface != "Ethernet 3" {
		t.Fatalf("NetworkInterface = %q, want Ethernet 3", dev.NetworkInterface)
	}
	wantUSB := "Remote NDIS Compatible Device (Ethernet 3) 425 Mbps"
	if dev.USB != wantUSB {
		t.Fatalf("USB = %q, want %q", dev.USB, wantUSB)
	}
}

func TestParseNetAdapterJSONFiltersWendyByDescription(t *testing.T) {
	in := `[
		{"Name":"Ethernet 3","InterfaceDescription":"Wendy Gadget Mode","MacAddress":"AA-BB-CC-DD-EE-FF","LinkSpeed":"100 Mbps","IPAddress":"169.254.2.7"},
		{"Name":"Wi-Fi","InterfaceDescription":"Intel AX201","MacAddress":"11-22-33-44-55-66","LinkSpeed":"866 Mbps","IPAddress":"192.168.0.20"}
	]`

	got := parseNetAdapterJSON(in)
	if len(got) != 1 {
		t.Fatalf("got %d devices, want 1 (case-insensitive Wendy match on InterfaceDescription)", len(got))
	}
	if got[0].Name != "Ethernet 3" || got[0].DisplayName != "Wendy Gadget Mode" {
		t.Fatalf("unexpected entry: %#v", got[0])
	}
}

func TestParseNetAdapterJSONIsCaseInsensitive(t *testing.T) {
	in := `{"Name":"WENDY ADAPTER","InterfaceDescription":"some vendor","MacAddress":"","LinkSpeed":"","IPAddress":""}`
	got := parseNetAdapterJSON(in)
	if len(got) != 1 {
		t.Fatalf("got %d devices, want 1 (uppercase WENDY should match)", len(got))
	}
}

func TestParseNetAdapterJSONEmpty(t *testing.T) {
	if got := parseNetAdapterJSON(""); got != nil {
		t.Fatalf("parseNetAdapterJSON(\"\") = %#v, want nil", got)
	}
	if got := parseNetAdapterJSON("   \n  "); got != nil {
		t.Fatalf("parseNetAdapterJSON(whitespace) = %#v, want nil", got)
	}
}

func TestParseNetAdapterJSONIgnoresNonWendy(t *testing.T) {
	in := `[
		{"Name":"Ethernet","InterfaceDescription":"Realtek PCIe","MacAddress":"00-00-00-00-00-00","LinkSpeed":"1 Gbps","IPAddress":"192.168.1.10"},
		{"Name":"vEthernet (Default Switch)","InterfaceDescription":"Hyper-V Virtual Ethernet Adapter","MacAddress":"","LinkSpeed":"10 Gbps","IPAddress":"172.20.0.1"}
	]`
	if got := parseNetAdapterJSON(in); len(got) != 0 {
		t.Fatalf("got %d devices, want 0 (no Wendy match)", len(got))
	}
}

func TestParseNetAdapterJSONMalformedReturnsNil(t *testing.T) {
	if got := parseNetAdapterJSON("{not json"); got != nil {
		t.Fatalf("malformed JSON returned %#v, want nil", got)
	}
}

func TestDeduplicateLANDevicesPrefersScopedIPv6(t *testing.T) {
	devices := []models.LANDevice{
		{
			ID:        "device-id",
			Hostname:  "wendyos-prudent-lark.local",
			IPAddress: "fe80::576f:1b86:d80b:a8b9",
			Port:      50051,
		},
		{
			ID:        "device-id",
			Hostname:  "wendyos-prudent-lark.local",
			IPAddress: "fe80::576f:1b86:d80b:a8b9%Ethernet",
			Port:      50051,
		},
	}

	got := deduplicateLANDevices(devices)
	if len(got) != 1 {
		t.Fatalf("deduplicateLANDevices returned %d devices, want 1", len(got))
	}
	want := "fe80::576f:1b86:d80b:a8b9%Ethernet"
	if got[0].IPAddress != want {
		t.Fatalf("IPAddress = %q, want %q", got[0].IPAddress, want)
	}
}
