//go:build !darwin && !linux

package rcm

import (
	"fmt"
	"time"
)

// Device is a stub on platforms where direct Jetson recovery USB access is not
// implemented (gousb-based Thor flashing runs on macOS and Linux; Windows uses winusb).
type Device struct{}

func (d *Device) String() string { return "" }
func (d *Device) Close()         {}

func (d *Device) ReadWithTimeout([]byte, time.Duration) (int, error) {
	return 0, errUnsupported
}
func (d *Device) Write([]byte) error          { return errUnsupported }
func (d *Device) ReadChipID() (string, error) { return "", errUnsupported }

func DownloadBootROMImages(dev *Device, images [][]byte) error { return errUnsupported }

var errUnsupported = fmt.Errorf("Jetson USB recovery flashing is not supported on this platform")
