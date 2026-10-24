//go:build (darwin) && !ios

package t234

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/gousb"
)

// ReleaseUSB forces a USB-level disconnect of the flashing gadget so the
// device sees the host let go (its initrd polls the UDC state and proceeds
// only when it leaves "configured" — a plain `diskutil eject` never touches
// the USB level). It captures the gadget from the mass-storage driver
// (libusb implements detach on macOS as a driver-suppressing re-enumeration;
// this is why the caller runs it as root) and holds it long enough for the
// device's 1 Hz poll to observe the unconfigured state, then lets go.
func ReleaseUSB(serial, port string) error {
	ctx := gousb.NewContext()
	ctx.Debug(0)
	defer ctx.Close()

	devs, err := ctx.OpenDevices(func(d *gousb.DeviceDesc) bool {
		return d.Vendor == gousb.ID(GadgetVendorID) && d.Product == gousb.ID(GadgetProductID)
	})
	for _, d := range devs {
		defer d.Close()
	}
	if err != nil && len(devs) == 0 {
		return fmt.Errorf("opening flashing gadget: %w", err)
	}
	var chosen *gousb.Device
	for _, d := range devs {
		if port != "" && darwinUSBPortKey(d.Desc) != port {
			continue
		}
		if serial != "" {
			if s, err := d.SerialNumber(); err == nil && s != serial {
				continue
			}
		}
		chosen = d
		break
	}
	if chosen == nil {
		return fmt.Errorf("flashing gadget (%04x:%04x) not found on USB", GadgetVendorID, GadgetProductID)
	}

	// Capture: terminates the kernel mass-storage driver and re-enumerates
	// with drivers suppressed, which the gadget observes as a disconnect.
	_ = chosen.SetAutoDetach(true)
	cfg, err := chosen.Config(1)
	if err != nil {
		// Fall back to a plain bus reset; the gadget still sees the state
		// leave "configured" for a window.
		if rerr := chosen.Reset(); rerr != nil {
			return fmt.Errorf("capturing gadget: %v (reset also failed: %v)", err, rerr)
		}
		return nil
	}
	// Hold the capture past the device's poll interval so it cannot miss
	// the unconfigured window, then release.
	time.Sleep(3 * time.Second)
	cfg.Close()
	return nil
}

func darwinUSBPortKey(desc *gousb.DeviceDesc) string {
	parts := make([]string, len(desc.Path))
	for i, p := range desc.Path {
		parts[i] = strconv.Itoa(p)
	}
	return fmt.Sprintf("%d-%s", desc.Bus, strings.Join(parts, "."))
}
