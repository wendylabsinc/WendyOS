//go:build linux

package central

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/wendylabsinc/wendy/go/internal/shared/ble/bluez"
)

const (
	// gattOpTimeout bounds one characteristic operation. It matches the hard
	// 10s the darwin backend uses, so a caller sees the same worst case on
	// either platform.
	gattOpTimeout = 10 * time.Second
	// resolvePollInterval is how often on-demand discovery re-enumerates the
	// object tree looking for the target device. Devices typically appear a few
	// hundred ms in, so a sub-second poll keeps the added latency small.
	resolvePollInterval = 300 * time.Millisecond
	// resolvedPollInterval is how often ServicesResolved is re-read while
	// waiting. The signal usually arrives first; this only has to cover a
	// resolve that happened before the match rule took effect.
	resolvedPollInterval = 250 * time.Millisecond
	// stopDiscoveryTimeout and disconnectTimeout bound best-effort cleanup,
	// which often runs after the request context is already canceled. Without
	// their own deadlines a plain Call would block forever on a wedged
	// bluetoothd.
	stopDiscoveryTimeout = 3 * time.Second
	disconnectTimeout    = 3 * time.Second
	// signalChanDepth buffers PropertiesChanged signals ahead of the router.
	// godbus does not drop when a registered channel is full — it spawns a
	// goroutine per signal — so depth here is what keeps a burst from turning
	// into goroutines.
	signalChanDepth = 64
	// notifyQueueDepth buffers notifications per characteristic between the
	// router and WaitNotification.
	notifyQueueDepth = 16
)

// charKey identifies a characteristic by the pair of canonical UUIDs the public
// API takes. Object paths never leave this file.
type charKey struct {
	service        string
	characteristic string
}

type charEntry struct {
	path  dbus.ObjectPath
	flags []string // GattCharacteristic1.Flags, for error messages
	mtu   uint16   // GattCharacteristic1.MTU; 0 before BlueZ 5.62
}

// gattSession is the BlueZ half of a Connection: one bus, one device object,
// the discovered characteristic index, and the goroutine that fans
// PropertiesChanged signals into per-characteristic queues.
type gattSession struct {
	bus         *dbus.Conn
	address     string // for messages
	adapterPath string
	devicePath  dbus.ObjectPath
	weConnected bool // we brought the ACL link up, so we may take it down

	// objFn is a seam: production hands back a bus object, tests hand back a
	// fake, so the D-Bus call/response shapes are testable with no bus.
	objFn func(dbus.ObjectPath) bluez.Caller

	signals    chan *dbus.Signal
	routerDone chan struct{}
	resolved   chan struct{} // buffered(1); the router pokes it, never blocks on it
	lost       chan struct{} // closed once, when Connected goes false
	lostOnce   sync.Once
	closeOnce  sync.Once

	mu       sync.Mutex
	services []string // canonical, sorted
	svcSet   map[string]bool
	chars    map[charKey]charEntry
	notify   map[dbus.ObjectPath]chan []byte
}

// ensureSession opens the bus, resolves the device object and starts the signal
// router. It is idempotent for the life of the Connection.
func (c *Connection) ensureSession(ctx context.Context) (*gattSession, error) {
	if c.g != nil {
		return c.g, nil
	}

	bus, err := dbus.ConnectSystemBus()
	if err != nil {
		return nil, fmt.Errorf("connecting to system bus: %w", err)
	}

	devicePath, adapterPath, props, err := c.resolveDevice(ctx, bus)
	if err != nil {
		bus.Close() //nolint:errcheck
		return nil, err
	}

	// Free win for the L2CAP half: BlueZ has now told us the address type
	// authoritatively, and liteclient reads the info service before it opens a
	// channel, so OpenL2CAP no longer has to guess.
	if t, ok := addressTypeFromProps(props); ok {
		c.addrType, c.addrKnown = t, true
	}

	g := &gattSession{
		bus:         bus,
		address:     c.address,
		adapterPath: adapterPath,
		devicePath:  devicePath,
		signals:     make(chan *dbus.Signal, signalChanDepth),
		routerDone:  make(chan struct{}),
		resolved:    make(chan struct{}, 1),
		lost:        make(chan struct{}),
		svcSet:      map[string]bool{},
		chars:       map[charKey]charEntry{},
		notify:      map[dbus.ObjectPath]chan []byte{},
	}
	g.objFn = func(p dbus.ObjectPath) bluez.Caller { return bus.Object(bluez.Service, p) }

	// One rule for the device and everything nested under it: the device object
	// itself carries ServicesResolved and Connected, its children carry the
	// characteristic Values. Per-characteristic rules would cost a bus round
	// trip per Subscribe and buy nothing, since the router filters on interface
	// anyway.
	//
	// Installed before Device1.Connect below, because a device that resolves
	// immediately would otherwise emit ServicesResolved into a rule that does
	// not exist yet.
	if err := bus.AddMatchSignalContext(ctx,
		dbus.WithMatchSender(bluez.Service),
		dbus.WithMatchInterface(bluez.PropsIface),
		dbus.WithMatchMember("PropertiesChanged"),
		dbus.WithMatchPathNamespace(devicePath),
	); err != nil {
		bus.Close() //nolint:errcheck
		return nil, fmt.Errorf("subscribing to BlueZ property changes: %w", err)
	}
	bus.Signal(g.signals)
	go g.route()

	c.g = g
	return g, nil
}

// resolveDevice finds the BlueZ object for this connection's address, running a
// bounded discovery when there is none.
//
// The device is matched on its Address property rather than by building
// /org/bluez/hciX/dev_AA_BB_..., which is only a naming convention and says
// nothing about whether the object exists.
func (c *Connection) resolveDevice(ctx context.Context, bus *dbus.Conn) (dbus.ObjectPath, string, map[string]dbus.Variant, error) {
	managed, err := bluez.GetManagedObjects(ctx, bus)
	if err != nil {
		return "", "", nil, err
	}
	adapterPath, err := bluez.ResolveAdapterPath(managed)
	if err != nil {
		return "", "", nil, err
	}
	if path, _, props, ok := bluez.FindDeviceByAddress(bluez.RestrictToAdapter(managed, adapterPath), c.address); ok {
		return path, adapterPath, props, nil
	}

	// Not cached. BlueZ evicts unpaired devices roughly 30s after discovery
	// stops, so this is the ordinary case when connecting a while after a scan
	// — and unlike the raw L2CAP socket, a GATT connect genuinely needs the
	// object to exist.
	if err := bluez.PowerOn(ctx, bus, adapterPath); err != nil {
		return "", "", nil, fmt.Errorf("powering on adapter %s: %w", adapterPath, err)
	}
	adapter := bus.Object(bluez.Service, dbus.ObjectPath(adapterPath))
	if call := adapter.CallWithContext(ctx, bluez.AdapterIface+".StartDiscovery", 0); call.Err != nil {
		// Another client already scanning works for us too.
		if !bluez.IsErrorName(call.Err, "org.bluez.Error.InProgress") {
			return "", "", nil, fmt.Errorf("starting discovery on %s: %w", adapterPath, call.Err)
		}
	}
	// Discovery must stop before the connect that follows: on older kernels an
	// outgoing LE connect issued while the controller is scanning fails.
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), stopDiscoveryTimeout)
		defer cancel()
		_ = adapter.CallWithContext(stopCtx, bluez.AdapterIface+".StopDiscovery", 0).Err
	}()

	ticker := time.NewTicker(resolvePollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", "", nil, fmt.Errorf("%w: %s was not seen advertising within the timeout",
				bluez.ErrDeviceNotFound, c.address)
		case <-ticker.C:
			managed, err := bluez.GetManagedObjects(ctx, bus)
			if err != nil {
				// The context expiring mid-call lands here; the next loop turn
				// reports it as the not-found it is.
				continue
			}
			if path, _, props, ok := bluez.FindDeviceByAddress(bluez.RestrictToAdapter(managed, adapterPath), c.address); ok {
				return path, adapterPath, props, nil
			}
		}
	}
}

// connect brings the ACL link up if it is not up already.
//
// A link someone else established is used as-is and never claimed: taking it
// down in Close would break whoever owns it.
func (g *gattSession) connect(ctx context.Context) error {
	dev := g.objFn(g.devicePath)
	if connected, ok := bluez.LiveBoolProp(ctx, dev, bluez.DeviceIface, "Connected"); ok && connected {
		return nil
	}
	if call := dev.CallWithContext(ctx, bluez.DeviceIface+".Connect", 0); call.Err != nil {
		if bluez.IsErrorName(call.Err, "org.bluez.Error.AlreadyConnected") {
			// Raced someone else's connect; same as the branch above.
			return nil
		}
		return wrapGATTError("connecting to "+g.address, call.Err)
	}
	g.weConnected = true
	return nil
}

// waitServicesResolved blocks until BlueZ finishes GATT discovery, which is
// when the GattService1 and GattCharacteristic1 objects appear.
//
// Signal and poll together: Device1.Connect normally returns only after
// bluetoothd has resolved, so the first read usually wins, and the signal
// covers the case where another client's connect is still in flight.
func (g *gattSession) waitServicesResolved(ctx context.Context) error {
	dev := g.objFn(g.devicePath)
	for {
		if resolved, ok := bluez.LiveBoolProp(ctx, dev, bluez.DeviceIface, "ServicesResolved"); ok && resolved {
			return nil
		}
		select {
		case <-g.resolved:
			return nil
		case <-g.lost:
			return fmt.Errorf("%w: link to %s dropped before services resolved", ErrGATTDisconnected, g.address)
		case <-ctx.Done():
			return fmt.Errorf("services on %s did not resolve within the timeout", g.address)
		case <-time.After(resolvedPollInterval):
		}
	}
}

// rebuildIndex re-reads the object tree and swaps in a fresh characteristic
// index. Safe to call more than once: a caller that reads the info service and
// later opens a provisioning session discovers twice on one connection.
func (g *gattSession) rebuildIndex(ctx context.Context) error {
	managed, err := bluez.GetManagedObjects(ctx, g.bus)
	if err != nil {
		return err
	}
	services, chars := buildIndex(managed, g.devicePath)

	set := make(map[string]bool, len(services))
	for _, s := range services {
		set[s] = true
	}

	g.mu.Lock()
	g.services, g.svcSet, g.chars = services, set, chars
	g.mu.Unlock()
	return nil
}

// buildIndex maps BlueZ's object tree under devicePath to the canonical UUID
// pairs the public API takes. Pure — no bus, no I/O — which is what makes it
// the most useful thing in this file to test without a radio.
func buildIndex(managed bluez.ManagedObjects, devicePath dbus.ObjectPath) (services []string, chars map[charKey]charEntry) {
	prefix := string(devicePath) + "/"
	chars = map[charKey]charEntry{}

	// Pass 1: service object path -> canonical UUID.
	svcUUID := map[dbus.ObjectPath]string{}
	seen := map[string]bool{}
	for path, ifaces := range managed {
		props, ok := ifaces[bluez.GattServiceIface]
		if !ok || !strings.HasPrefix(string(path), prefix) {
			continue
		}
		raw, _ := bluez.StringProp(props, "UUID")
		uuid := canonicalUUID(raw)
		if uuid == "" {
			continue
		}
		svcUUID[path] = uuid
		if !seen[uuid] {
			seen[uuid] = true
			services = append(services, uuid)
		}
	}
	// Sorted so ListServices is stable; map iteration order alone is randomized.
	sort.Strings(services)

	// Pass 2: characteristics, keyed by their parent service's UUID.
	for path, ifaces := range managed {
		props, ok := ifaces[bluez.GattCharIface]
		if !ok || !strings.HasPrefix(string(path), prefix) {
			continue
		}
		// The Service property is authoritative; the parent path is the
		// fallback for a BlueZ that omits it.
		svcPath := bluez.ObjectPathProp(props, "Service")
		if svcPath == "" {
			svcPath = parentPath(path)
		}
		service, ok := svcUUID[svcPath]
		if !ok {
			continue
		}
		raw, _ := bluez.StringProp(props, "UUID")
		uuid := canonicalUUID(raw)
		if uuid == "" {
			continue
		}

		key := charKey{service: service, characteristic: uuid}
		// A service may legitimately expose one characteristic UUID twice.
		// Lowest object path wins so repeated runs pick the same one.
		if prev, dup := chars[key]; dup && prev.path <= path {
			continue
		}
		chars[key] = charEntry{
			path:  path,
			flags: bluez.StringsProp(props, "Flags"),
			mtu:   bluez.Uint16Prop(props, "MTU"),
		}
	}
	return services, chars
}

func parentPath(p dbus.ObjectPath) dbus.ObjectPath {
	s := string(p)
	if i := strings.LastIndex(s, "/"); i > 0 {
		return dbus.ObjectPath(s[:i])
	}
	return ""
}

// route fans PropertiesChanged signals out until the bus closes.
//
// bus.Close is what ends this: godbus's Terminate closes every registered
// signal channel. RemoveSignal would not — it unregisters without closing — so
// the router would range forever.
func (g *gattSession) route() {
	defer close(g.routerDone)
	for sig := range g.signals {
		g.handleSignal(sig)
	}
}

// handleSignal is split from route so it can be driven with hand-built signals
// and no bus at all.
func (g *gattSession) handleSignal(sig *dbus.Signal) {
	if sig == nil || sig.Name != bluez.PropsChangedName || len(sig.Body) < 2 {
		return
	}
	iface, _ := sig.Body[0].(string)
	changed, _ := sig.Body[1].(map[string]dbus.Variant)
	if changed == nil {
		return
	}

	switch iface {
	case bluez.DeviceIface:
		if v, ok := changed["ServicesResolved"]; ok {
			if resolved, _ := v.Value().(bool); resolved {
				// Buffered(1) and non-blocking: a waiter that has already moved
				// on must not wedge the router.
				select {
				case g.resolved <- struct{}{}:
				default:
				}
			}
		}
		if v, ok := changed["Connected"]; ok {
			if connected, _ := v.Value().(bool); !connected {
				g.markLost()
			}
		}
	case bluez.GattCharIface:
		v, ok := changed["Value"]
		if !ok {
			return
		}
		val, ok := v.Value().([]byte)
		if !ok {
			return
		}
		// Copy: the variant's slice belongs to the unmarshaled message.
		g.deliver(sig.Path, append([]byte(nil), val...))
	}
}

// deliver queues one notification for the characteristic at path.
//
// It must never block. godbus does not drop signals for a full registered
// channel — it spawns a goroutine per signal — so a router parked on a full
// queue would turn a chatty peripheral into unbounded goroutine growth. When a
// queue is full the oldest value goes: these carry device state, where the
// freshest reading is the useful one.
func (g *gattSession) deliver(path dbus.ObjectPath, value []byte) {
	g.mu.Lock()
	queue := g.notify[path]
	g.mu.Unlock()
	if queue == nil {
		// Not subscribed: another client's notification, or the echo BlueZ
		// emits for our own ReadValue on a characteristic nobody watches.
		return
	}

	select {
	case queue <- value:
	default:
		select {
		case <-queue:
		default:
		}
		select {
		case queue <- value:
		default:
		}
	}
}

func (g *gattSession) markLost() {
	g.lostOnce.Do(func() { close(g.lost) })
}

// close tears the session down. l2capOpen reports whether a channel is still
// riding on the ACL link, which decides whether the link may go down with it.
func (g *gattSession) close(l2capOpen bool) {
	g.closeOnce.Do(func() {
		// Disconnect drops the HCI link, taking every L2CAP channel on it with
		// it — ours or another process's. Only tear it down when we brought it
		// up and nothing of ours is still using it.
		if g.weConnected && !l2capOpen {
			ctx, cancel := context.WithTimeout(context.Background(), disconnectTimeout)
			defer cancel()
			_ = g.objFn(g.devicePath).CallWithContext(ctx, bluez.DeviceIface+".Disconnect", 0).Err
		}
		// No StopNotify: bluetoothd tracks notify clients per bus name and
		// drops ours when the connection goes away.
		g.bus.Close() //nolint:errcheck
		<-g.routerDone
	})
}

// wrapGATTError turns a raw D-Bus failure into a user-facing error, keeping the
// underlying dbus.Error wrapped so errors.As still yields the org.bluez.Error.*
// name for a caller that wants to branch on it.
func wrapGATTError(op string, err error) error {
	name, message, ok := bluez.ErrorInfo(err)
	if !ok {
		return fmt.Errorf("%s: %w", op, err)
	}
	if text, ok := friendlyGATTError(name, message); ok {
		return fmt.Errorf("%s: %s (%w)", op, text, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}
