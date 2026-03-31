//go:build linux

package bluetooth

import (
	"context"
	"fmt"
	"os"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
	"go.uber.org/zap"
)

const (
	wendyServiceUUID = "7565e9eb-4c20-4b67-9272-d708b397b631"
	advObjectPath    = dbus.ObjectPath("/org/wendy/advertisement0")
	bluezService     = "org.bluez"
	bluezHCI0        = "/org/bluez/hci0"
	advManagerIface  = "org.bluez.LEAdvertisingManager1"
	advIface         = "org.bluez.LEAdvertisement1"
)

// advertisement implements org.bluez.LEAdvertisement1 on D-Bus.
type advertisement struct{}

// Release is called by BlueZ when the advertisement is unregistered.
func (a *advertisement) Release() *dbus.Error { return nil }

func startAdvertising(ctx context.Context, logger *zap.Logger) error {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return fmt.Errorf("connect system bus: %w", err)
	}

	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "WendyOS"
	}

	// Export the LEAdvertisement1 object.
	if err := conn.Export(&advertisement{}, advObjectPath, advIface); err != nil {
		conn.Close()
		return fmt.Errorf("export advertisement object: %w", err)
	}

	// Export properties via the prop subpackage.
	propsSpec := map[string]map[string]*prop.Prop{
		advIface: {
			"Type":         {Value: "peripheral", Writable: false, Emit: prop.EmitFalse},
			"ServiceUUIDs": {Value: []string{wendyServiceUUID}, Writable: false, Emit: prop.EmitFalse},
			"LocalName":    {Value: hostname, Writable: false, Emit: prop.EmitFalse},
			"Discoverable": {Value: true, Writable: false, Emit: prop.EmitFalse},
		},
	}
	if _, err := prop.Export(conn, advObjectPath, propsSpec); err != nil {
		conn.Close()
		return fmt.Errorf("export advertisement properties: %w", err)
	}

	// Register advertisement with BlueZ.
	hci := conn.Object(bluezService, bluezHCI0)
	opts := map[string]dbus.Variant{} // no extra options
	if call := hci.Call(advManagerIface+".RegisterAdvertisement", 0, advObjectPath, opts); call.Err != nil {
		conn.Close()
		return fmt.Errorf("register advertisement: %w", call.Err)
	}

	logger.Info("BLE advertisement registered", zap.String("uuid", wendyServiceUUID), zap.String("name", hostname))

	// Wait for context cancellation, then unregister.
	go func() {
		<-ctx.Done()
		if call := hci.Call(advManagerIface+".UnregisterAdvertisement", 0, advObjectPath); call.Err != nil {
			logger.Warn("unregister advertisement failed", zap.Error(call.Err))
		}
		conn.Close()
		logger.Info("BLE advertisement unregistered")
	}()

	return nil
}
