//go:build linux

package bluetooth

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
	"github.com/wendylabsinc/wendy/go/internal/agent/btradio"
	"go.uber.org/zap"
)

const (
	wendyServiceUUID = "7565e9eb-4c20-4b67-9272-d708b397b631"
	advObjectPath    = dbus.ObjectPath("/org/wendy/advertisement0")
	bluezService     = "org.bluez"
	advManagerIface  = "org.bluez.LEAdvertisingManager1"
	advIface         = "org.bluez.LEAdvertisement1"
)

// advertisingAdapterPath selects the BlueZ adapter to advertise on. If
// WENDY_BT_ADAPTER is set, that path is trusted verbatim. Otherwise it
// enumerates BlueZ's managed objects and returns the first adapter that
// implements org.bluez.LEAdvertisingManager1 — the interface that exposes
// RegisterAdvertisement.
//
// The default /org/bluez/hci0 is not always the advertising-capable
// controller: the onboard radio can enumerate as a higher hciN, or a USB
// dongle may be the only LE-capable adapter. Calling RegisterAdvertisement on
// an adapter that lacks the interface surfaces as a confusing
// "RegisterAdvertisement ... doesn't exist" D-Bus error, so discover the right
// adapter up front and fail fast with a clear message when none supports it.
func advertisingAdapterPath(conn *dbus.Conn) (string, error) {
	if p := os.Getenv("WENDY_BT_ADAPTER"); p != "" {
		return p, nil
	}

	managed, err := getManagedObjects(context.Background(), conn)
	if err != nil {
		return "", err
	}

	if p := findAdvertisingAdapter(managed); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("no bluetooth adapter implements %s (LE advertising unsupported)", advManagerIface)
}

// findAdapterByInterface returns the path of the adapter implementing iface,
// or "" if none do. When several adapters qualify it returns the
// lexicographically lowest path so selection is stable across runs despite
// Go's randomised map iteration order.
func findAdapterByInterface(managed managedObjects, iface string) string {
	var best string
	for path, ifaces := range managed {
		if _, ok := ifaces[iface]; !ok {
			continue
		}
		if s := string(path); best == "" || s < best {
			best = s
		}
	}
	return best
}

// findAdvertisingAdapter returns the path of the adapter implementing
// org.bluez.LEAdvertisingManager1, or "" if none do.
func findAdvertisingAdapter(managed managedObjects) string {
	return findAdapterByInterface(managed, advManagerIface)
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

	adapterPath, err := advertisingAdapterPath(conn)
	if err != nil {
		conn.Close()
		return err
	}
	hci := conn.Object(bluezService, dbus.ObjectPath(adapterPath))

	// Ensure the adapter is powered on before advertising.
	if err := powerOnAdapter(ctx, conn, adapterPath); err != nil {
		logger.Warn("BLE adapter power-on failed", zap.Error(err))
	}

	// Defensive unregister: if a previous run crashed without unregistering, the
	// slot may still be held in BlueZ. Ignore the error — it just means it wasn't
	// registered, which is the normal case.
	hci.Call(advManagerIface+".UnregisterAdvertisement", 0, advObjectPath)

	// Register advertisement with BlueZ. Retry for up to 30 seconds: BlueZ may
	// need time to finish adapter initialisation after power-on, and some
	// controllers need a moment to settle after clearing stale connections.
	var regErr error
	var unsubscribe func()
	for i := range 30 {
		nextUnsubscribe, applyErr := btradio.Register(ctx, "management", func(applyCtx context.Context) error {
			return btradio.ApplyAdvertisement(applyCtx, conn, dbus.ObjectPath(adapterPath), advObjectPath, advIface, "management", propsSpec[advIface])
		})
		if nextUnsubscribe != nil {
			unsubscribe = nextUnsubscribe
		}
		if applyErr == nil {
			regErr = nil
			break
		}
		regErr = applyErr
		// Log the D-Bus error name on the first attempt so the operator can see
		// the exact BlueZ error (e.g. org.bluez.Error.Failed vs NotPermitted).
		if i == 0 {
			if name, _, ok := dbusErrorInfo(applyErr); ok {
				logger.Debug("BLE advertisement registration attempt failed",
					zap.String("dbus_error", name),
					zap.Error(applyErr))
			}
		}
		select {
		case <-ctx.Done():
			if unsubscribe != nil {
				unsubscribe()
			}
			conn.Close()
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	if regErr != nil {
		// Retain the live subscription so correcting an unsupported saved policy
		// can recover advertising without an agent restart.
		logger.Warn("BLE advertisement inactive; radio policy changes can retry it", zap.Error(regErr))
	} else {
		logger.Info("BLE advertisement registered", zap.String("adapter", adapterPath),
			zap.String("uuid", wendyServiceUUID), zap.String("name", hostname))
	}
	// Wait for context cancellation, then unregister.
	go func() {
		<-ctx.Done()
		if unsubscribe != nil {
			unsubscribe()
		}
		if call := hci.Call(advManagerIface+".UnregisterAdvertisement", 0, advObjectPath); call.Err != nil {
			logger.Warn("unregister advertisement failed", zap.Error(call.Err))
		}
		conn.Close()
		logger.Info("BLE advertisement unregistered")
	}()

	return nil
}
