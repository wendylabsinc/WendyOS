//go:build linux && regression_bluetooth

package central

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/wendylabsinc/wendy/go/internal/shared/ble/bluez"
)

// This peer speaks BlueZ's actual D-Bus protocol on an owned dbus-daemon. It
// provides controller responses, not a radio. Production object enumeration,
// match rules, method calls, signal routing, queues and session cleanup run.
type gattTransportPeer struct {
	mu                                   sync.Mutex
	bus                                  *dbus.Conn
	connected, resolved                  bool
	connects, disconnects, subscriptions int
}

const transportDevice dbus.ObjectPath = "/org/bluez/hci0/dev_AA_BB_CC_DD_EE_FF"
const transportService dbus.ObjectPath = transportDevice + "/service0010"
const transportChar dbus.ObjectPath = transportService + "/char0011"
const transportOtherChar dbus.ObjectPath = transportService + "/char0012"
const transportOtherUUID = "4E57454E-4459-0002-0002-000000000000"

func (p *gattTransportPeer) GetManagedObjects() (bluez.ManagedObjects, *dbus.Error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return bluez.ManagedObjects{
		"/org/bluez/hci0":  {bluez.AdapterIface: {"Powered": dbus.MakeVariant(true)}},
		transportDevice:    {bluez.DeviceIface: {"Address": dbus.MakeVariant("AA:BB:CC:DD:EE:FF"), "AddressType": dbus.MakeVariant("random"), "Adapter": dbus.MakeVariant(dbus.ObjectPath("/org/bluez/hci0")), "Connected": dbus.MakeVariant(p.connected), "ServicesResolved": dbus.MakeVariant(p.resolved)}},
		transportService:   {bluez.GattServiceIface: {"UUID": dbus.MakeVariant(liteInfoLower)}},
		transportChar:      {bluez.GattCharIface: {"UUID": dbus.MakeVariant(psmCharLower), "Service": dbus.MakeVariant(transportService), "Flags": dbus.MakeVariant([]string{"notify", "read"})}},
		transportOtherChar: {bluez.GattCharIface: {"UUID": dbus.MakeVariant(transportOtherUUID), "Service": dbus.MakeVariant(transportService), "Flags": dbus.MakeVariant([]string{"notify"})}},
	}, nil
}
func (p *gattTransportPeer) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if iface != bluez.DeviceIface {
		return dbus.Variant{}, dbus.NewError("org.bluez.Error.InvalidArguments", nil)
	}
	switch name {
	case "Connected":
		return dbus.MakeVariant(p.connected), nil
	case "ServicesResolved":
		return dbus.MakeVariant(p.resolved), nil
	}
	return dbus.Variant{}, dbus.NewError("org.bluez.Error.InvalidArguments", nil)
}
func (p *gattTransportPeer) Connect() *dbus.Error {
	p.mu.Lock()
	p.connects++
	p.connected = true
	p.resolved = true
	p.mu.Unlock()
	return nil
}
func (p *gattTransportPeer) Disconnect() *dbus.Error {
	p.mu.Lock()
	p.disconnects++
	p.connected = false
	p.mu.Unlock()
	// A real method reply and PropertiesChanged signal, not a call into handleSignal.
	if err := p.bus.Emit(transportDevice, bluez.PropsChangedName, bluez.DeviceIface, map[string]dbus.Variant{"Connected": dbus.MakeVariant(false)}, []string{}); err != nil {
		return dbus.MakeFailedError(err)
	}
	return nil
}
func (p *gattTransportPeer) StartNotify() *dbus.Error {
	p.mu.Lock()
	p.subscriptions++
	p.mu.Unlock()
	return nil
}
func (p *gattTransportPeer) notify(t *testing.T, path dbus.ObjectPath, value []byte) {
	t.Helper()
	if err := p.bus.Emit(path, bluez.PropsChangedName, bluez.GattCharIface, map[string]dbus.Variant{"Value": dbus.MakeVariant(value)}, []string{}); err != nil {
		t.Fatal(err)
	}
}

func newGATTTransportPeer(t *testing.T, borrowed bool) *gattTransportPeer {
	t.Helper()
	daemon, err := exec.LookPath("dbus-daemon")
	if err != nil {
		t.Fatal("required private dbus-daemon unavailable:", err)
	}
	dir := t.TempDir()
	address := "unix:path=" + filepath.Join(dir, "bus")
	log, err := os.Create(filepath.Join(dir, "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(daemon, "--session", "--nofork", "--nopidfile", "--address="+address)
	cmd.Stdout, cmd.Stderr = io.Discard, log
	if err = cmd.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// The unreaped child remains owned until the final signal; no saved PID reuse.
		_ = cmd.Process.Kill()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("private GATT bus did not drain")
		}
		_ = log.Close()
	})
	var bus *dbus.Conn
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		bus, err = dbus.Connect(address)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close() })
	if result, err := bus.RequestName(bluez.Service, dbus.NameFlagDoNotQueue); err != nil || result != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("private BlueZ ownership: %v %v", result, err)
	}
	p := &gattTransportPeer{bus: bus, connected: borrowed, resolved: borrowed}
	for _, export := range []struct {
		path    dbus.ObjectPath
		iface   string
		methods map[string]interface{}
	}{
		{"/", "org.freedesktop.DBus.ObjectManager", map[string]interface{}{"GetManagedObjects": p.GetManagedObjects}},
		{transportDevice, bluez.DeviceIface, map[string]interface{}{"Connect": p.Connect, "Disconnect": p.Disconnect}},
		{transportDevice, bluez.PropsIface, map[string]interface{}{"Get": p.Get}},
		{transportChar, bluez.GattCharIface, map[string]interface{}{"StartNotify": p.StartNotify}},
		{transportOtherChar, bluez.GattCharIface, map[string]interface{}{"StartNotify": p.StartNotify}},
	} {
		if err := bus.ExportMethodTable(export.methods, export.path, export.iface); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", address)
	t.Setenv("WENDY_BT_ADAPTER", "")
	return p
}

func transportConnection(t *testing.T) *Connection {
	t.Helper()
	// Only bypass allocation of an unused real AF_BLUETOOTH socket. No GATT
	// implementation or D-Bus caller is replaced, and no L2CAP/radio claim is made.
	c := &Connection{fd: -1, address: "AA:BB:CC:DD:EE:FF"}
	t.Cleanup(c.Close)
	if err := c.DiscoverServices(3); err != nil {
		t.Fatal(err)
	}
	if !c.HasService(liteInfoUpper) || !c.addrKnown {
		t.Fatal("actual D-Bus discovery lost service/address type")
	}
	for _, char := range []string{psmCharUpper, transportOtherUUID} {
		if err := c.Subscribe(liteInfoUpper, char); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

func TestGATTPrivateBusDisconnectLifecycle(t *testing.T) {
	for _, borrowed := range []bool{false, true} {
		t.Run(fmt.Sprintf("borrowed-%v", borrowed), func(t *testing.T) {
			p := newGATTTransportPeer(t, borrowed)
			c := transportConnection(t)
			payload := []byte{0, 0xff, 0x82, 'o', 'n', 'e'}
			p.notify(t, transportChar, payload)
			if got, err := c.WaitNotification(liteInfoUpper, psmCharUpper, 2); err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("actual wire notification differs: %x %v", got, err)
			}
			// Both per-characteristic queues are empty. A notification on either must
			// not release the other; device disconnect must release every pending waiter.
			type reply struct {
				which int
				data  []byte
				err   error
			}
			replies := make(chan reply, 2)
			started := make(chan struct{}, 2)
			for i, char := range []string{psmCharUpper, transportOtherUUID} {
				go func(i int, char string) {
					started <- struct{}{}
					data, err := c.WaitNotification(liteInfoUpper, char, 4)
					replies <- reply{i, data, err}
				}(i, char)
			}
			<-started
			<-started
			select {
			case got := <-replies:
				t.Fatalf("empty queue completed before disconnect: %+v", got)
			case <-time.After(50 * time.Millisecond):
			}
			p.mu.Lock()
			p.connected = false
			p.mu.Unlock()
			if err := p.bus.Emit(transportDevice, bluez.PropsChangedName, bluez.DeviceIface, map[string]dbus.Variant{"Connected": dbus.MakeVariant(false)}, []string{}); err != nil {
				t.Fatal(err)
			}
			deadline := time.NewTimer(time.Second)
			defer deadline.Stop()
			seen := map[int]bool{}
			for range 2 {
				select {
				case got := <-replies:
					if !errors.Is(got.err, ErrGATTDisconnected) || len(got.data) != 0 {
						t.Errorf("disconnect did not reach pending GATT waiter: %+v", got)
					}
					seen[got.which] = true
				case <-deadline.C:
					// Let the production waiter deadlines drain before Connection.Close changes
					// its session pointer, including when running the missing-wakeup control.
					for len(seen) < 2 {
						select {
						case got := <-replies:
							seen[got.which] = true
						case <-time.After(5 * time.Second):
							t.Fatal("GATT waiter survived own timeout")
						}
					}
					t.Fatal("disconnect did not wake both pending GATT waiters within one second")
				}
			}
			if len(seen) != 2 {
				t.Fatal("duplicate waiter result")
			}
			session := c.g
			c.Close()
			c.Close()
			select {
			case <-session.routerDone:
			default:
				t.Fatal("GATT router survived client close")
			}
			p.mu.Lock()
			connects, disconnects := p.connects, p.disconnects
			p.mu.Unlock()
			want := 1
			if borrowed {
				want = 0
			}
			if connects != want || disconnects != want {
				t.Fatalf("ACL ownership violated: connects=%d disconnects=%d want=%d", connects, disconnects, want)
			}
			// New session performs discovery and notification again; no prior queued
			// payload, lost channel or closed router may be reused.
			replacement := transportConnection(t)
			next := []byte{0x7e, 'n', 'e', 'w', 0}
			p.notify(t, transportOtherChar, next)
			if got, err := replacement.WaitNotification(liteInfoUpper, transportOtherUUID, 2); err != nil || !bytes.Equal(got, next) {
				t.Fatalf("reconnected notification differs: %x %v", got, err)
			}
			replacement.Close()
			t.Logf("private GATT disconnect lifecycle verified: borrowed=%v; both pending waiters, router drain, exact replacement bytes", borrowed)
		})
	}
}
