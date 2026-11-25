package commands

import (
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
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

func TestDesktopLiteMergesUSBAndMeshByTenantIdentity(t *testing.T) {
	tenant := "11111111-1111-4111-8111-111111111111"
	device := "22222222-2222-4222-8222-222222222222"
	principal := "spiffe://wendy.sh/tenant/" + tenant + "/device/" + device
	now := time.Now()
	mesh := DesktopMeshDevices(sessionbroker.OperatorDirectory{State: "ready", Captured: now.UnixMilli(), Devices: []sessionbroker.OperatorTargetInfo{{
		Selector: "mesh://" + tenant + "/" + device, Principal: principal, Tenant: tenant, Device: device,
		Name: "camera", Platform: "Lite", Transport: "mtls-v1", Address: "[fd00::1234]:43024", Expires: now.Add(time.Minute).UnixMilli(),
	}}})
	if len(mesh) != 1 {
		t.Fatalf("missing mesh fixture: %+v", mesh)
	}
	external := models.ExternalDevice{ID: "wendy-lite:/dev/cu.board", DisplayName: "camera", ConnectionInfo: map[string]string{
		"type": "USB", "serialPort": "/dev/cu.board", "deviceId": "hardware-id", "enrolledDeviceId": device, "tenantId": tenant,
	}}
	usb := desktopNearbyLiteDevice(external)
	if usb.ID != principal || usb.Selector != external.ID || usb.Address != "/dev/cu.board" {
		t.Fatalf("lost USB identity or endpoint: %+v", usb)
	}
	for _, rows := range [][]DesktopDevice{{usb, mesh[0]}, {mesh[0], usb}} {
		got := mergeDesktopDevices(rows)
		if len(got) != 1 || got[0].Selector != mesh[0].Selector {
			t.Fatalf("duplicate Lite device or lost mesh route: %+v", got)
		}
	}
	for _, otherTenant := range []string{"", "33333333-3333-4333-8333-333333333333", "invalid"} {
		external.ConnectionInfo["tenantId"] = otherTenant
		got := mergeDesktopDevices([]DesktopDevice{desktopNearbyLiteDevice(external), mesh[0]})
		if len(got) != 2 {
			t.Fatalf("merged devices without matching tenant identity (%q): %+v", otherTenant, got)
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
