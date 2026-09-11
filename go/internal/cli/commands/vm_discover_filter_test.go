package commands

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/cli/vm"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// listenLoopback opens a TCP listener standing in for a VM's forwarded agent
// port, so the pre-dial gate passes, and returns its port.
func listenLoopback(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln.Addr().(*net.TCPAddr).Port
}

// stubAgentVersion answers GetAgentVersion with whatever fn says for the
// address dialed.
func stubAgentVersion(t *testing.T, fn func(addr string) (*agentpb.GetAgentVersionResponse, error)) {
	t.Helper()
	saved := dialAgentLadderFn
	dialAgentLadderFn = func(_ context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
		if !strings.HasPrefix(target.PinKey, vmDeviceIDPrefix) {
			t.Errorf("VM probe lost named identity: %+v", target)
		}
		resp, err := fn(target.Addr)
		if err != nil {
			return nil, nil, err
		}
		return &grpcclient.AgentConnection{AgentService: &fakeAgentVersionClient{resp: resp}}, nil, nil
	}
	t.Cleanup(func() { dialAgentLadderFn = saved })
}

// recordedHostnames captures what the code under test asks the VM store to
// remember, keyed by VM name, without touching the developer's real store.
func recordedHostnames(t *testing.T) func() map[string]string {
	t.Helper()
	var mu sync.Mutex
	got := map[string]string{}
	saved := vmRecordHostnameFn
	vmRecordHostnameFn = func(name, hostname string) error {
		mu.Lock()
		defer mu.Unlock()
		got[name] = hostname
		return nil
	}
	t.Cleanup(func() { vmRecordHostnameFn = saved })
	return func() map[string]string {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]string, len(got))
		for k, v := range got {
			out[k] = v
		}
		return out
	}
}

func awaitChanged(t *testing.T, f *simulatorFilter) {
	t.Helper()
	select {
	case <-f.Changed():
	case <-time.After(5 * time.Second):
		t.Fatal("the filter never reported learning a hostname")
	}
}

func TestSimulatorFilterSeedsBeforeStartingLearners(t *testing.T) {
	port := listenLoopback(t)
	statuses := []vm.Status{runningVM("new", vm.NetUser, port)}
	// A large seed set makes the constructor/learner overlap observable under
	// -race in the old implementation, without timing sleeps.
	for i := 0; i < 50000; i++ {
		statuses = append(statuses, vm.Status{Meta: vm.Meta{Hostname: fmt.Sprintf("known-%d", i)}})
	}
	stubVMStatuses(t, statuses...)
	stubAgentVersion(t, func(string) (*agentpb.GetAgentVersionResponse, error) {
		return &agentpb.GetAgentVersionResponse{Hostname: "learned"}, nil
	})
	recordedHostnames(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newSimulatorFilter(ctx)
	awaitChanged(t, f)
	for _, host := range []string{"known-0", "known-49999", "learned"} {
		if !f.Exclude(models.LANDevice{Hostname: host, IPAddress: "10.0.2.15"}) {
			t.Fatalf("hostname %s was lost", host)
		}
	}
}

// A VM board is a positive signal: no real board reports vm-*.
func TestSimulatorFilterExcludesVMBoards(t *testing.T) {
	stubVMStatuses(t)
	f := newSimulatorFilter(context.Background())
	for _, dt := range []string{"vm-arm64", "vm-x86-64"} {
		if !f.Exclude(models.LANDevice{DeviceType: dt, Hostname: "wendyos-gentle-forest.local", IPAddress: "10.0.2.15"}) {
			t.Errorf("%s was not excluded", dt)
		}
	}
	for _, dt := range []string{"jetson-orin-nano", "rpi5", ""} {
		if f.Exclude(models.LANDevice{DeviceType: dt, Hostname: "orin.local"}) {
			t.Errorf("%q was excluded", dt)
		}
	}
}

func TestSimulatorFilterPreservesReachableNetworkVMs(t *testing.T) {
	shared := runningVM("shared", vm.NetShared, 0)
	shared.Meta.Hostname = "shared"
	stubVMStatuses(t, shared)
	f := newSimulatorFilter(context.Background())
	for _, host := range []string{"shared.local", "another-hosts-vm.local"} {
		for _, addr := range []string{"192.168.64.2", "192.168.1.20", "fd00::2"} {
			if f.Exclude(models.LANDevice{Hostname: host, IPAddress: addr, DeviceType: "vm-arm64"}) {
				t.Fatalf("reachable VM %s at %s was hidden", host, addr)
			}
		}
	}
}

func TestSimulatorFilterDoesNotTreatRememberedNamesAsDeviceIdentity(t *testing.T) {
	stubVMStatuses(t, vm.Status{Name: "stopped", Exists: true, Meta: vm.Meta{Hostname: "lab-device"}})
	f := newSimulatorFilter(context.Background())
	for _, dev := range []models.LANDevice{
		{Hostname: "lab-device.local", IPAddress: "192.168.1.20", DeviceType: "rpi5"},
		{Hostname: "lab-device.local", IPAddress: "10.0.2.15", DeviceType: "rpi5"},
		{Hostname: "lab-device.local", IPAddress: "192.168.64.2"},
		{Hostname: "lab-device.local", IPAddress: "fd00::2"},
		{Hostname: "lab-device.local"},
	} {
		if f.Exclude(dev) {
			t.Fatalf("hostname collision hid a device: %+v", dev)
		}
	}
	if !f.Exclude(models.LANDevice{Hostname: "lab-device.local", IPAddress: "10.0.2.15"}) {
		t.Fatal("legacy leaked announcement must still be filtered")
	}
}

// The hostname a VM once reported identifies its sightings from then on --
// a stopped VM's too, whose stale cache row has to go the same way.
func TestSimulatorFilterExcludesKnownVMHostnames(t *testing.T) {
	stubVMStatuses(t, vm.Status{Name: "sim", Exists: true, Meta: vm.Meta{Hostname: "wendyos-gentle-forest"}})
	f := newSimulatorFilter(context.Background())
	if !f.Exclude(models.LANDevice{Hostname: "WendyOS-Gentle-Forest.local.", IPAddress: "10.0.2.15"}) {
		t.Error("the VM's own hostname was not excluded; case and the .local suffix must not matter")
	}
	if f.Exclude(models.LANDevice{Hostname: "wendyos-brave-dolphin.local"}) {
		t.Error("another device's hostname was excluded")
	}
	if f.Exclude(models.LANDevice{DisplayName: "Gentle Forest"}) {
		t.Error("a display name alone is not an identity")
	}
}

// A running VM the CLI has never talked to has no hostname on record. The
// filter asks its agent over the loopback forward, records the answer for
// good, and tells the session to re-check what it already listed.
func TestSimulatorFilterLearnsARunningVMsHostname(t *testing.T) {
	port := listenLoopback(t)
	stubVMStatuses(t, runningVM("sim", vm.NetUser, port))
	stubAgentVersion(t, func(addr string) (*agentpb.GetAgentVersionResponse, error) {
		if addr != fmt.Sprintf("127.0.0.1:%d", port) {
			return nil, fmt.Errorf("unexpected address %s", addr)
		}
		return &agentpb.GetAgentVersionResponse{Hostname: "wendyos-gentle-forest"}, nil
	})
	recorded := recordedHostnames(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	f := newSimulatorFilter(ctx)
	awaitChanged(t, f)
	if !f.Exclude(models.LANDevice{Hostname: "wendyos-gentle-forest.local", IPAddress: "10.0.2.15"}) {
		t.Error("the learned hostname is not excluded")
	}
	if got := recorded(); got["sim"] != "wendyos-gentle-forest" {
		t.Errorf("recorded = %v, want sim -> wendyos-gentle-forest", got)
	}
}

// The agent is the last thing up in the guest, and QEMU's forward accepts
// long before it answers. The learner keeps asking rather than giving up on
// the first refusal.
func TestSimulatorFilterKeepsAskingWhileTheGuestBoots(t *testing.T) {
	port := listenLoopback(t)
	stubVMStatuses(t, runningVM("sim", vm.NetUser, port))
	var attempts atomic.Int32
	stubAgentVersion(t, func(string) (*agentpb.GetAgentVersionResponse, error) {
		if attempts.Add(1) < 3 {
			return nil, errors.New("connection reset by peer")
		}
		return &agentpb.GetAgentVersionResponse{Hostname: "wendyos-gentle-forest"}, nil
	})
	recordedHostnames(t)
	saved := simulatorHostnameRetry
	simulatorHostnameRetry = 10 * time.Millisecond
	t.Cleanup(func() { simulatorHostnameRetry = saved })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	f := newSimulatorFilter(ctx)
	awaitChanged(t, f)
	if n := attempts.Load(); n < 3 {
		t.Errorf("attempts = %d, want the learner to have retried", n)
	}
	if !f.Exclude(models.LANDevice{Hostname: "wendyos-gentle-forest.local", IPAddress: "10.0.2.15"}) {
		t.Error("the learned hostname is not excluded")
	}
}

// A VM already on record, a stopped one, and a shared-mode one (no forward to
// ask on) give the learner nothing to do: nothing is dialed for them.
func TestSimulatorFilterAsksOnlyVMsItCanReachAndDoesNotKnow(t *testing.T) {
	port := listenLoopback(t)
	known := runningVM("sim", vm.NetUser, port)
	known.Meta.Hostname = "wendyos-gentle-forest"
	stubVMStatuses(t, known, vm.Status{Name: "off", Exists: true}, runningVM("shared", vm.NetShared, 0))
	var asked atomic.Int32
	stubAgentVersion(t, func(string) (*agentpb.GetAgentVersionResponse, error) {
		asked.Add(1)
		return &agentpb.GetAgentVersionResponse{Hostname: "wendyos-x"}, nil
	})
	recordedHostnames(t)

	f := newSimulatorFilter(context.Background())
	select {
	case <-f.Changed():
		t.Fatal("the filter learned something with nothing left to learn")
	case <-time.After(100 * time.Millisecond):
	}
	if n := asked.Load(); n != 0 {
		t.Errorf("agent asked %d times, want 0", n)
	}
	if !f.Exclude(models.LANDevice{Hostname: "wendyos-gentle-forest.local", IPAddress: "10.0.2.15"}) {
		t.Error("the hostname already on record is not excluded")
	}
}

// Every CLI LAN session gets the filter, so no surface lists a VM as a device.
func TestCLIStreamOptionsExcludeSimulators(t *testing.T) {
	stubVMStatuses(t, vm.Status{Name: "sim", Exists: true, Meta: vm.Meta{Hostname: "wendyos-gentle-forest"}})
	opts := cliLANStreamOptions(context.Background())
	if opts.Exclude == nil {
		t.Fatal("no simulator filter installed on the CLI's LAN stream options")
	}
	if !opts.Exclude.Exclude(models.LANDevice{Hostname: "wendyos-gentle-forest.local", IPAddress: "10.0.2.15"}) {
		t.Error("the installed filter does not exclude a known VM hostname")
	}
}

// The probe has the guest's answer in hand; recording the hostname there means
// the next session recognises the VM's sighting from the start.
func TestProbeRecordsTheHostnameItLearns(t *testing.T) {
	port := listenLoopback(t)
	stubVMStatuses(t, runningVM("sim", vm.NetUser, port))
	stubAgentVersion(t, func(string) (*agentpb.GetAgentVersionResponse, error) {
		return &agentpb.GetAgentVersionResponse{Hostname: "wendyos-gentle-forest", Version: "1"}, nil
	})
	recorded := recordedHostnames(t)

	got := probeLocalVMDevices(context.Background())
	if len(got) != 1 || got[0].ID != "vm:sim" || got[0].AgentVersion != "1" {
		t.Fatalf("probeLocalVMDevices() = %+v, want the one VM row", got)
	}
	if r := recorded(); r["sim"] != "wendyos-gentle-forest" {
		t.Errorf("recorded = %v, want sim -> wendyos-gentle-forest", r)
	}
}

// So does the wait `wendy run --device sim` goes through before it deploys.
func TestWaitForSimulatorReadyRecordsTheHostname(t *testing.T) {
	stubAgentVersion(t, func(string) (*agentpb.GetAgentVersionResponse, error) {
		return &agentpb.GetAgentVersionResponse{Hostname: "wendyos-gentle-forest"}, nil
	})
	recorded := recordedHostnames(t)

	if err := waitForSimulatorReady(context.Background(), "sim", "127.0.0.1:50051", time.Second); err != nil {
		t.Fatal(err)
	}
	if r := recorded(); r["sim"] != "wendyos-gentle-forest" {
		t.Errorf("recorded = %v, want sim -> wendyos-gentle-forest", r)
	}
}

// VM rows belong in their own list, never among LAN devices.
func TestLocalVMRowsAreListedAsSimulatorsNotLANDevices(t *testing.T) {
	c := &models.DevicesCollection{LANDevices: []models.LANDevice{{ID: "dev-1", DisplayName: "orin"}}}
	attachSimulators(c, []models.LANDevice{{ID: "vm:sim", DisplayName: "sim"}})
	if len(c.LANDevices) != 1 || c.LANDevices[0].ID != "dev-1" {
		t.Errorf("LANDevices = %+v, want only the real device", c.LANDevices)
	}
	if len(c.Simulators) != 1 || c.Simulators[0].ID != "vm:sim" {
		t.Errorf("Simulators = %+v, want the VM row", c.Simulators)
	}
}

// The one-shot table has no tabs, so a simulator is told apart by its type.
func TestDiscoverTableShowsSimulatorsWithTheirOwnType(t *testing.T) {
	items := discoverTableItems(&models.DevicesCollection{Simulators: []models.LANDevice{
		{ID: "vm:sim", DisplayName: "sim", IPAddress: "127.0.0.1", Port: 50051, DeviceType: "vm-arm64", AgentVersion: "1"},
	}})
	if len(items) != 1 {
		t.Fatalf("items = %+v, want one simulator row", items)
	}
	if items[0].picker.Type != "Simulator" {
		t.Errorf("Type = %q, want Simulator", items[0].picker.Type)
	}
	if items[0].picker.Name != "sim" || items[0].picker.Address != "127.0.0.1:50051" {
		t.Errorf("row = %+v, want sim at 127.0.0.1:50051", items[0].picker)
	}
}

// A retracted LAN row leaves the discover table, and its probe state with it.
func TestDiscoverModelDropsARetractedLANRow(t *testing.T) {
	m := newDiscoverModel(context.Background(), discovery.DiscoveryOptions{}, false)
	dev := models.LANDevice{ID: "vm-1", DisplayName: "Gentle Forest", Hostname: "wendyos-gentle-forest.local", IPAddress: "10.0.2.15", Port: 50051}
	updated, _ := m.Update(lanFoundEvent(dev))
	m = updated.(discoverModel)
	if len(m.collection.LANDevices) != 1 {
		t.Fatal("the row was not added")
	}

	updated, _ = m.Update(lanEventMsg{ev: discovery.LANEvent{Kind: discovery.LANRetracted, Device: dev}})
	m = updated.(discoverModel)
	if len(m.collection.LANDevices) != 0 {
		t.Errorf("retracted row still listed: %+v", m.collection.LANDevices)
	}
	if _, ok := m.probe[probeKey(dev)]; ok {
		t.Error("probe state for the retracted row lingers")
	}
}

// The run picker takes a row back under the very key it was added with.
func TestLANPickerRemoveKeyMatchesTheAddedRow(t *testing.T) {
	dev := models.LANDevice{ID: "vm-1", DisplayName: "Gentle Forest", Hostname: "wendyos-gentle-forest.local"}
	item := lanPickerItem(dev, false, tui.ProbePending)
	if item.DedupKey == "" {
		t.Fatal("LAN rows must carry a dedup key")
	}
	if got := lanPickerRemoveMsg(dev).Key; !strings.EqualFold(got, item.DedupKey) {
		t.Errorf("remove key %q does not match the row's dedup key %q", got, item.DedupKey)
	}
}
