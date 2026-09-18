package robotprobe

import (
	"context"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
)

// HostFacts is what the agent knows about the machine the robot runs on, flattened away
// from any proto type. The probes below read only this, so they are testable with a
// literal and a change to the agent's wire format lands in one adapter rather than in
// every probe.
type HostFacts struct {
	Hostname         string
	DeviceType       string
	CPUArchitecture  string
	CPUCount         int
	MemoryTotalBytes int64

	OS           string
	OSVersion    string
	AgentVersion string

	HasGPU          bool
	GPUVendor       string
	GPUArch         string
	CUDAVersion     string
	JetpackVersion  string
	ComputeBackends []string

	HasNPU    bool
	NPUVendor string

	DiskTotalBytes int64
	DiskUsedBytes  int64
	StorageMedium  string
	// ContainerStorage is the partition apps are deployed onto, which is not always
	// the same disk as the OS and is the one that runs out first.
	ContainerStorage *Partition

	Interfaces []HostInterface

	// Battery is nil on a machine with no battery, which is most of them. Absent is
	// not zero, so it stays a pointer.
	Battery *BatteryFacts
}

// Partition is one mounted filesystem.
type Partition struct {
	Mountpoint string
	TotalBytes int64
	UsedBytes  int64
}

// HostInterface is one network interface and the addresses on it.
type HostInterface struct {
	Name      string
	Addresses []string
}

// BatteryFacts is the host's own battery, as opposed to a robot battery published over
// ROS 2. Both can exist and they are different things.
type BatteryFacts struct {
	Percent float64
	State   string
}

// HostFactsSource supplies HostFacts. The CLI backs it with the agent's device-info RPC;
// a test backs it with a literal.
type HostFactsSource interface {
	HostFacts(ctx context.Context) (*HostFacts, error)
}

// hostFacts pulls the facts out of the environment for a probe that needs them.
func hostFacts(ctx context.Context, env *robotinspect.Env, probe string) (*HostFacts, error) {
	handle, ok := env.Handle(robotinspect.RequirementHostStats)
	if !ok {
		return nil, fmt.Errorf("%s: no agent handle in the environment", probe)
	}
	source, ok := handle.(HostFactsSource)
	if !ok {
		return nil, fmt.Errorf("%s: agent handle is %T, not a HostFactsSource", probe, handle)
	}
	facts, err := source.HostFacts(ctx)
	if err != nil {
		return nil, fmt.Errorf("%s: reading host facts: %w", probe, err)
	}
	if facts == nil {
		return nil, fmt.Errorf("%s: agent returned no host facts", probe)
	}
	return facts, nil
}

// declared is the common case for these probes: the agent reports what the host says
// about itself, which is a claim like any other. None of it is measured.
func declared(probe, origin string, id string, q robotinspect.Quantity) (robotinspect.Property, bool) {
	observation, err := robotinspect.NewObservation(q, robotinspect.Declared,
		robotinspect.Source{Probe: probe, Origin: origin})
	if err != nil {
		return robotinspect.Property{}, false
	}
	return robotinspect.Property{ID: id, Observations: []robotinspect.Observation{observation}}, true
}

// Compute reports the machine the robot's software runs on. It is the cheapest probe in
// the set — the agent already serves all of it — and it is what makes a report about a
// robot rather than about a camera.
type Compute struct{}

func (Compute) ID() string                { return "compute" }
func (Compute) Class() robotinspect.Class { return robotinspect.ClassPassive }
func (Compute) Requires() []robotinspect.Requirement {
	return []robotinspect.Requirement{robotinspect.RequirementHostStats}
}

func (Compute) Provides() []string {
	return []string{
		"compute.board", "compute.architecture", "compute.cpu_count", "compute.memory_total",
		"compute.gpu.vendor", "compute.gpu.arch", "compute.cuda_version", "compute.npu.vendor",
		"os.name", "os.version", "agent.version",
	}
}

func (p Compute) Observe(ctx context.Context, env *robotinspect.Env) ([]robotinspect.Property, error) {
	facts, err := hostFacts(ctx, env, p.ID())
	if err != nil {
		return nil, err
	}
	origin := "agent:device_info"

	var properties []robotinspect.Property
	add := func(id string, q robotinspect.Quantity) {
		if property, ok := declared(p.ID(), origin, id, q); ok {
			properties = append(properties, property)
		}
	}
	addText := func(id, value string) {
		if value == "" {
			return
		}
		properties = append(properties, textProperty(p.ID(), origin, id, value))
	}

	addText("compute.board", facts.DeviceType)
	addText("compute.architecture", facts.CPUArchitecture)
	if facts.CPUCount > 0 {
		add("compute.cpu_count", robotinspect.MustQuantity(float64(facts.CPUCount), robotinspect.Count))
	}
	if facts.MemoryTotalBytes > 0 {
		add("compute.memory_total", robotinspect.MustQuantity(float64(facts.MemoryTotalBytes), robotinspect.Bytes))
	}
	addText("os.name", facts.OS)
	addText("os.version", facts.OSVersion)
	addText("agent.version", facts.AgentVersion)

	if facts.HasGPU {
		addText("compute.gpu.vendor", facts.GPUVendor)
		addText("compute.gpu.arch", facts.GPUArch)
		addText("compute.cuda_version", facts.CUDAVersion)
		addText("compute.jetpack_version", facts.JetpackVersion)
		for _, backend := range facts.ComputeBackends {
			addText("compute.gpu.backend."+backend, "present")
		}
	} else {
		unknown := robotinspect.NewUnknown(robotinspect.ReasonSourceAbsent, "the agent reports no GPU on this host")
		properties = append(properties,
			robotinspect.Property{ID: "compute.gpu.vendor", Unknown: &unknown},
			robotinspect.Property{ID: "compute.gpu.arch", Unknown: &unknown})
	}

	if facts.HasNPU {
		addText("compute.npu.vendor", facts.NPUVendor)
	}
	return properties, nil
}

// Storage reports how much room is left, and on which partition apps land. A robot that
// cannot write is a robot that cannot record, which is worth knowing before a session
// rather than after.
type Storage struct{}

func (Storage) ID() string                { return "storage" }
func (Storage) Class() robotinspect.Class { return robotinspect.ClassPassive }
func (Storage) Requires() []robotinspect.Requirement {
	return []robotinspect.Requirement{robotinspect.RequirementHostStats}
}
func (Storage) Provides() []string {
	return []string{"storage.total", "storage.free", "storage.medium", "storage.apps.free"}
}

func (p Storage) Observe(ctx context.Context, env *robotinspect.Env) ([]robotinspect.Property, error) {
	facts, err := hostFacts(ctx, env, p.ID())
	if err != nil {
		return nil, err
	}
	origin := "agent:device_info"

	var properties []robotinspect.Property
	if facts.DiskTotalBytes > 0 {
		if property, ok := declared(p.ID(), origin, "storage.total",
			robotinspect.MustQuantity(float64(facts.DiskTotalBytes), robotinspect.Bytes)); ok {
			properties = append(properties, property)
		}
		free := facts.DiskTotalBytes - facts.DiskUsedBytes
		if property, ok := declared(p.ID(), origin, "storage.free",
			robotinspect.MustQuantity(float64(free), robotinspect.Bytes)); ok {
			properties = append(properties, property)
		}
	}
	if facts.StorageMedium != "" {
		properties = append(properties, textProperty(p.ID(), origin, "storage.medium", facts.StorageMedium))
	}
	if cs := facts.ContainerStorage; cs != nil && cs.TotalBytes > 0 {
		// Apps do not share the OS partition on every board, and this is the one that
		// fills up first.
		free := cs.TotalBytes - cs.UsedBytes
		if property, ok := declared(p.ID(), origin+":"+cs.Mountpoint, "storage.apps.free",
			robotinspect.MustQuantity(float64(free), robotinspect.Bytes)); ok {
			properties = append(properties, property)
		}
	}
	return properties, nil
}

// Network reports the interfaces the robot has. Multicast reachability is the fact that
// decides whether a ROS 2 graph is visible at all, and it needs its own probe; this one
// establishes what there is to reach it on.
type Network struct{}

func (Network) ID() string                { return "network" }
func (Network) Class() robotinspect.Class { return robotinspect.ClassPassive }
func (Network) Requires() []robotinspect.Requirement {
	return []robotinspect.Requirement{robotinspect.RequirementHostStats}
}
func (Network) Provides() []string { return []string{"network.interfaces"} }

func (p Network) Observe(ctx context.Context, env *robotinspect.Env) ([]robotinspect.Property, error) {
	facts, err := hostFacts(ctx, env, p.ID())
	if err != nil {
		return nil, err
	}
	origin := "agent:device_info"

	var properties []robotinspect.Property
	if property, ok := declared(p.ID(), origin, "network.interfaces",
		robotinspect.MustQuantity(float64(len(facts.Interfaces)), robotinspect.Count)); ok {
		properties = append(properties, property)
	}
	for _, iface := range facts.Interfaces {
		if len(iface.Addresses) == 0 {
			continue
		}
		properties = append(properties, textProperty(p.ID(), origin,
			fmt.Sprintf("network.%s.address", iface.Name), iface.Addresses[0]))
	}
	return properties, nil
}

// HostBattery reports the battery of the machine, which is not the robot's own battery.
// Both can exist and confusing them is how a laptop's charge ends up reported as a
// humanoid's.
type HostBattery struct{}

func (HostBattery) ID() string                { return "host-battery" }
func (HostBattery) Class() robotinspect.Class { return robotinspect.ClassPassive }
func (HostBattery) Requires() []robotinspect.Requirement {
	return []robotinspect.Requirement{robotinspect.RequirementHostStats}
}
func (HostBattery) Provides() []string { return []string{"battery.host.charge"} }

func (p HostBattery) Observe(ctx context.Context, env *robotinspect.Env) ([]robotinspect.Property, error) {
	facts, err := hostFacts(ctx, env, p.ID())
	if err != nil {
		return nil, err
	}
	if facts.Battery == nil {
		unknown := robotinspect.NewUnknown(robotinspect.ReasonSourceAbsent,
			"the agent reports no battery on this host")
		return []robotinspect.Property{{ID: "battery.host.charge", Unknown: &unknown}}, nil
	}

	// A charge level is read off the hardware, so it is measured, not declared. It is
	// an instant reading: there is no window to quote for a level.
	quantity, err := robotinspect.NewQuantity(facts.Battery.Percent, robotinspect.Percent)
	if err != nil {
		return nil, err
	}
	conditions := map[string]string{}
	if facts.Battery.State != "" {
		conditions["state"] = facts.Battery.State
	}
	observation, err := robotinspect.NewObservation(quantity, robotinspect.Measured,
		robotinspect.Source{Probe: p.ID(), Origin: "agent:device_info"},
		robotinspect.WithInstantReading(), robotinspect.WithConditions(conditions))
	if err != nil {
		return nil, err
	}
	return []robotinspect.Property{{
		ID:           "battery.host.charge",
		Observations: []robotinspect.Observation{observation},
	}}, nil
}

// textProperty records a textual fact — a board model, a version, a medium. These are
// claims the host makes about itself, so they are Declared; nothing here is measured.
func textProperty(probe, origin, id, value string) robotinspect.Property {
	observation, err := robotinspect.NewTextObservation(value, robotinspect.Declared,
		robotinspect.Source{Probe: probe, Origin: origin})
	if err != nil {
		// The only way this fails is an empty value, which callers filter first.
		unknown := robotinspect.NewUnknown(robotinspect.ReasonSourceAbsent, err.Error())
		return robotinspect.Property{ID: id, Unknown: &unknown}
	}
	return robotinspect.Property{ID: id, Observations: []robotinspect.Observation{observation}}
}
