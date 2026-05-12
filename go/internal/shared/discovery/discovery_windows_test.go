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

	dev, ok := lanDeviceFromMDNSEntry(entry, nil)
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

	dev, ok := lanDeviceFromMDNSEntry(entry, nil)
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

	dev, ok := lanDeviceFromMDNSEntry(entry, iface)
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

	dev, ok := lanDeviceFromMDNSEntry(entry, iface)
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

	_, ok := lanDeviceFromMDNSEntry(entry, nil)
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
