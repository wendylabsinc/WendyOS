//go:build linux

package bluetooth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/audio"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

const (
	adapterIface = "org.bluez.Adapter1"
	deviceIface  = "org.bluez.Device1"
	// A2DP profile UUIDs, named from the remote device's point of view:
	// a2dpSinkUUID is advertised by devices that receive audio (speakers,
	// headsets), a2dpSourceUUID by devices that send it (phones, microphones).
	a2dpSinkUUID   = "0000110b-0000-1000-8000-00805f9b34fb"
	a2dpSourceUUID = "0000110a-0000-1000-8000-00805f9b34fb"
	// scanDuration is how long discovery runs before results are collected.
	scanDuration = 8 * time.Second
	// quickScanDuration is how long to wait before sending an early, partial
	// batch of results, so a caller has something to show almost immediately
	// while discovery keeps running for the full scanDuration to find more
	// devices before the final, more thorough batch.
	quickScanDuration = 1 * time.Second
	// resolveDiscoveryTimeout bounds the on-demand discovery Connect runs when
	// the target device is not in BlueZ's cache (BlueZ evicts unpaired devices
	// ~30s after a scan stops, so this is the common case when connecting a
	// while after scanning). Worst case must stay well inside the CLI's 60s
	// connect timeout to leave room for pairing and connecting.
	resolveDiscoveryTimeout = 12 * time.Second
	// resolvePollInterval is how often on-demand discovery re-enumerates BlueZ
	// objects looking for the target device. There is no D-Bus signal plumbing
	// in this codebase, so polling keeps the interaction synchronous; devices
	// typically appear a few hundred ms into discovery, so a sub-second poll
	// keeps the added connect latency small.
	resolvePollInterval = 500 * time.Millisecond
	// maxConnectAttempts bounds how many times Connect retries the final
	// device.Connect() call when BlueZ reports a transient failure (see
	// isTransientBluetoothError). Real hardware (e.g. a JBL Flip 5 that is
	// already bonded/trusted) has been observed rejecting a connect with
	// BlueZ's generic "unknown" bearer failure while momentarily busy tearing
	// down a previous session; a couple of retries clears this without
	// requiring the caller to re-run the whole command.
	maxConnectAttempts = 3
	// connectRetryDelay is the wait between retry attempts.
	connectRetryDelay = 750 * time.Millisecond
	// HID service UUIDs for the two Bluetooth transports: 0x1812 is HID over
	// GATT (LE), while 0x1124 is the Classic Bluetooth HID service class. A
	// device advertising either UUID (or an input-* icon) is expected to
	// produce a Linux input device once its connection is actually usable,
	// which is what makes hollow connects detectable for this class.
	hidServiceUUID        = "00001812-0000-1000-8000-00805f9b34fb"
	classicHIDServiceUUID = "00001124-0000-1000-8000-00805f9b34fb"
	// inputArrivalTimeout bounds how long Connect waits for the kernel input
	// device of a just-connected HID peripheral. On a healthy encrypted link
	// bluetoothd attaches HID/HOG and uhid registers within a second or two;
	// well past that with no input device, the link is connected but unusable
	// (typically a stale bond: the peripheral lost its key and is waiting to
	// re-pair while encryption silently fails).
	inputArrivalTimeout = 8 * time.Second
	// inputArrivalPollInterval is how often the sysfs input registry is
	// re-read while waiting for the HID input device to appear.
	inputArrivalPollInterval = 250 * time.Millisecond
)

// inputDeviceUniqGlob locates the `uniq` attribute of every registered input
// device; for Bluetooth HID devices the kernel stores the peripheral's
// address there. A var so tests can point it at a fake sysfs tree.
var inputDeviceUniqGlob = "/sys/class/input/input*/uniq"

// managedObjects is the result shape of org.freedesktop.DBus.ObjectManager's
// GetManagedObjects: object path → interface name → property name → value.
type managedObjects = map[dbus.ObjectPath]map[string]map[string]dbus.Variant

type BlueZManager struct {
	logger *zap.Logger
	links  LinkReporter // nil: Scan reports no link timeouts
}

func newPlatformManager(logger *zap.Logger, o managerOptions) Manager {
	return &BlueZManager{logger: logger, links: o.links}
}

// getManagedObjects enumerates every object BlueZ exposes (adapters, devices)
// with their typed properties.
func getManagedObjects(ctx context.Context, conn *dbus.Conn) (managedObjects, error) {
	var managed managedObjects
	root := conn.Object(bluezService, "/")
	if err := root.CallWithContext(ctx, "org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Store(&managed); err != nil {
		return nil, fmt.Errorf("enumerating bluez objects: %w", err)
	}
	return managed, nil
}

// resolveAdapterPath selects the BlueZ adapter to operate on: the
// WENDY_BT_ADAPTER override verbatim when set, otherwise the lowest object
// path implementing org.bluez.Adapter1. The onboard radio is not always hci0
// (it can enumerate higher, or a USB dongle may be the only controller), so
// the path is discovered rather than assumed.
func resolveAdapterPath(managed managedObjects) (string, error) {
	if p := os.Getenv("WENDY_BT_ADAPTER"); p != "" {
		return p, nil
	}
	if p := findAdapterByInterface(managed, adapterIface); p != "" {
		return p, nil
	}
	return "", fmt.Errorf("no Bluetooth adapter found (no object implements %s)", adapterIface)
}

// powerOnAdapter powers the adapter on. The call is a no-op if it is already
// on, but it also clears Command Disallowed state left over from a previous
// BLE connection that wasn't fully torn down at the HCI level.
func powerOnAdapter(ctx context.Context, conn *dbus.Conn, adapterPath string) error {
	adapter := conn.Object(bluezService, dbus.ObjectPath(adapterPath))
	call := adapter.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Set", 0,
		adapterIface, "Powered", dbus.MakeVariant(true))
	return call.Err
}

// stopDiscoveryTimeout bounds the best-effort StopDiscovery cleanup. It uses
// its own deadline because cleanup often runs after the request context has
// already been canceled, and godbus's plain Call would otherwise block
// indefinitely on an unresponsive bluetoothd.
const stopDiscoveryTimeout = 3 * time.Second

// stopDiscovery stops a discovery session this connection started (best-effort).
func (m *BlueZManager) stopDiscovery(conn *dbus.Conn, adapterPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), stopDiscoveryTimeout)
	defer cancel()
	adapter := conn.Object(bluezService, dbus.ObjectPath(adapterPath))
	if call := adapter.CallWithContext(ctx, adapterIface+".StopDiscovery", 0); call.Err != nil {
		m.logger.Debug("Failed to stop Bluetooth discovery", zap.Error(call.Err))
	}
}

// findDeviceByAddress locates the org.bluez.Device1 object whose Address
// matches address (case-insensitively) across all adapters — BlueZ device
// paths are per-adapter, so a synthetic /org/bluez/hci0/dev_... guess breaks
// on multi-adapter systems and on devices BlueZ has evicted. The adapter path
// comes from the device's Adapter property, falling back to the object-path
// parent. When several adapters know the device, the lowest device path wins
// so selection is stable across Go's randomised map iteration order.
func findDeviceByAddress(managed managedObjects, address string) (devicePath dbus.ObjectPath, adapterPath string, props map[string]dbus.Variant, found bool) {
	var (
		bestPath  dbus.ObjectPath
		bestProps map[string]dbus.Variant
	)
	for path, ifaces := range managed {
		devProps, ok := ifaces[deviceIface]
		if !ok {
			continue
		}
		addr, ok := stringProp(devProps, "Address")
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

	var adapter string
	if v, ok := bestProps["Adapter"]; ok {
		if p, ok := v.Value().(dbus.ObjectPath); ok {
			adapter = string(p)
		}
	}
	if adapter == "" {
		if i := strings.LastIndex(string(bestPath), "/"); i > 0 {
			adapter = string(bestPath)[:i]
		}
	}
	return bestPath, adapter, bestProps, true
}

// restrictToAdapter narrows a managed-objects map to the adapter at
// adapterPath and the objects nested under it, so the WENDY_BT_ADAPTER
// override pins device lookups to the chosen controller. An empty adapterPath
// returns the input unchanged.
func restrictToAdapter(managed managedObjects, adapterPath string) managedObjects {
	if adapterPath == "" {
		return managed
	}
	prefix := adapterPath + "/"
	restricted := managedObjects{}
	for path, ifaces := range managed {
		if string(path) == adapterPath || strings.HasPrefix(string(path), prefix) {
			restricted[path] = ifaces
		}
	}
	return restricted
}

// includePeripheral reports whether a cached BlueZ device is worth listing:
// paired/connected/trusted devices always, otherwise only when RSSI is
// present — i.e. the device was actually seen during discovery rather than
// being a stale cache entry that would fail any connect attempt.
func includePeripheral(props map[string]dbus.Variant) bool {
	if boolProp(props, "Paired") || boolProp(props, "Connected") || boolProp(props, "Trusted") {
		return true
	}
	_, hasRSSI := props["RSSI"]
	return hasRSSI
}

// shouldListPeripheral decides whether a device belongs in scan results. A
// device object that appeared during this discovery is always listed — BlueZ
// documents RSSI as optional, so its absence must not hide a fresh device.
// Objects that predate the scan are cache entries and need a presence marker
// (paired/connected/trusted, or re-seen with RSSI) to be worth showing.
func shouldListPeripheral(props map[string]dbus.Variant, preexisting bool) bool {
	return !preexisting || includePeripheral(props)
}

// devicePathsUnder returns the set of org.bluez.Device1 object paths nested
// under the adapter, used to snapshot which devices were already cached
// before a discovery starts.
func devicePathsUnder(managed managedObjects, adapterPath string) map[dbus.ObjectPath]bool {
	prefix := adapterPath + "/"
	paths := map[dbus.ObjectPath]bool{}
	for path, ifaces := range managed {
		if _, ok := ifaces[deviceIface]; ok && strings.HasPrefix(string(path), prefix) {
			paths[path] = true
		}
	}
	return paths
}

// dbusErrorInfo unwraps err to a BlueZ/D-Bus error and returns its D-Bus error
// name and first string body element. godbus delivers error replies as a
// dbus.Error value, but pointer forms exist too (dbus.NewError), so both are
// checked.
func dbusErrorInfo(err error) (name, message string, ok bool) {
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

func firstStringBody(body []any) string {
	if len(body) > 0 {
		if s, ok := body[0].(string); ok {
			return s
		}
	}
	return ""
}

// wrapBluetoothError converts a raw D-Bus failure into a user-facing error.
// Missing-object errors wrap ErrDeviceNotFound so services can map them to a
// NotFound status; recognised BlueZ failures get actionable text; anything
// else keeps the raw error.
func (m *BlueZManager) wrapBluetoothError(op, address string, err error) error {
	name, message, ok := dbusErrorInfo(err)
	if !ok {
		return fmt.Errorf("%s %s: %w", op, address, err)
	}
	text, notFound, classified := friendlyBluetoothError(name, message)
	if !classified {
		return fmt.Errorf("%s %s: %w", op, address, err)
	}
	m.logger.Debug("BlueZ operation failed",
		zap.String("op", op),
		zap.String("address", address),
		zap.String("dbus_error", name),
		zap.String("dbus_message", message))
	if notFound {
		return fmt.Errorf("%w: device %s: %s", ErrDeviceNotFound, address, text)
	}
	return fmt.Errorf("%s %s: %s", op, address, text)
}

// connectFailureError picks which failure to report when a connect attempt
// fails after pairing also failed. A missing-device connect error wins so the
// caller still gets a NotFound status (the device vanished mid-flow); for any
// other connect failure the pairing error is the root cause.
func (m *BlueZManager) connectFailureError(address string, pairErr, connectErr error) error {
	wrapped := m.wrapBluetoothError("connecting to", address, connectErr)
	if pairErr == nil || errors.Is(wrapped, ErrDeviceNotFound) {
		return wrapped
	}
	return m.wrapBluetoothError("pairing with", address, pairErr)
}

// Scan runs a Bluetooth discovery via BlueZ over D-Bus and returns the
// discovered devices on the channel. It powers on the adapter, runs discovery
// for scanDuration, then enumerates known devices through the BlueZ
// ObjectManager so typed properties (RSSI, paired/connected/trusted, icon) are
// read directly rather than parsed from bluetoothctl text output. An early,
// partial batch is sent after quickScanDuration so callers get data fast,
// followed by a second, more thorough batch once the full scanDuration
// elapses.
func (m *BlueZManager) Scan(ctx context.Context) (<-chan []*agentpb.DiscoveredBluetoothPeripheral, error) {
	ch := make(chan []*agentpb.DiscoveredBluetoothPeripheral, 10)

	go func() {
		defer close(ch)

		conn, err := dbus.ConnectSystemBus()
		if err != nil {
			m.logger.Warn("Failed to connect to system bus for Bluetooth scan", zap.Error(err))
			return
		}
		defer conn.Close()

		managed, err := getManagedObjects(ctx, conn)
		if err != nil {
			m.logger.Warn("Failed to enumerate Bluetooth objects", zap.Error(err))
			return
		}
		adapterPath, err := resolveAdapterPath(managed)
		if err != nil {
			m.logger.Warn("No Bluetooth adapter available for scan", zap.Error(err))
			return
		}
		adapter := conn.Object(bluezService, dbus.ObjectPath(adapterPath))

		// Snapshot the devices BlueZ already had cached, so results can tell a
		// freshly discovered device apart from a stale cache entry.
		preexisting := devicePathsUnder(managed, adapterPath)

		if err := powerOnAdapter(ctx, conn, adapterPath); err != nil {
			m.logger.Warn("Failed to power on Bluetooth adapter", zap.Error(err))
			return
		}

		// Start discovery.
		if call := adapter.CallWithContext(ctx, adapterIface+".StartDiscovery", 0); call.Err != nil {
			m.logger.Warn("Failed to start Bluetooth discovery", zap.Error(call.Err))
			return
		}

		// Send an early, partial batch after a short quick pass so a caller has
		// something to show fast, while discovery keeps running underneath.
		select {
		case <-time.After(quickScanDuration):
		case <-ctx.Done():
		}
		if quick := m.collectPeripherals(ctx, conn, adapterPath, preexisting); len(quick) > 0 {
			select {
			case ch <- quick:
			case <-ctx.Done():
			}
		}

		// Let discovery run the rest of the way, then collect results while it
		// is still active — some BlueZ versions clear volatile properties
		// (RSSI) once discovery stops, which would defeat the includePeripheral
		// presence filter.
		select {
		case <-time.After(scanDuration - quickScanDuration):
		case <-ctx.Done():
		}
		peripherals := m.collectPeripherals(ctx, conn, adapterPath, preexisting)

		m.stopDiscovery(conn, adapterPath)

		if len(peripherals) > 0 {
			select {
			case ch <- peripherals:
			case <-ctx.Done():
			}
		}
	}()

	return ch, nil
}

// collectPeripherals enumerates devices known to BlueZ via the ObjectManager
// and returns those belonging to the given adapter that either appeared
// during this discovery or carry a presence marker (paired, connected,
// trusted, or re-seen with RSSI).
func (m *BlueZManager) collectPeripherals(ctx context.Context, conn *dbus.Conn, adapterPath string, preexisting map[dbus.ObjectPath]bool) []*agentpb.DiscoveredBluetoothPeripheral {
	managed, err := getManagedObjects(ctx, conn)
	if err != nil {
		m.logger.Warn("Failed to enumerate Bluetooth devices", zap.Error(err))
		return nil
	}

	// Device object paths are nested under the adapter, e.g.
	// /org/bluez/hci0/dev_XX_XX_XX_XX_XX_XX.
	prefix := adapterPath + "/"
	var peripherals []*agentpb.DiscoveredBluetoothPeripheral
	for path, ifaces := range managed {
		props, ok := ifaces[deviceIface]
		if !ok || !strings.HasPrefix(string(path), prefix) {
			continue
		}
		if !shouldListPeripheral(props, preexisting[path]) {
			continue
		}
		p := deviceFromProps(props)
		annotateLink(p, m.links)
		peripherals = append(peripherals, p)
	}
	return peripherals
}

// deviceFromProps maps org.bluez.Device1 properties to the proto peripheral.
func deviceFromProps(props map[string]dbus.Variant) *agentpb.DiscoveredBluetoothPeripheral {
	p := &agentpb.DiscoveredBluetoothPeripheral{}

	if s, ok := stringProp(props, "Address"); ok {
		p.Address = s
	}
	// Alias is the user-facing name (falls back to Name when unset by BlueZ).
	// BlueZ synthesizes a default Alias for devices advertising no real name —
	// the address with ':' replaced by '-' — which must not count as a name,
	// or every anonymous device would sort as if it were named.
	if s, ok := stringProp(props, "Alias"); ok && s != "" && !isDefaultAlias(s, p.Address) {
		p.Name = s
	} else if s, ok := stringProp(props, "Name"); ok {
		p.Name = s
	}
	if v, ok := props["RSSI"]; ok {
		if rssi, ok := v.Value().(int16); ok {
			p.Rssi = int32(rssi)
		}
	}
	// BlueZ icons for audio devices look like "audio-headset" / "audio-card".
	if icon, ok := stringProp(props, "Icon"); ok && strings.HasPrefix(icon, "audio") {
		p.DeviceType = "audio"
	}
	p.Paired = boolProp(props, "Paired")
	p.Connected = boolProp(props, "Connected")
	p.Trusted = boolProp(props, "Trusted")

	return p
}

// isDefaultAlias reports whether alias is BlueZ's synthesized default for a
// device advertising no name: the address with ':' replaced by '-' (e.g.
// "AA:BB:CC:DD:EE:FF" -> "AA-BB-CC-DD-EE-FF").
func isDefaultAlias(alias, address string) bool {
	if address == "" {
		return false
	}
	return strings.EqualFold(alias, strings.ReplaceAll(address, ":", "-"))
}

func stringProp(props map[string]dbus.Variant, key string) (string, bool) {
	if v, ok := props[key]; ok {
		s, ok := v.Value().(string)
		return s, ok
	}
	return "", false
}

func boolProp(props map[string]dbus.Variant, key string) bool {
	if v, ok := props[key]; ok {
		b, _ := v.Value().(bool)
		return b
	}
	return false
}

func stringsProp(props map[string]dbus.Variant, key string) []string {
	if v, ok := props[key]; ok {
		s, _ := v.Value().([]string)
		return s
	}
	return nil
}

// deviceUUIDs reads a device's live UUIDs property, falling back to cached when
// the read fails or returns nothing.
func deviceUUIDs(ctx context.Context, device dbus.BusObject, cached []string) []string {
	call := device.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, deviceIface, "UUIDs")
	if call.Err != nil {
		return cached
	}
	var v dbus.Variant
	if call.Store(&v) != nil {
		return cached
	}
	uuids, _ := v.Value().([]string)
	if len(uuids) == 0 {
		return cached
	}
	return uuids
}

// audioProfileUUID returns the A2DP profile to connect for an audio peripheral,
// or "" for one that offers neither role. Sink wins when both are offered; a
// device advertising only a source role is a microphone.
//
// Device1.Connect() connects every profile a peripheral supports, which lets a
// speaker that also implements A2DP Source claim our sink endpoint and strand
// WirePlumber on audio-gateway. Naming the profile pins the direction.
func audioProfileUUID(uuids []string) string {
	var hasSource bool
	for _, u := range uuids {
		switch strings.ToLower(u) {
		case a2dpSinkUUID:
			return a2dpSinkUUID
		case a2dpSourceUUID:
			hasSource = true
		}
	}
	if hasSource {
		return a2dpSourceUUID
	}
	return ""
}

// isAlreadyExists reports whether a BlueZ D-Bus error indicates the operation
// was a no-op because the resource already exists (e.g. pairing a device that
// is already paired). Such errors are safe to treat as success.
// liveBoolProp reads a device property over the bus right now, as opposed to
// the possibly minutes-old snapshot resolveDevice returned. ok=false means
// the read failed and the caller should fall back to the snapshot.
func liveBoolProp(ctx context.Context, device dbus.BusObject, prop string) (value, ok bool) {
	call := device.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0, deviceIface, prop)
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

// isHIDDevice reports whether the device presents as a HID peripheral —
// the class whose connect is only real once a kernel input device exists.
func isHIDDevice(props map[string]dbus.Variant) bool {
	if icon, ok := stringProp(props, "Icon"); ok && strings.HasPrefix(icon, "input-") {
		return true
	}
	if uuids, ok := props["UUIDs"].Value().([]string); ok {
		for _, uuid := range uuids {
			if strings.EqualFold(uuid, hidServiceUUID) || strings.EqualFold(uuid, classicHIDServiceUUID) {
				return true
			}
		}
	}
	return false
}

// waitForInputDevice polls the kernel input registry until a device whose
// uniq attribute equals the peripheral's address appears, or the timeout
// elapses. This is the ground truth for a usable HID connection: BlueZ can
// report Connected=yes on a link whose encryption silently failed (stale
// bond), and in that state no input device is ever created.
func waitForInputDevice(ctx context.Context, address string, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(inputArrivalPollInterval)
	defer tick.Stop()
	for {
		if inputDevicePresent(address) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-tick.C:
		}
	}
}

func inputDevicePresent(address string) bool {
	paths, err := filepath.Glob(inputDeviceUniqGlob)
	if err != nil {
		return false
	}
	for _, path := range paths {
		uniq, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(string(uniq)), address) {
			return true
		}
	}
	return false
}

func isAlreadyExists(err error) bool {
	name, _, ok := dbusErrorInfo(err)
	return ok && name == "org.bluez.Error.AlreadyExists"
}

// lookupCachedDevice finds the device by address among BlueZ's cached objects
// (honoring the WENDY_BT_ADAPTER restriction), without running discovery —
// used by Disconnect and Forget, where an uncached device means there is
// nothing to act on.
func lookupCachedDevice(ctx context.Context, conn *dbus.Conn, address string) (devicePath dbus.ObjectPath, adapterPath string, err error) {
	managed, err := getManagedObjects(ctx, conn)
	if err != nil {
		return "", "", err
	}
	managed = restrictToAdapter(managed, os.Getenv("WENDY_BT_ADAPTER"))
	devicePath, adapterPath, _, found := findDeviceByAddress(managed, address)
	if !found {
		return "", "", fmt.Errorf("%w: device %s is not known to the Bluetooth adapter", ErrDeviceNotFound, address)
	}
	return devicePath, adapterPath, nil
}

// resolveDevice locates the device by address. When BlueZ does not know it
// (its cache evicts unpaired devices ~30s after discovery stops), it powers
// on the adapter, starts discovery, and polls until the device appears, the
// timeout elapses, or ctx is done. Discovery is stopped before returning so
// the subsequent connect does not race an active inquiry.
func (m *BlueZManager) resolveDevice(ctx context.Context, conn *dbus.Conn, address string) (dbus.ObjectPath, map[string]dbus.Variant, error) {
	managed, err := getManagedObjects(ctx, conn)
	if err != nil {
		return "", nil, err
	}
	adapterOverride := os.Getenv("WENDY_BT_ADAPTER")
	if path, _, props, found := findDeviceByAddress(restrictToAdapter(managed, adapterOverride), address); found {
		return path, props, nil
	}

	adapterPath, err := resolveAdapterPath(managed)
	if err != nil {
		return "", nil, err
	}
	if err := powerOnAdapter(ctx, conn, adapterPath); err != nil {
		m.logger.Warn("Failed to power on Bluetooth adapter before discovery", zap.Error(err))
	}

	m.logger.Info("Bluetooth device not cached; running on-demand discovery",
		zap.String("address", address), zap.String("adapter", adapterPath))
	adapter := conn.Object(bluezService, dbus.ObjectPath(adapterPath))
	if call := adapter.CallWithContext(ctx, adapterIface+".StartDiscovery", 0); call.Err != nil {
		// Another client may already be scanning; that works for us too.
		if name, _, ok := dbusErrorInfo(call.Err); !ok || name != "org.bluez.Error.InProgress" {
			return "", nil, m.wrapBluetoothError("discovering", address, call.Err)
		}
	}
	defer m.stopDiscovery(conn, adapterPath)

	deadline := time.NewTimer(resolveDiscoveryTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(resolvePollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", nil, ctx.Err()
		case <-deadline.C:
			return "", nil, fmt.Errorf("%w: device %s was not seen within %s of discovery — make sure it is powered on and in range, then rescan",
				ErrDeviceNotFound, address, resolveDiscoveryTimeout)
		case <-tick.C:
			managed, err := getManagedObjects(ctx, conn)
			if err != nil {
				return "", nil, err
			}
			if path, _, props, found := findDeviceByAddress(restrictToAdapter(managed, adapterOverride), address); found {
				return path, props, nil
			}
		}
	}
}

// retryConnect calls attempt up to maxConnectAttempts times, retrying only
// when the failure is classified as transient by isTransientBluetoothError
// (BlueZ's generic "unknown" bearer failure, InProgress, or a bus-level
// NoReply). Any other failure, or a non-D-Bus error, returns immediately. It
// waits delay between attempts, returning early if ctx is done first.
func (m *BlueZManager) retryConnect(ctx context.Context, delay time.Duration, attempt func() error) error {
	var err error
	for i := 0; i < maxConnectAttempts; i++ {
		err = attempt()
		if err == nil {
			return nil
		}
		name, message, ok := dbusErrorInfo(err)
		if !ok || !isTransientBluetoothError(name, message) {
			return err
		}
		if i == maxConnectAttempts-1 {
			break
		}
		m.logger.Info("Retrying transient Bluetooth connect failure",
			zap.String("dbus_error", name), zap.Int("attempt", i+1))
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		}
	}
	return err
}

// connectDevice connects the advertised audio profile by name so a peripheral
// cannot choose the wrong A2DP direction. Non-audio devices, and audio devices
// whose advertised profile cannot be connected directly, fall back to the
// whole-device operation. UUIDs are read live because pairing can populate
// them after the original device snapshot was taken.
func (m *BlueZManager) connectDevice(ctx context.Context, device dbus.BusObject, address string, cachedUUIDs []string) error {
	profile := audioProfileUUID(deviceUUIDs(ctx, device, cachedUUIDs))
	return m.retryConnect(ctx, connectRetryDelay, func() error {
		if profile != "" {
			err := device.CallWithContext(ctx, deviceIface+".ConnectProfile", 0, profile).Err
			if err == nil {
				return nil
			}
			m.logger.Warn("Connecting audio profile failed; falling back to whole-device connect",
				zap.String("address", address), zap.String("profile", profile), zap.Error(err))
		}
		return device.CallWithContext(ctx, deviceIface+".Connect", 0).Err
	})
}

// Connect connects to a Bluetooth peripheral by address via BlueZ over D-Bus,
// discovering the device first if BlueZ no longer has it cached. When pair is
// set it registers a headless pairing agent and pairs first (skipped if the
// device is already paired, per a live property read); when trust is set it
// marks the device trusted so BlueZ reconnects it automatically. A connect
// that leaned on an existing bond and then fails a key-class error — or, for
// HID peripherals, never produces a kernel input device — is treated as a
// stale bond: the bond is dropped and the device is re-paired from scratch
// (repairFreshly). The returned bool reports whether the device is paired
// after the connect — success does not imply pairing because pairing
// failures fall back to a direct connect.
func (m *BlueZManager) Connect(ctx context.Context, address string, pair, trust bool) (bool, error) {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return false, fmt.Errorf("connecting to system bus: %w", err)
	}
	defer conn.Close()

	devicePath, props, err := m.resolveDevice(ctx, conn, address)
	if err != nil {
		return false, err
	}
	device := conn.Object(bluezService, devicePath)
	trustAfterRepair := trust || boolProp(props, "Trusted")
	if live, ok := liveBoolProp(ctx, device, "Trusted"); ok {
		trustAfterRepair = trust || live
	}

	if trust {
		if call := device.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Set", 0,
			deviceIface, "Trusted", dbus.MakeVariant(true)); call.Err != nil {
			// Trust is best-effort: it improves reconnection but is not required
			// for the connection itself to succeed.
			m.logger.Warn("Failed to trust device", zap.String("address", address), zap.Error(call.Err))
		}
	}

	// Pairing failures fall through to a direct connect attempt: some BLE
	// devices reject SMP pairing yet accept connections, and only the connect
	// result tells the two cases apart. If the connect also fails, the pairing
	// error is the root cause and is the one reported.
	//
	// The skip decision reads Paired live: the resolveDevice snapshot can be
	// minutes old, and skipping Pair on a stale "yes" is what strands a
	// peripheral that lost its key (it sits in pairing mode while the host
	// tries to encrypt with a bond the peripheral no longer has).
	var pairErr error
	pairSkipped := false
	if pair {
		// BlueZ rejects authenticated pairing unless an agent is registered.
		// Register a headless "just works" agent on this connection even when
		// Pair is skipped: a peripheral may initiate SMP itself during the
		// connect, and that request needs an answer too. It is unregistered
		// automatically when the connection closes. Best-effort: devices
		// needing no authentication pair without an agent.
		if err := registerPairingAgent(conn, m.logger, devicePath); err != nil {
			m.logger.Warn("Failed to register pairing agent; pairing may fail", zap.Error(err))
		}
		alreadyPaired := boolProp(props, "Paired")
		if live, ok := liveBoolProp(ctx, device, "Paired"); ok {
			alreadyPaired = live
		}
		if alreadyPaired {
			pairSkipped = true
		} else if call := device.CallWithContext(ctx, deviceIface+".Pair", 0); call.Err != nil && !isAlreadyExists(call.Err) {
			pairErr = call.Err
			m.logger.Warn("Pairing failed; attempting direct connect",
				zap.String("address", address), zap.Error(call.Err))
		}
	}

	connectErr := m.connectDevice(ctx, device, address, stringsProp(props, "UUIDs"))
	if connectErr != nil {
		// A key-class failure against a bond only this side still holds is
		// unrecoverable by retrying; dropping the stale bond and pairing
		// fresh is the only way forward.
		if name, message, ok := dbusErrorInfo(connectErr); ok && pairSkipped && isStaleBondBluetoothError(name, message) {
			m.logger.Warn("Connect failed with a stale-bond error; re-pairing from scratch",
				zap.String("address", address), zap.Error(connectErr))
			return m.repairFreshly(ctx, conn, address, trustAfterRepair, isHIDDevice(props))
		}
		return false, m.connectFailureError(address, pairErr, connectErr)
	}

	// A HID peripheral is only really connected once the kernel has an input
	// device for it. Whenever pairing was requested, verify that before
	// reporting success: over an existing bond, a peripheral that lost its key
	// accepts the link and then waits for pairing while encryption silently
	// fails — Connected=yes, no input device, and the old flow reported
	// success — and even a fresh pair has been observed reporting success a
	// second before the kernel device appeared. A connect without pairing is
	// not gated: its recovery would remove a bond the caller never asked to
	// touch.
	if pair && isHIDDevice(props) && !waitForInputDevice(ctx, address, inputArrivalTimeout) {
		m.logger.Warn("HID device connected but produced no input device; re-pairing from scratch",
			zap.String("address", address))
		return m.repairFreshly(ctx, conn, address, trustAfterRepair, true)
	}

	// The device's live Paired property is the source of truth: pairing may
	// have completed implicitly during the connect, or been skipped by the
	// fallback above. Fall back to the computed state if the read fails.
	paired := boolProp(props, "Paired") || (pair && pairErr == nil)
	if live, ok := liveBoolProp(ctx, device, "Paired"); ok {
		paired = live
	}

	if pairErr != nil {
		m.logger.Info("Connected without pairing", zap.String("address", address), zap.Error(pairErr))
	}
	m.logger.Info("Connected to Bluetooth device", zap.String("address", address), zap.Bool("paired", paired))
	return paired, nil
}

// repairFreshly recovers from a stale bond: it removes the device from BlueZ
// (dropping the stored keys), re-discovers it (it must be advertising, which
// in the stale-bond scenario it is — the peripheral is sitting in pairing
// mode), then pairs and connects from scratch. For HID peripherals the fresh
// connect must also produce a kernel input device before success is reported.
// wasHID carries the caller's HID classification, computed from the cached
// device whose properties were complete: the re-discovered device may have
// advertised only a subset of its UUIDs (and Icon is optional), so deciding
// from the fresh snapshot alone could skip the input-device check.
func (m *BlueZManager) repairFreshly(ctx context.Context, conn *dbus.Conn, address string, trust, wasHID bool) (bool, error) {
	devicePath, adapterPath, err := lookupCachedDevice(ctx, conn, address)
	if err != nil {
		return false, fmt.Errorf("locating stale pairing for %s: %w", address, err)
	}
	adapter := conn.Object(bluezService, dbus.ObjectPath(adapterPath))
	if call := adapter.CallWithContext(ctx, adapterIface+".RemoveDevice", 0, devicePath); call.Err != nil {
		return false, m.wrapBluetoothError("removing stale pairing for", address, call.Err)
	}

	devicePath, props, err := m.resolveDevice(ctx, conn, address)
	if err != nil {
		return false, fmt.Errorf("dropped the stale pairing for %s but the device was not seen advertising — put it in pairing mode and retry: %w", address, err)
	}
	device := conn.Object(bluezService, devicePath)

	// The agent object is re-exported at the same path with the new device
	// path as its scope; RegisterAgent then fails with AlreadyExists on this
	// connection, which is fine — the export swap is what mattered.
	if err := registerPairingAgent(conn, m.logger, devicePath); err != nil {
		m.logger.Debug("Pairing agent re-registration", zap.Error(err))
	}
	if call := device.CallWithContext(ctx, deviceIface+".Pair", 0); call.Err != nil && !isAlreadyExists(call.Err) {
		return false, m.wrapBluetoothError("pairing with", address, call.Err)
	}
	if trust {
		// RemoveDevice dropped the Trusted flag along with the keys.
		if call := device.CallWithContext(ctx, "org.freedesktop.DBus.Properties.Set", 0,
			deviceIface, "Trusted", dbus.MakeVariant(true)); call.Err != nil {
			m.logger.Warn("Failed to trust device", zap.String("address", address), zap.Error(call.Err))
		}
	}
	if connectErr := m.connectDevice(ctx, device, address, stringsProp(props, "UUIDs")); connectErr != nil {
		return false, m.wrapBluetoothError("connecting to", address, connectErr)
	}
	if (wasHID || isHIDDevice(props)) && !waitForInputDevice(ctx, address, inputArrivalTimeout) {
		return false, fmt.Errorf("paired and connected to %s but no input device appeared — power-cycle the device and retry", address)
	}

	m.logger.Info("Re-paired Bluetooth device after dropping stale bond", zap.String("address", address))
	return true, nil
}

// Disconnect disconnects from a Bluetooth peripheral via BlueZ over D-Bus.
func (m *BlueZManager) Disconnect(ctx context.Context, address string) error {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return fmt.Errorf("connecting to system bus: %w", err)
	}
	defer conn.Close()

	devicePath, _, err := lookupCachedDevice(ctx, conn, address)
	if err != nil {
		return err
	}

	device := conn.Object(bluezService, devicePath)
	if call := device.CallWithContext(ctx, deviceIface+".Disconnect", 0); call.Err != nil {
		return m.wrapBluetoothError("disconnecting from", address, call.Err)
	}

	m.logger.Info("Disconnected from Bluetooth device", zap.String("address", address))
	return nil
}

// Forget removes a paired Bluetooth peripheral via BlueZ's Adapter1.RemoveDevice.
func (m *BlueZManager) Forget(ctx context.Context, address string) error {
	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		return fmt.Errorf("connecting to system bus: %w", err)
	}
	defer conn.Close()

	devicePath, adapterPath, err := lookupCachedDevice(ctx, conn, address)
	if err != nil {
		return err
	}

	adapter := conn.Object(bluezService, dbus.ObjectPath(adapterPath))
	if call := adapter.CallWithContext(ctx, adapterIface+".RemoveDevice", 0, devicePath); call.Err != nil {
		return m.wrapBluetoothError("removing device", address, call.Err)
	}

	m.logger.Info("Forgot Bluetooth device", zap.String("address", address))
	return nil
}

// Reconnection pacing. Attempts are deliberately unhurried: a peripheral that
// is switched off never answers, and paging costs radio time the Pi's shared
// antenna also needs for Wi-Fi.
var (
	reconnectPasses         = 3
	reconnectAttemptTimeout = 15 * time.Second
	reconnectSpacing        = 20 * time.Second
)

// trustedAudioPeripheral is a bonded, trusted device that offers an A2DP
// profile, with the direction we would connect it in.
type trustedAudioPeripheral struct {
	path    dbus.ObjectPath
	address string
	profile string
}

// ReconnectTrusted connects trusted audio peripherals once per boot: at most
// one output and one input, lowest BlueZ object path first.
//
// BlueZ's policy plugin reconnects only after a link supervision timeout and
// has no startup path (plugins/policy.c), so after a reboot a paired speaker
// stays Trusted with Connected false until the peripheral itself pages us.
func (m *BlueZManager) ReconnectTrusted(ctx context.Context) {
	if bootReconnectClaimed() {
		m.logger.Debug("Bluetooth boot reconnect already attempted this boot")
		return
	}

	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		m.logger.Warn("Bluetooth reconnect: cannot reach system bus", zap.Error(err))
		return
	}
	defer conn.Close()

	candidates, err := m.trustedAudioPeripherals(ctx, conn)
	if err != nil {
		m.logger.Warn("Bluetooth reconnect: cannot enumerate devices", zap.Error(err))
		return
	}
	if len(candidates) == 0 {
		return
	}

	// Claimed once bluetoothd has answered with a device list, and before the
	// already-connected check below: a boot whose peripherals are all up has
	// still used its attempt, and not recording that would let a later agent
	// restart walk the list and undo a deliberate disconnect.
	if !claimBootReconnect(m.logger) {
		return
	}

	// A direction is "filled" once something is connected in it, whether we
	// did it or the peripheral paged us mid-walk.
	filled := map[string]bool{}
	wanted := map[string]bool{}
	for _, c := range candidates {
		wanted[c.profile] = true
	}
	complete := func() bool {
		for p := range wanted {
			if !filled[p] {
				return false
			}
		}
		return true
	}

	for _, c := range candidates {
		if m.deviceConnected(ctx, conn, c.path) {
			filled[c.profile] = true
		}
	}
	if complete() {
		return
	}

	// A peripheral connected before WirePlumber is running has no session
	// manager to claim its transport, and WirePlumber starts minutes after
	// bluetoothd.
	if !waitForAudioSession(ctx) {
		return
	}

	for pass := 0; pass < reconnectPasses && ctx.Err() == nil; pass++ {
		for _, c := range candidates {
			if ctx.Err() != nil {
				return
			}
			// Re-read rather than trusting the enumeration: the peripheral may
			// have connected itself since, which fills the slot without us.
			if filled[c.profile] || m.deviceConnected(ctx, conn, c.path) {
				filled[c.profile] = true
				continue
			}

			attemptCtx, cancel := context.WithTimeout(ctx, reconnectAttemptTimeout)
			callErr := conn.Object(bluezService, c.path).
				CallWithContext(attemptCtx, deviceIface+".ConnectProfile", 0, c.profile).Err
			cancel()

			if callErr == nil {
				filled[c.profile] = true
				m.logger.Info("Reconnected trusted Bluetooth peripheral",
					zap.String("address", c.address), zap.String("profile", c.profile))
				continue
			}
			// Expected whenever the peripheral is switched off or out of
			// range, which is most of the time. Not worth a warning.
			m.logger.Debug("Bluetooth reconnect attempt failed",
				zap.String("address", c.address), zap.Int("pass", pass+1), zap.Error(callErr))

			if complete() {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(reconnectSpacing):
			}
		}
		if complete() {
			return
		}
	}
}

// trustedAudioPeripherals lists bonded, trusted devices that advertise an A2DP
// profile, tagged with the direction to connect them in. The UUIDs BlueZ
// caches for a bonded device survive disconnection, so this needs no radio.
func (m *BlueZManager) trustedAudioPeripherals(ctx context.Context, conn *dbus.Conn) ([]trustedAudioPeripheral, error) {
	managed, err := getManagedObjects(ctx, conn)
	if err != nil {
		return nil, err
	}
	var out []trustedAudioPeripheral
	for path, ifaces := range managed {
		props, ok := ifaces[deviceIface]
		if !ok || !boolProp(props, "Paired") || !boolProp(props, "Trusted") {
			continue
		}
		profile := audioProfileUUID(stringsProp(props, "UUIDs"))
		if profile == "" {
			continue
		}
		address, _ := stringProp(props, "Address")
		out = append(out, trustedAudioPeripheral{path: path, address: address, profile: profile})
	}
	// managed is a map, so its range order is randomised per process.
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

func (m *BlueZManager) deviceConnected(ctx context.Context, conn *dbus.Conn, path dbus.ObjectPath) bool {
	call := conn.Object(bluezService, path).CallWithContext(ctx, "org.freedesktop.DBus.Properties.Get", 0,
		deviceIface, "Connected")
	if call.Err != nil {
		return false
	}
	var v dbus.Variant
	if call.Store(&v) != nil {
		return false
	}
	b, _ := v.Value().(bool)
	return b
}

// audioSessionTimeout bounds the wait; past it the reconnect proceeds anyway
// rather than being lost entirely on a board with no working audio stack.
var audioSessionTimeout = 4 * time.Minute

// waitForAudioSession blocks until the user session's PipeWire socket appears.
// It reports false only when ctx is cancelled — a timeout still returns true so
// the reconnect is attempted rather than silently skipped. The probe goes
// through audio.Available (not RuntimeDir directly) so tests can stub session
// presence: RuntimeDir only trusts a socket owned by the wendy user, which no
// test environment can fabricate.
func waitForAudioSession(ctx context.Context) bool {
	deadline := time.Now().Add(audioSessionTimeout)
	for {
		if audio.Available() {
			return true
		}
		if time.Now().After(deadline) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(2 * time.Second):
		}
	}
}

// bootReconnectMarker records that the boot-time reconnect has been attempted.
// /run is tmpfs, so it disappears on reboot and persists across agent
// restarts, which is exactly the lifetime wanted. Behind a var for tests.
var bootReconnectMarker = "/run/wendy-agent-bt-reconnect"

// bootReconnectClaimed reports whether the boot-time reconnect has already been
// attempted, without claiming it.
func bootReconnectClaimed() bool {
	_, err := os.Stat(bootReconnectMarker)
	return err == nil
}

// claimBootReconnect reports whether this process should perform the boot-time
// reconnect, claiming it if so. Exactly one process per boot wins.
func claimBootReconnect(logger *zap.Logger) bool {
	if bootReconnectClaimed() {
		logger.Debug("Bluetooth boot reconnect already attempted this boot")
		return false
	}
	f, err := os.OpenFile(bootReconnectMarker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		// Another process claimed it, or /run is not writable. Either way,
		// not ours to do.
		logger.Debug("Bluetooth boot reconnect not claimed", zap.Error(err))
		return false
	}
	// The claim is already made — O_EXCL created the inode — so a close
	// failure on a file we never write to must not skip the reconnect, which
	// would leave the boot with no attempt at all.
	if err := f.Close(); err != nil {
		logger.Debug("Closing Bluetooth reconnect marker failed", zap.Error(err))
	}
	return true
}
