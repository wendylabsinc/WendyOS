package robotprobe

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
)

type fakeHost struct {
	facts *HostFacts
	err   error
	calls int
}

func (f *fakeHost) HostFacts(context.Context) (*HostFacts, error) {
	f.calls++
	return f.facts, f.err
}

// The facts a Jetson Orin Nano actually reports, as read off wendy-box-theta.
func orinNanoFacts() *HostFacts {
	return &HostFacts{
		Hostname: "wendy-box-theta", DeviceType: "jetson-orin-nano",
		CPUArchitecture: "arm64", CPUCount: 6, MemoryTotalBytes: 8000352256,
		OS: "wendyos", OSVersion: "2026.08", AgentVersion: "dev",
		HasGPU: true, GPUVendor: "nvidia", GPUArch: "sm_87",
		CUDAVersion: "13.2", JetpackVersion: "7.2",
		ComputeBackends: []string{"cuda"},
		DiskTotalBytes:  11969634304, DiskUsedBytes: 6436712448, StorageMedium: "emmc",
		ContainerStorage: &Partition{Mountpoint: "/var/lib/containerd", TotalBytes: 8000000000, UsedBytes: 2000000000},
		Interfaces: []HostInterface{
			{Name: "eth0", Addresses: []string{"192.168.188.254"}},
			{Name: "wlan0", Addresses: []string{"10.10.20.129"}},
		},
	}
}

func hostEnv(source HostFactsSource) *robotinspect.Env {
	return robotinspect.NewEnv().Offer(robotinspect.RequirementHostStats, source)
}

func TestComputeReportsTheBoardAsText(t *testing.T) {
	properties, err := Compute{}.Observe(context.Background(), hostEnv(&fakeHost{facts: orinNanoFacts()}))
	if err != nil {
		t.Fatal(err)
	}

	board := findIn(t, properties, "compute.board")
	observation := board.Observations[0]
	if !observation.IsText() {
		t.Fatal("a board model is not a number; it must be recorded as text")
	}
	if observation.Text != "jetson-orin-nano" {
		t.Errorf("board = %q", observation.Text)
	}
	if got := board.Assess().Verdict; got != robotinspect.VerdictSingle {
		t.Errorf("verdict = %q, want %q", got, robotinspect.VerdictSingle)
	}

	// Numbers still come through as quantities.
	cpus := findIn(t, properties, "compute.cpu_count")
	if got := cpus.Observations[0].Quantity.Value(); got != 6 {
		t.Errorf("cpu_count = %v, want 6", got)
	}
	for _, id := range []string{"compute.architecture", "os.version", "agent.version", "compute.gpu.arch"} {
		findIn(t, properties, id)
	}
}

// A host with no GPU says so, rather than leaving the row out and letting a reader
// assume nobody looked.
func TestComputeReportsAnAbsentGPUExplicitly(t *testing.T) {
	facts := orinNanoFacts()
	facts.HasGPU, facts.GPUVendor, facts.GPUArch = false, "", ""

	properties, err := Compute{}.Observe(context.Background(), hostEnv(&fakeHost{facts: facts}))
	if err != nil {
		t.Fatal(err)
	}
	vendor := findIn(t, properties, "compute.gpu.vendor")
	if vendor.Unknown == nil || vendor.Unknown.Reason != robotinspect.ReasonSourceAbsent {
		t.Errorf("gpu vendor = %+v, want an unknown naming the absence", vendor.Unknown)
	}
}

// Apps do not always share the OS partition, and the app partition is the one that
// fills up first.
func TestStorageReportsTheAppPartitionSeparately(t *testing.T) {
	properties, err := Storage{}.Observe(context.Background(), hostEnv(&fakeHost{facts: orinNanoFacts()}))
	if err != nil {
		t.Fatal(err)
	}
	free := findIn(t, properties, "storage.free")
	if got := free.Observations[0].Quantity.Value(); got != 11969634304-6436712448 {
		t.Errorf("storage.free = %v, want total minus used", got)
	}
	apps := findIn(t, properties, "storage.apps.free")
	if got := apps.Observations[0].Quantity.Value(); got != 6000000000 {
		t.Errorf("storage.apps.free = %v, want 6000000000", got)
	}
	if !strings.Contains(apps.Observations[0].Source.Origin, "/var/lib/containerd") {
		t.Errorf("origin %q should name the partition", apps.Observations[0].Source.Origin)
	}
	findIn(t, properties, "storage.medium")
}

func TestNetworkReportsEachInterfaceAddress(t *testing.T) {
	properties, err := Network{}.Observe(context.Background(), hostEnv(&fakeHost{facts: orinNanoFacts()}))
	if err != nil {
		t.Fatal(err)
	}
	if got := findIn(t, properties, "network.interfaces").Observations[0].Quantity.Value(); got != 2 {
		t.Errorf("interface count = %v, want 2", got)
	}
	if got := findIn(t, properties, "network.eth0.address").Observations[0].Text; got != "192.168.188.254" {
		t.Errorf("eth0 address = %q", got)
	}
	findIn(t, properties, "network.wlan0.address")
}

// A robot's own battery and the battery of the machine running its software are
// different facts. This probe only ever reports the latter, under its own identifier.
func TestHostBatteryIsNamespacedAwayFromTheRobotBattery(t *testing.T) {
	facts := orinNanoFacts()
	facts.Battery = &BatteryFacts{Percent: 87.5, State: "discharging"}

	properties, err := HostBattery{}.Observe(context.Background(), hostEnv(&fakeHost{facts: facts}))
	if err != nil {
		t.Fatal(err)
	}
	charge := findIn(t, properties, "battery.host.charge")
	if got := charge.Observations[0].Quantity.Value(); got != 87.5 {
		t.Errorf("charge = %v, want 87.5", got)
	}
	if got := charge.Observations[0].Conditions["state"]; got != "discharging" {
		t.Errorf("state condition = %q", got)
	}
}

func TestHostBatteryReportsAbsenceOnAMainsPoweredBoard(t *testing.T) {
	properties, err := HostBattery{}.Observe(context.Background(), hostEnv(&fakeHost{facts: orinNanoFacts()}))
	if err != nil {
		t.Fatal(err)
	}
	charge := findIn(t, properties, "battery.host.charge")
	if charge.Unknown == nil || charge.Unknown.Reason != robotinspect.ReasonSourceAbsent {
		t.Errorf("charge = %+v, want an unknown", charge.Unknown)
	}
}

func TestHostProbesSurfaceAnAgentFailure(t *testing.T) {
	source := &fakeHost{err: errors.New("agent unreachable")}
	for _, probe := range []robotinspect.Probe{Compute{}, Storage{}, Network{}, HostBattery{}} {
		if _, err := probe.Observe(context.Background(), hostEnv(source)); err == nil {
			t.Errorf("%s swallowed an agent failure", probe.ID())
		}
	}
}

func TestHostProbesRefuseAnEnvironmentWithoutAnAgent(t *testing.T) {
	for _, probe := range []robotinspect.Probe{Compute{}, Storage{}, Network{}, HostBattery{}} {
		if _, err := probe.Observe(context.Background(), robotinspect.NewEnv()); err == nil {
			t.Errorf("%s ran with no agent handle", probe.ID())
		}
		wrong := robotinspect.NewEnv().Offer(robotinspect.RequirementHostStats, "not a source")
		if _, err := probe.Observe(context.Background(), wrong); err == nil {
			t.Errorf("%s accepted a handle that is not a HostFactsSource", probe.ID())
		}
	}
}

// All four are passive: they read what the agent already knows and touch nothing.
func TestHostProbesArePassive(t *testing.T) {
	for _, probe := range []robotinspect.Probe{Compute{}, Storage{}, Network{}, HostBattery{}} {
		if got := probe.Class(); got != robotinspect.ClassPassive {
			t.Errorf("%s class = %q, want passive", probe.ID(), got)
		}
	}
}

// Four probes, one RPC: the adapter caches, so a report does not interrogate the device
// once per section.
func TestHostProbesShareOneFetchThroughTheSource(t *testing.T) {
	source := &fakeHost{facts: orinNanoFacts()}
	env := hostEnv(source)
	registry := robotinspect.NewRegistry()
	for _, probe := range []robotinspect.Probe{Compute{}, Storage{}, Network{}, HostBattery{}} {
		registry.MustRegister(probe)
	}

	doc := robotinspect.Inspect(context.Background(), registry, env, robotinspect.Target{Device: "theta"})
	if len(doc.Properties) < 10 {
		t.Errorf("got %d properties, want the full host inventory", len(doc.Properties))
	}
	if source.calls != 4 {
		t.Errorf("source called %d times; the CLI adapter must cache so this is one RPC", source.calls)
	}
	if !doc.PassiveOnly {
		t.Error("PassiveOnly = false")
	}
	if got := doc.Summarise().Findings(); got != 0 {
		t.Errorf("findings = %d, want 0; host inventory has nothing to contradict", got)
	}
}

// Two robots are compared by identifier, so a fact must be a value on a shared row and
// not a row that only exists when the fact is true. compute.gpu.backend.cuda="present"
// gave a robot without CUDA no row at all, which is not a comparison.
func TestComputeListsBackendsOnOneRowSoTwoRobotsCompare(t *testing.T) {
	withCUDA := orinNanoFacts()
	withCUDA.ComputeBackends = []string{"cuda", "opencl"}
	properties, err := Compute{}.Observe(context.Background(), hostEnv(&fakeHost{facts: withCUDA}))
	if err != nil {
		t.Fatal(err)
	}
	backends := findIn(t, properties, "compute.gpu.backends")
	if got := backends.Observations[0].Text; got != "cuda opencl" {
		t.Errorf("backends = %q, want them sorted on one row", got)
	}

	// A GPU that names none still leaves the row, so the comparison has two sides.
	none := orinNanoFacts()
	none.ComputeBackends = nil
	properties, err = Compute{}.Observe(context.Background(), hostEnv(&fakeHost{facts: none}))
	if err != nil {
		t.Fatal(err)
	}
	empty := findIn(t, properties, "compute.gpu.backends")
	if empty.Unknown == nil || empty.Unknown.Reason != robotinspect.ReasonSourceAbsent {
		t.Errorf("backends = %+v, want an unknown rather than a missing row", empty.Unknown)
	}
}

// Every property Provides names must appear, as a value or as an unknown. A promised row
// that is simply absent reads as nobody having looked.
func TestComputeKeepsEveryPromiseItMakes(t *testing.T) {
	bare := &HostFacts{
		// A host that answers almost nothing: no GPU, no versions, no board name.
		CPUCount: 2, MemoryTotalBytes: 1 << 30,
	}
	properties, err := Compute{}.Observe(context.Background(), hostEnv(&fakeHost{facts: bare}))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, p := range properties {
		seen[p.ID] = true
	}
	for _, promised := range (Compute{}).Provides() {
		if !seen[promised] {
			t.Errorf("Provides names %q and the probe emitted no row for it", promised)
		}
	}
}
