//go:build !windows

package t234

import "os"

// prepareRawTarget has no additional IOCTL step off Windows. Linux claims
// the disk exclusively when opening it; macOS uses DiskArbitration claims
// and unmountUMSDisk before opening its raw device.
func prepareRawTarget(dev *os.File) error { return nil }
