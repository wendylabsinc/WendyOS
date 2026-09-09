//go:build linux

package central

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"

	"github.com/wendylabsinc/wendy/go/internal/shared/ble/bluez"
)

const (
	testDevicePath = dbus.ObjectPath("/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF")
	// The Wendy Lite info service, spelled the way BlueZ spells it: lowercase.
	// Callers pass it uppercase, which is the whole point of canonicalizing.
	liteInfoLower = "4e57454e-4459-0002-0000-000000000000"
	liteInfoUpper = "4E57454E-4459-0002-0000-000000000000"
	psmCharLower  = "4e57454e-4459-0002-0001-000000000000"
	psmCharUpper  = "4E57454E-4459-0002-0001-000000000000"
)

func gattService(path dbus.ObjectPath, uuid string) (dbus.ObjectPath, map[string]map[string]dbus.Variant) {
	return path, map[string]map[string]dbus.Variant{
		bluez.GattServiceIface: {"UUID": dbus.MakeVariant(uuid)},
	}
}

// gattChar builds a characteristic object. A nil servicePath omits the Service
// property, exercising the parent-path fallback.
func gattChar(path dbus.ObjectPath, uuid string, servicePath *dbus.ObjectPath, extra map[string]dbus.Variant) (dbus.ObjectPath, map[string]map[string]dbus.Variant) {
	props := map[string]dbus.Variant{"UUID": dbus.MakeVariant(uuid)}
	if servicePath != nil {
		props["Service"] = dbus.MakeVariant(*servicePath)
	}
	for k, v := range extra {
		props[k] = v
	}
	return path, map[string]map[string]dbus.Variant{bluez.GattCharIface: props}
}

func managedFrom(objects ...func() (dbus.ObjectPath, map[string]map[string]dbus.Variant)) bluez.ManagedObjects {
	managed := bluez.ManagedObjects{}
	for _, o := range objects {
		path, ifaces := o()
		if managed[path] == nil {
			managed[path] = map[string]map[string]dbus.Variant{}
		}
		for iface, props := range ifaces {
			managed[path][iface] = props
		}
	}
	return managed
}

func obj(path dbus.ObjectPath, ifaces map[string]map[string]dbus.Variant) func() (dbus.ObjectPath, map[string]map[string]dbus.Variant) {
	return func() (dbus.ObjectPath, map[string]map[string]dbus.Variant) { return path, ifaces }
}

func TestBuildIndex(t *testing.T) {
	svcPath := testDevicePath + "/service0010"
	otherSvc := testDevicePath + "/service0020"

	managed := managedFrom(
		obj(gattService(svcPath, liteInfoLower)),
		// A 16-bit service: BlueZ reports the expanded form, but a caller may
		// look it up either way.
		obj(gattService(otherSvc, "0000180f-0000-1000-8000-00805f9b34fb")),
		obj(gattChar(svcPath+"/char0011", psmCharLower, &svcPath, map[string]dbus.Variant{
			"Flags": dbus.MakeVariant([]string{"read"}),
			"MTU":   dbus.MakeVariant(uint16(517)),
		})),
		// No Service property: falls back to the parent path.
		obj(gattChar(svcPath+"/char0012", "4e57454e-4459-0002-0002-000000000000", nil, nil)),
		// Belongs to a different device entirely and must not appear.
		obj(gattService("/org/bluez/hci0/dev_11_22_33_44_55_66/service0010", liteInfoLower)),
		obj(gattChar("/org/bluez/hci0/dev_11_22_33_44_55_66/service0010/char0011", psmCharLower, nil, nil)),
		// A device-level object with no GATT interface at all.
		obj(testDevicePath, map[string]map[string]dbus.Variant{
			bluez.DeviceIface: {"Address": dbus.MakeVariant("AA:BB:CC:DD:EE:FF")},
		}),
	)

	services, chars := buildIndex(managed, testDevicePath)

	wantServices := []string{"0000180F-0000-1000-8000-00805F9B34FB", liteInfoUpper}
	if !reflect.DeepEqual(services, wantServices) {
		t.Errorf("services = %v, want %v (sorted, canonical)", services, wantServices)
	}

	t.Run("caller's uppercase UUID finds BlueZ's lowercase one", func(t *testing.T) {
		entry, ok := chars[charKey{service: liteInfoUpper, characteristic: psmCharUpper}]
		if !ok {
			t.Fatalf("characteristic not found; index holds %v", keysOf(chars))
		}
		if entry.path != svcPath+"/char0011" {
			t.Errorf("path = %q", entry.path)
		}
		if entry.mtu != 517 {
			t.Errorf("mtu = %d, want 517", entry.mtu)
		}
		if !reflect.DeepEqual(entry.flags, []string{"read"}) {
			t.Errorf("flags = %v", entry.flags)
		}
	})

	t.Run("Service property missing falls back to the parent path", func(t *testing.T) {
		if _, ok := chars[charKey{liteInfoUpper, "4E57454E-4459-0002-0002-000000000000"}]; !ok {
			t.Error("characteristic without a Service property was dropped")
		}
	})

	t.Run("MTU absent reads as zero", func(t *testing.T) {
		entry := chars[charKey{liteInfoUpper, "4E57454E-4459-0002-0002-000000000000"}]
		if entry.mtu != 0 {
			t.Errorf("mtu = %d, want 0 when BlueZ omits the property", entry.mtu)
		}
	})

	t.Run("another device's objects are excluded", func(t *testing.T) {
		if len(chars) != 2 {
			t.Errorf("index holds %d characteristics, want 2: %v", len(chars), keysOf(chars))
		}
	})

	t.Run("a 16-bit service is reachable by its shorthand", func(t *testing.T) {
		found := false
		for _, s := range services {
			if s == canonicalUUID("180F") {
				found = true
			}
		}
		if !found {
			t.Errorf("180F does not match the expanded service in %v", services)
		}
	})
}

func TestBuildIndexDuplicateCharacteristic(t *testing.T) {
	svcPath := testDevicePath + "/service0010"
	managed := managedFrom(
		obj(gattService(svcPath, liteInfoLower)),
		obj(gattChar(svcPath+"/char0099", psmCharLower, &svcPath, nil)),
		obj(gattChar(svcPath+"/char0011", psmCharLower, &svcPath, nil)),
	)

	// Map order is randomized, so an unstable choice shows up as a flake.
	for i := 0; i < 50; i++ {
		_, chars := buildIndex(managed, testDevicePath)
		entry := chars[charKey{liteInfoUpper, psmCharUpper}]
		if entry.path != svcPath+"/char0011" {
			t.Fatalf("duplicate resolved to %q, want the lowest path every time", entry.path)
		}
	}
}

func TestBuildIndexEmptyTree(t *testing.T) {
	services, chars := buildIndex(bluez.ManagedObjects{}, testDevicePath)
	if services != nil {
		t.Errorf("services = %v, want nil", services)
	}
	if len(chars) != 0 {
		t.Errorf("chars = %v, want empty", chars)
	}
}

func TestBuildIndexIgnoresCharacteristicWithUnknownService(t *testing.T) {
	// A characteristic whose Service points somewhere that is not a discovered
	// service has no key to be filed under.
	orphan := dbus.ObjectPath(testDevicePath + "/service9999")
	managed := managedFrom(
		obj(gattChar(testDevicePath+"/service0010/char0011", psmCharLower, &orphan, nil)),
	)
	if _, chars := buildIndex(managed, testDevicePath); len(chars) != 0 {
		t.Errorf("orphan characteristic was indexed: %v", keysOf(chars))
	}
}

func keysOf(chars map[charKey]charEntry) []charKey {
	out := make([]charKey, 0, len(chars))
	for k := range chars {
		out = append(out, k)
	}
	return out
}

// ── Signal routing ───────────────────────────────────────────────────────────

func newRoutingSession() *gattSession {
	return &gattSession{
		devicePath: testDevicePath,
		resolved:   make(chan struct{}, 1),
		lost:       make(chan struct{}),
		notify:     map[dbus.ObjectPath]chan []byte{},
	}
}

func propsChanged(path dbus.ObjectPath, iface string, changed map[string]dbus.Variant) *dbus.Signal {
	return &dbus.Signal{
		Path: path,
		Name: bluez.PropsChangedName,
		Body: []any{iface, changed, []string{}},
	}
}

func TestHandleSignalRoutesValues(t *testing.T) {
	g := newRoutingSession()
	charA := dbus.ObjectPath(testDevicePath + "/service0010/char0011")
	charB := dbus.ObjectPath(testDevicePath + "/service0010/char0012")
	g.notify[charA] = make(chan []byte, notifyQueueDepth)
	g.notify[charB] = make(chan []byte, notifyQueueDepth)

	g.handleSignal(propsChanged(charA, bluez.GattCharIface, map[string]dbus.Variant{
		"Value": dbus.MakeVariant([]byte{0x02, 'i', 'p'}),
	}))

	select {
	case got := <-g.notify[charA]:
		if !reflect.DeepEqual(got, []byte{0x02, 'i', 'p'}) {
			t.Errorf("value = %v", got)
		}
	default:
		t.Fatal("nothing was queued for the characteristic that changed")
	}

	// The darwin bug this design exists to avoid: a notification on one
	// characteristic must never wake a waiter on another.
	select {
	case got := <-g.notify[charB]:
		t.Fatalf("a value leaked into the other characteristic's queue: %v", got)
	default:
	}
}

func TestHandleSignalDropsUnsubscribedPath(t *testing.T) {
	g := newRoutingSession()
	// No queue for this path — another client's notification, or the echo BlueZ
	// emits for our own ReadValue. Must not panic, must not queue anything.
	g.handleSignal(propsChanged(testDevicePath+"/service0010/char0011", bluez.GattCharIface,
		map[string]dbus.Variant{"Value": dbus.MakeVariant([]byte{1})}))
	if len(g.notify) != 0 {
		t.Errorf("routing created a queue for an unsubscribed path: %v", g.notify)
	}
}

func TestHandleSignalFullQueueDropsOldest(t *testing.T) {
	g := newRoutingSession()
	char := dbus.ObjectPath(testDevicePath + "/service0010/char0011")
	g.notify[char] = make(chan []byte, 2)

	for i := byte(1); i <= 4; i++ {
		g.handleSignal(propsChanged(char, bluez.GattCharIface, map[string]dbus.Variant{
			"Value": dbus.MakeVariant([]byte{i}),
		}))
	}

	// A status byte is state, so the freshest two are the ones worth keeping.
	var got []byte
	for len(g.notify[char]) > 0 {
		got = append(got, (<-g.notify[char])[0])
	}
	if !reflect.DeepEqual(got, []byte{3, 4}) {
		t.Errorf("queue holds %v, want the newest two [3 4]", got)
	}
}

func TestHandleSignalIgnoresIrrelevantChanges(t *testing.T) {
	g := newRoutingSession()
	char := dbus.ObjectPath(testDevicePath + "/service0010/char0011")
	g.notify[char] = make(chan []byte, notifyQueueDepth)

	cases := []*dbus.Signal{
		// A characteristic property that is not Value.
		propsChanged(char, bluez.GattCharIface, map[string]dbus.Variant{"Notifying": dbus.MakeVariant(true)}),
		// Value of the wrong type.
		propsChanged(char, bluez.GattCharIface, map[string]dbus.Variant{"Value": dbus.MakeVariant("not bytes")}),
		// An interface this router does not care about.
		propsChanged(char, "org.bluez.GattDescriptor1", map[string]dbus.Variant{"Value": dbus.MakeVariant([]byte{1})}),
		// Device-level churn: RSSI changes on nearly every advertisement.
		propsChanged(testDevicePath, bluez.DeviceIface, map[string]dbus.Variant{"RSSI": dbus.MakeVariant(int16(-40))}),
		// Not a PropertiesChanged signal at all.
		{Path: char, Name: "org.freedesktop.DBus.ObjectManager.InterfacesAdded", Body: []any{}},
		// Malformed bodies.
		{Path: char, Name: bluez.PropsChangedName, Body: []any{bluez.GattCharIface}},
		{Path: char, Name: bluez.PropsChangedName, Body: []any{bluez.GattCharIface, nil, nil}},
		nil,
	}
	for i, sig := range cases {
		g.handleSignal(sig)
		if len(g.notify[char]) != 0 {
			t.Fatalf("case %d queued a value it should have ignored", i)
		}
	}

	select {
	case <-g.lost:
		t.Error("lost was closed by an irrelevant signal")
	default:
	}
}

func TestHandleSignalServicesResolved(t *testing.T) {
	g := newRoutingSession()

	g.handleSignal(propsChanged(testDevicePath, bluez.DeviceIface, map[string]dbus.Variant{
		"ServicesResolved": dbus.MakeVariant(false),
	}))
	select {
	case <-g.resolved:
		t.Fatal("ServicesResolved=false poked the waiter")
	default:
	}

	g.handleSignal(propsChanged(testDevicePath, bluez.DeviceIface, map[string]dbus.Variant{
		"ServicesResolved": dbus.MakeVariant(true),
	}))
	// A second one must not block: the channel is buffered(1) and the send is
	// non-blocking precisely so a waiter that already moved on cannot wedge the
	// router.
	done := make(chan struct{})
	go func() {
		defer close(done)
		g.handleSignal(propsChanged(testDevicePath, bluez.DeviceIface, map[string]dbus.Variant{
			"ServicesResolved": dbus.MakeVariant(true),
		}))
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a second ServicesResolved blocked the router")
	}

	select {
	case <-g.resolved:
	default:
		t.Error("ServicesResolved=true did not poke the waiter")
	}
}

func TestHandleSignalConnectedFalseClosesLostOnce(t *testing.T) {
	g := newRoutingSession()

	g.handleSignal(propsChanged(testDevicePath, bluez.DeviceIface, map[string]dbus.Variant{
		"Connected": dbus.MakeVariant(true),
	}))
	select {
	case <-g.lost:
		t.Fatal("Connected=true closed lost")
	default:
	}

	// Twice: a second close of the same channel would panic without the Once.
	for i := 0; i < 2; i++ {
		g.handleSignal(propsChanged(testDevicePath, bluez.DeviceIface, map[string]dbus.Variant{
			"Connected": dbus.MakeVariant(false),
		}))
	}
	select {
	case <-g.lost:
	default:
		t.Error("Connected=false did not close lost")
	}
}

// ── The read-long loop, through the objFn seam ───────────────────────────────

// fakeChar answers ReadValue from a script, recording the offsets it was asked
// for.
type fakeChar struct {
	replies [][]byte
	err     error
	offsets []uint16
	calls   int
}

func (f *fakeChar) CallWithContext(_ context.Context, method string, _ dbus.Flags, args ...any) *dbus.Call {
	f.calls++
	if f.err != nil {
		return &dbus.Call{Err: f.err}
	}
	var offset uint16
	if len(args) > 0 {
		if options, ok := args[0].(map[string]dbus.Variant); ok {
			if v, ok := options["offset"]; ok {
				offset, _ = v.Value().(uint16)
			}
		}
	}
	f.offsets = append(f.offsets, offset)
	if f.calls > len(f.replies) {
		return &dbus.Call{Body: []any{[]byte{}}}
	}
	return &dbus.Call{Body: []any{f.replies[f.calls-1]}}
}

// connWithChar builds a Connection whose GATT index holds one characteristic
// backed by fake. No bus, no radio.
func connWithChar(fake *fakeChar, mtu uint16) *Connection {
	const path = dbus.ObjectPath(testDevicePath + "/service0010/char0011")
	g := &gattSession{
		devicePath: testDevicePath,
		address:    "AA:BB:CC:DD:EE:FF",
		objFn:      func(dbus.ObjectPath) bluez.Caller { return fake },
		services:   []string{liteInfoUpper},
		svcSet:     map[string]bool{liteInfoUpper: true},
		chars: map[charKey]charEntry{
			{liteInfoUpper, psmCharUpper}: {path: path, mtu: mtu, flags: []string{"read", "notify"}},
		},
		notify:   map[dbus.ObjectPath]chan []byte{},
		resolved: make(chan struct{}, 1),
		lost:     make(chan struct{}),
	}
	return &Connection{fd: -1, address: "AA:BB:CC:DD:EE:FF", g: g}
}

func TestReadCharacteristicShortValue(t *testing.T) {
	// The Lite PSM characteristic: two bytes, nowhere near the payload
	// boundary, so exactly one read.
	fake := &fakeChar{replies: [][]byte{{0x80, 0x00}}}
	conn := connWithChar(fake, 517)

	got, err := conn.ReadCharacteristic(liteInfoUpper, psmCharUpper)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, []byte{0x80, 0x00}) {
		t.Errorf("value = %v", got)
	}
	if fake.calls != 1 {
		t.Errorf("made %d calls, want 1 for a short value", fake.calls)
	}
	if len(fake.offsets) != 1 || fake.offsets[0] != 0 {
		t.Errorf("offsets = %v, want a single unoffset read", fake.offsets)
	}
}

func TestReadCharacteristicContinuesAfterFullChunk(t *testing.T) {
	// A value that lands exactly on the payload boundary may have been
	// truncated, so the read continues at an offset.
	first := make([]byte, 21) // MTU 22 -> payload 21
	for i := range first {
		first[i] = byte(i)
	}
	fake := &fakeChar{replies: [][]byte{first, {0xAA, 0xBB}}}
	conn := connWithChar(fake, 22)

	got, err := conn.ReadCharacteristic(liteInfoUpper, psmCharUpper)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 23 || got[21] != 0xAA || got[22] != 0xBB {
		t.Errorf("value has length %d, tail %v", len(got), got[len(got)-2:])
	}
	if want := []uint16{0, 21}; !reflect.DeepEqual(fake.offsets, want) {
		t.Errorf("offsets = %v, want %v", fake.offsets, want)
	}
}

func TestReadCharacteristicStopsOnEmptyContinuation(t *testing.T) {
	first := make([]byte, 21)
	fake := &fakeChar{replies: [][]byte{first, {}}}
	conn := connWithChar(fake, 22)

	got, err := conn.ReadCharacteristic(liteInfoUpper, psmCharUpper)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 21 {
		t.Errorf("value has length %d, want 21", len(got))
	}
	if fake.calls != 2 {
		t.Errorf("made %d calls, want 2", fake.calls)
	}
}

func TestReadCharacteristicStopsAtMaxAttributeLength(t *testing.T) {
	// A device that keeps answering with full chunks must not loop forever.
	full := make([]byte, 21)
	replies := make([][]byte, 40)
	for i := range replies {
		replies[i] = full
	}
	fake := &fakeChar{replies: replies}
	conn := connWithChar(fake, 22)

	got, err := conn.ReadCharacteristic(liteInfoUpper, psmCharUpper)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) < maxAttributeLen {
		t.Errorf("stopped at %d bytes before reaching the attribute cap", len(got))
	}
	if len(got) > maxAttributeLen+21 {
		t.Errorf("read %d bytes, well past the %d-byte attribute cap", len(got), maxAttributeLen)
	}
}

func TestReadCharacteristicWrapsDBusError(t *testing.T) {
	fake := &fakeChar{err: dbus.Error{Name: "org.bluez.Error.NotPermitted", Body: []any{"denied"}}}
	conn := connWithChar(fake, 517)

	_, err := conn.ReadCharacteristic(liteInfoUpper, psmCharUpper)
	if err == nil {
		t.Fatal("expected an error")
	}
	// The friendly text is there for the user...
	if !containsStr(err.Error(), "bluetoothctl") {
		t.Errorf("error = %q, want the pairing hint", err)
	}
	// ...and the dbus.Error is still wrapped, so a caller can branch on the name.
	var dbusErr dbus.Error
	if !errors.As(err, &dbusErr) || dbusErr.Name != "org.bluez.Error.NotPermitted" {
		t.Errorf("error did not keep the dbus.Error: %v", err)
	}
}

func TestChunkLooksFull(t *testing.T) {
	tests := []struct {
		n    int
		mtu  uint16
		want bool
	}{
		// Known MTU: exactly payload-sized means "maybe truncated".
		{21, 22, true},
		{20, 22, false},
		{22, 22, false},
		{516, 517, true},
		// Unknown MTU (BlueZ < 5.62): fall back to the default ATT payload.
		{22, 0, true},
		{23, 0, true},
		{21, 0, false},
		// An MTU too small to mean anything is treated as unknown.
		{22, 1, true},
	}
	for _, tc := range tests {
		if got := chunkLooksFull(tc.n, tc.mtu); got != tc.want {
			t.Errorf("chunkLooksFull(%d, %d) = %v, want %v", tc.n, tc.mtu, got, tc.want)
		}
	}
}

// ── Lookup and the pre-discovery state ───────────────────────────────────────

func TestLookupCharBeforeDiscovery(t *testing.T) {
	conn := &Connection{fd: -1}

	if conn.HasService(liteInfoUpper) {
		t.Error("HasService is true before discovery")
	}
	if conn.ListServices() != "" {
		t.Error("ListServices is non-empty before discovery")
	}

	_, err := conn.ReadCharacteristic(liteInfoUpper, psmCharUpper)
	if !errors.Is(err, ErrGATTNotFound) {
		t.Errorf("error = %v, want ErrGATTNotFound", err)
	}
	if !containsStr(err.Error(), "DiscoverServices") {
		t.Errorf("error = %q, want it to name DiscoverServices", err)
	}
}

func TestLookupCharMissingCharacteristic(t *testing.T) {
	conn := connWithChar(&fakeChar{}, 517)

	err := conn.WriteCharacteristic(liteInfoUpper, "4E57454E-4459-0002-0009-000000000000", []byte{1})
	if !errors.Is(err, ErrGATTNotFound) {
		t.Fatalf("error = %v, want ErrGATTNotFound", err)
	}
	// The discovered services go in the message so the mismatch is diagnosable
	// without a second run.
	if !containsStr(err.Error(), liteInfoUpper) {
		t.Errorf("error = %q, want it to list the discovered services", err)
	}
}

func TestHasServiceAndListServices(t *testing.T) {
	conn := connWithChar(&fakeChar{}, 517)

	if !conn.HasService(liteInfoUpper) {
		t.Error("HasService missed the discovered service")
	}
	// BlueZ's own spelling must match too.
	if !conn.HasService(liteInfoLower) {
		t.Error("HasService missed the lowercase spelling")
	}
	if conn.HasService("0000180F-0000-1000-8000-00805F9B34FB") {
		t.Error("HasService reported a service the device does not have")
	}
	if got := conn.ListServices(); got != liteInfoUpper {
		t.Errorf("ListServices = %q", got)
	}
}

// ── Writes ───────────────────────────────────────────────────────────────────

// fakeWriter records the write type and value it was called with.
type fakeWriter struct {
	method    string
	value     []byte
	writeType string
	err       error
}

func (f *fakeWriter) CallWithContext(_ context.Context, method string, _ dbus.Flags, args ...any) *dbus.Call {
	f.method = method
	if len(args) > 0 {
		f.value, _ = args[0].([]byte)
	}
	if len(args) > 1 {
		if options, ok := args[1].(map[string]dbus.Variant); ok {
			if v, ok := options["type"]; ok {
				f.writeType, _ = v.Value().(string)
			}
		}
	}
	return &dbus.Call{Err: f.err}
}

func TestWriteCharacteristicTypes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		write    func(*Connection) error
		wantType string
	}{
		{"with response", func(c *Connection) error {
			return c.WriteCharacteristic(liteInfoUpper, psmCharUpper, []byte{0x01})
		}, "request"},
		{"without response", func(c *Connection) error {
			return c.WriteCharacteristicNoResponse(liteInfoUpper, psmCharUpper, []byte{0x01})
		}, "command"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeWriter{}
			conn := connWithChar(&fakeChar{}, 517)
			conn.g.objFn = func(dbus.ObjectPath) bluez.Caller { return fake }

			if err := tc.write(conn); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if fake.method != bluez.GattCharIface+".WriteValue" {
				t.Errorf("method = %q", fake.method)
			}
			if fake.writeType != tc.wantType {
				t.Errorf("write type = %q, want %q", fake.writeType, tc.wantType)
			}
			if !reflect.DeepEqual(fake.value, []byte{0x01}) {
				t.Errorf("value = %v", fake.value)
			}
		})
	}
}

func TestWriteCharacteristicNilValueMarshalsAsEmptyArray(t *testing.T) {
	fake := &fakeWriter{}
	conn := connWithChar(&fakeChar{}, 517)
	conn.g.objFn = func(dbus.ObjectPath) bluez.Caller { return fake }

	if err := conn.WriteCharacteristic(liteInfoUpper, psmCharUpper, nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The "ay" signature needs an array, not nothing.
	if fake.value == nil {
		t.Error("nil value was passed through as nil")
	}
	if len(fake.value) != 0 {
		t.Errorf("value = %v, want empty", fake.value)
	}
}

func TestWriteCharacteristicErrorNamesTheFlags(t *testing.T) {
	// A write-without-response to a request-only characteristic is otherwise
	// impossible to diagnose from the error alone.
	fake := &fakeWriter{err: dbus.Error{Name: "org.bluez.Error.NotSupported"}}
	conn := connWithChar(&fakeChar{}, 517)
	conn.g.objFn = func(dbus.ObjectPath) bluez.Caller { return fake }

	err := conn.WriteCharacteristicNoResponse(liteInfoUpper, psmCharUpper, []byte{1})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"command", "read, notify"} {
		if !containsStr(err.Error(), want) {
			t.Errorf("error = %q, want it to mention %q", err, want)
		}
	}
}

// ── Notifications ────────────────────────────────────────────────────────────

func TestWaitNotificationRequiresSubscribe(t *testing.T) {
	conn := connWithChar(&fakeChar{}, 517)
	_, err := conn.WaitNotification(liteInfoUpper, psmCharUpper, 1)
	if err == nil || !containsStr(err.Error(), "Subscribe") {
		t.Errorf("error = %v, want it to point at Subscribe", err)
	}
}

func TestWaitNotificationDeliversQueuedValue(t *testing.T) {
	conn := connWithChar(&fakeChar{}, 517)
	entry := conn.g.chars[charKey{liteInfoUpper, psmCharUpper}]
	queue := make(chan []byte, notifyQueueDepth)
	conn.g.notify[entry.path] = queue

	// A notification that arrived before the caller asked for it is still
	// there — the queue is what makes subscribe-then-write-then-wait safe.
	queue <- []byte{0x02}

	got, err := conn.WaitNotification(liteInfoUpper, psmCharUpper, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, []byte{0x02}) {
		t.Errorf("value = %v", got)
	}
}

func TestWaitNotificationTimesOut(t *testing.T) {
	conn := connWithChar(&fakeChar{}, 517)
	entry := conn.g.chars[charKey{liteInfoUpper, psmCharUpper}]
	conn.g.notify[entry.path] = make(chan []byte, notifyQueueDepth)

	_, err := conn.WaitNotification(liteInfoUpper, psmCharUpper, 1)
	if err == nil {
		t.Fatal("expected a timeout")
	}
	// A GATT timeout is a hard failure, not the retry-me sentinel the L2CAP
	// receive path uses; leaking that would make a net.Conn consumer spin.
	if errors.Is(err, ErrRecvTimeout) {
		t.Error("a GATT timeout surfaced as ErrRecvTimeout")
	}
}

func TestWaitNotificationWakesOnDisconnect(t *testing.T) {
	conn := connWithChar(&fakeChar{}, 517)
	entry := conn.g.chars[charKey{liteInfoUpper, psmCharUpper}]
	conn.g.notify[entry.path] = make(chan []byte, notifyQueueDepth)

	go func() {
		time.Sleep(10 * time.Millisecond)
		conn.g.markLost()
	}()

	start := time.Now()
	_, err := conn.WaitNotification(liteInfoUpper, psmCharUpper, 30)
	if !errors.Is(err, ErrGATTDisconnected) {
		t.Fatalf("error = %v, want ErrGATTDisconnected", err)
	}
	// It must not sit out the full timeout: there is nothing left to wait for.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("waited %v after the link dropped", elapsed)
	}
}

func TestParentPath(t *testing.T) {
	tests := []struct {
		in   dbus.ObjectPath
		want dbus.ObjectPath
	}{
		{"/org/bluez/hci0/dev_AA/service0010/char0011", "/org/bluez/hci0/dev_AA/service0010"},
		{"/org/bluez", "/org"},
		{"/", ""},
		{"", ""},
	}
	for _, tc := range tests {
		if got := parentPath(tc.in); got != tc.want {
			t.Errorf("parentPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func containsStr(haystack, needle string) bool {
	return len(needle) == 0 || len(haystack) >= len(needle) && indexOfStr(haystack, needle) >= 0
}

func indexOfStr(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
