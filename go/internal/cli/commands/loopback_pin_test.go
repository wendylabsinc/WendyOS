package commands

import (
	"bytes"
	"context"
	"errors"
	"net"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// stubLoopbackVMs describes which running VM forwards which plaintext agent
// port on 127.0.0.1, for both views of the VM store the pin code reads.
func stubLoopbackVMs(t *testing.T, byPort map[int]string) {
	t.Helper()
	origName, origPort := loopbackVMNameFn, runningVMAgentPortFn
	loopbackVMNameFn = func(port int) (string, bool) {
		name, ok := byPort[port]
		return name, ok
	}
	runningVMAgentPortFn = func(name string) (int, bool) {
		for port, n := range byPort {
			if n == name {
				return port, true
			}
		}
		return 0, false
	}
	t.Cleanup(func() { loopbackVMNameFn, runningVMAgentPortFn = origName, origPort })
}

var pinA, pinB = config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"}, config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "43"}

func assetObs(asset string) observedDeviceIdentity {
	return observedDeviceIdentity{mTLS: true, orgID: 7, assetID: asset}
}

// mainPinKeyForAddr is pinKeyForAddr exactly as origin/main shipped it, before
// any VM was keyed by its forward: the reference every non-VM loopback key is
// held to.
func mainPinKeyForAddr(addr string) string {
	if name, matched, err := simulatorName(addr); err == nil && matched {
		return vmDeviceIDPrefix + name
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return strings.TrimSpace(addr)
	}
	return host
}

// Strict loopback keys: only the literal 127.0.0.1 on a running VM's plaintext
// agent port is the VM (vm:<name>); every other loopback host:port keys by its
// bare host, exactly as main did.
func TestPinKeyDerivationKeysOnlyARunningVMsForward(t *testing.T) {
	calls := 0
	orig := loopbackVMNameFn
	loopbackVMNameFn = func(port int) (string, bool) {
		calls++
		if port == 50151 {
			return "dev", true
		}
		return "", false
	}
	t.Cleanup(func() { loopbackVMNameFn = orig })

	for addr, want := range map[string]string{
		"127.0.0.1:50151": "vm:dev",
		// The mTLS port beside it is not the VM's key: a front door re-aims a
		// dial there first (vmForwardDialAddr); keyed on its own it is main's.
		"127.0.0.1:50152": "127.0.0.1",
		// Only the IPv4 address QEMU's forward binds names the VM: localhost
		// may resolve to ::1 first, and ::1 / 127.0.0.x / an IPv4-mapped
		// address are other sockets that something else can answer on.
		"localhost:50151":          "localhost",
		"[::1]:50151":              "::1",
		"127.0.0.2:50151":          "127.0.0.2",
		"[::ffff:127.0.0.1]:50151": "::ffff:127.0.0.1",
		// Only the literal text 127.0.0.1 is the forward: a trailing dot is
		// looked up as a DNS name, and padding is not an address at all.
		"127.0.0.1.:50151": "127.0.0.1.",
		" 127.0.0.1:50151": " 127.0.0.1",
		// Every port no VM forwards: the bare host.
		"127.0.0.1:50051":  "127.0.0.1",
		"127.0.0.1:50061":  "127.0.0.1",
		"localhost:50051":  "localhost",
		"LOCALHOST.:50051": "LOCALHOST.",
		"[::1]:50051":      "::1",
		"127.0.0.1":        "127.0.0.1",
		"vm:dev":           "vm:dev",
		"sim":              "vm:sim",
	} {
		if got := pinKeyForAddr(addr); got != want {
			t.Errorf("pinKeyForAddr(%q) = %q, want %q", addr, got, want)
		}
		// Every address but the VM's own forward keys exactly as main keyed it.
		if addr != "127.0.0.1:50151" {
			if got, main := pinKeyForAddr(addr), mainPinKeyForAddr(addr); got != main {
				t.Errorf("pinKeyForAddr(%q) = %q, main keyed it %q", addr, got, main)
			}
		}
	}
	calls = 0
	for _, addr := range []string{"localhost:50151", "[::1]:50151", "127.0.0.2:50151", "127.0.0.1.:50151", " 127.0.0.1:50151", "127.0.0.1"} {
		pinKeyForAddr(addr)
	}
	if calls != 0 {
		t.Errorf("the VM store was consulted %d times for addresses QEMU's forward never answers", calls)
	}
	calls = 0
	for addr, want := range map[string]string{
		"rpi5.local:50051":      "rpi5.local",
		"192.168.2.253":         "192.168.2.253",
		"192.168.2.253:50051":   "192.168.2.253",
		"[fe80::1%en0]:50051":   "fe80::1%en0",
		"wendyos-thor.local:99": "wendyos-thor.local",
	} {
		if got := pinKeyForAddr(addr); got != want {
			t.Errorf("non-loopback pinKeyForAddr(%q) = %q, want %q (must be unchanged)", addr, got, want)
		}
	}
	if calls != 0 {
		t.Errorf("the VM store was consulted %d times for non-loopback addresses", calls)
	}
	for _, addr := range []string{"127.0.0.1:50051", "localhost:50051", "127.0.0.1:50151"} {
		if pinKeyForAddr(addr) == "" {
			t.Errorf("pinKeyForAddr(%q) is empty; that would disarm the downgrade guard", addr)
		}
	}
}

func TestRunningVMOnLoopbackPort(t *testing.T) {
	orig := vmStatusesFn
	t.Cleanup(func() { vmStatusesFn = orig })
	vmStatusesFn = func() ([]vm.Status, error) {
		return []vm.Status{
			{Name: "dev", Exists: true, Running: true, State: vm.State{Name: "dev", AgentPort: 50151, NetMode: vm.NetUser}},
			{Name: "stopped", Exists: true, Meta: vm.Meta{Name: "stopped", AgentPort: 50051}},
			{Name: "bridged", Exists: true, Running: true, State: vm.State{Name: "bridged", AgentPort: 50061, NetMode: vm.NetShared}},
		}, nil
	}
	// Only the plaintext agent port names the VM: a dial at AgentPort+1 also
	// tries AgentPort+2, which QEMU does not forward.
	for port, want := range map[int]string{50151: "dev", 50152: "", 50051: "", 50061: "", 50153: ""} {
		got, ok := runningVMOnLoopbackPort(port)
		if got != want || ok != (want != "") {
			t.Errorf("runningVMOnLoopbackPort(%d) = (%q, %v), want %q", port, got, ok, want)
		}
	}
	vmStatusesFn = func() ([]vm.Status, error) {
		return []vm.Status{
			{Name: "a", Exists: true, Running: true, State: vm.State{Name: "a", AgentPort: 50171, NetMode: vm.NetUser}},
			{Name: "b", Exists: true, Running: true, State: vm.State{Name: "b", AgentPort: 50171, NetMode: vm.NetUser}},
		}, nil
	}
	if name, ok := runningVMOnLoopbackPort(50171); ok {
		t.Errorf("two run records claim port 50171; picked %q instead of trusting neither", name)
	}
	vmStatusesFn = func() ([]vm.Status, error) { return nil, errors.New("store unreadable") }
	if _, ok := runningVMOnLoopbackPort(50151); ok {
		t.Error("an unreadable VM store matched a VM")
	}
}

// Loopback keys add no lookup candidates of their own: pinCandidateKeys is
// main's for every key.
func TestPinCandidateKeysHasNoLoopbackSpecialCase(t *testing.T) {
	setPinCache(t)
	for key, want := range map[string][]string{
		"127.0.0.1":  {"127.0.0.1"},
		"::1":        {"::1"},
		"localhost":  {"localhost"},
		"vm:dev":     {"vm:dev"},
		"rpi5.local": {"rpi5.local"},
	} {
		if got := pinCandidateKeys(key); !reflect.DeepEqual(got, want) {
			t.Errorf("pinCandidateKeys(%q) = %q, want %q", key, got, want)
		}
	}
}

// T-a: an unknown loopback port (no VM forwards it) under a legacy bare pin is
// judged exactly as on main: keyed by the bare host, whose pin governs the dial
// (Expected set, plaintext rung blocked) and refuses a different device and an
// unprovisioned one, naming the bare key.
func TestUnknownLoopbackPortIsGovernedByTheBarePin(t *testing.T) {
	for _, tc := range []struct {
		addr, bare string
		frontDoor  bool
	}{
		{"127.0.0.1:50061", "127.0.0.1", true},
		{"localhost:50061", "localhost", false},
		{"[::1]:50061", "::1", false},
	} {
		t.Run(tc.addr, func(t *testing.T) {
			restoreDeviceGlobals(t)
			stubNonInteractive(t)
			setPinCache(t)
			// A VM runs, on another port.
			stubLoopbackVMs(t, map[int]string{50151: "dev"})
			legacy := map[string]config.DevicePin{tc.bare: pinA}
			readPins := writePinTestConfig(t, legacy)

			key := pinKeyForAddr(tc.addr)
			if key != tc.bare {
				t.Fatalf("pinKeyForAddr(%q) = %q, want the bare host %q (as main)", tc.addr, key, tc.bare)
			}
			target := newDialTarget(key, tc.addr)
			if !target.pinned() || target.PinnedKey != tc.bare || target.Expected == nil || target.Expected.EntityID != "42" {
				t.Fatalf("dial target %+v, want the bare pin (asset 42) governing it", target)
			}
			assertPlaintextRungRefused(t, target)
			for _, obs := range []observedDeviceIdentity{{}, assetObs("99")} {
				err := enforceDeviceIdentity(key, obs)
				if !errors.Is(err, errDeviceIdentityRefused) {
					t.Fatalf("%s answered as %+v: got %v, want a refusal", tc.addr, obs, err)
				}
				if !strings.Contains(err.Error(), "wendy device unpin "+tc.bare+"\n") {
					t.Errorf("refusal must name the bare key %q:\n%s", tc.bare, err)
				}
			}
			if tc.frontDoor {
				for _, obs := range []observedDeviceIdentity{{}, assetObs("99")} {
					dialled, err := connectTypedLoopback(t, tc.addr, obs)
					if !errors.Is(err, errDeviceIdentityRefused) {
						t.Fatalf("typed %s answered as %+v: got %v, want a refusal", tc.addr, obs, err)
					}
					if dialled.PinKey != tc.bare || dialled.Expected == nil || dialled.Expected.EntityID != "42" || !dialled.pinned() {
						t.Fatalf("typed %s dial target = %+v, want the bare pin (asset 42) governing it", tc.addr, dialled)
					}
				}
			}
			if pins := readPins(); !reflect.DeepEqual(pins, legacy) {
				t.Fatalf("pins = %+v, want the legacy pin untouched", pins)
			}
		})
	}
}

func TestTypedLoopbackAddressOfARunningVMUsesItsVMPin(t *testing.T) {
	setPinCache(t)
	stubLoopbackVMs(t, map[int]string{50151: "dev", 50161: "fresh"})
	setPinConfig(t, map[string]config.DevicePin{"127.0.0.1": pinA, "vm:dev": pinB})

	key := pinKeyForAddr("127.0.0.1:50151")
	target := newDialTarget(key, "127.0.0.1:50151")
	if key != "vm:dev" || target.PinnedKey != "vm:dev" || target.Expected == nil || target.Expected.EntityID != "43" {
		t.Fatalf("typed address of VM dev: key %q target %+v, want vm:dev's pin", key, target)
	}
	// A VM with no pin of its own does not inherit another VM's localhost pin:
	// this is the collision that blocked a second VM.
	fresh := newDialTarget(pinKeyForAddr("127.0.0.1:50161"), "127.0.0.1:50161")
	if fresh.pinned() {
		t.Fatalf("fresh VM inherited the bare 127.0.0.1 pin: %+v", fresh)
	}
}

// connectVMAlias drives connectSimulatorAgent — the vm:<name> alias's path —
// with the ladder answering as obs where it is dialled.
func connectVMAlias(t *testing.T, name, addr string, obs observedDeviceIdentity) error {
	t.Helper()
	origLadder, origObserve := dialAgentLadderFn, observeDeviceIdentityFn
	dialAgentLadderFn = func(_ context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
		return &grpcclient.AgentConnection{Host: "127.0.0.1", Addr: target.Addr,
			AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{}}}, nil, nil
	}
	observeDeviceIdentityFn = func(*grpcclient.AgentConnection) observedDeviceIdentity { return obs }
	defer func() { dialAgentLadderFn, observeDeviceIdentityFn = origLadder, origObserve }()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, _, err := connectSimulatorAgent(ctx, name, addr)
	if conn != nil {
		conn.Close()
	}
	return err
}

// T-b: two VMs on different ports each keep their own pin, in either order and
// through either door. The bare 127.0.0.1 pin the first typed connection files
// (as main did) never blocks the second VM, and is never overwritten by it.
func TestTwoVMsOnDifferentPortsNeverCollide(t *testing.T) {
	type vmConn struct{ name, addr, asset string }
	dev := vmConn{"dev", "127.0.0.1:50051", "42"}
	dev2 := vmConn{"dev2", "127.0.0.1:50061", "77"}
	for _, order := range [][2]vmConn{{dev, dev2}, {dev2, dev}} {
		t.Run(order[0].name+" first", func(t *testing.T) {
			restoreDeviceGlobals(t)
			stubNonInteractive(t)
			setPinCache(t)
			stubLoopbackVMs(t, map[int]string{50051: "dev", 50061: "dev2"})
			readPins := writePinTestConfig(t, map[string]config.DevicePin{})

			for _, c := range order {
				target, err := connectTypedLoopback(t, c.addr, assetObs(c.asset))
				if err != nil {
					t.Fatalf("typed %s (vm:%s) as asset %s: %v", c.addr, c.name, c.asset, err)
				}
				if target.PinKey != vmDeviceIDPrefix+c.name || target.pinned() {
					t.Fatalf("first typed dial of vm:%s = %+v, want it keyed vm:%s and governed by no pin", c.name, target, c.name)
				}
			}
			// Again, through both doors: neither VM is ever refused.
			for _, c := range order {
				if _, err := connectTypedLoopback(t, c.addr, assetObs(c.asset)); err != nil {
					t.Fatalf("typed %s again: %v", c.addr, err)
				}
				if err := connectVMAlias(t, c.name, c.addr, assetObs(c.asset)); err != nil {
					t.Fatalf("vm:%s alias: %v", c.name, err)
				}
			}
			want := map[string]string{"vm:dev": "42", "vm:dev2": "77", "127.0.0.1": order[0].asset}
			pins := readPins()
			if len(pins) != len(want) {
				t.Fatalf("pins = %+v, want exactly %v", pins, want)
			}
			for key, asset := range want {
				if pins[key].AssetID != asset {
					t.Errorf("pin %s = %+v, want asset %s", key, pins[key], asset)
				}
			}
		})
	}
}

// T-c: unpinning one VM clears that VM's pin only. The bare 127.0.0.1 pin every
// other loopback port shares, and the other VM's pin, stay; a non-VM loopback
// port is still governed by the bare pin afterwards.
func TestUnpinVMLeavesTheBareLoopbackPinAndOtherVMs(t *testing.T) {
	stubNonInteractive(t)
	setPinCache(t)
	stubLoopbackVMs(t, map[int]string{50051: "dev", 50061: "dev2"})
	pin77 := config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "77"}
	readPins := writePinTestConfig(t, map[string]config.DevicePin{"vm:dev": pinA, "vm:dev2": pin77, "127.0.0.1": pinA})

	runUnpin(t, "vm:dev")
	pins := readPins()
	if _, ok := pins["vm:dev"]; ok {
		t.Error("unpin vm:dev left its pin")
	}
	if pins["127.0.0.1"].AssetID != "42" || pins["vm:dev2"].AssetID != "77" || len(pins) != 2 {
		t.Fatalf("pins = %+v, want the bare 127.0.0.1 pin and vm:dev2 untouched", pins)
	}

	const other = "127.0.0.1:50071"
	key := pinKeyForAddr(other)
	if key != "127.0.0.1" {
		t.Fatalf("pinKeyForAddr(%q) = %q, want the bare host", other, key)
	}
	if target := newDialTarget(key, other); !target.pinned() || target.Expected == nil || target.Expected.EntityID != "42" {
		t.Fatalf("%s after unpin vm:dev: dial target %+v, want the bare pin (asset 42) governing it", other, target)
	}
	if err := enforceDeviceIdentity(key, assetObs("99")); !errors.Is(err, errDeviceIdentityRefused) {
		t.Fatalf("asset 99 at %s after unpin vm:dev: got %v, want a refusal", other, err)
	}
}

// stubSetDefaultOffline keeps set-default's post-save reachability probe off
// the network and off the VM store: every dial fails cleanly.
func stubSetDefaultOffline(t *testing.T) {
	t.Helper()
	origConnect := connectSimulatorChoiceFn
	connectSimulatorChoiceFn = func(context.Context, *simulatorChoice, bool) (*SelectedDevice, error) {
		return nil, errors.New("VM offline in test")
	}
	origLookup, origBrowse, origLadder, origDiscover := osLookupHostFn, lanBrowseFn, dialAgentLadderFn, discoverLANDevices
	osLookupHostFn = func(context.Context, string) ([]string, error) { return nil, errors.New("no resolver in test") }
	lanBrowseFn = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
	dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
		return nil, nil, errors.New("device offline in test")
	}
	// A literal IP skips name resolution, so the connect reaches the
	// provisioned-mTLS hint's LAN browse and, once the dial fails, the
	// USB-direct fallback; keep both off the network too.
	discoverLANDevices = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
	origUSB := usbDirectCandidatesFn
	usbDirectCandidatesFn = func() []discovery.USBDirectCandidate { return nil }
	t.Cleanup(func() {
		connectSimulatorChoiceFn = origConnect
		osLookupHostFn, lanBrowseFn, dialAgentLadderFn, discoverLANDevices = origLookup, origBrowse, origLadder, origDiscover
		usbDirectCandidatesFn = origUSB
	})
}

func runSetDefault(t *testing.T, device string) {
	t.Helper()
	cmd := newDeviceSetDefaultCmd()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd.SetContext(ctx)
	if err := cmd.RunE(cmd, []string{device}); err != nil {
		t.Fatal(err)
	}
}

// set-default of a bare loopback host clears the bare pin its dial is checked
// under — as main — and never a VM's.
func TestSetDefaultOnBareLoopbackClearsTheBarePin(t *testing.T) {
	restoreDeviceGlobals(t)
	stubNonInteractive(t)
	stubLoopbackVMs(t, nil)
	setPinCache(t)
	stubSetDefaultOffline(t)
	readPins := writePinTestConfig(t, map[string]config.DevicePin{"127.0.0.1": pinA, "vm:dev": pinB})

	runSetDefault(t, "127.0.0.1")
	pins := readPins()
	if _, ok := pins["127.0.0.1"]; ok {
		t.Error("set-default 127.0.0.1 did not clear the bare pin its dial is checked under")
	}
	if _, ok := pins["vm:dev"]; !ok {
		t.Error("set-default 127.0.0.1 cleared a VM's pin")
	}
}

// T-c (set-default): `wendy device set-default vm:<name>` clears the VM's own
// pin and nothing else — not the bare 127.0.0.1 pin other loopback ports
// share, and not another VM's.
func TestSetDefaultOnVMLeavesTheBareLoopbackPin(t *testing.T) {
	restoreDeviceGlobals(t)
	stubNonInteractive(t)
	stubLoopbackVMs(t, nil) // the VMs are stopped
	setPinCache(t)
	stubSetDefaultOffline(t)
	pin77 := config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "77"}
	readPins := writePinTestConfig(t, map[string]config.DevicePin{"vm:dev": pinA, "vm:dev2": pin77, "127.0.0.1": pinA})

	runSetDefault(t, "vm:dev")
	pins := readPins()
	if _, ok := pins["vm:dev"]; ok {
		t.Error("set-default vm:dev did not clear the VM's own pin")
	}
	if pins["127.0.0.1"].AssetID != "42" || pins["vm:dev2"].AssetID != "77" || len(pins) != 2 {
		t.Fatalf("pins = %+v, want the bare 127.0.0.1 pin and vm:dev2 untouched", pins)
	}
}

func TestVMPrintReachabilityNamesTheVMAlias(t *testing.T) {
	var buf bytes.Buffer
	vmPrintReachability(&buf, "dev", vm.NetConfig{Mode: vm.NetUser}, 50151)
	out := buf.String()
	if !strings.Contains(out, "wendy --device vm:dev device info") {
		t.Errorf("vm start output does not suggest --device vm:dev:\n%s", out)
	}
	if strings.Contains(out, "--device 127.0.0.1") {
		t.Errorf("vm start still suggests a loopback address:\n%s", out)
	}
	if !strings.Contains(out, "127.0.0.1:50151") {
		t.Errorf("vm start no longer says where the agent is forwarded:\n%s", out)
	}
}

// localhost resolves to ::1 as well as 127.0.0.1, and the dial ladder moves on
// to the next address when one fails, so a VM's forward on 127.0.0.1 does not
// make localhost:PORT that VM. Its bare pin keeps governing it, as on main,
// and a different device answering there is refused.
func TestLocalhostOnAVMPortStaysUnderItsBarePin(t *testing.T) {
	stubNonInteractive(t)
	setPinCache(t)
	stubLoopbackVMs(t, map[int]string{50151: "dev"})
	readPins := writePinTestConfig(t, map[string]config.DevicePin{"localhost": pinA, "::1": pinA})

	for _, addr := range []string{"localhost:50151", "[::1]:50151"} {
		key := pinKeyForAddr(addr)
		if strings.HasPrefix(key, vmDeviceIDPrefix) {
			t.Fatalf("pinKeyForAddr(%q) = %q: only 127.0.0.1 is the VM's forward", addr, key)
		}
		target := newDialTarget(key, addr)
		if !target.pinned() || target.Expected == nil || target.Expected.EntityID != "42" {
			t.Fatalf("%s: dial not governed by the bare pin: %+v", addr, target)
		}
		err := enforceDeviceIdentity(key, assetObs("99"))
		if !errors.Is(err, errDeviceIdentityRefused) {
			t.Fatalf("%s: asset 99 under a bare pin for 42: got %v, want a refusal", addr, err)
		}
	}
	pins := readPins()
	if _, ok := pins["vm:dev"]; ok {
		t.Error("a refused device was pinned as vm:dev")
	}
	if pins["localhost"].AssetID != "42" || pins["::1"].AssetID != "42" {
		t.Errorf("bare pins changed by a refusal: %+v", pins)
	}
}

// T-d: a typed connection to a running VM also files its identity under the
// bare 127.0.0.1 key, which main filed for the same connection. Once the VM
// stops, its address keys as the bare host again and is governed by that pin:
// a different device and an unprovisioned one are refused there, as on main.
func TestTypedVMConnectionFilesTheBarePinForWhenTheVMStops(t *testing.T) {
	restoreDeviceGlobals(t)
	stubNonInteractive(t)
	setPinCache(t)
	readPins := writePinTestConfig(t, map[string]config.DevicePin{})
	const addr = "127.0.0.1:50051"
	stubLoopbackVMs(t, map[int]string{50051: "dev"})

	// Only an accepted mTLS judgement records: an unprovisioned VM writes
	// nothing, under either key (main recorded nothing for it either).
	if _, err := connectTypedLoopback(t, addr, observedDeviceIdentity{}); err != nil {
		t.Fatalf("an unprovisioned VM on first contact: %v", err)
	}
	if pins := readPins(); len(pins) != 0 {
		t.Fatalf("pins = %+v after an unprovisioned answer, want none", pins)
	}

	if _, err := connectTypedLoopback(t, addr, assetObs("42")); err != nil {
		t.Fatalf("the running VM was refused: %v", err)
	}
	pins := readPins()
	if len(pins) != 2 || pins["vm:dev"].AssetID != "42" || pins["127.0.0.1"].AssetID != "42" {
		t.Fatalf("pins = %+v, want exactly vm:dev and 127.0.0.1 naming asset 42", pins)
	}
	if src := pins["127.0.0.1"].Source; src != config.PinSourceLAN {
		t.Errorf("bare pin source = %q, want %q", src, config.PinSourceLAN)
	}

	stubLoopbackVMs(t, nil) // the VM stops
	for _, obs := range []observedDeviceIdentity{{}, assetObs("99")} {
		target, err := connectTypedLoopback(t, addr, obs)
		if !errors.Is(err, errDeviceIdentityRefused) {
			t.Fatalf("the stopped VM's %s answered as %+v: got %v, want a refusal", addr, obs, err)
		}
		if target.PinKey != "127.0.0.1" || target.Expected == nil || target.Expected.EntityID != "42" || !target.pinned() {
			t.Fatalf("dial target = %+v, want the bare pin (asset 42) governing it", target)
		}
	}
	assertPlaintextRungRefused(t, newDialTarget(pinKeyForAddr(addr), addr))
	if pins := readPins(); len(pins) != 2 || pins["127.0.0.1"].AssetID != "42" {
		t.Fatalf("pins = %+v, want them unchanged by the refusals", pins)
	}
}

// T-d (alias): the vm:<name> alias records the VM's own pin only — main never
// filed a bare pin for it — and a typed connection refused under vm:<name>
// records nothing at all.
func TestVMAliasNeverFilesTheBarePin(t *testing.T) {
	stubNonInteractive(t)
	setPinCache(t)
	stubLoopbackVMs(t, map[int]string{50051: "dev"})
	readPins := writePinTestConfig(t, map[string]config.DevicePin{})
	if err := connectVMAlias(t, "dev", "127.0.0.1:50051", assetObs("42")); err != nil {
		t.Fatal(err)
	}
	if pins := readPins(); len(pins) != 1 || pins["vm:dev"].AssetID != "42" {
		t.Fatalf("pins = %+v, want only vm:dev (asset 42)", pins)
	}

	restoreDeviceGlobals(t)
	if _, err := connectTypedLoopback(t, "127.0.0.1:50051", assetObs("99")); !errors.Is(err, errDeviceIdentityRefused) {
		t.Fatalf("asset 99 at vm:dev's forward: got %v, want a refusal", err)
	}
	if pins := readPins(); len(pins) != 1 || pins["vm:dev"].AssetID != "42" {
		t.Fatalf("pins = %+v after a refusal, want only vm:dev (asset 42)", pins)
	}
}

// T-d (asset-less): an mTLS VM whose certificate names no asset files an
// org-only bare pin, as main did — so an unprovisioned answer is refused once
// the VM stops — and a later connection naming the asset adopts it in place.
func TestAssetlessTypedVMConnectionFilesAnOrgOnlyBarePin(t *testing.T) {
	restoreDeviceGlobals(t)
	stubNonInteractive(t)
	setPinCache(t)
	readPins := writePinTestConfig(t, map[string]config.DevicePin{})
	const addr = "127.0.0.1:50051"

	stubLoopbackVMs(t, map[int]string{50051: "dev"})
	if _, err := connectTypedLoopback(t, addr, observedDeviceIdentity{mTLS: true, orgID: 7}); err != nil {
		t.Fatal(err)
	}
	orgOnly := config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443", Source: config.PinSourceLAN}
	if pins := readPins(); len(pins) != 2 || pins["127.0.0.1"] != orgOnly || pins["vm:dev"] != orgOnly {
		t.Fatalf("pins = %+v, want vm:dev and 127.0.0.1 both org-only %+v", pins, orgOnly)
	}
	stubLoopbackVMs(t, nil) // the VM stops
	if target := newDialTarget(pinKeyForAddr(addr), addr); !target.pinned() {
		t.Fatalf("the stopped VM's address is unpinned: %+v", target)
	}
	if _, err := connectTypedLoopback(t, addr, observedDeviceIdentity{}); !errors.Is(err, errDeviceIdentityRefused) {
		t.Fatalf("an unprovisioned answer at the stopped VM's address: got %v, want a refusal", err)
	}

	stubLoopbackVMs(t, map[int]string{50051: "dev"}) // it runs again
	if _, err := connectTypedLoopback(t, addr, assetObs("42")); err != nil {
		t.Fatal(err)
	}
	adopted := config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42", Source: config.PinSourceLAN}
	if pins := readPins(); pins["127.0.0.1"] != adopted || pins["vm:dev"].AssetID != "42" {
		t.Fatalf("pins = %+v, want the bare pin adopted in place as %+v", pins, adopted)
	}
	stubLoopbackVMs(t, nil) // and stops
	target, err := connectTypedLoopback(t, addr, assetObs("99"))
	if !errors.Is(err, errDeviceIdentityRefused) {
		t.Fatalf("asset 99 at the stopped VM's address: got %v, want a refusal", err)
	}
	if target.Expected == nil || target.Expected.EntityID != "42" {
		t.Fatalf("dial target = %+v, want Expected asset 42", target)
	}
}

// T-g: an org-only bare 127.0.0.1 pin (an older CLI's, naming no device)
// adopts the asset a typed VM connection proves, in place, as main's
// connection did. A VM from another organisation leaves it alone and is still
// accepted under its own vm:<name> key.
func TestTypedVMConnectionAdoptsAnOrgOnlyBarePin(t *testing.T) {
	restoreDeviceGlobals(t)
	stubNonInteractive(t)
	setPinCache(t)
	orgOnly := config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443", Source: config.PinSourceLAN}
	readPins := writePinTestConfig(t, map[string]config.DevicePin{"127.0.0.1": orgOnly})
	const addr = "127.0.0.1:50051"

	stubLoopbackVMs(t, map[int]string{50051: "dev"})
	if _, err := connectTypedLoopback(t, addr, assetObs("42")); err != nil {
		t.Fatalf("the running VM was refused: %v", err)
	}
	adopted := orgOnly
	adopted.AssetID = "42"
	pins := readPins()
	if pins["127.0.0.1"] != adopted || pins["vm:dev"].AssetID != "42" || len(pins) != 2 {
		t.Fatalf("pins = %+v, want the org-only bare pin adopted in place as %+v, and vm:dev", pins, adopted)
	}
	stubLoopbackVMs(t, nil) // the VM stops
	target, err := connectTypedLoopback(t, addr, assetObs("99"))
	if !errors.Is(err, errDeviceIdentityRefused) {
		t.Fatalf("asset 99 at the stopped VM's address: got %v, want a refusal", err)
	}
	if target.Expected == nil || target.Expected.EntityID != "42" || !target.pinned() {
		t.Fatalf("dial target = %+v, want Expected asset 42 and the plaintext rung blocked", target)
	}

	readPins = writePinTestConfig(t, map[string]config.DevicePin{"127.0.0.1": orgOnly})
	stubLoopbackVMs(t, map[int]string{50051: "dev"})
	if _, err := connectTypedLoopback(t, addr, observedDeviceIdentity{mTLS: true, orgID: 8, assetID: "42"}); err != nil {
		t.Fatalf("a VM of organisation 8 was refused under its own key: %v", err)
	}
	pins = readPins()
	if pins["127.0.0.1"] != orgOnly {
		t.Fatalf("bare pin = %+v, want it untouched (%+v): another organisation's VM names nothing it covers", pins["127.0.0.1"], orgOnly)
	}
	if pins["vm:dev"].OrgID != 8 {
		t.Fatalf("vm:dev = %+v, want organisation 8", pins["vm:dev"])
	}
}

// Only a vm:<name> judgement dialled at the typed literal 127.0.0.1 forward
// records under the bare key; the alias, a substituted connection and every
// other key record nothing extra.
func TestOnlyATypedVMForwardRecordsTheBarePin(t *testing.T) {
	for _, tc := range []struct{ key, dialled, want string }{
		{"vm:dev", "127.0.0.1:50051", "127.0.0.1"},
		{"vm:dev", "127.0.0.1:50052", "127.0.0.1"},
		{"vm:dev", "127.0.0.1.:50051", ""},
		{"vm:dev", " 127.0.0.1:50051", ""},
		{"vm:dev", "localhost:50051", ""},
		{"vm:dev", "[::1]:50051", ""},
		{"vm:dev", "127.0.0.2:50051", ""},
		{"vm:dev", "vm:dev", ""},
		// A connection some fallback substituted for the dial.
		{"vm:dev", "", ""},
		// Keys that are already the bare host, or not loopback at all.
		{"127.0.0.1", "127.0.0.1:50061", ""},
		{"localhost", "localhost:50061", ""},
		{"rpi5.local", "rpi5.local:50051", ""},
		{"10.0.0.5", "10.0.0.5:50051", ""},
	} {
		if got := typedVMBarePinKey(tc.key, tc.dialled); got != tc.want {
			t.Errorf("typedVMBarePinKey(%q, %q) = %q, want %q", tc.key, tc.dialled, got, tc.want)
		}
	}
}

// T-b/T-d through every door: the typed front doors (connectToAgent,
// resolveTarget) file the bare pin beside vm:<name>; the alias does not.
func TestTypedFrontDoorsFileTheBarePinTheAliasDoesNot(t *testing.T) {
	for _, tc := range []struct {
		name     string
		connect  func(ctx context.Context) error
		wantBare bool
	}{
		{"vm alias", func(ctx context.Context) error {
			conn, _, err := connectSimulatorAgent(ctx, "dev", "127.0.0.1:50151")
			if conn != nil {
				conn.Close()
			}
			return err
		}, false},
		{"connectToAgent", func(ctx context.Context) error {
			deviceFlag = "127.0.0.1:50151"
			conn, err := connectToAgent(ctx, SuppressProvisioningHint(), SuppressUpdateCheck(), NonInteractive(), DisableSessionBroker())
			if conn != nil {
				conn.Close()
			}
			return err
		}, true},
		{"resolveTarget", func(ctx context.Context) error {
			deviceFlag = "127.0.0.1:50151"
			sel, err := resolveTarget(ctx, SuppressProvisioningHint(), SuppressUpdateCheck(), NonInteractive(), DisableSessionBroker())
			if sel != nil {
				sel.Close()
			}
			return err
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restoreDeviceGlobals(t)
			stubNonInteractive(t)
			setPinCache(t)
			stubLoopbackVMs(t, map[int]string{50151: "dev"})
			readPins := writePinTestConfig(t, map[string]config.DevicePin{})

			origLadder, origObserve, origDiscover := dialAgentLadderFn, observeDeviceIdentityFn, discoverLANDevices
			dialAgentLadderFn = func(_ context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
				return &grpcclient.AgentConnection{Host: "127.0.0.1", Addr: target.Addr,
					AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{}}}, nil, nil
			}
			observeDeviceIdentityFn = func(*grpcclient.AgentConnection) observedDeviceIdentity { return assetObs("42") }
			discoverLANDevices = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
			t.Cleanup(func() {
				dialAgentLadderFn, observeDeviceIdentityFn, discoverLANDevices = origLadder, origObserve, origDiscover
			})

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := tc.connect(ctx); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"vm:dev": "42"}
			if tc.wantBare {
				want["127.0.0.1"] = "42"
			}
			pins := readPins()
			if len(pins) != len(want) {
				t.Fatalf("pins = %+v, want exactly %v", pins, want)
			}
			for key, asset := range want {
				if pins[key].AssetID != asset {
					t.Errorf("pin %s = %+v, want asset %s", key, pins[key], asset)
				}
			}
		})
	}
}

// R18: a VM is never on USB. A typed address keyed as vm:<name> whose ladder
// fails must not fall back to a USB gadget that merely reports that name — it
// would be judged under a key that consults no loopback pin.
func TestNoUSBFallbackForAVMKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		obs  observedDeviceIdentity
	}{
		{"same-org gadget", assetObs("99")},
		{"unprovisioned gadget", observedDeviceIdentity{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restoreDeviceGlobals(t)
			stubNonInteractive(t)
			setPinCache(t)
			stubLoopbackVMs(t, map[int]string{50051: "dev"})
			legacy := map[string]config.DevicePin{"127.0.0.1": pinA}
			readPins := writePinTestConfig(t, legacy)

			origLadder, origObserve, origDiscover := dialAgentLadderFn, observeDeviceIdentityFn, discoverLANDevices
			origCands, origPreDial, origConnect := usbDirectCandidatesFn, usbDirectPreDialFn, usbDirectConnectFn
			dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
				return nil, nil, errors.New("VM agent still booting")
			}
			observeDeviceIdentityFn = func(*grpcclient.AgentConnection) observedDeviceIdentity { return tc.obs }
			discoverLANDevices = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
			usbDirectCandidatesFn = func() []discovery.USBDirectCandidate {
				t.Error("the USB-direct fallback was consulted for a vm: key")
				return []discovery.USBDirectCandidate{{Interface: "gadget", Zone: "gadget"}}
			}
			usbDirectPreDialFn = func(context.Context, discovery.USBDirectCandidate) bool { return true }
			usbDirectConnectFn = func(context.Context, string) (*grpcclient.AgentConnection, error) {
				return &grpcclient.AgentConnection{Host: "fe80::5741:1", AgentService: &fakeAgentVersionClient{
					resp: &agentpb.GetAgentVersionResponse{Hostname: "vm:dev"}}}, nil
			}
			t.Cleanup(func() {
				dialAgentLadderFn, observeDeviceIdentityFn, discoverLANDevices = origLadder, origObserve, origDiscover
				usbDirectCandidatesFn, usbDirectPreDialFn, usbDirectConnectFn = origCands, origPreDial, origConnect
			})

			deviceFlag = "127.0.0.1:50051"
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			conn, err := connectToAgent(ctx, SuppressProvisioningHint(), SuppressUpdateCheck(), NonInteractive(), DisableSessionBroker())
			if conn != nil {
				conn.Close()
				t.Fatal("a USB gadget reporting vm:dev was accepted for 127.0.0.1:50051")
			}
			if err == nil {
				t.Fatal("no error for an unreachable VM")
			}
			if pins := readPins(); !reflect.DeepEqual(pins, legacy) {
				t.Fatalf("pins = %+v, want only the legacy pin", pins)
			}
		})
	}
}

// R18: a connection the USB-direct fallback substituted is not the dialled
// endpoint, so it is never treated as the typed address.
func TestUSBFallbackConnectionIsNotTheDialledEndpoint(t *testing.T) {
	restoreDeviceGlobals(t)
	stubNonInteractive(t)
	setPinCache(t)
	writePinTestConfig(t, map[string]config.DevicePin{})
	origLookup, origBrowse, origLadder, origDiscover := osLookupHostFn, lanBrowseFn, dialAgentLadderFn, discoverLANDevices
	origCands, origPreDial, origConnect := usbDirectCandidatesFn, usbDirectPreDialFn, usbDirectConnectFn
	osLookupHostFn = func(context.Context, string) ([]string, error) { return nil, errors.New("no resolver in test") }
	lanBrowseFn = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
	dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
		return nil, nil, errors.New("device offline in test")
	}
	discoverLANDevices = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
	usbDirectCandidatesFn = func() []discovery.USBDirectCandidate {
		return []discovery.USBDirectCandidate{{Interface: "gadget", Zone: "gadget"}}
	}
	usbDirectPreDialFn = func(context.Context, discovery.USBDirectCandidate) bool { return true }
	usbDirectConnectFn = func(context.Context, string) (*grpcclient.AgentConnection, error) {
		return &grpcclient.AgentConnection{Host: "fe80::5741:1", AgentService: &fakeAgentVersionClient{
			resp: &agentpb.GetAgentVersionResponse{Hostname: "wendy-thor.local"}}}, nil
	}
	t.Cleanup(func() {
		osLookupHostFn, lanBrowseFn, dialAgentLadderFn, discoverLANDevices = origLookup, origBrowse, origLadder, origDiscover
		usbDirectCandidatesFn, usbDirectPreDialFn, usbDirectConnectFn = origCands, origPreDial, origConnect
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, dialled, finished, err := connectToAgentDirect(ctx, resolveConfig{nonInteractive: true}, "wendy-thor", "wendy-thor.local:50051", "wendy-thor.local", false)
	if err != nil || finished || conn == nil {
		t.Fatalf("connectToAgentDirect = (%v, finished %v, %v), want the USB connection", conn, finished, err)
	}
	conn.Close()
	if dialled != "" {
		t.Fatalf("a USB-fallback connection reports dialled endpoint %q, want none", dialled)
	}
}

// connectTypedLoopback drives the real connectToAgent path for a typed
// --device address, with the ladder answering as obs. It returns the dial
// target the ladder was handed and the connect error.
func connectTypedLoopback(t *testing.T, addr string, obs observedDeviceIdentity) (dialTarget, error) {
	t.Helper()
	var dialled dialTarget
	origLadder, origObserve, origDiscover := dialAgentLadderFn, observeDeviceIdentityFn, discoverLANDevices
	origUSB := usbDirectCandidatesFn
	dialAgentLadderFn = func(_ context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
		dialled = target
		return &grpcclient.AgentConnection{Host: "127.0.0.1", Addr: target.Addr,
			AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{}}}, nil, nil
	}
	observeDeviceIdentityFn = func(*grpcclient.AgentConnection) observedDeviceIdentity { return obs }
	discoverLANDevices = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
	usbDirectCandidatesFn = func() []discovery.USBDirectCandidate { return nil }
	defer func() {
		dialAgentLadderFn, observeDeviceIdentityFn, discoverLANDevices = origLadder, origObserve, origDiscover
		usbDirectCandidatesFn = origUSB
	}()
	deviceFlag = addr
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := connectToAgent(ctx, SuppressProvisioningHint(), SuppressUpdateCheck(), NonInteractive(), DisableSessionBroker())
	if conn != nil {
		conn.Close()
	}
	return dialled, err
}

// R25: a VM reconnect after an agent update passes conn.Addr, which is the
// mTLS forward. It is dialled at the plaintext forward instead, so the ladder
// only ever tries the two ports QEMU forwards; the alias path pins only
// vm:<name>.
func TestVMReconnectAtItsMTLSForwardDialsThePlaintextForward(t *testing.T) {
	stubNonInteractive(t)
	setPinCache(t)
	stubLoopbackVMs(t, map[int]string{50151: "dev"})
	readPins := writePinTestConfig(t, map[string]config.DevicePin{})
	origLadder, origObserve := dialAgentLadderFn, observeDeviceIdentityFn
	var dialled []string
	dialAgentLadderFn = func(_ context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
		dialled = append(dialled, target.Addr)
		return &grpcclient.AgentConnection{Host: "127.0.0.1", Addr: target.Addr,
			AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{}}}, nil, nil
	}
	observeDeviceIdentityFn = func(*grpcclient.AgentConnection) observedDeviceIdentity { return assetObs("42") }
	t.Cleanup(func() { dialAgentLadderFn, observeDeviceIdentityFn = origLadder, origObserve })

	conn, _, err := connectSimulatorAgent(context.Background(), "dev", "127.0.0.1:50152")
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if !reflect.DeepEqual(dialled, []string{"127.0.0.1:50151"}) {
		t.Fatalf("dialled %q, want only the plaintext forward 127.0.0.1:50151", dialled)
	}
	if pins := readPins(); len(pins) != 1 || pins["vm:dev"].AssetID != "42" {
		t.Fatalf("pins = %+v, want only vm:dev naming asset 42", pins)
	}
}

// mtlsAnswerAddr is where a provisioned agent's authenticated connection
// lands when the ladder dials addr: the mTLS port after it, which is what
// conn.Addr then holds (and what a reconnect after an agent update dials).
func mtlsAnswerAddr(t *testing.T, addr string) string {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("mtlsAnswerAddr(%q): %v", addr, err)
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("mtlsAnswerAddr(%q): %v", addr, err)
	}
	return net.JoinHostPort(host, strconv.Itoa(p+agentMTLSPortOffset))
}

// connectTypedAnsweringAtMTLS drives the real connectToAgent path for a typed
// --device address with the ladder answering as obs the way a provisioned
// agent does: on the mTLS port after the one dialled. It returns the
// connection (the caller closes it) and the dial target the ladder was handed.
func connectTypedAnsweringAtMTLS(t *testing.T, addr string, obs observedDeviceIdentity) (*grpcclient.AgentConnection, dialTarget, error) {
	t.Helper()
	var dialled dialTarget
	origLadder, origObserve, origDiscover := dialAgentLadderFn, observeDeviceIdentityFn, discoverLANDevices
	origUSB := usbDirectCandidatesFn
	dialAgentLadderFn = func(_ context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
		dialled = target
		return &grpcclient.AgentConnection{Host: "127.0.0.1", Addr: mtlsAnswerAddr(t, target.Addr), IsMTLS: obs.mTLS,
			AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{}}}, nil, nil
	}
	observeDeviceIdentityFn = func(*grpcclient.AgentConnection) observedDeviceIdentity { return obs }
	discoverLANDevices = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
	usbDirectCandidatesFn = func() []discovery.USBDirectCandidate { return nil }
	defer func() {
		dialAgentLadderFn, observeDeviceIdentityFn, discoverLANDevices = origLadder, origObserve, origDiscover
		usbDirectCandidatesFn = origUSB
	}()
	deviceFlag = addr
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := connectToAgent(ctx, SuppressProvisioningHint(), SuppressUpdateCheck(), NonInteractive(), DisableSessionBroker())
	return conn, dialled, err
}

// reconnectAfterUpdate runs the reconnect an agent update makes with conn
// (reconnectAgentAfterRestart), against a ladder that answers every dial as
// obs, and returns each dial target it handed the ladder plus the VMs whose
// simulator path it took.
func reconnectAfterUpdate(t *testing.T, conn *grpcclient.AgentConnection, obs observedDeviceIdentity) (targets []dialTarget, simulatorPath []string, err error) {
	t.Helper()
	origLadder, origObserve, origRecord := dialAgentLadderFn, observeDeviceIdentityFn, vmRecordHostnameFn
	dialAgentLadderFn = func(_ context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
		targets = append(targets, target)
		return &grpcclient.AgentConnection{Host: "127.0.0.1", Addr: mtlsAnswerAddr(t, target.Addr), IsMTLS: obs.mTLS,
			AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{}}}, nil, nil
	}
	observeDeviceIdentityFn = func(*grpcclient.AgentConnection) observedDeviceIdentity { return obs }
	vmRecordHostnameFn = func(name, _ string) error {
		simulatorPath = append(simulatorPath, name)
		return nil
	}
	defer func() {
		dialAgentLadderFn, observeDeviceIdentityFn, vmRecordHostnameFn = origLadder, origObserve, origRecord
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	newConn, err := reconnectAgentAfterRestart(ctx, conn)
	if newConn != nil {
		newConn.Close()
	}
	return targets, simulatorPath, err
}

// assertPlaintextRungRefused runs the real ladder over target with no CLI
// certificate, so nothing can authenticate: a pinned target must end in a
// refusal without ever offering the plaintext rung.
func assertPlaintextRungRefused(t *testing.T, target dialTarget) {
	t.Helper()
	// Counted per address and answered with an error, so a stray probe from
	// elsewhere in the package can neither skew the count nor get a
	// half-built connection back.
	var plaintextCalls atomic.Int32
	origPlaintext := plaintextConnectFn
	plaintextConnectFn = func(_ context.Context, address string) (*grpcclient.AgentConnection, error) {
		if address == target.Addr {
			plaintextCalls.Add(1)
		}
		return nil, errors.New("plaintext rung reached in test")
	}
	defer func() { plaintextConnectFn = origPlaintext }()
	conn, _, err := dialAgentLadderWithCerts(context.Background(), target, nil)
	if conn != nil {
		conn.Close()
	}
	if n := plaintextCalls.Load(); n != 0 || conn != nil {
		t.Errorf("%s: the plaintext rung was offered (%d calls) — an unprovisioned listener there would be accepted", target.Addr, n)
	}
	if !errors.Is(err, errNoAuthenticatedEndpoint) {
		t.Errorf("%s: ladder err = %v, want a pinned-host refusal", target.Addr, err)
	}
}

// T-e: a non-VM loopback agent reached at typed 127.0.0.1:P answers on P+1
// once provisioned, so conn.Addr is P+1 — and the reconnect after an agent
// update (device update, os update, the connect-time update offer) dials
// exactly that address. Both are keyed by the one bare 127.0.0.1 pin, exactly
// as on main: the reconnect is pinned (plaintext rung blocked, another device
// refused) and nothing is filed under a port.
func TestNonVMLoopbackReconnectStaysUnderTheBarePin(t *testing.T) {
	orgOnly := config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443"}
	for _, tc := range []struct {
		name string
		pins map[string]config.DevicePin
	}{
		{"fresh user", map[string]config.DevicePin{}},
		{"legacy bare pin", map[string]config.DevicePin{"127.0.0.1": pinA}},
		{"legacy org-only bare pin", map[string]config.DevicePin{"127.0.0.1": orgOnly}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restoreDeviceGlobals(t)
			stubNonInteractive(t)
			setPinCache(t)
			stubLoopbackVMs(t, nil)
			readPins := writePinTestConfig(t, tc.pins)
			const typed, mtls = "127.0.0.1:50051", "127.0.0.1:50052"

			conn, first, err := connectTypedAnsweringAtMTLS(t, typed, assetObs("42"))
			if err != nil {
				t.Fatalf("first connect: %v", err)
			}
			defer conn.Close()
			if first.PinKey != "127.0.0.1" {
				t.Fatalf("typed %s dialled under %q, want the bare host", typed, first.PinKey)
			}
			if conn.Addr != mtls {
				t.Fatalf("conn.Addr = %q, want the mTLS port %s", conn.Addr, mtls)
			}
			if pins := readPins(); len(pins) != 1 || pins["127.0.0.1"].AssetID != "42" {
				t.Fatalf("pins = %+v, want only the bare 127.0.0.1 naming asset 42", pins)
			}

			targets, simulatorPath, err := reconnectAfterUpdate(t, conn, assetObs("42"))
			if err != nil {
				t.Fatalf("reconnect: %v", err)
			}
			if len(simulatorPath) != 0 {
				t.Fatalf("a non-VM connection reconnected through the VM path %q", simulatorPath)
			}
			if len(targets) == 0 {
				t.Fatal("the reconnect never dialled")
			}
			target := targets[0]
			if target.Addr != mtls || target.PinKey != "127.0.0.1" || target.PinnedKey != "127.0.0.1" {
				t.Fatalf("reconnect target = %+v, want %s under the bare 127.0.0.1 pin", target, mtls)
			}
			if !target.pinned() || target.Expected == nil || target.Expected.EntityID != "42" {
				t.Errorf("reconnect target at %s = %+v: want it pinned to asset 42 (plaintext blocked, another device refused)", mtls, target)
			}
			assertPlaintextRungRefused(t, target)
			for _, obs := range []observedDeviceIdentity{{}, assetObs("99")} {
				if err := enforceDeviceIdentity(pinKeyForAddr(mtls), obs); !errors.Is(err, errDeviceIdentityRefused) {
					t.Errorf("%s answered as %+v: got %v, want a refusal", mtls, obs, err)
				}
			}
			if pins := readPins(); len(pins) != 1 || pins["127.0.0.1"].AssetID != "42" {
				t.Fatalf("pins = %+v, want only the bare 127.0.0.1 naming asset 42", pins)
			}
		})
	}
}

// T-f / R25: a connection typed at a running VM's mTLS forward, 127.0.0.1:A+1,
// is dialled at the VM's agent forward A and keyed vm:<name>, like a dial of A
// or the alias; and, typed, it files the bare 127.0.0.1 pin as main did. Once
// the VM stops, A and A+1 key as the bare host again and that pin refuses
// another device and an unprovisioned one at both.
func TestTypedMTLSForwardOfARunningVMConnectsAsTheVM(t *testing.T) {
	orgOnly := config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443"}
	for _, tc := range []struct {
		name string
		pins map[string]config.DevicePin
	}{
		{"legacy bare pin", map[string]config.DevicePin{"127.0.0.1": pinA}},
		{"legacy org-only bare pin", map[string]config.DevicePin{"127.0.0.1": orgOnly}},
		{"fresh user", map[string]config.DevicePin{}},
	} {
		for _, door := range []string{"connectToAgent", "resolveTarget"} {
			t.Run(tc.name+"/"+door, func(t *testing.T) {
				restoreDeviceGlobals(t)
				stubNonInteractive(t)
				setPinCache(t)
				readPins := writePinTestConfig(t, tc.pins)
				const agent, mtls = "127.0.0.1:50051", "127.0.0.1:50052"

				stubLoopbackVMs(t, map[int]string{50051: "dev"})
				var target dialTarget
				var err error
				if door == "connectToAgent" {
					var conn *grpcclient.AgentConnection
					conn, target, err = connectTypedAnsweringAtMTLS(t, mtls, assetObs("42"))
					if conn != nil {
						conn.Close()
					}
				} else {
					target, err = resolveTypedAnsweringAtMTLS(t, mtls, assetObs("42"))
				}
				if err != nil {
					t.Fatalf("the running VM was refused: %v", err)
				}
				if target.Addr != agent || target.PinKey != "vm:dev" {
					t.Errorf("typed %s dialled %q under %q, want %s under vm:dev", mtls, target.Addr, target.PinKey, agent)
				}
				pins := readPins()
				if len(pins) != 2 || pins["vm:dev"].AssetID != "42" || pins["127.0.0.1"].AssetID != "42" {
					t.Errorf("pins = %+v, want exactly vm:dev and the bare 127.0.0.1 naming asset 42", pins)
				}

				stubLoopbackVMs(t, nil) // the VM stops
				for _, addr := range []string{agent, mtls} {
					key := pinKeyForAddr(addr)
					if key != "127.0.0.1" {
						t.Errorf("stopped VM's %s keys as %q, want the bare host", addr, key)
					}
					if target := newDialTarget(key, addr); !target.pinned() || target.Expected == nil || target.Expected.EntityID != "42" {
						t.Errorf("stopped VM's %s: dial target %+v, want Expected 42 and the plaintext rung blocked", addr, target)
					}
					for _, obs := range []observedDeviceIdentity{{}, assetObs("99")} {
						if err := enforceDeviceIdentity(key, obs); !errors.Is(err, errDeviceIdentityRefused) {
							t.Errorf("stopped VM's %s answered as %+v: got %v, want a refusal", addr, obs, err)
						}
					}
				}
			})
		}
	}
}

// resolveTypedAnsweringAtMTLS is connectTypedAnsweringAtMTLS through
// resolveTarget's direct path.
func resolveTypedAnsweringAtMTLS(t *testing.T, addr string, obs observedDeviceIdentity) (dialTarget, error) {
	t.Helper()
	var dialled dialTarget
	origLadder, origObserve, origDiscover := dialAgentLadderFn, observeDeviceIdentityFn, discoverLANDevices
	dialAgentLadderFn = func(_ context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
		dialled = target
		return &grpcclient.AgentConnection{Host: "127.0.0.1", Addr: mtlsAnswerAddr(t, target.Addr), IsMTLS: obs.mTLS,
			AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{}}}, nil, nil
	}
	observeDeviceIdentityFn = func(*grpcclient.AgentConnection) observedDeviceIdentity { return obs }
	discoverLANDevices = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
	defer func() {
		dialAgentLadderFn, observeDeviceIdentityFn, discoverLANDevices = origLadder, origObserve, origDiscover
	}()
	deviceFlag = addr
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sel, err := resolveTarget(ctx, SuppressProvisioningHint(), SuppressUpdateCheck(), NonInteractive(), DisableSessionBroker())
	if sel != nil {
		sel.Close()
	}
	return dialled, err
}

// R25: the reconnect path (waitForAgentRestart → connectWithAutoTLS), and
// every other direct dial through connectWithAutoTLS, dials a running VM's
// mTLS forward as the VM too — never under the bare key, and never with a
// ladder that would also try A+2, which QEMU does not forward.
func TestDirectDialOfARunningVMsMTLSForwardIsKeyedAsTheVM(t *testing.T) {
	setPinCache(t)
	stubLoopbackVMs(t, map[int]string{50051: "dev"})
	setPinConfig(t, map[string]config.DevicePin{"vm:dev": pinA, "127.0.0.1": pinB})
	var targets []dialTarget
	origLadder := dialAgentLadderFn
	dialAgentLadderFn = func(_ context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
		targets = append(targets, target)
		return nil, nil, errors.New("agent restarting")
	}
	t.Cleanup(func() { dialAgentLadderFn = origLadder })

	_, _ = connectWithAutoTLS(context.Background(), "127.0.0.1:50052")
	if len(targets) != 1 {
		t.Fatalf("ladder dialled %d times, want 1", len(targets))
	}
	got := targets[0]
	if got.Addr != "127.0.0.1:50051" || got.PinKey != "vm:dev" || got.Expected == nil || got.Expected.EntityID != "42" {
		t.Fatalf("dial target = %+v, want 127.0.0.1:50051 under vm:dev (asset 42)", got)
	}

	// Anything else is left alone: a port no VM forwards, the agent port
	// itself, and spellings other than the literal forward address.
	for _, addr := range []string{"127.0.0.1:50061", "127.0.0.1:50051", "localhost:50052", "127.0.0.2:50052", "10.0.0.5:50052"} {
		if got := vmForwardDialAddr(addr); got != addr {
			t.Errorf("vmForwardDialAddr(%q) = %q, want it unchanged", addr, got)
		}
	}
	if got := dialPinKeyForDevice("127.0.0.1:50052"); got != "vm:dev" {
		t.Errorf("dialPinKeyForDevice(127.0.0.1:50052) = %q, want vm:dev (the key its dial is checked under)", got)
	}
	if got := dialPinKeyForDevice("127.0.0.1:50061"); got != "127.0.0.1" {
		t.Errorf("dialPinKeyForDevice(127.0.0.1:50061) = %q, want the bare host", got)
	}
}

// R27(1): a reconnect answered by a device that is refused must say so at
// once. Retrying only asks the same wrong device again until the deadline, and
// then reports "timed out waiting for agent to restart" instead of the refusal
// and the unpin that resolves it.
func TestWaitForAgentRestartReturnsAnIdentityRefusalImmediately(t *testing.T) {
	calls := 0
	origLadder := dialAgentLadderFn
	dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
		calls++
		return nil, nil, refuseDevicePin(devicePinDiagnostic{hostname: "127.0.0.1", heading: "Connection blocked."})
	}
	t.Cleanup(func() { dialAgentLadderFn = origLadder })
	stubLoopbackVMs(t, nil)
	setPinCache(t)
	setPinConfig(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	_, err := waitForAgentRestart(ctx, "127.0.0.1:50052")
	if !errors.Is(err, errDeviceIdentityRefused) {
		t.Fatalf("waitForAgentRestart = %v, want the identity refusal", err)
	}
	if calls != 1 {
		t.Fatalf("ladder dialled %d times after a refusal, want 1", calls)
	}
}

// R27(1): an agent that is simply not back yet is still waited for.
func TestWaitForAgentRestartStillRetriesAnAgentThatIsNotBackYet(t *testing.T) {
	calls := 0
	origLadder := dialAgentLadderFn
	dialAgentLadderFn = func(_ context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
		calls++
		if calls == 1 {
			return nil, nil, &noAuthenticatedEndpointError{msg: "nothing answered"}
		}
		return &grpcclient.AgentConnection{Addr: target.Addr,
			AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{}}}, nil, nil
	}
	t.Cleanup(func() { dialAgentLadderFn = origLadder })
	stubLoopbackVMs(t, nil)
	setPinCache(t)
	setPinConfig(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	conn, err := waitForAgentRestart(ctx, "127.0.0.1:50052")
	if err != nil {
		t.Fatalf("waitForAgentRestart = %v, want the agent once it answers", err)
	}
	conn.Close()
	if calls != 2 {
		t.Fatalf("ladder dialled %d times, want 2", calls)
	}
}

// R27(2): VM b runs on the port where an earlier VM a was reached, and the bare
// 127.0.0.1 pin names VM a. A typed connection is keyed vm:b, and so must its
// reconnect after an agent update be: it goes through the VM's own path, by
// name, not through conn.Addr (the mTLS forward). VM a's bare pin is left as
// it was — recording never overwrites it.
func TestTypedVMConnectionReconnectsThroughTheVM(t *testing.T) {
	restoreDeviceGlobals(t)
	stubNonInteractive(t)
	setPinCache(t)
	stale := config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"}
	readPins := writePinTestConfig(t, map[string]config.DevicePin{"vm:a": stale, "127.0.0.1": stale})
	stubLoopbackVMs(t, map[int]string{50051: "b"})
	obs43 := assetObs("43")

	conn, _, err := connectTypedAnsweringAtMTLS(t, "127.0.0.1:50051", obs43)
	if err != nil {
		t.Fatalf("typed connect to VM b: %v", err)
	}
	defer conn.Close()
	if conn.SimulatorName != "b" {
		t.Fatalf("conn.SimulatorName = %q, want b: the connection was judged as vm:b", conn.SimulatorName)
	}
	targets, simulatorPath, err := reconnectAfterUpdate(t, conn, obs43)
	if err != nil {
		t.Fatalf("reconnect to VM b: %v", err)
	}
	if !reflect.DeepEqual(simulatorPath, []string{"b"}) {
		t.Fatalf("reconnect took the simulator path for %q, want [b]", simulatorPath)
	}
	if len(targets) == 0 || targets[0].PinKey != "vm:b" || targets[0].Addr != "127.0.0.1:50051" ||
		targets[0].Expected == nil || targets[0].Expected.EntityID != "43" {
		t.Fatalf("reconnect targets = %+v, want 127.0.0.1:50051 under vm:b (asset 43)", targets)
	}
	pins := readPins()
	if len(pins) != 3 || pins["vm:b"].AssetID != "43" || pins["vm:a"].AssetID != "42" || pins["127.0.0.1"].AssetID != "42" {
		t.Fatalf("pins = %+v, want vm:b=43 and VM a's pins (vm:a, 127.0.0.1) untouched", pins)
	}
}

// R25: a port that is itself a running VM's agent port keys as that VM, even
// when it is also the port after another VM's.
func TestVMForwardDialAddrLeavesAnotherVMsAgentPortAlone(t *testing.T) {
	stubLoopbackVMs(t, map[int]string{50051: "a", 50052: "b"})
	if got := vmForwardDialAddr("127.0.0.1:50052"); got != "127.0.0.1:50052" {
		t.Fatalf("vmForwardDialAddr(127.0.0.1:50052) = %q, want it left as VM b's agent port", got)
	}
	if key := pinKeyForAddr(vmForwardDialAddr("127.0.0.1:50052")); key != "vm:b" {
		t.Fatalf("key = %q, want vm:b", key)
	}
}

// stubFlippingVM makes VM dev forward agentPort on 127.0.0.1 while it runs,
// and flips its running state once `after` VM-store reads have been made — a
// VM started or stopped mid-connect. Both store views count as reads.
func stubFlippingVM(t *testing.T, agentPort int, running bool, after int32) *atomic.Int32 {
	t.Helper()
	var reads atomic.Int32
	isRunning := func() bool {
		if reads.Add(1) > after {
			return !running
		}
		return running
	}
	origName, origPort := loopbackVMNameFn, runningVMAgentPortFn
	loopbackVMNameFn = func(port int) (string, bool) {
		if isRunning() && port == agentPort {
			return "dev", true
		}
		return "", false
	}
	runningVMAgentPortFn = func(name string) (int, bool) {
		if isRunning() && name == "dev" {
			return agentPort, true
		}
		return 0, false
	}
	t.Cleanup(func() { loopbackVMNameFn, runningVMAgentPortFn = origName, origPort })
	return &reads
}

// dialKeyReads is how many VM-store reads one derivation of addr's dial
// address and pin key makes with the VM in the given state: exactly the reads
// a front door makes before it dials.
func dialKeyReads(t *testing.T, addr string, agentPort int, running bool) (int32, string) {
	t.Helper()
	reads := stubFlippingVM(t, agentPort, running, 1<<30)
	key := pinKeyForAddr(vmForwardDialAddr(addr))
	return reads.Load(), key
}

// addTestAuthOrg gives the test config a login for org, so a certificate of
// that organisation is one the user could hold.
func addTestAuthOrg(t *testing.T, org int, cloud string) {
	t.Helper()
	if err := config.Update(func(cfg *config.Config) (bool, error) {
		cfg.Auth = append(cfg.Auth, config.AuthConfig{CloudGRPC: cloud, Certificates: []config.CertificateInfo{{OrganizationID: org}}})
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
}

// connectThroughDoor drives a front door (connectToAgent or resolveTarget)
// for a typed --device address, with a ladder that answers where it is
// dialled as obs. It returns every dial target the ladder was handed, the
// connection the ladder built (so the caller can see whether it was marked),
// and the front door's error.
func connectThroughDoor(t *testing.T, door, addr string, obs observedDeviceIdentity) ([]dialTarget, *grpcclient.AgentConnection, error) {
	t.Helper()
	var targets []dialTarget
	var built *grpcclient.AgentConnection
	origLadder, origObserve, origDiscover := dialAgentLadderFn, observeDeviceIdentityFn, discoverLANDevices
	origUSB, origLookup, origBrowse := usbDirectCandidatesFn, osLookupHostFn, lanBrowseFn
	dialAgentLadderFn = func(_ context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
		targets = append(targets, target)
		built = &grpcclient.AgentConnection{Host: "127.0.0.1", Addr: target.Addr,
			AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{}}}
		return built, nil, nil
	}
	observeDeviceIdentityFn = func(*grpcclient.AgentConnection) observedDeviceIdentity { return obs }
	discoverLANDevices = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
	usbDirectCandidatesFn = func() []discovery.USBDirectCandidate { return nil }
	osLookupHostFn = func(context.Context, string) ([]string, error) { return []string{"192.168.2.10"}, nil }
	lanBrowseFn = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
	defer func() {
		dialAgentLadderFn, observeDeviceIdentityFn, discoverLANDevices = origLadder, origObserve, origDiscover
		usbDirectCandidatesFn, osLookupHostFn, lanBrowseFn = origUSB, origLookup, origBrowse
	}()
	deviceFlag = addr
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	opts := []resolveOption{SuppressProvisioningHint(), SuppressUpdateCheck(), NonInteractive(), DisableSessionBroker()}
	var err error
	if door == "connectToAgent" {
		var conn *grpcclient.AgentConnection
		conn, err = connectToAgent(ctx, opts...)
		if conn != nil {
			conn.Close()
		}
	} else {
		var sel *SelectedDevice
		sel, err = resolveTarget(ctx, opts...)
		if sel != nil {
			sel.Close()
		}
	}
	return targets, built, err
}

// R31 (C1): a front door derives its pin key once, dials under it (the
// ladder's Expected identity and plaintext block), and judges the connection
// under it. If a local VM starts or stops mid-connect, the key it would derive
// afterwards differs: the connection is refused as retryable, with nothing
// pinned and nothing marked. Before this, the ladder re-derived its own key:
// with a VM stopping mid-connect, it dialled under an org-only bare pin and
// the check then ran under an unpinned vm:dev, accepting (and pinning) a
// listener with another organisation's certificate that main refused.
func TestFrontDoorRefusesAConnectionWhoseVMChangedMidConnect(t *testing.T) {
	orgOnly := config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443"}
	// Another local account's listener, with a certificate from another
	// organisation the user belongs to.
	other := observedDeviceIdentity{mTLS: true, orgID: 8, assetID: "99"}
	for _, door := range []string{"connectToAgent", "resolveTarget"} {
		for _, tc := range []struct {
			name    string
			addr    string
			running bool
		}{
			{"agent port, VM stops", "127.0.0.1:50051", true},
			{"agent port, VM starts", "127.0.0.1:50051", false},
			{"mTLS port, VM stops", "127.0.0.1:50052", true},
			{"mTLS port, VM starts", "127.0.0.1:50052", false},
		} {
			t.Run(door+"/"+tc.name, func(t *testing.T) {
				restoreDeviceGlobals(t)
				stubNonInteractive(t)
				setPinCache(t)
				start := map[string]config.DevicePin{"127.0.0.1": orgOnly}
				readPins := writePinTestConfig(t, start)
				addTestAuthOrg(t, 8, "grpc.b.sh:443")

				n, keyBefore := dialKeyReads(t, tc.addr, 50051, tc.running)
				_, keyAfter := dialKeyReads(t, tc.addr, 50051, !tc.running)
				if keyBefore == keyAfter {
					t.Fatalf("test setup: %s keys as %q either way", tc.addr, keyBefore)
				}
				stubFlippingVM(t, 50051, tc.running, n)

				targets, built, err := connectThroughDoor(t, door, tc.addr, other)
				if !errors.Is(err, errVMChangedDuringConnect) {
					t.Fatalf("got %v, want the VM-changed-during-connect refusal", err)
				}
				for _, want := range []string{strconv.Quote(keyBefore), strconv.Quote(keyAfter), "retry"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal %q does not name %s", err, want)
					}
				}
				if len(targets) != 1 {
					t.Fatalf("ladder dialled %d times, want once", len(targets))
				}
				// The ladder dialled under the key the front door derived first —
				// the key the check would have used — not a second derivation.
				want := newDialTarget(keyBefore, targets[0].Addr)
				if got := targets[0]; got.PinKey != keyBefore || got.PinnedKey != want.PinnedKey || got.pinned() != want.pinned() || (got.Expected == nil) != (want.Expected == nil) {
					t.Errorf("ladder target = %+v, want it governed by %q (%+v)", got, keyBefore, want)
				}
				if built != nil && built.SimulatorName != "" {
					t.Errorf("the refused connection was marked as VM %q", built.SimulatorName)
				}
				if pins := readPins(); !reflect.DeepEqual(pins, start) {
					t.Errorf("pins = %+v, want them untouched (%+v)", pins, start)
				}
			})
		}
	}
}

// R31: when no VM changes, the ladder and the check use the same key, and the
// connection goes through; a typed VM connection is marked and pinned as before.
func TestFrontDoorDialsAndJudgesUnderOneKey(t *testing.T) {
	for _, door := range []string{"connectToAgent", "resolveTarget"} {
		t.Run(door, func(t *testing.T) {
			restoreDeviceGlobals(t)
			stubNonInteractive(t)
			setPinCache(t)
			stubLoopbackVMs(t, map[int]string{50051: "dev"})
			readPins := writePinTestConfig(t, map[string]config.DevicePin{"vm:dev": pinA, "127.0.0.1": pinB})
			targets, built, err := connectThroughDoor(t, door, "127.0.0.1:50051", assetObs("42"))
			if err != nil {
				t.Fatal(err)
			}
			if len(targets) != 1 || targets[0].PinKey != "vm:dev" || targets[0].Expected == nil || targets[0].Expected.EntityID != "42" {
				t.Fatalf("ladder targets = %+v, want one dial under vm:dev (asset 42)", targets)
			}
			if built.SimulatorName != "dev" {
				t.Errorf("SimulatorName = %q, want dev", built.SimulatorName)
			}
			if pins := readPins(); pins["vm:dev"].AssetID != "42" || pins["127.0.0.1"].AssetID != "43" || len(pins) != 2 {
				t.Errorf("pins = %+v, want vm:dev=42 and the bare 43 untouched", pins)
			}
		})
	}
}

// R31: a non-loopback key never reads the VM store, so it can never change
// between the dial and the re-check: no extra refusals, however the VM store
// behaves.
func TestNonLoopbackKeysNeverTripTheVMChangeCheck(t *testing.T) {
	for _, door := range []string{"connectToAgent", "resolveTarget"} {
		for _, addr := range []string{"192.168.2.253:50051", "10.0.0.5", "[fe80::1%en0]:50051", "rpi5.local:50051", "wendyos-thor.local:99"} {
			t.Run(door+"/"+addr, func(t *testing.T) {
				restoreDeviceGlobals(t)
				stubNonInteractive(t)
				setPinCache(t)
				readPins := writePinTestConfig(t, map[string]config.DevicePin{})
				// A store whose answer changes on every read.
				reads := stubFlippingVM(t, 50051, true, 0)
				targets, built, err := connectThroughDoor(t, door, addr, assetObs("42"))
				if err != nil {
					t.Fatalf("got %v, want the connection", err)
				}
				key := pinKeyForAddr(addr)
				if len(targets) != 1 || targets[0].PinKey != key {
					t.Fatalf("ladder targets = %+v, want one dial under %q", targets, key)
				}
				if n := reads.Load(); n != 0 {
					t.Errorf("the VM store was read %d times for a non-loopback address", n)
				}
				if built.SimulatorName != "" {
					t.Errorf("SimulatorName = %q, want none", built.SimulatorName)
				}
				// The pin store files an mDNS name without its ".local".
				if pins := readPins(); len(pins) != 1 || pins[strings.TrimSuffix(key, ".local")].AssetID != "42" {
					t.Errorf("pins = %+v, want only %s=42", pins, key)
				}
			})
		}
	}
}

// m1: adopting an asset into an org-only bare pin writes exactly what main's
// connection wrote under that key — SetDevicePin with the observed identity,
// principal included.
func TestTypedVMBarePinAdoptWritesWhatMainWrote(t *testing.T) {
	const oldPrincipal = "spiffe://wendy.sh/tenant/0f8fad5b-d9cb-469f-a165-70867728950e/device/1"
	const newPrincipal = "spiffe://wendy.sh/tenant/0f8fad5b-d9cb-469f-a165-70867728950e/device/42"
	start := map[string]config.DevicePin{"127.0.0.1": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", Principal: oldPrincipal}}
	obs := observedDeviceIdentity{mTLS: true, orgID: 7, assetID: "42", principal: newPrincipal}

	restoreDeviceGlobals(t)
	stubNonInteractive(t)
	setPinCache(t)
	stubLoopbackVMs(t, map[int]string{50051: "dev"})
	readPins := writePinTestConfig(t, start)
	if _, err := connectTypedLoopback(t, "127.0.0.1:50051", obs); err != nil {
		t.Fatal(err)
	}
	got := readPins()["127.0.0.1"]

	// Main judged and recorded the same typed connection under the bare key.
	readMain := writePinTestConfig(t, start)
	if err := enforceDeviceIdentity("127.0.0.1", obs); err != nil {
		t.Fatal(err)
	}
	want := readMain()["127.0.0.1"]
	if got != want {
		t.Fatalf("bare pin after the typed VM connection = %+v, main wrote %+v", got, want)
	}
	if got.Principal != newPrincipal || got.AssetID != "42" {
		t.Fatalf("bare pin = %+v, want asset 42 with the observed principal", got)
	}
}

// m2: `wendy device unpin` clears the key a dial of its target is checked
// under. A host:port target is re-aimed like the dial, so a running VM's mTLS
// forward names the VM; a bare host never is, so `unpin 127.0.0.1` clears the
// shared pin.
func TestUnpinOfARunningVMsMTLSForwardClearsTheVMPin(t *testing.T) {
	setPinCache(t)
	for _, tc := range []struct {
		name    string
		vms     map[int]string
		target  string
		cleared string
	}{
		{"mTLS port of a running VM", map[int]string{50051: "dev"}, "127.0.0.1:50052", "vm:dev"},
		{"agent port of a running VM", map[int]string{50051: "dev"}, "127.0.0.1:50051", "vm:dev"},
		{"bare host while the VM runs", map[int]string{50051: "dev"}, "127.0.0.1", "127.0.0.1"},
		{"mTLS port once the VM stops", nil, "127.0.0.1:50052", "127.0.0.1"},
		{"another port", map[int]string{50051: "dev"}, "127.0.0.1:50061", "127.0.0.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubLoopbackVMs(t, tc.vms)
			readPins := writePinTestConfig(t, map[string]config.DevicePin{"vm:dev": pinA, "127.0.0.1": pinB})
			runUnpin(t, tc.target)
			pins := readPins()
			if _, ok := pins[tc.cleared]; ok {
				t.Errorf("unpin %s left %s", tc.target, tc.cleared)
			}
			if len(pins) != 1 {
				t.Errorf("unpin %s: pins = %+v, want only %s cleared", tc.target, pins, tc.cleared)
			}
		})
	}
}

// m3: when another wendy process holds the config lock, the fallback judges
// the connection but cannot record it; its warning names every key it could
// not record — for a typed VM connection that is the bare 127.0.0.1 too, and
// sometimes only that.
func TestLockFallbackWarningNamesWhatWasNotRecorded(t *testing.T) {
	lockErr := errors.New("another wendy process has held config.lock")
	for _, tc := range []struct {
		name  string
		start map[string]config.DevicePin
		want  []string
	}{
		{"only the bare record", map[string]config.DevicePin{"vm:dev": pinA}, []string{"127.0.0.1"}},
		{"both keys", map[string]config.DevicePin{}, []string{"127.0.0.1", "vm:dev"}},
		{"only the VM's key", map[string]config.DevicePin{"127.0.0.1": pinA}, []string{"vm:dev"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{
				Auth:       []config.AuthConfig{{CloudGRPC: "grpc.a.sh:443", Certificates: []config.CertificateInfo{{OrganizationID: 7}}}},
				DevicePins: mxCopyPins(tc.start),
			}
			before := mxCopyPins(cfg.DevicePins)
			changed, refusal := applyDeviceIdentity(cfg, "vm:dev", "127.0.0.1", assetObs("42"))
			if refusal != nil || !changed {
				t.Fatalf("applyDeviceIdentity = (%v, %v), want a change to record", changed, refusal)
			}
			keys := unrecordedPinKeys(before, cfg.DevicePins)
			if !reflect.DeepEqual(keys, tc.want) {
				t.Fatalf("unrecorded keys = %q, want %q", keys, tc.want)
			}
			msg := unrecordedIdentityWarning("vm:dev", keys, lockErr)
			for _, key := range []string{"127.0.0.1", "vm:dev"} {
				if named := strings.Contains(msg, strconv.Quote(key)); named != slices.Contains(tc.want, key) {
					t.Errorf("warning names %q = %v, want %v:\n%s", key, named, !named, msg)
				}
			}
			if !strings.Contains(msg, "was not recorded") || !strings.Contains(msg, lockErr.Error()) {
				t.Errorf("warning = %q", msg)
			}
		})
	}
}

func mxCopyPins(p map[string]config.DevicePin) map[string]config.DevicePin {
	out := make(map[string]config.DevicePin, len(p))
	for k, v := range p {
		out[k] = v
	}
	return out
}

// R31: the session broker is consulted under the same key as the ladder and
// the check — the broker's expected identity comes from the pin the front
// door derived once — and a broker hit is re-checked like any connection.
func TestSessionBrokerIsConsultedUnderTheDialKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the session broker is not used on Windows")
	}
	for _, door := range []string{"connectToAgent", "resolveTarget"} {
		t.Run(door, func(t *testing.T) {
			restoreDeviceGlobals(t)
			stubNonInteractive(t)
			setPinCache(t)
			start := map[string]config.DevicePin{"vm:dev": pinA, "127.0.0.1": pinB}
			readPins := writePinTestConfig(t, start)
			n, _ := dialKeyReads(t, "127.0.0.1:50051", 50051, true)
			stubFlippingVM(t, 50051, true, n) // the VM stops once the key is derived

			var consulted []string
			origConnect, origStart, origLadder := connectSessionBrokerFn, startSessionBrokerFn, dialAgentLadderFn
			connectSessionBrokerFn = func(_ context.Context, key string, expected certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
				consulted = append(consulted, key+" expecting asset "+expected.EntityID)
				return &grpcclient.AgentConnection{Host: "127.0.0.1", Addr: key,
					AgentService: &fakeAgentVersionClient{resp: &agentpb.GetAgentVersionResponse{}}}, nil
			}
			startSessionBrokerFn = func(string, *grpcclient.AgentConnection) error {
				t.Error("a broker was seeded for a refused connection")
				return nil
			}
			dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
				t.Error("the ladder ran despite a broker hit")
				return nil, nil, errors.New("unreachable")
			}
			t.Cleanup(func() {
				connectSessionBrokerFn, startSessionBrokerFn, dialAgentLadderFn = origConnect, origStart, origLadder
			})

			deviceFlag = "127.0.0.1:50051"
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			opts := []resolveOption{SuppressProvisioningHint(), SuppressUpdateCheck(), NonInteractive()}
			var err error
			if door == "connectToAgent" {
				var conn *grpcclient.AgentConnection
				if conn, err = connectToAgent(ctx, opts...); conn != nil {
					conn.Close()
				}
			} else {
				var sel *SelectedDevice
				if sel, err = resolveTarget(ctx, opts...); sel != nil {
					sel.Close()
				}
			}
			// vm:dev's pin (asset 42), the key derived before the VM stopped —
			// not the bare 127.0.0.1 pin (asset 43) a second derivation gives.
			if want := []string{"127.0.0.1:50051 expecting asset 42"}; !reflect.DeepEqual(consulted, want) {
				t.Errorf("broker consulted %q, want %q", consulted, want)
			}
			if !errors.Is(err, errVMChangedDuringConnect) {
				t.Fatalf("got %v, want the VM-changed-during-connect refusal", err)
			}
			if pins := readPins(); !reflect.DeepEqual(pins, start) {
				t.Errorf("pins = %+v, want them untouched", pins)
			}
		})
	}
}
