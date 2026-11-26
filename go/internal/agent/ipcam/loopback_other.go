//go:build !linux

package ipcam

import (
	"context"
	"errors"
)

// v4l2loopback is Linux kernel functionality. Every other platform this
// agent builds for — chiefly macOS, for local development — gets deps that
// fail every operation the same way, so Loopback.Available degrades exactly
// as it would on a WendyOS build with the module missing: `camera view` still
// works, container mirroring does not. This mirrors dhcpsock_other.go's
// approach to the same problem for the DHCP link layer.

var errLoopbackUnsupported = errors.New("v4l2loopback is only supported on Linux")

func defaultLoopbackDeps() loopbackDeps {
	return loopbackDeps{
		statControl:  func() error { return errLoopbackUnsupported },
		modprobe:     func(context.Context) error { return errLoopbackUnsupported },
		addNode:      func(int, string) error { return errLoopbackUnsupported },
		addShareNode: func(int, string) error { return errLoopbackUnsupported },
		removeNode:   func(int) error { return errLoopbackUnsupported },
		nodeExists:   func(int) bool { return false },
	}
}
