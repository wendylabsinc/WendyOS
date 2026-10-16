package robotinspect

import (
	"context"
	"fmt"
	"sort"
)

// Class says whether a probe can move the robot. Inspection runs only ClassPassive, so
// read-only is a property of the plan rather than a promise in a comment: a probe that
// might actuate cannot be selected even by mistake, and a caller that wants one has to
// ask for it somewhere else.
type Class string

const (
	// ClassPassive cannot command an actuator under any input. Reading a topic, a
	// sysfs file, a URDF or a servo's limit register.
	ClassPassive Class = "passive"
	// ClassMayActuate might move something, including indirectly — a vendor self-test,
	// a homing sweep, anything that enters a control mode.
	ClassMayActuate Class = "may-actuate"
)

// Requirement is something a probe needs the robot or host to offer. It is a plain
// string so a backend can introduce its own without a change here.
type Requirement string

// Requirements the core probes ask for. A serial-only arm and a DDS humanoid both
// resolve through this list, which is why it names transports rather than robot kinds.
const (
	RequirementROS2Graph             Requirement = "ros2:graph"
	RequirementROS2ControllerManager Requirement = "ros2:controller_manager"
	RequirementDDSDomain             Requirement = "dds:domain"
	RequirementCameraTransport       Requirement = "camera:transport"
	RequirementSerialPort            Requirement = "serial:port"
	RequirementCANInterface          Requirement = "can:interface"
	RequirementURDF                  Requirement = "artifact:urdf"
	RequirementHostStats             Requirement = "host:stats"
	RequirementTimeSync              Requirement = "host:timesync"
	RequirementVendorBackend         Requirement = "backend:vendor"
)

// Env is what this robot offers. Handles carry transport-specific objects — a DDS
// participant, an open serial port, a camera client — that only the probe asking for
// them knows how to use. The core never looks inside one, which is what keeps a servo
// bus and a DDS graph equally first-class.
type Env struct {
	offers  map[Requirement]bool
	handles map[Requirement]any
}

// NewEnv returns an Env offering nothing.
func NewEnv() *Env {
	return &Env{offers: map[Requirement]bool{}, handles: map[Requirement]any{}}
}

// Offer records that a requirement is available, with an optional handle for the probe
// that consumes it.
func (e *Env) Offer(req Requirement, handle any) *Env {
	e.offers[req] = true
	if handle != nil {
		e.handles[req] = handle
	}
	return e
}

// Offers reports whether a requirement is available.
func (e *Env) Offers(req Requirement) bool { return e != nil && e.offers[req] }

// Handle returns the handle recorded for a requirement.
func (e *Env) Handle(req Requirement) (any, bool) {
	if e == nil {
		return nil, false
	}
	handle, ok := e.handles[req]
	return handle, ok
}

// Probe observes some properties of a robot over one transport. Implementations live
// beside the transport they speak, never here.
type Probe interface {
	// ID is stable and appears in every observation's Source.
	ID() string
	// Class decides whether inspection may run this probe at all.
	Class() Class
	// Requires lists what must be on offer. A probe runs only if all are.
	Requires() []Requirement
	// Provides lists the property IDs this probe can answer, so a property with no
	// probe at all is reported as unknown rather than silently omitted.
	Provides() []string
	// Observe returns what it found. Returning properties and an error together is
	// allowed: partial answers are more useful than none.
	Observe(ctx context.Context, env *Env) ([]Property, error)
}

// Registry is the set of known probes.
type Registry struct {
	probes map[string]Probe
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{probes: map[string]Probe{}} }

// Register adds a probe, rejecting a duplicate ID because the ID is what an observation
// is traced back through.
func (r *Registry) Register(p Probe) error {
	if p.ID() == "" {
		return fmt.Errorf("robotinspect: probe with no ID")
	}
	if _, exists := r.probes[p.ID()]; exists {
		return fmt.Errorf("robotinspect: probe %q already registered", p.ID())
	}
	r.probes[p.ID()] = p
	return nil
}

// MustRegister is Register for a package-level registration that cannot fail at runtime.
func (r *Registry) MustRegister(p Probe) {
	if err := r.Register(p); err != nil {
		panic(err)
	}
}

// Plan is which probes an inspection will run, and why the rest were left out.
type Plan struct {
	Run     []Probe
	Skipped map[string]Unknown
}

// PassivePlan selects the probes an inspection may run against env. A probe is skipped
// when it may actuate, or when env does not offer everything it needs; either way the
// reason is recorded so the report can say what was not looked at.
func (r *Registry) PassivePlan(env *Env) Plan {
	plan := Plan{Skipped: map[string]Unknown{}}
	ids := make([]string, 0, len(r.probes))
	for id := range r.probes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := r.probes[id]
		if p.Class() != ClassPassive {
			plan.Skipped[id] = NewUnknown(ReasonNotPassive,
				fmt.Sprintf("%q may actuate the robot and is never run by inspection", id))
			continue
		}
		if missing := missingRequirements(p, env); len(missing) > 0 {
			plan.Skipped[id] = NewUnknown(ReasonRequirementUnmet,
				fmt.Sprintf("%q needs %s", id, joinRequirements(missing)))
			continue
		}
		plan.Run = append(plan.Run, p)
	}
	return plan
}

// UnprovidedProperties returns the property IDs in want that no probe in the plan can
// answer. The caller turns these into explicit unknowns, so a robot that cannot report
// its joint limits says so instead of omitting the row.
func (p Plan) UnprovidedProperties(want []string) []string {
	provided := map[string]bool{}
	for _, probe := range p.Run {
		for _, id := range probe.Provides() {
			provided[id] = true
		}
	}
	var missing []string
	for _, id := range want {
		if !provided[id] {
			missing = append(missing, id)
		}
	}
	sort.Strings(missing)
	return missing
}

func missingRequirements(p Probe, env *Env) []Requirement {
	var missing []Requirement
	for _, req := range p.Requires() {
		if !env.Offers(req) {
			missing = append(missing, req)
		}
	}
	return missing
}

func joinRequirements(reqs []Requirement) string {
	out := make([]string, 0, len(reqs))
	for _, req := range reqs {
		out = append(out, string(req))
	}
	sort.Strings(out)
	joined := ""
	for i, s := range out {
		if i > 0 {
			joined += ", "
		}
		joined += s
	}
	return joined
}
