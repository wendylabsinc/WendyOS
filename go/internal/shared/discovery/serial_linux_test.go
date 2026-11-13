//go:build linux

package discovery

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHasSupportedESP32SerialUSBID(t *testing.T) {
	root := t.TempDir()
	usbDevice := filepath.Join(root, "pci0000:00", "usb1", "1-2")
	ttyNode := filepath.Join(usbDevice, "1-2:1.0", "ttyUSB0")
	if err := os.MkdirAll(ttyNode, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(usbDevice, "idVendor"), []byte("10C4\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(usbDevice, "idProduct"), []byte("EA60\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	id, ok := findSupportedESP32SerialUSBID(ttyNode, root)
	if !ok {
		t.Fatal("CP210x ID on a USB-device ancestor was not detected")
	}
	if id.transport != SerialTransportUARTBridge {
		t.Fatal("CP210x ID was not classified as a UART bridge")
	}
}

func TestHasSupportedESP32SerialUSBIDRejectsUnsupportedAndEscapedPaths(t *testing.T) {
	root := t.TempDir()
	unsupported := filepath.Join(root, "usb1", "1-3", "1-3:1.0", "ttyUSB0")
	if err := os.MkdirAll(unsupported, 0o755); err != nil {
		t.Fatal(err)
	}
	usbDevice := filepath.Dir(filepath.Dir(unsupported))
	if err := os.WriteFile(filepath.Join(usbDevice, "idVendor"), []byte("0403\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(usbDevice, "idProduct"), []byte("6001\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := findSupportedESP32SerialUSBID(unsupported, root); ok {
		t.Fatal("unsupported USB adapter was accepted")
	}

	outside := t.TempDir()
	if _, ok := findSupportedESP32SerialUSBID(outside, root); ok {
		t.Fatal("path outside the configured sysfs root was accepted")
	}
}

func TestDualCDCSysfsFiltersDiagnostics(t *testing.T) {
	root := t.TempDir()
	device := filepath.Join(root, "usb1", "1-2")
	for _, iface := range []string{"00", "02"} {
		node := filepath.Join(device, iface)
		if err := os.MkdirAll(node, 0755); err != nil {
			t.Fatal(err)
		}
		for name, value := range map[string]string{"idVendor": "303a", "idProduct": "4002"} {
			if err := os.WriteFile(filepath.Join(device, name), []byte(value), 0644); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(node, "bInterfaceNumber"), []byte(iface), 0644); err != nil {
			t.Fatal(err)
		}
		if _, ok := findSupportedESP32SerialUSBID(node, root); ok != (iface == "00") {
			t.Fatalf("interface %s accepted=%v", iface, ok)
		}
	}
}
