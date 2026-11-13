//go:build linux

package bleprovider

import (
	"errors"
	"fmt"

	"github.com/godbus/dbus/v5"
)

var errBLEAdapterUnavailable = errors.New("BLE adapter unavailable")

// BlueZ can keep serving its object tree after a controller error powers the
// adapter off. Successful discovery queries alone do not prove carrier health.
func requirePoweredAdapter(objects managedObjects, adapter dbus.ObjectPath) error {
	powered, ok := objects[adapter][adapterInterface]["Powered"].Value().(bool)
	if !ok {
		return fmt.Errorf("%w: %s has no powered state", errBLEAdapterUnavailable, adapter)
	}
	if !powered {
		return fmt.Errorf("%w: %s is powered off", errBLEAdapterUnavailable, adapter)
	}
	return nil
}
