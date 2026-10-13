//go:build linux

package bleprovider

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/prop"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"go.uber.org/zap"
)

func TestAdvertisementRefreshCoalescesAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &runtime{advertisementRefresh: make(chan struct{}, 1)}
	for i := 0; i < 10000; i++ {
		r.requestAdvertisementRefresh()
	}
	if len(r.advertisementRefresh) != 1 {
		t.Fatal("unbounded requests")
	}
	entered := make(chan time.Time, 4)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runAdvertisementRefresh(ctx, r.advertisementRefresh, 40*time.Millisecond, func(ctx context.Context) error {
			entered <- time.Now()
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	first := <-entered
	for i := 0; i < 10000; i++ {
		r.requestAdvertisementRefresh()
	}
	release <- struct{}{}
	second := <-entered
	if second.Sub(first) < 35*time.Millisecond {
		t.Fatal("refresh burst")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("not joined")
	}
	select {
	case <-entered:
		t.Fatal("pulse after cancellation")
	default:
	}
}
func TestAdvertisementRefreshTransientErrorRetries(t *testing.T) {
	// A failed refresh must not kill the provider loop (previously one
	// controller-busy took down every established link); only a changed
	// BlueZ owner is fatal.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requests := make(chan struct{}, 1)
	calls := 0
	done := make(chan error, 1)
	go func() {
		done <- runAdvertisementRefresh(ctx, requests, 40*time.Millisecond, func(context.Context) error {
			calls++
			if calls == 1 {
				return errors.New("bus send failed")
			}
			return nil
		})
	}()
	requests <- struct{}{}
	time.Sleep(100 * time.Millisecond)
	requests <- struct{}{}
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil || calls != 2 {
			t.Fatal(err, calls)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("not joined")
	}
}
func TestAdvertisementRefreshOwnerChangeStillFatal(t *testing.T) {
	requests := make(chan struct{}, 1)
	requests <- struct{}{}
	calls := 0
	expected := fmt.Errorf("wrap: %w", errBlueZOwnerChanged)
	err := runAdvertisementRefresh(context.Background(), requests, time.Second, func(context.Context) error { calls++; return expected })
	if !errors.Is(err, errBlueZOwnerChanged) || calls != 1 {
		t.Fatal(err, calls)
	}
}
func TestAdvertisementRefreshIdleAndCancelledDoNotPulse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	requests := make(chan struct{}, 1)
	requests <- struct{}{}
	if err := runAdvertisementRefresh(ctx, requests, time.Second, func(context.Context) error { t.Fatal("cancelled pulse"); return nil }); err != nil {
		t.Fatal(err)
	}
}
func TestAdvertisementBlockedSendClosesOnlyOwnedBusAndJoins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	unblock := make(chan struct{})
	var ended, closed atomic.Bool
	err := boundedAdvertisementEmit(ctx, func() error { <-unblock; ended.Store(true); return errors.New("closed") }, func() error { closed.Store(true); close(unblock); return nil })
	if !errors.Is(err, context.DeadlineExceeded) || !closed.Load() || !ended.Load() {
		t.Fatal(err, closed.Load(), ended.Load())
	}
}
func TestAdvertisementImmediateSendErrorDoesNotCloseBus(t *testing.T) {
	err := boundedAdvertisementEmit(context.Background(), func() error { return errors.New("failure") }, func() error { t.Fatal("unexpected close"); return nil })
	if err == nil {
		t.Fatal("error lost")
	}
}

func privateAdvertisementBus(t *testing.T) (string, func() *dbus.Conn) {
	t.Helper()
	path, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Skip("private dbus-daemon unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	cmd := exec.CommandContext(ctx, path, "--session", "--nofork", "--nopidfile", "--print-address=1")
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); _ = cmd.Wait() })
	address, err := bufio.NewReader(output).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	connect := func() *dbus.Conn {
		c, e := dbus.Connect(strings.TrimSpace(address))
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { _ = c.Close() })
		return c
	}
	return strings.TrimSpace(address), connect
}
func TestAdvertisementRefreshRealDBusExactOwnedSignalAndProperties(t *testing.T) {
	address, connect := privateAdvertisementBus(t)
	bluez := connect()
	app, closeTransport, err := connectProviderBusAddress(context.Background(), address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeTransport(); _ = app.Close() })
	if !app.SupportsUnixFDs() {
		t.Fatal("native Unix FD transport was lost")
	}
	foreign := connect()
	if reply, err := bluez.RequestName(bluezName, dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatal(reply, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	owner, err := bluezOwner(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	uuid, _ := ServiceUUID(64)
	adv, _ := NewAdvertisement(445, "field", DefaultPSM)
	payload, _ := adv.MarshalBinary()
	properties := advertisementProperties(uuid, payload, nil)
	if _, err = prop.Export(app, advertPath, map[string]map[string]*prop.Prop{advertInterface: properties}); err != nil {
		t.Fatal(err)
	}
	var before map[string]dbus.Variant
	if err = bluez.Object(app.Names()[0], advertPath).Call("org.freedesktop.DBus.Properties.GetAll", 0, advertInterface).Store(&before); err != nil {
		t.Fatal(err)
	}
	signals := make(chan *dbus.Signal, 4)
	bluez.Signal(signals)
	if err = bluez.AddMatchSignal(dbus.WithMatchInterface("org.freedesktop.DBus.Properties"), dbus.WithMatchMember("PropertiesChanged")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = refreshOwnedAdvertisement(ctx, app, closeTransport, owner, uuid, payload); err != nil {
			t.Fatal(err)
		}
		select {
		case signal := <-signals:
			if signal.Sender != app.Names()[0] || signal.Sender == foreign.Names()[0] || signal.Path != advertPath || signal.Name != "org.freedesktop.DBus.Properties.PropertiesChanged" {
				t.Fatalf("wrong signal: %#v", signal)
			}
			if got := dbus.SignatureOf(signal.Body...).String(); got != "sa{sv}as" {
				t.Fatal(got)
			}
			expected := map[string]dbus.Variant{"ServiceData": before["ServiceData"]}
			if len(signal.Body) != 3 || signal.Body[0] != advertInterface || !reflect.DeepEqual(signal.Body[1], expected) || len(signal.Body[2].([]string)) != 0 {
				t.Fatalf("changed payload: %#v", signal.Body)
			}
		case <-ctx.Done():
			t.Fatal("no signal", ctx.Err())
		}
	}
	var after map[string]dbus.Variant
	if err = bluez.Object(app.Names()[0], advertPath).Call("org.freedesktop.DBus.Properties.GetAll", 0, advertInterface).Store(&after); err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("export changed", err)
	}
	if err = refreshOwnedAdvertisement(ctx, app, closeTransport, ":999.999", uuid, payload); !errors.Is(err, errBlueZOwnerChanged) {
		t.Fatal(err)
	}
	select {
	case s := <-signals:
		t.Fatal("wrong-owner emitted", s)
	case <-time.After(20 * time.Millisecond):
	}
	if _, err = bluez.ReleaseName(bluezName); err != nil {
		t.Fatal(err)
	}
	if _, err = foreign.RequestName(bluezName, dbus.NameFlagDoNotQueue); err != nil {
		t.Fatal(err)
	}
	if err = refreshOwnedAdvertisement(ctx, app, closeTransport, owner, uuid, payload); !errors.Is(err, errBlueZOwnerChanged) {
		t.Fatal("old registration used after owner change", err)
	}
}

type refreshTestNode struct {
	attached chan struct{}
	cheaper  bool
}

func (n *refreshTestNode) HasCheaperLink(int32, uint16) bool { return n.cheaper }
func (n *refreshTestNode) AttachStream(ctx context.Context, _ int32, _ net.Conn, _ uint16) error {
	close(n.attached)
	<-ctx.Done()
	return ctx.Err()
}
func TestAdvertisementRefreshOnlyAfterAuthenticatedInboundAdmission(t *testing.T) {
	for _, kind := range []string{"admitted", "untrusted", "full", "cheaper"} {
		t.Run(kind, func(t *testing.T) {
			server, client := metricsTLSConfigs(t)
			credentials := &localmesh.Credentials{Org: 64, Asset: 1, Certificate: server.Certificates[0], Verify: func(chain [][]byte, _ time.Time) (localmesh.Identity, error) {
				if kind == "untrusted" {
					return localmesh.Identity{}, errors.New("untrusted")
				}
				return localmesh.Identity{Org: 64, Asset: 2}, nil
			}}
			node := &refreshTestNode{attached: make(chan struct{}), cheaper: kind == "cheaper"}
			r := &runtime{cfg: Config{Credentials: credentials, Node: node, TargetPeers: 1, Logger: zap.NewNop()}, active: map[int32]struct{}{}, nextAttempt: map[int32]time.Time{}, advertisementRefresh: make(chan struct{}, 1)}
			if kind == "full" {
				r.active[77] = struct{}{}
			}
			r.serverTLS = r.makeServerTLS()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			done := make(chan struct{})
			go func() { r.acceptLink(ctx, a); close(done) }()
			secure := tls.Client(b, client)
			_ = secure.HandshakeContext(ctx)
			clientDone := make(chan struct{})
			go func() { _, _ = io.Copy(io.Discard, secure); close(clientDone) }()
			if kind == "admitted" {
				select {
				case <-node.attached:
				case <-ctx.Done():
					t.Fatal("inbound was not attached")
				}
				if len(r.advertisementRefresh) != 1 {
					t.Fatal("admitted inbound did not request refresh")
				}
				cancel()
			}
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("inbound worker leaked")
			}
			_ = b.Close()
			<-clientDone
			if kind != "admitted" && len(r.advertisementRefresh) != 0 {
				t.Fatal("rejected inbound requested refresh")
			}
		})
	}
}

type advertisementBlockedConn struct {
	net.Conn
	entered chan struct{}
	once    sync.Once
}

func (c *advertisementBlockedConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.entered) })
	return c.Conn.Write(p)
}

func TestAdvertisementBlockedGodbusWriteIsJoinedByRawTransportClose(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	raw := &advertisementBlockedConn{Conn: a, entered: make(chan struct{})}
	bus, err := dbus.NewConn(raw)
	if err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = boundedAdvertisementEmit(ctx, func() error {
		return bus.Emit(advertPath, "org.freedesktop.DBus.Properties.PropertiesChanged", advertInterface, map[string]dbus.Variant{}, []string{})
	}, raw.Close)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	select {
	case <-raw.entered:
	default:
		t.Fatal("write never blocked")
	}
}

func TestAdvertisementGodbusCloseAloneCannotInterruptWrite(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	raw := &advertisementBlockedConn{Conn: a, entered: make(chan struct{})}
	bus, err := dbus.NewConn(raw)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- bus.Emit(advertPath, "org.freedesktop.DBus.Properties.PropertiesChanged", advertInterface, map[string]dbus.Variant{}, []string{})
	}()
	<-raw.entered
	closed := make(chan struct{})
	go func() { _ = bus.Close(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("expected pinned godbus to wait for output lock")
	case <-time.After(20 * time.Millisecond):
	}
	_ = raw.Close()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("godbus close not joined")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("blocked write unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("write not joined")
	}
}

func TestAdvertisementLocalBusAddress(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"unix:path=/run/dbus/system_bus_socket", "/run/dbus/system_bus_socket"},
		{"unix:abstract=some%20bus,guid=123", "@some bus"},
		{"unix:path=/tmp/a%2cb", "/tmp/a,b"},
	} {
		got, err := providerBusSocket(tc.input)
		if err != nil || got != tc.want {
			t.Fatal(tc, got, err)
		}
	}
	for _, value := range []string{"", "tcp:host=localhost,port=99", "unix:path=", "unix:path=/a,abstract=b", "unix:path=%zz", "unix:path"} {
		if _, err := providerBusSocket(value); err == nil {
			t.Fatal("accepted", value)
		}
	}
}

func TestAdvertisementBusCancelledConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if bus, _, err := connectProviderBusAddress(ctx, "unix:path=/nonexistent-wendy-private-bus"); err == nil || bus != nil {
		t.Fatal(bus, err)
	}
}

func TestAdvertisementBusShutdownJoinsBlockedContextCall(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	raw := &advertisementBlockedConn{Conn: a, entered: make(chan struct{})}
	bus, err := dbus.NewConn(raw)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := guardProviderBusShutdown(ctx, 20*time.Millisecond, raw.Close)
	defer stop()
	defer bus.Close()
	done := make(chan error, 1)
	go func() {
		done <- bus.Object("org.bluez", "/").CallWithContext(ctx, "org.freedesktop.DBus.ObjectManager.GetManagedObjects", 0).Err
	}()
	<-raw.entered
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled call succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("context call send not joined")
	}
}
func TestAdvertisementBusShutdownNormalExitDoesNotCloseTransport(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := guardProviderBusShutdown(ctx, time.Hour, func() error { t.Error("normal teardown closed transport early"); return nil })
	cancel()
	stop()
}
