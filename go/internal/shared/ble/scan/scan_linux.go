//go:build linux

package scan

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/wendylabsinc/wendy/go/internal/shared/ble/bluez"
)

// snapshotTimeout bounds a single GetManagedObjects round trip, matching the
// D-Bus timeouts the sibling central package uses for BlueZ calls that can
// hang (gattOpTimeout, stopDiscoveryTimeout). Without it, a wedged bluetoothd
// would stall the sampling loop indefinitely even when the caller never
// cancels — the loop calls Snapshot synchronously on every tick.
const snapshotTimeout = 5 * time.Second

// RunBLECheck is a no-op on Linux. The CoreBluetooth entitlement problem it
// exists for is macOS-only, and BlueZ reports adapter trouble as an ordinary
// error from newScanner instead of aborting the process.
func RunBLECheck() int { return 0 }

// linuxScanner holds a BlueZ discovery session. The D-Bus plumbing lives in
// the sibling bluez package, which the central's GATT client shares.
type linuxScanner struct {
	mu           sync.Mutex
	conn         *dbus.Conn
	adapterPath  string
	closed       bool
	limit        int
	observations linuxObservations
}

// newScanner powers on an adapter and starts LE discovery, filtered to the
// requested services where BlueZ can do it natively.
func newScanner(ctx context.Context, services []string) (scanner, error) {
	s := &linuxScanner{}
	observer := &linuxAdvertisementHandler{observations: &s.observations}
	conn, err := dbus.ConnectSystemBus(dbus.WithSignalHandler(observer))
	if err != nil {
		return nil, fmt.Errorf("connecting to system bus: %w", err)
	}

	managed, err := bluez.GetManagedObjects(ctx, conn)
	if err != nil {
		conn.Close() //nolint:errcheck
		return nil, err
	}

	adapterPath, err := bluez.ResolveAdapterPath(managed)
	if err != nil {
		conn.Close() //nolint:errcheck
		return nil, err
	}

	if err := bluez.PowerOn(ctx, conn, adapterPath); err != nil {
		conn.Close() //nolint:errcheck
		return nil, fmt.Errorf("powering on adapter %s: %w", adapterPath, err)
	}

	s.conn = conn
	s.adapterPath = adapterPath
	observer.setAdapter(adapterPath)
	if err := conn.AddMatchSignalContext(ctx, dbus.WithMatchSender(bluez.Service), dbus.WithMatchInterface(bluez.PropsIface), dbus.WithMatchMember("PropertiesChanged"), dbus.WithMatchPathNamespace(dbus.ObjectPath(adapterPath))); err != nil {
		conn.Close()
		return nil, fmt.Errorf("subscribing to advertisement observations: %w", err)
	}
	adapter := conn.Object(bluez.Service, dbus.ObjectPath(adapterPath))

	// Ask BlueZ to filter for us. Transport "le" keeps classic Bluetooth out,
	// DuplicateData keeps advertisement fields refreshing rather than being
	// reported once, and UUIDs is BlueZ's own service filter. The engine still
	// matches in Go so behavior is identical on every platform.
	filter := map[string]dbus.Variant{
		"Transport":     dbus.MakeVariant("le"),
		"DuplicateData": dbus.MakeVariant(true),
	}
	if len(services) > 0 {
		filter["UUIDs"] = dbus.MakeVariant(services)
	}
	if call := adapter.CallWithContext(ctx, bluez.AdapterIface+".SetDiscoveryFilter", 0, filter); call.Err != nil {
		// Not fatal: an older BlueZ may reject a key, and an unfiltered scan
		// still produces correct results once the engine filters in Go.
	}

	if call := adapter.CallWithContext(ctx, bluez.AdapterIface+".StartDiscovery", 0); call.Err != nil {
		conn.Close() //nolint:errcheck
		return nil, fmt.Errorf("starting discovery on %s: %w", adapterPath, call.Err)
	}

	return s, nil
}

// Snapshot returns genuine advertisement observations for bounded background
// scans. Interactive scans additionally read the cached object tree for display
// names and RSSI; cached properties never acquire advertisement timestamps.
//
// ctx is wrapped with snapshotTimeout rather than passed straight through: that
// way both a caller cancellation and a bluetoothd that simply stops replying
// unblock this call, instead of only the former.
func (s *linuxScanner) Snapshot(ctx context.Context) ([]BLEDeviceInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, fmt.Errorf("BLE scan session is closed")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if s.limit > 0 {
		return s.observations.snapshot(s.limit), nil
	}

	ctx, cancel := context.WithTimeout(ctx, snapshotTimeout)
	defer cancel()
	managed, err := bluez.GetManagedObjects(ctx, s.conn)
	if err != nil {
		return nil, err
	}

	// Device objects are nested under the adapter, e.g.
	// /org/bluez/hci0/dev_XX_XX_XX_XX_XX_XX.
	prefix := s.adapterPath + "/"
	var devices []BLEDeviceInfo
	for path, ifaces := range managed {
		props, ok := ifaces[bluez.DeviceIface]
		if !ok || !strings.HasPrefix(string(path), prefix) {
			continue
		}
		address, ok := bluez.StringProp(props, "Address")
		if !ok || address == "" {
			continue
		}
		device := s.observations.device(path)
		device.Address = address
		device.Name = deviceName(props, address)
		device.RSSI = bluez.RSSIProp(props)
		device.ServiceUUIDs = canonicalUUIDs(append(bluez.StringsProp(props, "UUIDs"), device.ServiceUUIDs...))
		devices = append(devices, device)
	}
	return devices, nil
}

// Close stops discovery and drops the bus connection. Idempotent.
func (s *linuxScanner) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true

	adapter := s.conn.Object(bluez.Service, dbus.ObjectPath(s.adapterPath))
	// Best-effort: the adapter may already have gone away.
	stop, cancel := context.WithTimeout(context.Background(), snapshotTimeout)
	_ = adapter.CallWithContext(stop, bluez.AdapterIface+".StopDiscovery", 0).Err
	cancel()
	s.conn.Close()
}

// deviceName resolves the display name. Alias is the user-facing value and
// falls back to Name, but BlueZ synthesizes an Alias for devices advertising no
// name, which must not count as a name or every anonymous device would appear
// named.
func deviceName(props map[string]dbus.Variant, address string) string {
	if alias, ok := bluez.StringProp(props, "Alias"); ok && alias != "" && !bluez.IsDefaultAlias(alias, address) {
		return alias
	}
	if name, ok := bluez.StringProp(props, "Name"); ok {
		return name
	}
	return ""
}

// A bounded background scan uses genuine signal observations directly instead
// of copying BlueZ's potentially long-lived cache into a fresh-looking roster.
// Existing unbounded picker scans keep their established cached display view.
func (s *linuxScanner) setDeviceLimit(limit int) { s.mu.Lock(); s.limit = limit; s.mu.Unlock() }
