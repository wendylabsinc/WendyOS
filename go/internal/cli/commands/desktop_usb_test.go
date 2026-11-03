package commands

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func TestDesktopUSBKeepsCableAddressWithoutHostnameResolution(t *testing.T) {
	for _, address := range []string{"fe80::5741:1", "fe80::5741:1%en77", "169.254.133.171"} {
		d := models.LANDevice{Hostname: "uno-q.local.", IPAddress: address, NetworkInterface: "en77", USB: "WendyOS Device", Port: 50052, OS: "wendyos"}
		got := desktopNearbyDevice(d)
		if got.Selector != "[fe80::5741:1%en77]:50052" || got.Transport != "USB" {
			t.Fatalf("USB endpoint lost: %+v", got)
		}
		d.Mesh = true
		d.Hostname = "mesh://tenant/device"
		got = desktopNearbyDevice(d)
		if got.Selector != d.Hostname || got.Transport != "Mesh" {
			t.Fatalf("mesh selector lost: %+v", got)
		}
	}
}

func TestDesktopDeviceIdentitySurvivesAddressAndTransportChanges(t *testing.T) {
	principal := "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/22222222-2222-4222-8222-222222222222"
	d := models.LANDevice{PrincipalURI: principal, Hostname: "old.local", IPAddress: "192.0.2.1", Port: 50052, OS: "wendyos"}
	lan := desktopNearbyDevice(d)
	d.IPAddress = "169.254.133.171"
	d.USB = "WendyOS Device"
	d.NetworkInterface = "en77"
	usb := desktopNearbyDevice(d)
	d.Mesh = true
	d.Hostname = "mesh://11111111-1111-4111-8111-111111111111/22222222-2222-4222-8222-222222222222"
	mesh := desktopNearbyDevice(d)
	for _, device := range []DesktopDevice{lan, usb, mesh} {
		if device.ID != principal {
			t.Fatalf("identity follows address: %+v", device)
		}
	}
	for _, input := range [][]DesktopDevice{{lan, usb, mesh}, {mesh, usb, lan}} {
		got := mergeDesktopDevices(input)
		if len(got) != 1 || got[0].Selector != mesh.Selector {
			t.Fatalf("device duplicated or mesh lost: %+v", got)
		}
	}
	got := mergeDesktopDevices([]DesktopDevice{lan, usb})
	if len(got) != 1 || got[0].Selector != "[fe80::5741:1%en77]:50052" {
		t.Fatalf("USB was not retained: %+v", got)
	}
}
