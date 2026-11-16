//go:build linux

package discovery

import (
	"context"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// TestCachedLinuxInterfaceLinkSpeedMemoizes pins the memoization
// cachedLinuxInterfaceLinkSpeed exists for: a sysfs read per interface, at
// most once per discovery session, mirroring darwin's
// darwinCachedInterfaceLinkSpeed.
func TestCachedLinuxInterfaceLinkSpeedMemoizes(t *testing.T) {
	orig := linuxInterfaceLinkSpeedFn
	t.Cleanup(func() { linuxInterfaceLinkSpeedFn = orig })

	calls := 0
	linuxInterfaceLinkSpeedFn = func(name string) string {
		calls++
		return "1 Gbps"
	}

	linkSpeeds := make(map[string]string)
	first := cachedLinuxInterfaceLinkSpeed("eth0", linkSpeeds)
	second := cachedLinuxInterfaceLinkSpeed("eth0", linkSpeeds)

	if first != "1 Gbps" || second != "1 Gbps" {
		t.Fatalf("got %q, %q, want %q both times", first, second, "1 Gbps")
	}
	if calls != 1 {
		t.Fatalf("linuxInterfaceLinkSpeedFn called %d times, want 1 (second call should hit the cache)", calls)
	}
}

// TestCachedLinuxInterfaceLinkSpeedEmptyName reports empty without
// consulting the sysfs read at all, matching darwin's guard.
func TestCachedLinuxInterfaceLinkSpeedEmptyName(t *testing.T) {
	orig := linuxInterfaceLinkSpeedFn
	t.Cleanup(func() { linuxInterfaceLinkSpeedFn = orig })

	linuxInterfaceLinkSpeedFn = func(string) string {
		t.Fatal("linuxInterfaceLinkSpeedFn must not be called for an empty interface name")
		return ""
	}

	if got := cachedLinuxInterfaceLinkSpeed("", make(map[string]string)); got != "" {
		t.Fatalf("got %q, want empty", got)
	}
}

// TestLinuxLANAnnotatorAppliesLinkSpeedAndUSB is the property the missing
// linux newLANAnnotator override broke: it must actually reach
// setLANNetworkInterface, so a live-sighted device picks up its link speed
// and USB summary the way the old (now-deleted) discoverLAN batch path did,
// instead of being left with only the bare interface name
// lanDeviceFromService set.
func TestLinuxLANAnnotatorAppliesLinkSpeedAndUSB(t *testing.T) {
	useUSBNICFixture(t, map[string]usbNIC{"usb0": wendyGadget})
	orig := linuxInterfaceLinkSpeedFn
	t.Cleanup(func() { linuxInterfaceLinkSpeedFn = orig })
	linuxInterfaceLinkSpeedFn = func(name string) string {
		if name == "usb0" {
			return "480 Mbps"
		}
		return ""
	}

	annotate := newLANAnnotator(context.Background())

	dev := &models.LANDevice{NetworkInterface: "usb0"}
	annotate(dev)

	if dev.NetworkInterface != "usb0" {
		t.Errorf("NetworkInterface = %q, want %q", dev.NetworkInterface, "usb0")
	}
	wantUSB := "usb0 480 Mbps"
	if dev.USB != wantUSB {
		t.Errorf("USB = %q, want %q", dev.USB, wantUSB)
	}

	// A second device on the same interface, within the same session, must
	// reuse the cached link speed rather than reading sysfs again.
	linuxInterfaceLinkSpeedFn = func(string) string {
		t.Fatal("link speed should have been cached from the first device on this interface")
		return ""
	}
	dev2 := &models.LANDevice{NetworkInterface: "usb0"}
	annotate(dev2)
	if dev2.USB != wantUSB {
		t.Errorf("second device USB = %q, want cached %q", dev2.USB, wantUSB)
	}
}
