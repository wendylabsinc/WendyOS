//go:build linux && regression_bluetooth

package bluetooth

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
	"go.uber.org/zap"
)

// This peer controls BlueZ responses on a real private D-Bus daemon. It does
// not emulate a controller or claim physical pairing/HID input coverage.
const bondAddress = "AA:BB:CC:DD:EE:01"
const bondAdapter = dbus.ObjectPath("/org/bluez/hci7")
const bondDevice = dbus.ObjectPath("/org/bluez/hci7/dev_AA_BB_CC_DD_EE_01")

type bondPeer struct {
	mu                             sync.Mutex
	conn                           *dbus.Conn
	phase                          int // cached bond, removed, rediscovered
	paired, trusted, connected     bool
	hid, holdDiscovery, rejectPair bool
	input                          string
	owner                          string
	actions                        []string
	started                        chan struct{}
	discovered, notified           bool
}

func (p *bondPeer) GetManagedObjects() (managedObjects, *dbus.Error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.holdDiscovery && p.discovered && !p.notified {
		p.notified = true
		close(p.started)
	}
	objects := managedObjects{bondAdapter: {adapterIface: {"Powered": dbus.MakeVariant(true)}}}
	if p.phase != 1 {
		props := map[string]dbus.Variant{"Address": dbus.MakeVariant(bondAddress), "Adapter": dbus.MakeVariant(bondAdapter),
			"Paired": dbus.MakeVariant(p.paired), "Trusted": dbus.MakeVariant(false), "UUIDs": dbus.MakeVariant([]string{})}
		if p.hid {
			props["UUIDs"] = dbus.MakeVariant([]string{hidServiceUUID})
		}
		objects[bondDevice] = map[string]map[string]dbus.Variant{deviceIface: props}
	}
	return objects, nil
}

func (p *bondPeer) Get(iface, name string) (dbus.Variant, *dbus.Error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch name {
	case "Trusted":
		return dbus.MakeVariant(p.trusted), nil
	case "Paired":
		return dbus.MakeVariant(p.paired), nil
	case "UUIDs":
		if p.hid {
			return dbus.MakeVariant([]string{hidServiceUUID}), nil
		}
		return dbus.MakeVariant([]string{}), nil
	}
	return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", []any{iface + "." + name})
}

func (p *bondPeer) Set(iface, name string, value dbus.Variant) *dbus.Error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if iface == adapterIface && name == "Powered" {
		return nil
	}
	if iface != deviceIface || name != "Trusted" || value.Value() != true {
		return dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", nil)
	}
	p.actions = append(p.actions, "trust")
	p.trusted = true
	return nil
}

func (p *bondPeer) RegisterAgent(sender dbus.Sender, path dbus.ObjectPath, capability string) *dbus.Error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if path != agentObjectPath || capability != agentCapability {
		return dbus.NewError("org.bluez.Error.InvalidArguments", nil)
	}
	if p.owner != "" {
		return dbus.NewError("org.bluez.Error.AlreadyExists", nil)
	}
	p.owner = string(sender)
	return nil
}
func (p *bondPeer) RequestDefaultAgent(dbus.ObjectPath) *dbus.Error { return nil }

func (p *bondPeer) RemoveDevice(path dbus.ObjectPath) *dbus.Error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if path != bondDevice || p.phase != 0 {
		return dbus.NewError("org.bluez.Error.DoesNotExist", nil)
	}
	p.actions = append(p.actions, "remove")
	p.phase, p.paired, p.trusted, p.connected = 1, false, false, false
	return nil
}
func (p *bondPeer) StartDiscovery() *dbus.Error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.actions = append(p.actions, "discover")
	if !p.holdDiscovery {
		p.phase = 2
	}
	p.discovered = true
	return nil
}
func (p *bondPeer) StopDiscovery() *dbus.Error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.actions = append(p.actions, "stop-discovery")
	return nil
}
func (p *bondPeer) Pair() *dbus.Error {
	p.mu.Lock()
	p.actions = append(p.actions, "pair")
	phase, owner, reject := p.phase, p.owner, p.rejectPair
	p.mu.Unlock()
	if phase != 2 || owner == "" {
		return dbus.NewError("org.bluez.Error.Failed", []any{"pair before fresh discovery/agent registration"})
	}
	// The actual production pairing agent must reject another device, then
	// authorize precisely this caller-selected device over the same bus.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	callback := p.conn.Object(owner, agentObjectPath)
	err := callback.CallWithContext(ctx, agentIface+".RequestAuthorization", 0, dbus.ObjectPath("/org/bluez/hci7/dev_OTHER")).Err
	if name, _, ok := dbusErrorInfo(err); !ok || name != "org.bluez.Error.Rejected" {
		return dbus.NewError("org.bluez.Error.Failed", []any{"foreign pairing callback was not rejected"})
	}
	if err := callback.CallWithContext(ctx, agentIface+".RequestAuthorization", 0, bondDevice).Err; err != nil {
		return dbus.NewError("org.bluez.Error.Failed", []any{err.Error()})
	}
	if reject {
		return dbus.NewError("org.bluez.Error.AuthenticationRejected", []any{"fixture rejects fresh pairing"})
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.paired = true
	if p.hid {
		if err := os.WriteFile(p.input, []byte(bondAddress), 0600); err != nil {
			return dbus.NewError("org.bluez.Error.Failed", []any{err.Error()})
		}
	}
	return nil
}
func (p *bondPeer) Connect() *dbus.Error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.actions = append(p.actions, "connect")
	if p.phase == 0 && !p.hid {
		return dbus.NewError("org.bluez.Error.AuthenticationFailed", []any{"fixture peripheral lost its bond"})
	}
	if !p.paired {
		return dbus.NewError("org.bluez.Error.AuthenticationFailed", nil)
	}
	p.connected = true
	return nil
}

func newBondPeer(t *testing.T) *bondPeer {
	t.Helper()
	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Fatal("required private dbus-daemon unavailable:", err)
	}
	directory := t.TempDir()
	address := "unix:path=" + filepath.Join(directory, "bus")
	log, err := os.Create(filepath.Join(directory, "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("dbus-daemon", "--session", "--nofork", "--nopidfile", "--address="+address)
	command.Stdout, command.Stderr = io.Discard, log
	if err := command.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// This unreaped os.Process is our daemon; never signal a saved/reused PID.
		_ = command.Process.Kill()
		done := make(chan error, 1)
		go func() { done <- command.Wait() }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("private D-Bus daemon did not drain")
		}
		_ = log.Close()
	})
	var connection *dbus.Conn
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		connection, err = dbus.Connect(address)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := connection.Close(); err != nil {
			t.Error(err)
		}
	})
	if reply, err := connection.RequestName(bluezService, dbus.NameFlagDoNotQueue); err != nil || reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("private BlueZ name: %v %v", reply, err)
	}
	p := &bondPeer{conn: connection, paired: true, trusted: true, started: make(chan struct{}), input: filepath.Join(directory, "uniq")}
	for _, export := range []struct {
		path    dbus.ObjectPath
		iface   string
		methods map[string]interface{}
	}{
		{"/", "org.freedesktop.DBus.ObjectManager", map[string]interface{}{"GetManagedObjects": p.GetManagedObjects}},
		{bondAdapter, adapterIface, map[string]interface{}{"RemoveDevice": p.RemoveDevice, "StartDiscovery": p.StartDiscovery, "StopDiscovery": p.StopDiscovery}},
		{bondAdapter, "org.freedesktop.DBus.Properties", map[string]interface{}{"Set": p.Set}},
		{bondDevice, deviceIface, map[string]interface{}{"Pair": p.Pair, "Connect": p.Connect}},
		{bondDevice, "org.freedesktop.DBus.Properties", map[string]interface{}{"Get": p.Get, "Set": p.Set}},
		{"/org/bluez", agentManagerIface, map[string]interface{}{"RegisterAgent": p.RegisterAgent, "RequestDefaultAgent": p.RequestDefaultAgent}},
	} {
		if err := connection.ExportMethodTable(export.methods, export.path, export.iface); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", address)
	t.Setenv("WENDY_BT_ADAPTER", "")
	previous := inputDeviceUniqGlob
	inputDeviceUniqGlob = p.input
	t.Cleanup(func() { inputDeviceUniqGlob = previous })
	return p
}

func TestStaleBondDBusLifecycle(t *testing.T) {
	for _, name := range []string{"key-error", "hollow-hid", "pair-rejected", "cancel-discovery", "pair-not-authorized"} {
		t.Run(name, func(t *testing.T) {
			p := newBondPeer(t)
			p.mu.Lock()
			p.hid, p.rejectPair, p.holdDiscovery = name == "hollow-hid", name == "pair-rejected", name == "cancel-discovery"
			holdDiscovery := p.holdDiscovery
			p.mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cancelDone := make(chan struct{})
			if holdDiscovery {
				go func() {
					defer close(cancelDone)
					select {
					case <-p.started:
						cancel()
					case <-ctx.Done():
					}
				}()
			}
			paired, err := (&BlueZManager{logger: zap.NewNop()}).Connect(ctx, bondAddress, name != "pair-not-authorized", false)
			if holdDiscovery {
				<-cancelDone
			}
			p.mu.Lock()
			actions := append([]string(nil), p.actions...)
			connected, trusted, owner := p.connected, p.trusted, p.owner
			p.mu.Unlock()
			want := []string{"connect", "remove", "discover", "stop-discovery", "pair", "trust", "connect"}
			switch name {
			case "key-error", "hollow-hid":
				if err != nil || !paired || !connected || !trusted {
					t.Fatalf("stale-bond recovery did not establish fresh trusted connection: paired=%v connected=%v trusted=%v err=%v actions=%v", paired, connected, trusted, err, actions)
				}
			case "pair-rejected":
				want = want[:5]
				if err == nil || paired || connected {
					t.Fatalf("fresh pairing rejection reported success: %v %v", paired, err)
				}
			case "cancel-discovery":
				want = want[:4]
				if err == nil || paired || connected || ctx.Err() == nil {
					t.Fatalf("cancelled rediscovery reported success: %v %v", paired, err)
				}
			case "pair-not-authorized":
				want = want[:1]
				if err == nil || paired || connected {
					t.Fatalf("unrequested repair reported success: %v %v", paired, err)
				}
			}
			if !reflect.DeepEqual(actions, want) {
				t.Fatalf("stale-bond operation order: got %v want %v", actions, want)
			}
			if owner != "" {
				// The daemon processes the production connection's disconnect on
				// another socket. Wait for its actual name withdrawal, not a sleep.
				drain, finish := context.WithTimeout(context.Background(), time.Second)
				defer finish()
				for {
					var exists bool
					err := p.conn.BusObject().CallWithContext(drain, "org.freedesktop.DBus.NameHasOwner", 0, owner).Store(&exists)
					if err != nil {
						t.Fatal("production pairing client did not drain:", err)
					}
					if !exists {
						break
					}
					time.Sleep(time.Millisecond)
				}
			}
			t.Logf("private D-Bus lifecycle verified: %v", actions)
		})
	}
}
