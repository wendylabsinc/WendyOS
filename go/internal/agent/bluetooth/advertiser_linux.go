//go:build linux

package bluetooth

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
	"go.uber.org/zap"
)

const (
	wendyServiceUUID    = "7565e9eb-4c20-4b67-9272-d708b397b631"
	advObjectPath       = dbus.ObjectPath("/org/wendy/advertisement0")
	bluezService        = "org.bluez"
	defaultBluezAdapter = "/org/bluez/hci0"
	advManagerIface     = "org.bluez.LEAdvertisingManager1"
	advIface            = "org.bluez.LEAdvertisement1"
)

func bluezAdapterPath() string {
	if p := os.Getenv("WENDY_BT_ADAPTER"); p != "" {
		return p
	}
	return defaultBluezAdapter
}


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
	// Note: "Discoverable" is intentionally omitted — it is not part of the core
	// LEAdvertisement1 spec and causes registration failures on some BlueZ versions.
	// Scanners discover the device via ServiceUUIDs regardless.
	propsSpec := map[string]map[string]*prop.Prop{
		advIface: {
			"Type":         {Value: "peripheral", Writable: false, Emit: prop.EmitFalse},
			"ServiceUUIDs": {Value: []string{wendyServiceUUID}, Writable: false, Emit: prop.EmitFalse},
			"LocalName":    {Value: hostname, Writable: false, Emit: prop.EmitFalse},
		},
	}
	if _, err := prop.Export(conn, advObjectPath, propsSpec); err != nil {
		conn.Close()
		return fmt.Errorf("export advertisement properties: %w", err)
	}

	hci := conn.Object(bluezService, dbus.ObjectPath(bluezAdapterPath()))

	// Ensure the adapter is powered on. The call is a no-op if it already is,
	// but it also clears Command Disallowed state that lingers after a previous
	// BLE connection wasn't fully torn down at the HCI level.
	if call := hci.Call("org.freedesktop.DBus.Properties.Set", 0,
		"org.bluez.Adapter1", "Powered", dbus.MakeVariant(true)); call.Err != nil {
		logger.Warn("BLE adapter power-on failed", zap.Error(call.Err))
	}

	// Defensive unregister: if a previous run crashed without unregistering, the
	// slot may still be held in BlueZ. Ignore the error — it just means it wasn't
	// registered, which is the normal case.
	hci.Call(advManagerIface+".UnregisterAdvertisement", 0, advObjectPath)

	// Register advertisement with BlueZ. Retry for up to 30 seconds: BlueZ may
	// need time to finish adapter initialisation after power-on, and some
	// controllers need a moment to settle after clearing stale connections.
	opts := map[string]dbus.Variant{}
	var regErr error
	for i := range 30 {
		call := hci.Call(advManagerIface+".RegisterAdvertisement", 0, advObjectPath, opts)
		if call.Err == nil {
			regErr = nil
			break
		}
		regErr = call.Err
		// Log the D-Bus error name on the first attempt so the operator can see
		// the exact BlueZ error (e.g. org.bluez.Error.Failed vs NotPermitted).
		if i == 0 {
			if dbusErr, ok := call.Err.(*dbus.Error); ok {
				logger.Debug("BLE advertisement registration attempt failed",
					zap.String("dbus_error", dbusErr.Name),
					zap.Error(call.Err))
			}
		}
		select {
		case <-ctx.Done():
			conn.Close()
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if regErr != nil {
		conn.Close()
		return fmt.Errorf("register advertisement: %w", regErr)
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
