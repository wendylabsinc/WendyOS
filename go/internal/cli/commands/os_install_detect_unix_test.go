//go:build (darwin || linux) && !ios

package commands

import (
	"strings"
	"testing"

	"github.com/google/gousb"
)

// gousb panics when libusb_init fails, as libusb 1.0.29 does on a Linux host
// without usbfs. Installs that never use USB must get an error instead.
func TestScanInstallUSBRecoversLibusbInitPanic(t *testing.T) {
	devices, err := scanInstallUSBContext(func() *gousb.Context {
		panic("libusb: unknown error [code -99]")
	})
	if devices != nil || err == nil || !strings.Contains(err.Error(), "code -99") {
		t.Fatalf("scan = %v, %v; want the init failure as an error", devices, err)
	}
}
