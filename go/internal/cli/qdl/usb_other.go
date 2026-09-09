//go:build !darwin && !linux

package qdl

import (
	"errors"
	"time"
)

// Windows binds Qualcomm's QDLoader driver as a serial port rather than
// exposing the raw bulk interface libusb needs, so EDL flashing needs a
// separate transport there and is not wired up yet.
var errUnsupported = errors.New("flashing a device in EDL mode is not supported on this platform")

// Supported reports whether this platform has an EDL transport.
func Supported() bool { return false }

// DeviceInfo identifies one attached device in EDL mode.
type DeviceInfo struct {
	Serial  string
	CID     string
	Bus     int
	Address int
}

func (d DeviceInfo) String() string { return d.Serial }

// USBConn is a stub on platforms without an EDL transport.
type USBConn struct{}

func List() ([]DeviceInfo, error) { return nil, errUnsupported }

func Open(DeviceInfo) (*USBConn, error) { return nil, errUnsupported }

func (c *USBConn) Info() DeviceInfo { return DeviceInfo{} }

func (c *USBConn) Read([]byte, time.Duration) (int, error) { return 0, errUnsupported }

func (c *USBConn) Write([]byte, time.Duration) (int, error) { return 0, errUnsupported }

func (c *USBConn) Close() error { return nil }
