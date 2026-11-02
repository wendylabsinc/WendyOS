//go:build (darwin || linux) && !ios

package commands

import (
	"fmt"
	"os"
	"runtime"

	"github.com/google/gousb"
)

func scanInstallUSBDevices() ([]installUSBDevice, error) {
	// Containers, WSL2 and VMs without USB devices have no usbfs, so there is
	// nothing to flash. The libusb that release builds link fails to
	// initialize there, so skip it rather than warn on every install.
	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/dev/bus/usb"); err != nil {
			return nil, nil
		}
	}
	return scanInstallUSBContext(gousb.NewContext)
}

func scanInstallUSBContext(newContext func() *gousb.Context) (devices []installUSBDevice, err error) {
	// gousb panics when libusb cannot initialize. This scan only orders the
	// picker, so it must not take down installs that never use USB.
	defer func() {
		if r := recover(); r != nil {
			devices, err = nil, fmt.Errorf("initializing libusb: %v", r)
		}
	}()
	ctx := newContext()
	defer ctx.Close()
	enumerated := false
	_, err = ctx.OpenDevices(func(d *gousb.DeviceDesc) bool {
		enumerated = true
		device := installUSBDevice{vendor: uint16(d.Vendor), product: uint16(d.Product)}
		if installUSBTarget(device) != "" {
			devices = append(devices, device)
		}
		return false // Enumerate descriptors without opening any device.
	})
	// OpenDevices also returns the last unreadable descriptor of an unrelated
	// device. Only a scan that saw no device at all has failed.
	if enumerated {
		err = nil
	}
	return devices, err
}
