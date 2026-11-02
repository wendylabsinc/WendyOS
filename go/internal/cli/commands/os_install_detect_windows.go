//go:build windows

package commands

import (
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/qdl"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/rcm"
	"github.com/wendylabsinc/wendy/go/internal/cli/tegraflash/winusb"
)

func scanInstallUSBDevices() ([]installUSBDevice, error) {
	// qdl.List applies the same EDL interface check as Dragonwing flashing,
	// so a 05c6:9008 devnode is offered only when the flasher would find it.
	edl, err := qdl.List()
	if err != nil {
		return nil, err
	}
	var devices []installUSBDevice
	for range edl {
		devices = append(devices, installUSBDevice{vendor: qdl.VendorQualcomm, product: qdl.ProductEDL})
	}
	for _, vendor := range []uint16{rcm.VendorNVIDIA, espressifVendor} {
		found, err := winusb.ListVendor(vendor)
		if err != nil {
			// A SetupAPI failure repeats for every vendor; report it once.
			return devices, err
		}
		for _, d := range found {
			// Composite interface devnodes repeat the parent's VID/PID. Count
			// only the physical USB device, as libusb does on macOS/Linux.
			if strings.Contains(strings.ToUpper(d.InstanceID), "&MI_") {
				continue
			}
			device := installUSBDevice{vendor: d.VID, product: d.PID}
			if installUSBTarget(device) != "" {
				devices = append(devices, device)
			}
		}
	}
	return devices, nil
}
