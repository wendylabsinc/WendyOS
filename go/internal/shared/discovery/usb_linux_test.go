//go:build linux

package discovery

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUSBDeviceFromSysfs(t *testing.T) {
	tests := []struct {
		name            string
		vid, pid        string
		mfr, product    string
		wantOK          bool
		wantName        string
		wantDisplayName string
		wantVendorID    string
	}{
		{
			name:            "wendy device by name",
			vid:             "1234",
			pid:             "5678",
			mfr:             "Wendy Labs",
			product:         "Edge Device",
			wantOK:          true,
			wantName:        "Wendy Labs Edge Device",
			wantDisplayName: "Wendy Labs Edge Device",
			wantVendorID:    "0x1234",
		},
		{
			name:    "unrelated device is filtered out",
			vid:     "8086",
			pid:     "0001",
			mfr:     "Intel",
			product: "Hub",
			wantOK:  false,
		},
		{
			name:   "esp32 by vid:pid with no name is excluded",
			vid:    "303a",
			pid:    "1001",
			wantOK: false,
		},
		{
			name:    "esp32 vid:pid is excluded even when the name also matches wendy",
			vid:     "303a",
			pid:     "1001",
			mfr:     "Wendy Labs",
			product: "Lite",
			wantOK:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dev, ok := usbDeviceFromSysfs(tt.vid, tt.pid, tt.mfr, tt.product)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if dev.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", dev.Name, tt.wantName)
			}
			if dev.DisplayName != tt.wantDisplayName {
				t.Errorf("DisplayName = %q, want %q", dev.DisplayName, tt.wantDisplayName)
			}
			if !dev.IsWendyDevice {
				t.Errorf("IsWendyDevice = false, want true")
			}
			if dev.VendorID != tt.wantVendorID {
				t.Errorf("VendorID = %q, want %q", dev.VendorID, tt.wantVendorID)
			}
		})
	}
}

func TestDiscoverUSB_FromSysfsTree(t *testing.T) {
	root := t.TempDir()

	// Helper to create a fake sysfs device directory with the given attributes.
	mkdev := func(name string, attrs map[string]string) {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for k, v := range attrs {
			if err := os.WriteFile(filepath.Join(dir, k), []byte(v+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	// A Wendy whole-device directory.
	mkdev("3-1", map[string]string{"idVendor": "1234", "idProduct": "5678", "manufacturer": "Wendy Labs", "product": "Edge Device"})
	// An unrelated device that must be ignored.
	mkdev("3-2", map[string]string{"idVendor": "8086", "idProduct": "0001", "manufacturer": "Intel"})
	// An Espressif ESP32 (Wendy Lite) whole-device directory — must be excluded here;
	// it's surfaced separately via serial/WendyCom discovery.
	mkdev("3-3", map[string]string{"idVendor": "303a", "idProduct": "1001"})
	// An interface directory for the Wendy device — must be skipped (has a colon, no ids).
	mkdev("3-1:1.0", map[string]string{})
	// A bus root without idVendor/idProduct — must be skipped.
	mkdev("usb3", map[string]string{})

	devices, err := discoverUSBAt(root)
	if err != nil {
		t.Fatalf("discoverUSBAt: %v", err)
	}

	if len(devices) != 1 {
		t.Fatalf("expected exactly 1 matched device, got %d: %+v", len(devices), devices)
	}
	if !devices[0].IsWendyDevice {
		t.Errorf("expected the matched device to be flagged as a Wendy device, got %+v", devices[0])
	}
}

func TestDiscoverUSB_RecordsTheNetworkInterface(t *testing.T) {
	root := t.TempDir()
	mkdev := func(name string, interfaces map[string][]string) {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for k, v := range map[string]string{"idVendor": "1d6b", "idProduct": "0104", "manufacturer": "Wendy Labs Inc", "product": "WendyOS Device " + name} {
			if err := os.WriteFile(filepath.Join(dir, k), []byte(v+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		for usbInterface, nets := range interfaces {
			if err := os.MkdirAll(filepath.Join(dir, usbInterface), 0o755); err != nil {
				t.Fatal(err)
			}
			for _, net := range nets {
				if err := os.MkdirAll(filepath.Join(dir, usbInterface, "net", net), 0o755); err != nil {
					t.Fatal(err)
				}
			}
		}
	}

	// A WendyOS gadget: network function on interface 0, serial console on 2.
	mkdev("3-1", map[string][]string{"3-1:1.0": {"enx02a1b2c3d4e5"}, "3-1:1.1": nil, "3-1:1.2": nil})
	// No network function bound.
	mkdev("3-2", map[string][]string{"3-2:1.0": nil})
	// Two network interfaces: which one a device answers on is not known.
	mkdev("3-3", map[string][]string{"3-3:1.0": {"usb0"}, "3-3:1.2": {"usb1"}})

	devices, err := discoverUSBAt(root)
	if err != nil {
		t.Fatalf("discoverUSBAt: %v", err)
	}
	got := make(map[string]string, len(devices))
	for _, dev := range devices {
		got[dev.Name] = dev.NetworkInterface
	}
	want := map[string]string{
		"Wendy Labs Inc WendyOS Device 3-1": "enx02a1b2c3d4e5",
		"Wendy Labs Inc WendyOS Device 3-2": "",
		"Wendy Labs Inc WendyOS Device 3-3": "",
	}
	if len(got) != len(want) {
		t.Fatalf("devices = %v, want %v", got, want)
	}
	for name, iface := range want {
		if got[name] != iface {
			t.Errorf("%s: NetworkInterface = %q, want %q", name, got[name], iface)
		}
	}
}
