package robotprobe

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotinspect"
)

// HardwareDevice is one thing the agent found attached to the host.
type HardwareDevice struct {
	Category    string
	DevicePath  string
	Description string
}

// HardwareSource enumerates attached hardware. The CLI backs it with the agent's
// hardware-capabilities RPC.
type HardwareSource interface {
	Hardware(ctx context.Context) ([]HardwareDevice, error)
}

// Hardware reports the device nodes the kernel exposes: video nodes, CAN interfaces,
// serial buses, audio devices.
//
// Identifiers say "node" deliberately. A RealSense presents six video nodes for three
// cameras, so "hardware.camera.count = 6" beside "camera.count = 3" read as a
// contradiction when the two were counting different things. This probe counts what the
// kernel exposes; the camera probe counts cameras.
//
// Each category gets a node count and the sorted list of paths. The list is recorded as
// one textual value on purpose: inspect the same robot twice and a camera that has
// disappeared shows up as a disagreement on that row, which is exactly the question an
// operator asks when a sensor stops working. A row per device path would instead make
// every reboot look like a change.
type Hardware struct{}

func (Hardware) ID() string                { return "hardware" }
func (Hardware) Class() robotinspect.Class { return robotinspect.ClassPassive }
func (Hardware) Requires() []robotinspect.Requirement {
	return []robotinspect.Requirement{robotinspect.RequirementHostStats}
}

// Provides cannot be known before enumeration, since categories depend on the robot.
// The count of attached devices is always answerable, so that is what it promises.
func (Hardware) Provides() []string { return []string{"hardware.nodes"} }

func (p Hardware) Observe(ctx context.Context, env *robotinspect.Env) ([]robotinspect.Property, error) {
	handle, ok := env.Handle(robotinspect.RequirementHostStats)
	if !ok {
		return nil, fmt.Errorf("hardware: no agent handle in the environment")
	}
	source, ok := handle.(HardwareSource)
	if !ok {
		// The host-facts handle and the hardware handle come from the same agent
		// connection, but a caller may offer only one of them.
		return nil, fmt.Errorf("hardware: agent handle is %T, not a HardwareSource", handle)
	}

	devices, err := source.Hardware(ctx)
	if err != nil {
		return nil, fmt.Errorf("hardware: enumerating: %w", err)
	}

	origin := "agent:hardware_capabilities"
	if len(devices) == 0 {
		unknown := robotinspect.NewUnknown(robotinspect.ReasonSourceAbsent,
			"the agent enumerated no attached hardware")
		return []robotinspect.Property{{ID: "hardware.nodes", Unknown: &unknown}}, nil
	}

	byCategory := map[string][]string{}
	for _, device := range devices {
		category := strings.ToLower(device.Category)
		if category == "" {
			category = "other"
		}
		name := device.DevicePath
		if name == "" {
			name = device.Description
		}
		if name != "" {
			byCategory[category] = append(byCategory[category], name)
		}
	}

	properties := []robotinspect.Property{}
	if property, ok := declared(p.ID(), origin, "hardware.nodes",
		robotinspect.MustQuantity(float64(len(devices)), robotinspect.Count)); ok {
		properties = append(properties, property)
	}

	categories := make([]string, 0, len(byCategory))
	for category := range byCategory {
		categories = append(categories, category)
	}
	sort.Strings(categories)

	for _, category := range categories {
		names := byCategory[category]
		sort.Strings(names)
		if property, ok := declared(p.ID(), origin,
			fmt.Sprintf("hardware.%s.node_count", category),
			robotinspect.MustQuantity(float64(len(names)), robotinspect.Count)); ok {
			properties = append(properties, property)
		}
		properties = append(properties, textProperty(p.ID(), origin,
			fmt.Sprintf("hardware.%s.nodes", category), strings.Join(names, " ")))
	}
	return properties, nil
}
