package usbthermal

import (
	"errors"
	"time"
)

// Device is an opened USB device, as much of usbfs as the driver uses. The
// Linux implementation issues the usbdevfs ioctls directly (the agent builds
// without cgo, so there is no libusb); tests substitute a fake that records
// the calls and replays captured bytes.
type Device interface {
	// Control performs a control transfer. For an IN request (bit 7 of
	// requestType set) data is filled; for OUT it is sent. It returns the
	// number of bytes transferred.
	Control(requestType, request uint8, value, index uint16, data []byte, timeout time.Duration) (int, error)
	// Bulk reads from (or writes to) a bulk endpoint and returns the number of
	// bytes transferred. A read that times out returns ErrTimeout.
	Bulk(endpoint uint8, data []byte, timeout time.Duration) (int, error)
	ClaimInterface(iface uint32) error
	ReleaseInterface(iface uint32) error
	SetInterface(iface, altSetting uint32) error
	Close() error
}

// OpenFunc opens the usbfs node of a device, e.g. /dev/bus/usb/001/015.
type OpenFunc func(path string) (Device, error)

var (
	// ErrTimeout is a transfer that did not complete in time.
	ErrTimeout = errors.New("usb transfer timed out")
	// ErrBusy is an interface another process (or a kernel driver) already
	// owns. Only one process can stream these cameras, and it has to be the
	// agent.
	ErrBusy = errors.New("usb interface is in use by another process")
	// ErrUnsupported is returned where usbfs does not exist (not Linux).
	ErrUnsupported = errors.New("usb thermal cameras are only supported on Linux")
	// ErrClosed is a subscription to a camera or manager that has shut down.
	ErrClosed = errors.New("usb thermal camera driver is shut down")
)
