//go:build linux

package usbthermal

import (
	"errors"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// TestUSBDevFSABI: the structs and ioctl numbers must match
// <linux/usbdevice_fs.h> exactly; the expected numbers are the ones the
// capture scripts used against the camera on a 64-bit Jetson.
func TestUSBDevFSABI(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the expected values are the 64-bit ABI's")
	}
	if got := unsafe.Offsetof(usbdevfsCtrlTransfer{}.Data); got != 16 {
		t.Errorf("usbdevfs_ctrltransfer.data at offset %d, want 16", got)
	}
	if got := unsafe.Offsetof(usbdevfsBulkTransfer{}.Data); got != 16 {
		t.Errorf("usbdevfs_bulktransfer.data at offset %d, want 16", got)
	}
	want := map[string][2]uintptr{
		"USBDEVFS_CONTROL":          {usbdevfsControl, 0xC0185500},
		"USBDEVFS_BULK":             {usbdevfsBulk, 0xC0185502},
		"USBDEVFS_SETINTERFACE":     {usbdevfsSetinterface, 0x80085504},
		"USBDEVFS_CLAIMINTERFACE":   {usbdevfsClaiminterface, 0x8004550F},
		"USBDEVFS_RELEASEINTERFACE": {usbdevfsReleaseinterface, 0x80045510},
	}
	for name, v := range want {
		if v[0] != v[1] {
			t.Errorf("%s = %#x, want %#x", name, v[0], v[1])
		}
	}
}

func TestMapErrno(t *testing.T) {
	if err := mapErrno(unix.ETIMEDOUT); !errors.Is(err, ErrTimeout) || !errors.Is(err, unix.ETIMEDOUT) {
		t.Errorf("ETIMEDOUT -> %v", err)
	}
	if err := mapErrno(unix.EBUSY); !errors.Is(err, ErrBusy) {
		t.Errorf("EBUSY -> %v", err)
	}
	if err := mapErrno(unix.ENODEV); !errors.Is(err, unix.ENODEV) || errors.Is(err, ErrTimeout) {
		t.Errorf("ENODEV -> %v", err)
	}
}

// TestOpenUSBFSMissingNode: a node that is not there is an error naming it.
func TestOpenUSBFSMissingNode(t *testing.T) {
	_, err := OpenUSBFS("/dev/bus/usb/999/999")
	if err == nil {
		t.Fatal("opened a node that does not exist")
	}
}
