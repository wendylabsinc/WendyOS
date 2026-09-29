//go:build linux

package bleprovider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
	"go.uber.org/zap"
)

const (
	bluezName              = "org.bluez"
	adapterInterface       = "org.bluez.Adapter1"
	deviceInterface        = "org.bluez.Device1"
	advertManagerInterface = "org.bluez.LEAdvertisingManager1"
	advertInterface        = "org.bluez.LEAdvertisement1"
	advertPath             = dbus.ObjectPath("/org/wendy/mesh/advertisement")
)

var errBlueZOwnerChanged = errors.New("BlueZ service owner changed")

// BlueZ loses per-client discovery and advertisement registrations when its
// daemon restarts. GetManagedObjects can succeed against the new daemon, so a
// successful scan alone cannot tell the provider that it must register again.
func bluezOwner(ctx context.Context, bus *dbus.Conn) (string, error) {
	var owner string
	err := bus.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, bluezName).Store(&owner)
	if err != nil {
		return "", fmt.Errorf("BlueZ service owner: %w", err)
	}
	if owner == "" {
		return "", errors.New("BlueZ service has no owner")
	}
	return owner, nil
}

func verifyBlueZOwner(expected, current string) error {
	if expected == "" || current == "" || expected != current {
		return fmt.Errorf("%w: %q -> %q", errBlueZOwnerChanged, expected, current)
	}
	return nil
}

type managedObjects map[dbus.ObjectPath]map[string]map[string]dbus.Variant

type candidate struct {
	asset                int32
	address, addressType string
	path                 dbus.ObjectPath
	psm                  uint16
}

func candidates(objects managedObjects, adapter dbus.ObjectPath, uuid, meshName string, self int32) []candidate {
	peers := make(map[int32]candidate)
	for path, ifaces := range objects {
		if !strings.HasPrefix(string(path), string(adapter)+"/dev_") {
			continue
		}
		props, ok := ifaces[deviceInterface]
		if !ok {
			continue
		}
		address, ok := variantString(props["Address"])
		if !ok {
			continue
		}
		kind, ok := variantString(props["AddressType"])
		if !ok {
			continue
		}
		adv, ok := matchingServiceData(serviceData(props["ServiceData"]), uuid, meshName)
		if !ok || adv.Asset >= self {
			continue
		}
		if _, _, err := parseAddress(address, kind, adv.PSM); err != nil {
			continue
		}
		if prior, exists := peers[adv.Asset]; !exists || address < prior.address {
			peers[adv.Asset] = candidate{asset: adv.Asset, address: address, addressType: kind, path: path, psm: adv.PSM}
		}
	}
	out := make([]candidate, 0, len(peers))
	for _, p := range peers {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].asset < out[j].asset })
	return out
}

func variantString(v dbus.Variant) (string, bool) {
	s, ok := v.Value().(string)
	return s, ok && s != ""
}
func serviceData(v dbus.Variant) map[string][]byte {
	out := make(map[string][]byte)
	switch data := v.Value().(type) {
	case map[string]dbus.Variant:
		for key, value := range data {
			if b, ok := value.Value().([]byte); ok {
				out[key] = b
			}
		}
	case map[string][]byte:
		for key, value := range data {
			out[key] = value
		}
	}
	return out
}

func getManaged(ctx context.Context, bus *dbus.Conn) (managedObjects, error) {
	var objects managedObjects
	err := bus.Object(bluezName, "/").CallWithContext(ctx, "org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&objects)
	return objects, err
}

func selectAdapter(ctx context.Context, bus *dbus.Conn, requested string) (dbus.ObjectPath, error) {
	objects, err := getManaged(ctx, bus)
	if err != nil {
		return "", fmt.Errorf("BlueZ adapters: %w", err)
	}
	var paths []string
	for path, ifaces := range objects {
		if _, ok := ifaces[adapterInterface]; !ok {
			continue
		}
		if _, ok := ifaces[advertManagerInterface]; !ok {
			continue
		}
		paths = append(paths, string(path))
	}
	sort.Strings(paths)
	for _, path := range paths {
		if requested == "" || requested == path {
			return dbus.ObjectPath(path), nil
		}
	}
	return "", errors.New("no BlueZ adapter supports both LE advertising and discovery")
}

type advertisement struct{}

func (*advertisement) Release() *dbus.Error { return nil }

// BlueZ accepts a requested advertising-set power only when the manager
// advertises CanSetTxPower. This setting does not control connected LE links.
func advertisementTxPower(manager map[string]dbus.Variant) (int16, bool) {
	features, ok := manager["SupportedFeatures"].Value().([]string)
	if !ok {
		return 0, false
	}
	canSet := false
	for _, feature := range features {
		if feature == "CanSetTxPower" {
			canSet = true
			break
		}
	}
	if !canSet {
		return 0, false
	}
	capabilities, ok := manager["SupportedCapabilities"].Value().(map[string]dbus.Variant)
	if !ok {
		return 0, false
	}
	maximum, ok := capabilities["MaxTxPower"].Value().(int16)
	if !ok || maximum < -127 {
		return 0, false
	}
	if maximum > 20 {
		maximum = 20
	}
	return maximum, true
}

func advertisementProperties(uuid string, payload []byte, manager map[string]dbus.Variant) map[string]*prop.Prop {
	properties := map[string]*prop.Prop{
		"Type":        {Value: "peripheral", Writable: false, Emit: prop.EmitFalse},
		"ServiceData": {Value: map[string]dbus.Variant{uuid: dbus.MakeVariant(payload)}, Writable: false, Emit: prop.EmitFalse},
		// Relaxed advertising cadence: 100-200ms intervals (up to ~10
		// advertisements/second) are plenty for discovery and keep
		// advertising airtime negligible next to scanning and ACLs.
		"MinInterval": {Value: uint16(160), Writable: false, Emit: prop.EmitFalse},
		"MaxInterval": {Value: uint16(320), Writable: false, Emit: prop.EmitFalse},
	}
	if power, ok := advertisementTxPower(manager); ok {
		properties["TxPower"] = &prop.Prop{Value: power, Writable: false, Emit: prop.EmitFalse}
	}
	return properties
}

func registerAdvertisementWithFallback(properties map[string]*prop.Prop, export func(map[string]*prop.Prop) error, register func() error) error {
	if err := export(properties); err != nil {
		return fmt.Errorf("exporting BLE advertisement: %w", err)
	}
	if err := register(); err != nil {
		if _, requested := properties["TxPower"]; !requested {
			return fmt.Errorf("registering BLE advertisement: %w", err)
		}
		// A controller or BlueZ version may report support yet reject this
		// optional request. Retry at the controller's default power.
		delete(properties, "TxPower")
		if exportErr := export(properties); exportErr != nil {
			return fmt.Errorf("registering BLE advertisement with TxPower: %w; fallback export: %v", err, exportErr)
		}
		if retryErr := register(); retryErr != nil {
			return fmt.Errorf("registering BLE advertisement with TxPower: %w; fallback: %v", err, retryErr)
		}
	}
	return nil
}

func setupBlueZ(ctx context.Context, bus *dbus.Conn, adapter dbus.ObjectPath, uuid string, payload []byte) error {
	obj := bus.Object(bluezName, adapter)
	if err := obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Set", 0, adapterInterface, "Powered", dbus.MakeVariant(true)).Err; err != nil {
		return fmt.Errorf("powering BLE adapter: %w", err)
	}
	// These optional manager properties are absent on some BlueZ versions and
	// controllers. A failed lookup must not prevent ordinary BLE advertising.
	var manager map[string]dbus.Variant
	_ = obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", 0, advertManagerInterface).Store(&manager)
	if err := bus.Export(&advertisement{}, advertPath, advertInterface); err != nil {
		return err
	}
	properties := advertisementProperties(uuid, payload, manager)
	if err := registerAdvertisementWithFallback(properties, func(p map[string]*prop.Prop) error {
		_, err := prop.Export(bus, advertPath, map[string]map[string]*prop.Prop{advertInterface: p})
		return err
	}, func() error {
		return obj.CallWithContext(ctx, advertManagerInterface+".RegisterAdvertisement", 0, advertPath, map[string]dbus.Variant{}).Err
	}); err != nil {
		return err
	}
	if err := obj.CallWithContext(ctx, adapterInterface+".SetDiscoveryFilter", 0, meshDiscoveryFilter()).Err; err != nil {
		_ = obj.CallWithContext(ctx, advertManagerInterface+".UnregisterAdvertisement", 0, advertPath).Err
		return fmt.Errorf("setting BLE discovery filter: %w", err)
	}
	if err := obj.CallWithContext(ctx, adapterInterface+".StartDiscovery", 0).Err; err != nil {
		_ = obj.CallWithContext(ctx, advertManagerInterface+".UnregisterAdvertisement", 0, advertPath).Err
		return fmt.Errorf("starting BLE discovery: %w", err)
	}
	return nil
}

func cleanupBlueZ(bus *dbus.Conn, adapter dbus.ObjectPath, logger *zap.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	obj := bus.Object(bluezName, adapter)
	if err := obj.CallWithContext(ctx, adapterInterface+".StopDiscovery", 0).Err; err != nil {
		logger.Debug("BLE StopDiscovery failed", zap.Error(err))
	}
	if err := obj.CallWithContext(ctx, advertManagerInterface+".UnregisterAdvertisement", 0, advertPath).Err; err != nil {
		logger.Debug("BLE UnregisterAdvertisement failed", zap.Error(err))
	}
}
