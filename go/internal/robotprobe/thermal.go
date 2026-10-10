package robotprobe

import (
	"context"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
)

// ThermalZone is one temperature sensor the host exposes, named as the kernel names it.
type ThermalZone struct {
	Name    string
	Celsius float64
}

// GPUStats is one GPU's live state. Temperature is a pointer because a GPU that reports
// none is different from one reporting zero.
type GPUStats struct {
	Index           uint32
	Name            string
	UtilPercent     float64
	MemoryUsedBytes int64
	Celsius         *float64
}

// LiveHostStats is the host's moment-to-moment state, as opposed to the inventory the
// Compute probe reports.
type LiveHostStats struct {
	MemoryAvailableBytes int64
	ThermalZones         []ThermalZone
	GPUs                 []GPUStats
}

// LiveHostSource supplies them. The CLI backs it with the agent's resource-stats RPC,
// the same one `wendy device top` uses.
type LiveHostSource interface {
	LiveHostStats(ctx context.Context) (*LiveHostStats, error)
}

// Thermal reports how hot the computer is running and how much memory is left.
//
// It is separate from Compute because these are readings, not facts: a board model is
// true until the board changes, while a temperature is true for a moment. They are
// recorded as instant measurements, with no sampling window, because that is what they
// are.
//
// There is no power draw here. The agent's resource-stats RPC exposes thermal zones,
// GPU state and memory, and no watts — so reporting a power figure would mean inventing
// one.
type Thermal struct{}

func (Thermal) ID() string                { return "thermal" }
func (Thermal) Class() robotinspect.Class { return robotinspect.ClassPassive }
func (Thermal) Requires() []robotinspect.Requirement {
	return []robotinspect.Requirement{robotinspect.RequirementHostStats}
}

// Provides promises only what every host answers. Zone names are per board, so the
// per-zone rows cannot be promised by name.
func (Thermal) Provides() []string {
	return []string{"thermal.max", "compute.memory.available"}
}

func (p Thermal) Observe(ctx context.Context, env *robotinspect.Env) ([]robotinspect.Property, error) {
	handle, ok := env.Handle(robotinspect.RequirementHostStats)
	if !ok {
		return nil, fmt.Errorf("thermal: no agent handle in the environment")
	}
	source, ok := handle.(LiveHostSource)
	if !ok {
		return nil, fmt.Errorf("thermal: agent handle is %T, not a LiveHostSource", handle)
	}

	stats, err := source.LiveHostStats(ctx)
	if err != nil {
		// An agent too old for this RPC is a finding about the fleet, not a failure
		// of the inspection.
		unknown := robotinspect.NewUnknown(robotinspect.ReasonProbeFailed, err.Error())
		return []robotinspect.Property{
			{ID: "thermal.max", Unknown: &unknown},
			{ID: "compute.memory.available", Unknown: &unknown},
		}, nil
	}

	origin := "agent:resource_stats"
	observation := func(id string, value float64, unit robotinspect.Unit, conditions map[string]string) (robotinspect.Property, bool) {
		quantity, err := robotinspect.NewQuantity(value, unit)
		if err != nil {
			return robotinspect.Property{}, false
		}
		opts := []robotinspect.ObservationOption{robotinspect.WithInstantReading()}
		if len(conditions) > 0 {
			opts = append(opts, robotinspect.WithConditions(conditions))
		}
		o, err := robotinspect.NewObservation(quantity, robotinspect.Measured,
			robotinspect.Source{Probe: p.ID(), Origin: origin}, opts...)
		if err != nil {
			return robotinspect.Property{}, false
		}
		return robotinspect.Property{ID: id, Observations: []robotinspect.Observation{o}}, true
	}

	properties := []robotinspect.Property{}
	if property, ok := observation("compute.memory.available", float64(stats.MemoryAvailableBytes), robotinspect.Bytes, nil); ok {
		properties = append(properties, property)
	}

	// The hottest zone is what an operator acts on, so it gets its own row naming the
	// zone — the same shape as the hottest motor.
	var hottest *ThermalZone
	for i := range stats.ThermalZones {
		zone := stats.ThermalZones[i]
		if property, ok := observation("thermal."+sanitiseKey(zone.Name), zone.Celsius, robotinspect.Celsius, nil); ok {
			properties = append(properties, property)
		}
		if hottest == nil || zone.Celsius > hottest.Celsius {
			hottest = &stats.ThermalZones[i]
		}
	}
	if hottest == nil {
		unknown := robotinspect.NewUnknown(robotinspect.ReasonSourceAbsent,
			"the agent reports no thermal zones on this host")
		properties = append(properties, robotinspect.Property{ID: "thermal.max", Unknown: &unknown})
	} else if property, ok := observation("thermal.max", hottest.Celsius, robotinspect.Celsius,
		map[string]string{"zone": hottest.Name}); ok {
		properties = append(properties, property)
	}

	for _, gpu := range stats.GPUs {
		prefix := fmt.Sprintf("compute.gpu.%d", gpu.Index)
		if property, ok := observation(prefix+".utilisation", gpu.UtilPercent, robotinspect.Percent, nil); ok {
			properties = append(properties, property)
		}
		if gpu.Celsius != nil {
			if property, ok := observation(prefix+".temperature", *gpu.Celsius, robotinspect.Celsius, nil); ok {
				properties = append(properties, property)
			}
		} else {
			// An Orin reports GPU load and no GPU temperature; saying so beats
			// leaving the row out.
			unknown := robotinspect.NewUnknown(robotinspect.ReasonSourceAbsent,
				fmt.Sprintf("GPU %d reports no temperature", gpu.Index))
			properties = append(properties, robotinspect.Property{ID: prefix + ".temperature", Unknown: &unknown})
		}
	}
	return properties, nil
}
