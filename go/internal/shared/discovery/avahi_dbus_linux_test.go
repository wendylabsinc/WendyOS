//go:build linux

package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

// ── txtFromByteSlices (pure logic) ─────────────────────────────────────────

func TestTxtFromByteSlices(t *testing.T) {
	tests := []struct {
		name string
		txt  [][]byte
		want map[string]string
	}{
		{
			name: "key=value entries",
			txt:  [][]byte{[]byte("displayname=Test Board"), []byte("tls=true")},
			want: map[string]string{"displayname": "Test Board", "tls": "true"},
		},
		{
			// Boolean attribute (no '=') maps to an empty value — same rule
			// as parseTXTRecord (mdns.go:26).
			name: "boolean attribute maps to empty value",
			txt:  [][]byte{[]byte("flag")},
			want: map[string]string{"flag": ""},
		},
		{
			// First occurrence of a repeated key wins (RFC 6763 §6.4).
			name: "first key wins on repeat",
			txt:  [][]byte{[]byte("id=first"), []byte("id=second")},
			want: map[string]string{"id": "first"},
		},
		{
			name: "empty key is skipped, not treated as ending the record",
			txt:  [][]byte{[]byte(""), []byte("id=agent-1")},
			want: map[string]string{"id": "agent-1"},
		},
		{
			name: "nil input yields an empty, non-nil map",
			txt:  nil,
			want: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := txtFromByteSlices(tt.txt)
			if len(got) != len(tt.want) {
				t.Fatalf("txtFromByteSlices(%v) = %v, want %v", tt.txt, got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("txtFromByteSlices(%v)[%q] = %q, want %q", tt.txt, k, got[k], v)
				}
			}
		})
	}
}

// ── decodeItemNew (pure logic) ─────────────────────────────────────────────

func TestDecodeItemNew(t *testing.T) {
	t.Run("valid body decodes every field", func(t *testing.T) {
		body := []any{int32(2), int32(0), "wendyos-prudent-lark", "_wendyos._udp", "local", uint32(0)}
		item, ok := decodeItemNew(body)
		if !ok {
			t.Fatal("decodeItemNew returned false for a valid body")
		}
		want := avahiItemNew{iface: 2, proto: 0, name: "wendyos-prudent-lark", stype: "_wendyos._udp", domain: "local"}
		if item != want {
			t.Errorf("decodeItemNew = %+v, want %+v", item, want)
		}
	})

	t.Run("too short is rejected", func(t *testing.T) {
		if _, ok := decodeItemNew([]any{int32(2), int32(0), "name"}); ok {
			t.Error("decodeItemNew should reject a body missing the domain field")
		}
	})

	t.Run("wrong type in a field is rejected rather than panicking", func(t *testing.T) {
		body := []any{"not-an-int32", int32(0), "name", "_wendyos._udp", "local", uint32(0)}
		if _, ok := decodeItemNew(body); ok {
			t.Error("decodeItemNew should reject a body with the wrong type for iface")
		}
	})

	t.Run("empty body is rejected", func(t *testing.T) {
		if _, ok := decodeItemNew(nil); ok {
			t.Error("decodeItemNew should reject a nil body")
		}
	})
}

// ── decodeResolveReply (pure logic, with a faked interface lookup) ─────────

func withFakeInterfaceLookup(t *testing.T, lookup func(index int) (*net.Interface, error)) {
	t.Helper()
	orig := interfaceByIndexFn
	t.Cleanup(func() { interfaceByIndexFn = orig })
	interfaceByIndexFn = lookup
}

func resolveReplyBody(iface int32, name, host, address string, port uint16, txt [][]byte) []any {
	return []any{iface, int32(0), name, "_wendyos._udp", "local", host, int32(0), address, port, txt, uint32(0)}
}

func TestDecodeResolveReply(t *testing.T) {
	withFakeInterfaceLookup(t, func(index int) (*net.Interface, error) {
		if index == 3 {
			return &net.Interface{Name: "eth3"}, nil
		}
		return nil, fmt.Errorf("no such interface: %d", index)
	})

	t.Run("IPv4 reply decodes hostname, address, port and TXT", func(t *testing.T) {
		body := resolveReplyBody(3, "wendyos-foo", "wendyos-foo.local.", "192.168.1.50", 50051,
			[][]byte{[]byte("displayname=Foo"), []byte("tls=true")})

		svc, ok := decodeResolveReply(body)
		if !ok {
			t.Fatal("decodeResolveReply returned false for a valid reply")
		}
		if svc.InstanceName != "wendyos-foo" {
			t.Errorf("InstanceName = %q, want %q", svc.InstanceName, "wendyos-foo")
		}
		// Trailing dot stripped.
		if svc.Hostname != "wendyos-foo.local" {
			t.Errorf("Hostname = %q, want %q (trailing dot stripped)", svc.Hostname, "wendyos-foo.local")
		}
		if svc.IPAddress != "192.168.1.50" {
			t.Errorf("IPAddress = %q, want %q", svc.IPAddress, "192.168.1.50")
		}
		if svc.Port != 50051 {
			t.Errorf("Port = %d, want 50051", svc.Port)
		}
		if svc.InterfaceName != "eth3" {
			t.Errorf("InterfaceName = %q, want %q", svc.InterfaceName, "eth3")
		}
		if svc.TXTRecords["displayname"] != "Foo" || svc.TXTRecords["tls"] != "true" {
			t.Errorf("TXTRecords = %v, want displayname/tls parsed", svc.TXTRecords)
		}
	})

	t.Run("IPv6 link-local address gets a zone suffix", func(t *testing.T) {
		body := resolveReplyBody(3, "wendyos-bar", "wendyos-bar.local.", "fe80::1", 50051, nil)

		svc, ok := decodeResolveReply(body)
		if !ok {
			t.Fatal("decodeResolveReply returned false for a valid reply")
		}
		if svc.IPAddress != "fe80::1%eth3" {
			t.Errorf("IPAddress = %q, want the %%eth3 zone suffix appended", svc.IPAddress)
		}
	})

	t.Run("routable IPv6 address gets no zone suffix", func(t *testing.T) {
		body := resolveReplyBody(3, "wendyos-baz", "wendyos-baz.local.", "2001:db8::1", 50051, nil)

		svc, ok := decodeResolveReply(body)
		if !ok {
			t.Fatal("decodeResolveReply returned false for a valid reply")
		}
		if svc.IPAddress != "2001:db8::1" {
			t.Errorf("IPAddress = %q, want no zone suffix for a non-link-local address", svc.IPAddress)
		}
	})

	t.Run("unresolvable interface index leaves InterfaceName empty and skips the zone suffix", func(t *testing.T) {
		body := resolveReplyBody(99, "wendyos-qux", "wendyos-qux.local.", "fe80::1", 50051, nil)

		svc, ok := decodeResolveReply(body)
		if !ok {
			t.Fatal("decodeResolveReply returned false for a valid reply")
		}
		if svc.InterfaceName != "" {
			t.Errorf("InterfaceName = %q, want empty when the interface cannot be resolved", svc.InterfaceName)
		}
		if svc.IPAddress != "fe80::1" {
			t.Errorf("IPAddress = %q, want no zone suffix when the interface name is unknown", svc.IPAddress)
		}
	})

	t.Run("too short is rejected", func(t *testing.T) {
		if _, ok := decodeResolveReply([]any{int32(3), int32(0)}); ok {
			t.Error("decodeResolveReply should reject a truncated body")
		}
	})

	t.Run("wrong type in a field is rejected rather than panicking", func(t *testing.T) {
		body := []any{int32(3), int32(0), "name", "_wendyos._udp", "local", "host.local.", int32(0),
			"192.168.1.1", "not-a-uint16", [][]byte{}, uint32(0)}
		if _, ok := decodeResolveReply(body); ok {
			t.Error("decodeResolveReply should reject a body with the wrong type for port")
		}
	})
}

// ── fakeAvahiConn: scripted avahiConn for resolveAvahiItem/avahiBrowse ─────

type fakeAvahiCall struct {
	path   dbus.ObjectPath
	method string
	args   []any
}

// fakeAvahiConn is a scripted avahiConn: it never touches a real bus, so
// these tests pin ordering, path filtering and the resolve retry
// deterministically without depending on a Linux host's Avahi daemon.
type fakeAvahiConn struct {
	mu sync.Mutex

	events []string // ordered log: "AddMatchSignal", "Signal", "Call:<method>", "RemoveSignal"
	calls  []fakeAvahiCall

	browserPath      dbus.ObjectPath
	addMatchErr      error
	browserNewErr    error
	freeErr          error
	resolveFn        func(args []any) ([]any, error)
	resolveContextFn func(context.Context, []any) ([]any, error)

	// blockMethods, when set for a method name, makes Call hang until its ctx
	// is done instead of replying — simulating a wedged daemon that still
	// owns the bus name, to pin avahiCallTimeout's bound on GetVersionString
	// and ServiceBrowserNew.
	blockMethods map[string]bool

	sigCh chan *dbus.Signal
	ready chan struct{}
	once  sync.Once
}

func newFakeAvahiConn(browserPath dbus.ObjectPath) *fakeAvahiConn {
	return &fakeAvahiConn{browserPath: browserPath, ready: make(chan struct{})}
}

func (f *fakeAvahiConn) logEvent(e string) {
	f.mu.Lock()
	f.events = append(f.events, e)
	f.mu.Unlock()
}

func (f *fakeAvahiConn) eventIndex(e string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, ev := range f.events {
		if ev == e {
			return i
		}
	}
	return -1
}

func (f *fakeAvahiConn) AddMatchSignal(_ context.Context, iface string) error {
	f.logEvent("AddMatchSignal:" + iface)
	return f.addMatchErr
}

func (f *fakeAvahiConn) Signal(ch chan *dbus.Signal) {
	f.mu.Lock()
	f.sigCh = ch
	f.mu.Unlock()
	f.logEvent("Signal")
	f.once.Do(func() { close(f.ready) })
}

func (f *fakeAvahiConn) RemoveSignal(_ chan *dbus.Signal) {
	f.logEvent("RemoveSignal")
}

func (f *fakeAvahiConn) Call(ctx context.Context, path dbus.ObjectPath, method string, args ...any) ([]any, error) {
	f.mu.Lock()
	f.calls = append(f.calls, fakeAvahiCall{path: path, method: method, args: append([]any(nil), args...)})
	blocking := f.blockMethods[method]
	f.mu.Unlock()
	f.logEvent("Call:" + method)

	if blocking {
		// Simulates a wedged daemon that still owns the bus name: the call
		// never replies on its own, so only a caller-supplied ctx deadline
		// can end it — exactly what avahiCallTimeout exists to bound.
		<-ctx.Done()
		return nil, ctx.Err()
	}

	switch method {
	case avahiGetVersionStringMethod:
		return []any{"avahi 0.8"}, nil
	case avahiServiceBrowserNewMethod:
		if f.browserNewErr != nil {
			return nil, f.browserNewErr
		}
		return []any{f.browserPath}, nil
	case avahiResolveServiceMethod:
		if f.resolveContextFn != nil {
			return f.resolveContextFn(ctx, args)
		}
		if f.resolveFn != nil {
			return f.resolveFn(args)
		}
		return nil, errors.New("fakeAvahiConn: no resolveFn scripted")
	case avahiServiceBrowserFreeMethod:
		return nil, f.freeErr
	default:
		return nil, fmt.Errorf("fakeAvahiConn: unexpected method %q", method)
	}
}

func (f *fakeAvahiConn) resolveCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.method == avahiResolveServiceMethod {
			n++
		}
	}
	return n
}

// ── resolveAvahiItem (sighted address family and bounded retry) ────────────

func TestResolveAvahiItemUsesInetFirst(t *testing.T) {
	withFakeInterfaceLookup(t, func(int) (*net.Interface, error) { return nil, errors.New("n/a") })

	var gotAprotocols []int32
	fake := newFakeAvahiConn("/browser")
	fake.resolveFn = func(args []any) ([]any, error) {
		aprotocol := args[5].(int32)
		gotAprotocols = append(gotAprotocols, aprotocol)
		return resolveReplyBody(1, "wendyos-foo", "wendyos-foo.local.", "192.168.1.1", 50051, nil), nil
	}

	item := avahiItemNew{iface: 1, proto: 0, name: "wendyos-foo", stype: "_wendyos._udp", domain: "local"}
	svc, ok := resolveAvahiItem(context.Background(), fake, item)
	if !ok {
		t.Fatal("resolveAvahiItem returned false for a successful resolve")
	}
	if svc.InstanceName != "wendyos-foo" {
		t.Errorf("InstanceName = %q, want %q", svc.InstanceName, "wendyos-foo")
	}
	if len(gotAprotocols) != 1 || gotAprotocols[0] != avahiProtoInet {
		t.Fatalf("aprotocols tried = %v, want exactly [avahiProtoInet] (no retry needed)", gotAprotocols)
	}
}

func TestResolveAvahiItemRetriesWithProtoUnspecOnFailure(t *testing.T) {
	withFakeInterfaceLookup(t, func(int) (*net.Interface, error) { return nil, errors.New("n/a") })

	var gotAprotocols []int32
	fake := newFakeAvahiConn("/browser")
	fake.resolveFn = func(args []any) ([]any, error) {
		aprotocol := args[5].(int32)
		gotAprotocols = append(gotAprotocols, aprotocol)
		if aprotocol == avahiProtoInet {
			return nil, errors.New("no IPv4 address for this device")
		}
		return resolveReplyBody(1, "wendyos-v6only", "wendyos-v6only.local.", "2001:db8::1", 50051, nil), nil
	}

	item := avahiItemNew{iface: 1, proto: 0, name: "wendyos-v6only", stype: "_wendyos._udp", domain: "local"}
	svc, ok := resolveAvahiItem(context.Background(), fake, item)
	if !ok {
		t.Fatal("resolveAvahiItem returned false; want the retry to succeed")
	}
	if svc.IPAddress != "2001:db8::1" {
		t.Errorf("IPAddress = %q, want the IPv6 address from the retried resolve", svc.IPAddress)
	}
	if len(gotAprotocols) != 2 || gotAprotocols[0] != avahiProtoInet || gotAprotocols[1] != avahiProtoUnspec {
		t.Fatalf("aprotocols tried = %v, want [avahiProtoInet, avahiProtoUnspec] in that order", gotAprotocols)
	}
}

func TestResolveAvahiItemFailsWhenBothAttemptsFail(t *testing.T) {
	fake := newFakeAvahiConn("/browser")
	fake.resolveFn = func(args []any) ([]any, error) {
		return nil, errors.New("device gone")
	}

	item := avahiItemNew{iface: 1, proto: 0, name: "wendyos-gone", stype: "_wendyos._udp", domain: "local"}
	if _, ok := resolveAvahiItem(context.Background(), fake, item); ok {
		t.Error("resolveAvahiItem should return false when both attempts fail")
	}
	if got := fake.resolveCallCount(); got != 2 {
		t.Errorf("resolve call count = %d, want 2 (both attempts made)", got)
	}
}

// ── avahiBrowse (full sequence: ordering, path filtering, cleanup) ─────────

// TestAvahiBrowseOrderingPathFilterAndCleanup is the property this whole
// backend exists for: subscribe-before-browse ordering (so an ItemNew cannot
// race the browser's creation reply), signals from a foreign ServiceBrowser
// object are dropped, a resolved ItemNew is emitted, an ItemRemove is a
// harmless no-op, and on ctx-done the resolver pool drains and the browser is
// freed — all driven through fakeAvahiConn, no real system bus involved.
func TestAvahiBrowseOrderingPathFilterAndCleanup(t *testing.T) {
	withFakeInterfaceLookup(t, func(index int) (*net.Interface, error) {
		if index == 7 {
			return &net.Interface{Name: "eth7"}, nil
		}
		return nil, errors.New("n/a")
	})

	const browserPath = dbus.ObjectPath("/org/freedesktop/Avahi/Client1/ServiceBrowser1")
	fake := newFakeAvahiConn(browserPath)
	fake.resolveFn = func(args []any) ([]any, error) {
		name := args[2].(string)
		return resolveReplyBody(7, name, name+".local.", "192.168.1.9", 50051,
			[][]byte{[]byte("displayname=Prudent Lark")}), nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	emitted := make(chan MDNSService, 8)
	done := make(chan error, 1)
	go func() {
		done <- avahiBrowse(ctx, fake, "_wendyos._udp", func(svc MDNSService) {
			emitted <- svc
		})
	}()

	select {
	case <-fake.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("avahiBrowse never registered its signal channel")
	}

	// A signal from some other ServiceBrowser object on the bus must be
	// dropped: the match rule filters only by interface, not by path.
	fake.sigCh <- &dbus.Signal{
		Path: "/org/freedesktop/Avahi/Client2/ServiceBrowser9",
		Name: avahiItemNewSignal,
		Body: []any{int32(7), int32(0), "not-ours", "_wendyos._udp", "local", uint32(0)},
	}

	// ItemRemove for our own browser: must not emit and must not crash on a
	// well-formed body (engine owns removals — YAGNI here).
	fake.sigCh <- &dbus.Signal{
		Path: browserPath,
		Name: avahiItemRemoveSignal,
		Body: []any{int32(7), int32(0), "wendyos-prudent-lark", "_wendyos._udp", "local", uint32(0)},
	}

	// The real sighting: ItemNew for our browser's own path.
	fake.sigCh <- &dbus.Signal{
		Path: browserPath,
		Name: avahiItemNewSignal,
		Body: []any{int32(7), int32(0), "wendyos-prudent-lark", "_wendyos._udp", "local", uint32(0)},
	}

	var svc MDNSService
	select {
	case svc = <-emitted:
	case <-time.After(2 * time.Second):
		t.Fatal("avahiBrowse did not emit the resolved ItemNew sighting")
	}
	if svc.InstanceName != "wendyos-prudent-lark" {
		t.Errorf("InstanceName = %q, want %q", svc.InstanceName, "wendyos-prudent-lark")
	}
	if svc.InterfaceName != "eth7" {
		t.Errorf("InterfaceName = %q, want %q", svc.InterfaceName, "eth7")
	}
	if svc.TXTRecords["displayname"] != "Prudent Lark" {
		t.Errorf("TXTRecords[displayname] = %q, want %q", svc.TXTRecords["displayname"], "Prudent Lark")
	}

	select {
	case extra := <-emitted:
		t.Errorf("unexpected extra emission for the foreign-path/ItemRemove signals: %+v", extra)
	default:
	}

	// Cancelling must make avahiBrowse return promptly, with its resolver
	// pool fully drained — no goroutine may outlive the call.
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("avahiBrowse returned %v, want nil after ctx cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("avahiBrowse did not return after ctx cancellation")
	}

	// Ordering: subscribing (both the match rule and the signal channel) must
	// happen before the browser is created, so an ItemNew cannot race
	// ServiceBrowserNew's reply.
	addMatchIdx := fake.eventIndex("AddMatchSignal:" + avahiBrowserIface)
	signalIdx := fake.eventIndex("Signal")
	browserNewIdx := fake.eventIndex("Call:" + avahiServiceBrowserNewMethod)
	if addMatchIdx == -1 || signalIdx == -1 || browserNewIdx == -1 {
		t.Fatalf("missing expected event(s): events=%v", fake.events)
	}
	if !(addMatchIdx < browserNewIdx && signalIdx < browserNewIdx) {
		t.Errorf("events = %v, want AddMatchSignal and Signal both before ServiceBrowserNew", fake.events)
	}

	// Cleanup: the browser is freed on its own path after the loop ends, and
	// the signal channel is deregistered.
	freeIdx := fake.eventIndex("Call:" + avahiServiceBrowserFreeMethod)
	removeSignalIdx := fake.eventIndex("RemoveSignal")
	if freeIdx == -1 || removeSignalIdx == -1 {
		t.Fatalf("missing cleanup event(s): events=%v", fake.events)
	}

	fake.mu.Lock()
	var freeCall *fakeAvahiCall
	for i := range fake.calls {
		if fake.calls[i].method == avahiServiceBrowserFreeMethod {
			freeCall = &fake.calls[i]
		}
	}
	fake.mu.Unlock()
	if freeCall == nil {
		t.Fatal("ServiceBrowser.Free was never called")
	}
	if freeCall.path != browserPath {
		t.Errorf("Free called on path %q, want the browser's own path %q", freeCall.path, browserPath)
	}
}

// TestAvahiBrowseServiceBrowserNewFailureCleansUpSignal pins that a failure
// creating the browser still deregisters the signal channel it already
// registered, rather than leaking it.
func TestAvahiBrowseServiceBrowserNewFailureCleansUpSignal(t *testing.T) {
	fake := newFakeAvahiConn("/browser")
	fake.browserNewErr = errors.New("daemon refused ServiceBrowserNew")

	err := avahiBrowse(context.Background(), fake, "_wendyos._udp", func(MDNSService) {
		t.Error("emit must not be called when ServiceBrowserNew fails")
	})
	if err == nil {
		t.Fatal("avahiBrowse should return an error when ServiceBrowserNew fails")
	}
	if fake.eventIndex("RemoveSignal") == -1 {
		t.Error("avahiBrowse should RemoveSignal even when ServiceBrowserNew fails")
	}
}

// TestAvahiBrowseAddMatchSignalFailure pins that a failure subscribing to
// ServiceBrowser signals is returned as a restartable error without ever
// attempting to create a browser.
func TestAvahiBrowseAddMatchSignalFailure(t *testing.T) {
	fake := newFakeAvahiConn("/browser")
	fake.addMatchErr = errors.New("AddMatch rejected")

	err := avahiBrowse(context.Background(), fake, "_wendyos._udp", func(MDNSService) {
		t.Error("emit must not be called when AddMatchSignal fails")
	})
	if err == nil {
		t.Fatal("avahiBrowse should return an error when AddMatchSignal fails")
	}
	if fake.eventIndex("Call:"+avahiServiceBrowserNewMethod) != -1 {
		t.Error("avahiBrowse should not attempt ServiceBrowserNew after AddMatchSignal fails")
	}
}

// TestAvahiBrowseSignalChannelClosedWithLiveCtxReturnsRestartableError pins
// the CRITICAL fix: godbus closes every channel registered via Signal() when
// the connection's transport dies (a bus/daemon crash calls conn.Close()
// internally, which calls Terminate()) — this can happen with ctx still
// live, and previously produced the exact same nil return as a clean
// ctx.Done() stop. That made the streaming engine treat a dead connection as
// "browse finished cleanly" and never restart it: LAN discovery would
// silently stop for the rest of the session. sigCh is closed here directly,
// without ever cancelling ctx, reproducing exactly that condition.
func TestAvahiBrowseSignalChannelClosedWithLiveCtxReturnsRestartableError(t *testing.T) {
	fake := newFakeAvahiConn("/browser")
	fake.resolveFn = func(args []any) ([]any, error) {
		return resolveReplyBody(1, args[2].(string), "host.local.", "192.168.1.1", 50051, nil), nil
	}

	ctx := context.Background() // deliberately never cancelled
	done := make(chan error, 1)
	go func() {
		done <- avahiBrowse(ctx, fake, "_wendyos._udp", func(MDNSService) {})
	}()

	select {
	case <-fake.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("avahiBrowse never registered its signal channel")
	}

	// Simulate the transport dying: the channel closes on its own, ctx is
	// still live.
	close(fake.sigCh)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("avahiBrowse returned nil for a signal channel that closed with ctx still live; want a restartable error")
		}
		if !errors.Is(err, errAvahiConnectionLost) {
			t.Errorf("err = %v, want errAvahiConnectionLost", err)
		}
		if ctx.Err() != nil {
			t.Fatal("test bug: ctx must still be live for this to pin the right condition")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("avahiBrowse did not return after its signal channel closed")
	}

	// Cleanup must still happen on this path: Free/RemoveSignal are not
	// skipped just because the failure was detected via the channel itself.
	if fake.eventIndex("RemoveSignal") == -1 {
		t.Error("avahiBrowse should still RemoveSignal after a lost connection")
	}
	if fake.eventIndex("Call:"+avahiServiceBrowserFreeMethod) == -1 {
		t.Error("avahiBrowse should still attempt Free after a lost connection")
	}
}

// TestAvahiBrowseFailureSignalReturnsRestartableError pins the other CRITICAL
// half: the avahi daemon can die/restart (systemd bounce, package upgrade)
// while the D-Bus connection to the bus itself stays up. Avahi reports this
// per-browser via a Failure signal rather than dropping the connection, so
// without handling it the read loop would block on sigCh forever, producing
// nothing, with no error and no restart.
func TestAvahiBrowseFailureSignalReturnsRestartableError(t *testing.T) {
	const browserPath = dbus.ObjectPath("/browser")
	fake := newFakeAvahiConn(browserPath)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- avahiBrowse(ctx, fake, "_wendyos._udp", func(MDNSService) {
			t.Error("emit must not be called for a Failure signal")
		})
	}()

	select {
	case <-fake.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("avahiBrowse never registered its signal channel")
	}

	fake.sigCh <- &dbus.Signal{
		Path: browserPath,
		Name: avahiBrowserFailureSig,
		Body: []any{"org.freedesktop.Avahi.Server.Error.Failure"},
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("avahiBrowse returned nil for a ServiceBrowser Failure signal; want a restartable error")
		}
		if errors.Is(err, errAvahiUnavailable) {
			t.Error("a Failure signal after browsing started must not be errAvahiUnavailable (not a fallback trigger)")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("avahiBrowse did not return after a Failure signal")
	}
}

// TestAvahiBrowseCtxCancelStillReturnsNil re-pins, in isolation, that a plain
// ctx cancellation (no channel close, no Failure signal) still returns nil —
// the existing, correct "clean stop" behavior that the two fixes above must
// not disturb. (TestAvahiBrowseOrderingPathFilterAndCleanup already covers
// this end to end; this test isolates just the property in case that test's
// scope ever narrows.)
func TestAvahiBrowseCtxCancelStillReturnsNil(t *testing.T) {
	fake := newFakeAvahiConn("/browser")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- avahiBrowse(ctx, fake, "_wendyos._udp", func(MDNSService) {})
	}()

	select {
	case <-fake.ready:
	case <-time.After(2 * time.Second):
		t.Fatal("avahiBrowse never registered its signal channel")
	}

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("avahiBrowse returned %v, want nil after a plain ctx cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("avahiBrowse did not return after ctx cancellation")
	}
}

// ── bounded availability/setup calls (avahiCallTimeout) ────────────────────

// TestProbeAvahiAvailableTimesOutOnBlockedCall pins the IMPORTANT fix: a
// wedged avahi daemon that still owns its bus name (hung, not gone) must not
// block the probe forever — without a bound, there is no errAvahiUnavailable
// fallback and the backend hangs indefinitely instead of falling back to
// hashicorp/mdns.
func TestProbeAvahiAvailableTimesOutOnBlockedCall(t *testing.T) {
	orig := avahiCallTimeout
	t.Cleanup(func() { avahiCallTimeout = orig })
	avahiCallTimeout = 50 * time.Millisecond

	fake := newFakeAvahiConn("/browser")
	fake.blockMethods = map[string]bool{avahiGetVersionStringMethod: true}

	start := time.Now()
	err := probeAvahiAvailable(context.Background(), fake)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("probeAvahiAvailable should return an error when GetVersionString never replies")
	}
	if elapsed > 2*time.Second {
		t.Errorf("probeAvahiAvailable took %v, want it bounded by avahiCallTimeout (50ms)", elapsed)
	}
}

// TestAvahiBrowseServiceBrowserNewTimesOutAfterSuccessfulProbe pins the other
// half of the IMPORTANT fix: ServiceBrowserNew itself must be bounded too, so
// a daemon that answers the availability probe but then wedges on browser
// creation still produces a restartable error instead of hanging
// avahiBrowse (and therefore the whole streaming session) forever.
func TestAvahiBrowseServiceBrowserNewTimesOutAfterSuccessfulProbe(t *testing.T) {
	orig := avahiCallTimeout
	t.Cleanup(func() { avahiCallTimeout = orig })
	avahiCallTimeout = 50 * time.Millisecond

	fake := newFakeAvahiConn("/browser")
	fake.blockMethods = map[string]bool{avahiServiceBrowserNewMethod: true}

	start := time.Now()
	err := avahiBrowse(context.Background(), fake, "_wendyos._udp", func(MDNSService) {
		t.Error("emit must not be called when ServiceBrowserNew never replies")
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("avahiBrowse should return an error when ServiceBrowserNew never replies")
	}
	if errors.Is(err, errAvahiUnavailable) {
		t.Error("a ServiceBrowserNew timeout after a successful probe must not be errAvahiUnavailable (not a fallback trigger)")
	}
	if elapsed > 2*time.Second {
		t.Errorf("avahiBrowse took %v, want it bounded by avahiCallTimeout (50ms)", elapsed)
	}
	if fake.eventIndex("RemoveSignal") == -1 {
		t.Error("avahiBrowse should still RemoveSignal after a ServiceBrowserNew timeout")
	}
}

// ── probeAvahiAvailable ─────────────────────────────────────────────────────

func TestProbeAvahiAvailable(t *testing.T) {
	t.Run("succeeds when GetVersionString answers", func(t *testing.T) {
		fake := newFakeAvahiConn("/browser")
		if err := probeAvahiAvailable(context.Background(), fake); err != nil {
			t.Errorf("probeAvahiAvailable() = %v, want nil", err)
		}
	})

	t.Run("propagates the error when the call fails", func(t *testing.T) {
		// A conn whose Call always errors, simulating an absent/unreachable
		// Avahi daemon (e.g. GetVersionString itself returns
		// org.freedesktop.DBus.Error.ServiceUnknown).
		errConn := callErrorConn{err: errors.New("org.freedesktop.DBus.Error.ServiceUnknown")}
		if err := probeAvahiAvailable(context.Background(), errConn); err == nil {
			t.Error("probeAvahiAvailable() = nil, want the underlying error")
		}
	})
}

// callErrorConn is an avahiConn whose every Call fails; used to test
// probeAvahiAvailable's plain error-propagation path without scripting a
// full fakeAvahiConn.
type callErrorConn struct{ err error }

func (c callErrorConn) Call(context.Context, dbus.ObjectPath, string, ...any) ([]any, error) {
	return nil, c.err
}
func (c callErrorConn) AddMatchSignal(context.Context, string) error { return nil }
func (c callErrorConn) Signal(chan *dbus.Signal)                     {}
func (c callErrorConn) RemoveSignal(chan *dbus.Signal)               {}

// ── mdnsStreamBackend fallback wiring ───────────────────────────────────────

// TestMdnsStreamBackendFallsBackOnAvahiUnavailable pins the exact wiring
// mdns_linux.go's mdnsStreamBackend must have: errAvahiUnavailable (and only
// that) triggers a fallback to the hashicorp/mdns backend. connectAvahiSystemBus
// is forced to fail so the errAvahiUnavailable path is deterministic
// regardless of whether the test environment has a reachable D-Bus system
// bus, and hashicorpFallbackFn is faked out so this test only pins the
// wiring — not hashicorp/mdns's own real multicast query behavior (already
// covered by backend_hashicorp_test.go).
func TestMdnsStreamBackendFallsBackOnAvahiUnavailable(t *testing.T) {
	origConnect := connectAvahiSystemBus
	t.Cleanup(func() { connectAvahiSystemBus = origConnect })
	connectAvahiSystemBus = func(...dbus.ConnOption) (*dbus.Conn, error) {
		return nil, errors.New("simulated: no system bus reachable")
	}

	var gotServiceType string
	origFallback := hashicorpFallbackFn
	t.Cleanup(func() { hashicorpFallbackFn = origFallback })
	hashicorpFallbackFn = func(ctx context.Context, serviceType string, emit func(MDNSService)) error {
		gotServiceType = serviceType
		emit(MDNSService{InstanceName: "from-hashicorp-fallback"})
		return nil
	}

	var got MDNSService
	err := mdnsStreamBackend(context.Background(), "_wendyos-avahifallbacktest._udp", func(svc MDNSService) {
		got = svc
	})
	if err != nil {
		t.Errorf("mdnsStreamBackend() = %v, want nil", err)
	}
	if gotServiceType != "_wendyos-avahifallbacktest._udp" {
		t.Errorf("fallback called with serviceType %q, want the original service type passed through", gotServiceType)
	}
	if got.InstanceName != "from-hashicorp-fallback" {
		t.Errorf("emit not wired through to the fallback backend: got %+v", got)
	}
}

// TestErrorsIsAvahiUnavailableWrapping pins that mdnsStreamBackend's
// errors.Is(err, errAvahiUnavailable) check only fires for errors that
// actually wrap errAvahiUnavailable (via avahiStreamBackend's %w), and not
// for an unrelated error that merely has similar text — the distinction that
// makes "avahi died mid-browse" restartable instead of silently downgraded.
// (mdnsStreamBackend's non-availability path is just `return err`, and the
// non-availability errors themselves are exercised directly by
// TestAvahiBrowseServiceBrowserNewFailureCleansUpSignal and
// TestAvahiBrowseAddMatchSignalFailure above.)
func TestErrorsIsAvahiUnavailableWrapping(t *testing.T) {
	wrapped := fmt.Errorf("%w: connecting to the system bus: %v", errAvahiUnavailable, errors.New("no such file"))
	if !errors.Is(wrapped, errAvahiUnavailable) {
		t.Error("a %w-wrapped errAvahiUnavailable must satisfy errors.Is")
	}

	unrelated := errors.New("avahi d-bus service unavailable") // same text, not wrapped
	if errors.Is(unrelated, errAvahiUnavailable) {
		t.Error("an unrelated error with matching text must not satisfy errors.Is")
	}
}

// ── opt-in live test against a real Avahi daemon ────────────────────────────

// avahiEntryGroupIface is Avahi's D-Bus interface for publishing records
// (AddService/Commit/Free). Only this test needs it — avahiStreamBackend only
// ever browses/resolves — so it is not promoted to the production consts.
const avahiEntryGroupIface = "org.freedesktop.Avahi.EntryGroup"

// TestAvahiStreamBackendLive is an opt-in end-to-end test against a real
// Avahi daemon: skipped unless WENDY_AVAHI_LIVE_TEST is set, since this
// darwin dev loop (and most CI containers) has no avahi-daemon to talk to.
// Run it on a real Linux box with avahi-daemon running:
//
//	WENDY_AVAHI_LIVE_TEST=1 go test ./go/internal/shared/discovery/ -run TestAvahiStreamBackendLive -v
//
// It registers a throwaway service directly over D-Bus via Avahi's
// EntryGroup API — no avahi-publish-service child process, keeping the test
// itself honest about this backend's whole point being zero child
// processes — then confirms the real avahiStreamBackend (hitting the real
// system bus, not a fake) streams it back with its TXT records intact.
func TestAvahiStreamBackendLive(t *testing.T) {
	if os.Getenv("WENDY_AVAHI_LIVE_TEST") == "" {
		t.Skip("set WENDY_AVAHI_LIVE_TEST=1 to run this test against a real Avahi daemon (Linux only, requires avahi-daemon running)")
	}

	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		t.Fatalf("connecting to the system bus: %v", err)
	}
	defer conn.Close()

	server := conn.Object(avahiService, avahiServerPath)
	var groupPath dbus.ObjectPath
	if err := server.Call(avahiServerIface+".EntryGroupNew", 0).Store(&groupPath); err != nil {
		t.Fatalf("EntryGroupNew: %v", err)
	}
	group := conn.Object(avahiService, groupPath)
	t.Cleanup(func() {
		if call := group.Call(avahiEntryGroupIface+".Free", 0); call.Err != nil {
			t.Logf("freeing entry group: %v", call.Err)
		}
	})

	const serviceType = "_wendy-avahilivetest._tcp"
	instance := fmt.Sprintf("wendy-avahilivetest-%d", os.Getpid())
	wantTXT := map[string]string{"displayname": "Avahi Live Test", "tls": "true"}
	txt := [][]byte{[]byte("displayname=Avahi Live Test"), []byte("tls=true")}

	if call := group.Call(avahiEntryGroupIface+".AddService", 0,
		avahiIfUnspec, avahiProtoUnspec, uint32(0), instance, serviceType, avahiDomainLocal, "", uint16(51260), txt,
	); call.Err != nil {
		t.Fatalf("AddService: %v", call.Err)
	}
	if call := group.Call(avahiEntryGroupIface+".Commit", 0); call.Err != nil {
		t.Fatalf("Commit: %v", call.Err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	emitted := make(chan MDNSService, 4)
	done := make(chan error, 1)
	go func() {
		done <- avahiStreamBackend(ctx, serviceType, func(svc MDNSService) {
			emitted <- svc
		})
	}()

	for {
		select {
		case svc := <-emitted:
			if svc.InstanceName != instance {
				continue // some other service of this type on the LAN
			}
			for k, v := range wantTXT {
				if svc.TXTRecords[k] != v {
					t.Errorf("TXTRecords[%q] = %q, want %q", k, svc.TXTRecords[k], v)
				}
			}
			if svc.Port != 51260 {
				t.Errorf("Port = %d, want 51260", svc.Port)
			}
			cancel()
			<-done
			return
		case err := <-done:
			t.Fatalf("avahiStreamBackend returned before the registered service arrived: %v", err)
		case <-ctx.Done():
			t.Fatal("timed out waiting for the registered service to be discovered")
		}
	}
}

// IPv6-only local mesh/veth sightings must not occupy every resolver worker
// while a reachable remote IPv4 service waits behind them.
func TestAvahiBrowseIPv6SightingsDoNotStarveRemoteIPv4(t *testing.T) {
	fake := newFakeAvahiConn("/browser")
	fake.resolveContextFn = func(ctx context.Context, args []any) ([]any, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		protocol, addressProtocol := args[1].(int32), args[5].(int32)
		if protocol == avahiProtoInet6 && addressProtocol != avahiProtoInet6 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		address := "192.168.2.3"
		if protocol == avahiProtoInet6 {
			address = "fe80::1"
		}
		name := args[2].(string)
		return resolveReplyBody(1, name, name+".local.", address, 50052, nil), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	remote := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- avahiBrowse(ctx, fake, "_wendyos._udp", func(service MDNSService) {
			if service.InstanceName == "remote" {
				remote <- struct{}{}
			}
		})
	}()
	<-fake.ready
	for i := 0; i < probeWorkers; i++ {
		fake.sigCh <- &dbus.Signal{Path: "/browser", Name: avahiItemNewSignal, Body: []any{int32(1), avahiProtoInet6, fmt.Sprintf("local-veth-%d", i), "_wendyos._udp", "local", uint32(0)}}
	}
	fake.sigCh <- &dbus.Signal{Path: "/browser", Name: avahiItemNewSignal, Body: []any{int32(1), avahiProtoInet, "remote", "_wendyos._udp", "local", uint32(0)}}
	select {
	case <-remote:
	case <-ctx.Done():
		t.Error("IPv6-only sightings starved the remote IPv4 service")
	}
	cancel()
	<-done
}

func TestResolveAvahiItemExpiredAttemptDoesNotRetryExpiredContext(t *testing.T) {
	fake := newFakeAvahiConn("/browser")
	fake.blockMethods = map[string]bool{avahiResolveServiceMethod: true}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, ok := resolveAvahiItem(ctx, fake, avahiItemNew{iface: 1, proto: avahiProtoInet, name: "gone", stype: "_wendyos._udp", domain: "local"})
	if ok || fake.resolveCallCount() != 1 {
		t.Fatalf("expired resolver success=%v calls=%d", ok, fake.resolveCallCount())
	}
}

func TestResolveAvahiItemUsesSightedIPv6Family(t *testing.T) {
	fake := newFakeAvahiConn("/browser")
	fake.resolveFn = func(args []any) ([]any, error) {
		if args[1] != avahiProtoInet6 || args[5] != avahiProtoInet6 {
			t.Fatalf("IPv6 sighting resolve protocol=%v address protocol=%v", args[1], args[5])
		}
		return resolveReplyBody(1, "v6-peer", "v6-peer.local.", "2001:db8::1", 50052, nil), nil
	}
	_, ok := resolveAvahiItem(context.Background(), fake, avahiItemNew{iface: 1, proto: avahiProtoInet6, name: "v6-peer", stype: "_wendyos._udp", domain: "local"})
	if !ok || fake.resolveCallCount() != 1 {
		t.Fatalf("IPv6 resolver success=%v calls=%d", ok, fake.resolveCallCount())
	}
}
