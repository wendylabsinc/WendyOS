package discovery

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// TestSetLANNetworkInterface is the pure logic every platform's newLANAnnotator
// override (darwin's init() in discovery_darwin.go, linux's in
// annotate_linux.go, windows' in annotate_windows.go) ultimately funnels
// through: it must set NetworkInterface, and derive the USB summary from the
// interface/display name and link speed only when the interface actually
// looks USB-backed and only when USB has not already been set upstream.
func TestSetLANNetworkInterface(t *testing.T) {
	nameRulesOnly(t)
	t.Run("USB-looking interface gets a USB summary with link speed", func(t *testing.T) {
		dev := &models.LANDevice{}
		setLANNetworkInterface(dev, "usb0", "", "480 Mbps")
		if dev.NetworkInterface != "usb0" {
			t.Errorf("NetworkInterface = %q, want %q", dev.NetworkInterface, "usb0")
		}
		if want := "usb0 480 Mbps"; dev.USB != want {
			t.Errorf("USB = %q, want %q", dev.USB, want)
		}
	})

	t.Run("display name distinct from interface name is parenthesized", func(t *testing.T) {
		dev := &models.LANDevice{}
		setLANNetworkInterface(dev, "Ethernet 3", "Remote NDIS Compatible Device", "425 Mbps")
		if want := "Remote NDIS Compatible Device (Ethernet 3) 425 Mbps"; dev.USB != want {
			t.Errorf("USB = %q, want %q", dev.USB, want)
		}
	})

	t.Run("no link speed omits the trailing speed", func(t *testing.T) {
		dev := &models.LANDevice{}
		setLANNetworkInterface(dev, "usb0", "", "")
		if want := "usb0"; dev.USB != want {
			t.Errorf("USB = %q, want %q", dev.USB, want)
		}
	})

	t.Run("non-USB interface leaves USB empty but still sets NetworkInterface", func(t *testing.T) {
		dev := &models.LANDevice{}
		setLANNetworkInterface(dev, "eth0", "", "1 Gbps")
		if dev.NetworkInterface != "eth0" {
			t.Errorf("NetworkInterface = %q, want %q", dev.NetworkInterface, "eth0")
		}
		if dev.USB != "" {
			t.Errorf("USB = %q, want empty for a non-USB-looking interface", dev.USB)
		}
	})

	t.Run("empty interface name is a no-op", func(t *testing.T) {
		dev := &models.LANDevice{NetworkInterface: "stale", USB: "stale-usb"}
		setLANNetworkInterface(dev, "", "irrelevant", "irrelevant")
		if dev.NetworkInterface != "stale" || dev.USB != "stale-usb" {
			t.Errorf("dev = %+v, want untouched", dev)
		}
	})

	t.Run("an already-set USB is not overwritten", func(t *testing.T) {
		dev := &models.LANDevice{USB: "already set upstream"}
		setLANNetworkInterface(dev, "usb0", "", "480 Mbps")
		if dev.USB != "already set upstream" {
			t.Errorf("USB = %q, want the pre-existing value preserved", dev.USB)
		}
		if dev.NetworkInterface != "usb0" {
			t.Errorf("NetworkInterface = %q, want %q", dev.NetworkInterface, "usb0")
		}
	})
}

// nameRulesOnly tests the name rules macOS and Windows use on any host; Linux
// otherwise classifies by USB identity.
func nameRulesOnly(t *testing.T) {
	t.Helper()
	orig := usbGadgetIdentityFn
	usbGadgetIdentityFn = nil
	t.Cleanup(func() { usbGadgetIdentityFn = orig })
}

func TestLooksLikeUSBConnectionNCM(t *testing.T) {
	nameRulesOnly(t)
	if !looksLikeUSBConnection("ncm0", "") {
		t.Fatal("ncm0 should be classified as a USB connection")
	}
}
