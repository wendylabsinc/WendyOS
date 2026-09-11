//go:build windows

package commands

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/winusb"
)

func TestFindThorGadgetPinsLocationAndProduct(t *testing.T) {
	want := winusb.Device{InstanceID: "gadget-a", LocationPath: "port-a", VID: winusb.VendorNVIDIA, PID: winusb.ProductGadget}
	other := want
	other.InstanceID, other.LocationPath = "gadget-b", "port-b"
	recovery := want
	recovery.InstanceID, recovery.PID = "recovery", winusb.ProductThor
	wrongVendor := want
	wrongVendor.VID = 0x05c6
	devices := []winusb.Device{other, recovery, wrongVendor, want}
	got, found, err := findThorGadget("PORT-A", devices)
	if err != nil || !found || got.InstanceID != want.InstanceID {
		t.Fatalf("selected wrong device: %+v %v %v", got, found, err)
	}
	if _, found, err := findThorGadget("port-c", devices); err != nil || found {
		t.Fatalf("missing target: found=%v err=%v", found, err)
	}
	if _, _, err := findThorGadget("", devices); err == nil {
		t.Fatal("accepted unpinned handoff")
	}
	if _, _, err := findThorGadget("port-a", append(devices, want)); err == nil {
		t.Fatal("accepted ambiguous handoff")
	}
}
