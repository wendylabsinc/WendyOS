//go:build linux

package bluez

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/godbus/dbus/v5"
)

// BlueZ D-Bus names.
const (
	Service      = "org.bluez"
	AdapterIface = "org.bluez.Adapter1"
	DeviceIface  = "org.bluez.Device1"

	GattServiceIface = "org.bluez.GattService1"
	GattCharIface    = "org.bluez.GattCharacteristic1"

	PropsIface       = "org.freedesktop.DBus.Properties"
	PropsChangedName = PropsIface + ".PropertiesChanged"
	ObjectManagerGet = "org.freedesktop.DBus.ObjectManager.GetManagedObjects"
)

// AdapterEnvVar pins a controller when a host has several. It is the one
// product-specific name in this package, and has to match what the agent reads.
const AdapterEnvVar = "WENDY_BT_ADAPTER"

// ManagedObjects is the shape org.freedesktop.DBus.ObjectManager returns.
type ManagedObjects = map[dbus.ObjectPath]map[string]map[string]dbus.Variant

// GetManagedObjects enumerates BlueZ's whole object tree: adapters, devices,
// and — once a device's services are resolved — its GATT services,
// characteristics and descriptors.
func GetManagedObjects(ctx context.Context, conn *dbus.Conn) (ManagedObjects, error) {
	var managed ManagedObjects
	root := conn.Object(Service, "/")
	if err := root.CallWithContext(ctx, ObjectManagerGet, 0).Store(&managed); err != nil {
		return nil, fmt.Errorf("enumerating BlueZ objects: %w", err)
	}
	return managed, nil
}

// ResolveAdapterPath selects the adapter to operate on. The onboard radio is
// not always hci0 — it can enumerate higher, or a USB dongle may be the only
// controller — so the path is discovered rather than assumed, and the lowest
// one wins so repeated runs pick the same controller on a host with several.
//
// AdapterEnvVar pins a specific controller, by full object path or by bare name
// ("hci1"). Unlike the agent's older copy, an override naming an adapter that
// does not exist is an error rather than a path that will fail later with a
// worse message.
func ResolveAdapterPath(managed ManagedObjects) (string, error) {
	if want := os.Getenv(AdapterEnvVar); want != "" {
		for path, ifaces := range managed {
			if _, ok := ifaces[AdapterIface]; !ok {
				continue
			}
			if string(path) == want || strings.HasSuffix(string(path), "/"+want) {
				return string(path), nil
			}
		}
		return "", fmt.Errorf("no Bluetooth adapter matching %s=%q", AdapterEnvVar, want)
	}
	if p := FindAdapterByInterface(managed, AdapterIface); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("no Bluetooth adapter found (no object implements %s)", AdapterIface)
}

// FindAdapterByInterface returns the lowest object path implementing iface, or
// "" when none does. Taking the interface as an argument matters because an
// adapter that can scan is not necessarily one that can advertise or serve
// GATT — those are separate interfaces on the same object.
func FindAdapterByInterface(managed ManagedObjects, iface string) string {
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

// FindDeviceByAddress locates a device object by its Bluetooth address. The
// adapter comes from the device's own Adapter property rather than from the
// parent path, which is only a convention; the path prefix is the fallback.
// Lowest path wins so the choice is stable under Go's randomized map order.
func FindDeviceByAddress(managed ManagedObjects, address string) (devicePath dbus.ObjectPath, adapterPath string, props map[string]dbus.Variant, found bool) {
	var (
		bestPath  dbus.ObjectPath
		bestProps map[string]dbus.Variant
	)
	for path, ifaces := range managed {
		devProps, ok := ifaces[DeviceIface]
		if !ok {
			continue
		}
		addr, ok := StringProp(devProps, "Address")
		if !ok || !strings.EqualFold(addr, address) {
			continue
		}
		if bestPath == "" || path < bestPath {
			bestPath, bestProps = path, devProps
		}
	}
	if bestPath == "" {
		return "", "", nil, false
	}

	adapter := string(ObjectPathProp(bestProps, "Adapter"))
	if adapter == "" {
		if i := strings.LastIndex(string(bestPath), "/"); i > 0 {
			adapter = string(bestPath)[:i]
		}
	}
	return bestPath, adapter, bestProps, true
}

// RestrictToAdapter narrows a managed-objects map to the adapter at adapterPath
// and the objects nested under it, so an AdapterEnvVar override pins device
// lookups to the chosen controller. An empty adapterPath returns the input
// unchanged.
func RestrictToAdapter(managed ManagedObjects, adapterPath string) ManagedObjects {
	if adapterPath == "" {
		return managed
	}
	prefix := adapterPath + "/"
	restricted := ManagedObjects{}
	for path, ifaces := range managed {
		if string(path) == adapterPath || strings.HasPrefix(string(path), prefix) {
			restricted[path] = ifaces
		}
	}
	return restricted
}

// PowerOn powers the adapter on. The call is a no-op if it is already on, but
// it also clears Command Disallowed state left over from a previous BLE
// connection that wasn't fully torn down at the HCI level.
func PowerOn(ctx context.Context, conn *dbus.Conn, adapterPath string) error {
	adapter := conn.Object(Service, dbus.ObjectPath(adapterPath))
	return adapter.CallWithContext(ctx, PropsIface+".Set", 0,
		AdapterIface, "Powered", dbus.MakeVariant(true)).Err
}

// Caller is the slice of dbus.BusObject these helpers need. Narrowing it lets a
// caller substitute a fake and test its D-Bus interaction with no bus present;
// *dbus.Object satisfies it as-is.
type Caller interface {
	CallWithContext(ctx context.Context, method string, flags dbus.Flags, args ...any) *dbus.Call
}

// LiveBoolProp reads one boolean property straight off the bus rather than from
// a cached GetManagedObjects snapshot. ok is false when the read failed or the
// property is not a bool, which callers polling for a state change should treat
// as "not yet" rather than as false.
func LiveBoolProp(ctx context.Context, obj Caller, iface, prop string) (value, ok bool) {
	call := obj.CallWithContext(ctx, PropsIface+".Get", 0, iface, prop)
	if call.Err != nil {
		return false, false
	}
	var v dbus.Variant
	if call.Store(&v) != nil {
		return false, false
	}
	b, isBool := v.Value().(bool)
	return b, isBool
}

// ErrorInfo pulls the D-Bus error name and message out of a godbus error,
// through any wrapping. ok is false for an error that did not come from the
// bus at all.
func ErrorInfo(err error) (name, message string, ok bool) {
	var val dbus.Error
	if errors.As(err, &val) {
		return val.Name, firstStringBody(val.Body), true
	}
	var ptr *dbus.Error
	if errors.As(err, &ptr) && ptr != nil {
		return ptr.Name, firstStringBody(ptr.Body), true
	}
	return "", "", false
}

// IsErrorName reports whether err is a D-Bus error with one of the given names.
func IsErrorName(err error, names ...string) bool {
	got, _, ok := ErrorInfo(err)
	if !ok {
		return false
	}
	for _, want := range names {
		if got == want {
			return true
		}
	}
	return false
}

func firstStringBody(body []any) string {
	if len(body) > 0 {
		if s, ok := body[0].(string); ok {
			return s
		}
	}
	return ""
}

// ── Property readers ─────────────────────────────────────────────────────────
//
// Each yields the zero value for a missing property or a type mismatch, so a
// caller never has to distinguish "absent" from "wrong shape" — for BlueZ they
// mean the same thing, and StringProp's second result covers the one case
// (an empty-but-present name) where it matters.

func StringProp(props map[string]dbus.Variant, key string) (string, bool) {
	v, ok := props[key]
	if !ok {
		return "", false
	}
	s, ok := v.Value().(string)
	return s, ok
}

func BoolProp(props map[string]dbus.Variant, key string) bool {
	v, ok := props[key]
	if !ok {
		return false
	}
	b, _ := v.Value().(bool)
	return b
}

func StringsProp(props map[string]dbus.Variant, key string) []string {
	v, ok := props[key]
	if !ok {
		return nil
	}
	ss, _ := v.Value().([]string)
	return ss
}

func ObjectPathProp(props map[string]dbus.Variant, key string) dbus.ObjectPath {
	v, ok := props[key]
	if !ok {
		return ""
	}
	p, _ := v.Value().(dbus.ObjectPath)
	return p
}

func BytesProp(props map[string]dbus.Variant, key string) []byte {
	v, ok := props[key]
	if !ok {
		return nil
	}
	b, _ := v.Value().([]byte)
	return b
}

// Uint16Prop reads a uint16 property. BlueZ types GattCharacteristic1.MTU this
// way, and omits it entirely before 5.62.
func Uint16Prop(props map[string]dbus.Variant, key string) uint16 {
	v, ok := props[key]
	if !ok {
		return 0
	}
	n, _ := v.Value().(uint16)
	return n
}

// RSSIProp reads the RSSI property, which BlueZ types as int16 and omits
// entirely for a device it is not currently seeing.
func RSSIProp(props map[string]dbus.Variant) int {
	v, ok := props["RSSI"]
	if !ok {
		return 0
	}
	rssi, ok := v.Value().(int16)
	if !ok {
		return 0
	}
	return int(rssi)
}

// IsDefaultAlias reports whether an Alias is the placeholder BlueZ synthesizes
// for a device advertising no name — the address with ':' replaced by '-'.
// Without this check every anonymous device would appear named.
func IsDefaultAlias(alias, address string) bool {
	if address == "" {
		return false
	}
	return strings.EqualFold(alias, strings.ReplaceAll(address, ":", "-"))
}
