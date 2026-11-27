//go:build linux

package bleprovider

import (
	"errors"
	"fmt"
	"os"

	"github.com/godbus/dbus/v5"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

var errBLEAdapterUnavailable = errors.New("BLE adapter unavailable")

// BlueZ may have no D-Bus owner on hosts without an HCI controller. An
// unavailable D-Bus service is a setup fault when a controller is present.
func bluezStartupError(err error, adapterDirectory string) error {
	entries, scanErr := os.ReadDir(adapterDirectory)
	if scanErr != nil && !errors.Is(scanErr, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		if _, indexErr := adapterHCIIndex(dbus.ObjectPath("/org/bluez/" + entry.Name())); indexErr == nil {
			return err
		}
	}
	return fmt.Errorf("%w: no Bluetooth HCI adapter", localmesh.ErrNoCarrierHardware)
}

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
