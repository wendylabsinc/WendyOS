//go:build !darwin && !linux && !windows

package qdl

import (
	"errors"
	"time"
)

// Platforms without a USB transport cannot flash EDL devices.
var errUnsupported = errors.New("flashing a device in EDL mode is not supported on this platform")

// Supported reports whether this platform has an EDL transport.
func Supported() bool { return false }

// USBConn is a stub on platforms without an EDL transport.
type USBConn struct{}

func List() ([]DeviceInfo, error) { return nil, errUnsupported }

func Open(DeviceInfo) (*USBConn, error) { return nil, errUnsupported }

func (c *USBConn) Info() DeviceInfo { return DeviceInfo{} }

func (c *USBConn) Read([]byte, time.Duration) (int, error) { return 0, errUnsupported }

func (c *USBConn) Write([]byte, time.Duration) (int, error) { return 0, errUnsupported }

func (c *USBConn) Close() error { return nil }
