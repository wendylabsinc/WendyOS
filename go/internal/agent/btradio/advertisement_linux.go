//go:build linux

package btradio

import (
	"context"
	"fmt"
	"slices"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
)

// ApplyAdvertisement updates only the caller's instance and never silently
// discards an explicit power request. Established connections remain intact.
func ApplyAdvertisement(ctx context.Context, bus *dbus.Conn, adapter, path dbus.ObjectPath, iface, name string, properties map[string]*prop.Prop) (err error) {
	return ApplyAdvertisementSnapshot(ctx, bus, adapter, path, iface, name, func() (map[string]*prop.Prop, error) { return properties, nil })
}

func ApplyAdvertisementSnapshot(ctx context.Context, bus *dbus.Conn, adapter, path dbus.ObjectPath, iface, name string, snapshot func() (map[string]*prop.Prop, error)) (err error) {
	defer func() {
		if err != nil {
			ReportFailure(name, err)
		}
	}()
	return WithAdvertisementSnapshot(ctx, snapshot, func(properties map[string]*prop.Prop) error {
		return applyAdvertisement(ctx, bus, adapter, path, iface, name, properties)
	})
}

func applyAdvertisement(ctx context.Context, bus *dbus.Conn, adapter, path dbus.ObjectPath, iface, name string, properties map[string]*prop.Prop) (err error) {
	var requested *int16
	unregistered := false
	defer func() {
		if err != nil && !unregistered {
			ReportFailure(name, err)
		} else {
			Report(name, requested, err)
		}
	}()
	obj := bus.Object("org.bluez", adapter)
	const manager = "org.bluez.LEAdvertisingManager1"
	var values map[string]dbus.Variant
	if err = obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", 0, manager).Store(&values); err != nil {
		p, policyErr := Desired()
		if policyErr != nil {
			return policyErr
		}
		if p.Mode != "default" {
			return err
		}
		// Optional capability inspection must not break ordinary advertising on
		// older BlueZ versions. Explicit power still requires known capabilities.
		err = nil
	}
	features, _ := values["SupportedFeatures"].Value().([]string)
	caps, _ := values["SupportedCapabilities"].Value().(map[string]dbus.Variant)
	min, minOK := caps["MinTxPower"].Value().(int16)
	max, maxOK := caps["MaxTxPower"].Value().(int16)
	requested, err = Request(slices.Contains(features, "CanSetTxPower") && minOK && maxOK, int32(min), int32(max))
	if err != nil {
		return err
	}
	valuesCopy := make(map[string]*prop.Prop, len(properties)+1)
	for k, v := range properties {
		if k != "TxPower" {
			valuesCopy[k] = v
		}
	}
	if requested != nil {
		valuesCopy["TxPower"] = &prop.Prop{Value: *requested, Writable: false, Emit: prop.EmitFalse}
	}
	call := obj.CallWithContext(ctx, manager+".UnregisterAdvertisement", 0, path)
	if call.Err != nil {
		de, ok := call.Err.(dbus.Error)
		if !ok || de.Name != "org.bluez.Error.DoesNotExist" {
			return fmt.Errorf("unregister %s: %w", name, call.Err)
		}
	}
	unregistered = true
	return registerAdvertisement(valuesCopy, func(p map[string]*prop.Prop) error {
		_, e := prop.Export(bus, path, map[string]map[string]*prop.Prop{iface: p})
		return e
	}, func() error {
		return obj.CallWithContext(ctx, manager+".RegisterAdvertisement", 0, path, map[string]dbus.Variant{}).Err
	})
}
