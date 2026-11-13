//go:build !linux && !darwin && !windows

package linklocal

import (
	"errors"
	"net"
)

// bindToInterface is unsupported here; callers fall back to unpinned sockets.
func bindToInterface(uintptr, net.Interface) error {
	return errors.ErrUnsupported
}
