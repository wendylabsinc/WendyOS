//go:build linux

package discovery

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// buildNetFixture creates a fake /sys/class/net tree where each interface name
// is a symlink to a device directory under devicesRoot. It returns the path to
// use as netClassRoot.
func buildNetFixture(t *testing.T, ifaces map[string]string) string {
	t.Helper()
	base := t.TempDir()
	classRoot := filepath.Join(base, "class", "net")
	if err := os.MkdirAll(classRoot, 0o755); err != nil {
		t.Fatalf("mkdir class root: %v", err)
	}
	for iface, devRel := range ifaces {
		devAbs := filepath.Join(base, devRel)
		if err := os.MkdirAll(devAbs, 0o755); err != nil {
			t.Fatalf("mkdir device dir %s: %v", devAbs, err)
		}
		if err := os.Symlink(devAbs, filepath.Join(classRoot, iface)); err != nil {
			t.Fatalf("symlink %s: %v", iface, err)
		}
	}
	return classRoot
}

// usbNIC is the identity a fixture USB device reports in sysfs.
type usbNIC struct{ vid, pid, manufacturer, product string }

var (
	// Values every WendyOS image's gadget-setup.sh writes (wendyos-builder).
	wendyGadget = usbNIC{"1d6b", "0104", "Wendy Labs Inc", "WendyOS Device loyal-rail"}
	// The Framework USB-Ethernet card that matched the old name heuristic.
	realtekNIC = usbNIC{"0bda", "8156", "Realtek", "USB 10/100/1G/2.5G LAN"}
	// Another Linux gadget on the generic Linux Foundation composite VID:PID.
	otherGadget = usbNIC{"1d6b", "0104", "Raspberry Pi", "Pi USB Gadget"}
)

// useUSBNICFixture points sysfs and the interface list at fake USB network
// interfaces, each on its own USB device with the given identity.
func useUSBNICFixture(t *testing.T, nics map[string]usbNIC) {
	t.Helper()
	paths := make(map[string]string, len(nics))
	bus := 0
	for iface := range nics {
		bus++
		port := fmt.Sprintf("%d-1", bus)
		paths[iface] = fmt.Sprintf("devices/pci0000:00/usb%d/%s/%s:1.0/net/%s", bus, port, port, iface)
	}
	origRoot, origList := netClassRoot, netInterfacesFn
	t.Cleanup(func() { netClassRoot, netInterfacesFn = origRoot, origList })
	netClassRoot = buildNetFixture(t, paths)
	base := filepath.Dir(filepath.Dir(netClassRoot))
	var list []net.Interface
	for iface, id := range nics {
		dev := filepath.Join(base, filepath.Dir(filepath.Dir(filepath.Dir(paths[iface]))))
		for attr, v := range map[string]string{"idVendor": id.vid, "idProduct": id.pid, "manufacturer": id.manufacturer, "product": id.product} {
			if err := os.WriteFile(filepath.Join(dev, attr), []byte(v+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		list = append(list, net.Interface{Index: len(list) + 2, Name: iface, Flags: net.FlagUp})
	}
	netInterfacesFn = func() ([]net.Interface, error) { return list, nil }
}

// Only a Wendy device's gadget link is a candidate for USB setup; an ordinary
// USB network adapter never is, whatever its name.
func TestUSBNetworkInterfaceNames_WendyIdentityOnly(t *testing.T) {
	tests := []struct {
		name string
		nics map[string]usbNIC
		want []string
	}{
		{"wendy gadget alone", map[string]usbNIC{"enp0s20f0u2": wendyGadget}, []string{"enp0s20f0u2"}},
		{"wendy gadget plus USB ethernet adapter", map[string]usbNIC{"enp0s20f0u2": wendyGadget, "enp195s0f3u1": realtekNIC}, []string{"enp0s20f0u2"}},
		{"USB ethernet adapter alone", map[string]usbNIC{"enp195s0f3u1": realtekNIC}, nil},
		{"usb-named adapter", map[string]usbNIC{"usb0": realtekNIC, "enxaabbccddeeff": realtekNIC}, nil},
		{"non-Wendy gadget on the generic composite id", map[string]usbNIC{"enp0s20f0u3": otherGadget}, nil},
		// eth1: classic naming (net.ifnames=0) is still found by identity.
		{"two wendy gadgets", map[string]usbNIC{"enp0s20f0u2": wendyGadget, "eth1": wendyGadget}, []string{"enp0s20f0u2", "eth1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			useUSBNICFixture(t, tt.nics)
			got, err := USBNetworkInterfaceNames()
			if err != nil {
				t.Fatal(err)
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("USBNetworkInterfaceNames() = %v, want %v", got, tt.want)
			}
		})
	}
}

// The well-known-address dial and the USB label follow the same identity.
func TestUSBDirectCandidates_WendyIdentityOnly(t *testing.T) {
	useUSBNICFixture(t, map[string]usbNIC{"enp0s20f0u2": wendyGadget, "enp195s0f3u1": realtekNIC})
	got := USBDirectCandidates()
	if len(got) != 1 || got[0].Interface != "enp0s20f0u2" {
		t.Fatalf("USBDirectCandidates() = %+v, want only enp0s20f0u2", got)
	}
	if looksLikeUSBConnection("enp195s0f3u1", "") {
		t.Error("USB ethernet adapter labelled as a USB link to a device")
	}
	for _, bad := range []string{"", ".", "..", "../net/enp0s20f0u2", "missing0"} {
		if IsWendyUSBGadget(bad) {
			t.Errorf("IsWendyUSBGadget(%q) = true, want false", bad)
		}
	}
}
