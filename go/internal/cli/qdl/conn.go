package qdl

import (
	"errors"
	"time"
)

// Conn is a bulk transport to a device in EDL mode. Timeouts are per-call
// because commands answer in milliseconds while a data chunk can take a minute.
// Read must wrap ErrTimeout on silence: the protocol layers poll.
type Conn interface {
	Read(p []byte, timeout time.Duration) (int, error)
	Write(p []byte, timeout time.Duration) (int, error)
	Close() error
}

// ErrUSBAccess reports that the OS refused access to a device in EDL mode —
// on Linux a missing udev rule, on macOS a driver that already claimed it,
// or on Windows a missing/inaccessible WinUSB binding.
var ErrUSBAccess = errors.New("qdl: the OS refused access to the device in EDL mode")
