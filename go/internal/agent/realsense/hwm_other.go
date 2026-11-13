//go:build !linux

package realsense

import (
	"errors"
	"runtime"
)

// nodeTransport exists on every platform so the package's types and parsers
// stay portable; the live UVC transport is Linux-only.
type nodeTransport struct{}

func OpenTransport(devPath string) (*nodeTransport, error) {
	return nil, errors.New("realsense: UVC calibration read is not supported on " + runtime.GOOS)
}

func (t *nodeTransport) Close() error { return nil }

func (t *nodeTransport) Exchange(tx []byte) ([]byte, error) {
	return nil, errors.New("realsense: UVC calibration read is not supported on " + runtime.GOOS)
}
