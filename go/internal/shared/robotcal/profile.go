package robotcal

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Profile is what a model of robot is, in the few terms the platform can
// enforce something with. Everything else about a robot — its URDF, its
// bring-up sequence, its torque limits — is carried opaquely, because a field
// the platform interprets is a field the platform has to keep working for every
// robot that ever ships.
//
// The rule this encodes: WendyOS defines the slots and runs the procedure loop,
// the profile defines the contents and picks the method, and apps own whatever
// the fixed method set does not cover.
type Profile struct {
	// Kind identifies the model, not the unit: every G1 shares one profile, and
	// the per-unit facts live in calibration records.
	Kind        string `yaml:"kind" json:"kind"`
	DisplayName string `yaml:"display_name,omitempty" json:"display_name,omitempty"`

	Control Control  `yaml:"control" json:"control"`
	Joints  Joints   `yaml:"joints" json:"joints"`
	Sensors []Sensor `yaml:"sensors,omitempty" json:"sensors,omitempty"`

	// RequiresCalibration is what makes readiness decidable: the robot itself
	// says what it needs measured before it can be trusted.
	RequiresCalibration []Procedure `yaml:"requires_calibration,omitempty" json:"requires_calibration,omitempty"`

	// Opaque carries everything the platform does not interpret — description
	// (URDF), limits, bringup. It is kept, round-tripped and handed to apps
	// verbatim. Nothing in this package reads inside it.
	Opaque map[string]any `yaml:",inline" json:"opaque,omitempty"`
}

// Control is how the robot is commanded, and — the part calibration needs —
// what "not powered" means on it.
type Control struct {
	// Transport names the command channel: dds, feetech-serial, canopen. The
	// platform does not interpret it; it is recorded and it is what the motion
	// entitlement will gate (WDY-3128).
	Transport string `yaml:"transport,omitempty" json:"transport,omitempty"`
	Interface string `yaml:"interface,omitempty" json:"interface,omitempty"`
	// Unpowered is the state a nothing-powered procedure requires, in the
	// robot's own words. The platform quotes it to the operator and asks them
	// to confirm it; it never interprets it. On a humanoid this is a vendor
	// state-machine state, on a servo-bus arm it is "torque disabled", and on
	// the next robot it will be something else again — which is exactly why
	// this is a string the profile supplies rather than a value the platform
	// enumerates.
	Unpowered Unpowered `yaml:"unpowered,omitempty" json:"unpowered,omitempty"`
}

// Unpowered describes, and tells an operator how to reach, the state in which
// nothing on the robot is energised.
type Unpowered struct {
	// Name is what this robot calls the state, printed verbatim.
	Name string `yaml:"name" json:"name"`
	// Instruction is how the operator puts the robot into it.
	Instruction string `yaml:"instruction" json:"instruction"`
	// Hazard is what physically happens when they do. Printed before the
	// operator is asked to do anything, never after.
	Hazard string `yaml:"hazard,omitempty" json:"hazard,omitempty"`
}

// Joints is the joint contract: the names, the order commands are indexed in,
// and where live positions can be read from.
//
// Order matters more than it looks. A policy that drives joints by index needs
// index 31 on this robot to be the physical joint it was during training. If a
// hand is replaced or a firmware revision reorders the array, the policy drives
// the wrong limb at full confidence. Recording the order here is what lets a
// sweep check it.
type Joints struct {
	Order []string `yaml:"order" json:"order"`
	// Unit is the unit joint positions are reported in — rad, tick, deg. The
	// platform never converts between units; it requires that a procedure's
	// budget is quoted in the same one and refuses otherwise.
	Unit string `yaml:"unit" json:"unit"`
	// Declared is the vendor's travel per joint, where the vendor declares one.
	// It is deliberately optional: on the SO-101 the limits are a measured,
	// per-arm fact rather than a datasheet one, so its profile declares none
	// and its procedure carries a minimum-travel parameter instead. A design
	// that assumed declared limits exist would fit the humanoid and break on
	// the arm.
	Declared map[string]Span `yaml:"declared,omitempty" json:"declared,omitempty"`
	// Source says where live joint positions come from.
	Source JointSourceSpec `yaml:"source" json:"source"`
}

// Span is a closed interval of joint travel, in Joints.Unit.
type Span struct {
	Min float64 `yaml:"min" json:"min"`
	Max float64 `yaml:"max" json:"max"`
}

// Travel is the width of the interval.
func (s Span) Travel() float64 { return s.Max - s.Min }

// JointSourceSpec selects a backend that can read this robot's joints, and
// parameterises it. The backend is named, not described: the platform owns the
// set of backends exactly as it owns the set of methods.
type JointSourceSpec struct {
	Backend string            `yaml:"backend" json:"backend"`
	Params  map[string]string `yaml:"params,omitempty" json:"params,omitempty"`
	// Note is what this robot's author has to say about the backend — usually
	// why it is the one this machine needs, and what a build that cannot open
	// it is missing. The platform prints it verbatim when it has to refuse and
	// never interprets a word of it.
	//
	// It is here rather than in a case arm in the CLI so that explaining a
	// vendor transport does not require naming that vendor in platform code. A
	// switch on backend names is one refactor away from a switch that changes
	// behaviour.
	Note string `yaml:"note,omitempty" json:"note,omitempty"`
}

// Sensor is a logical role — "there is a head RGB-D camera" — not a device. The
// physical device behind it is a per-unit fact and is bound separately, by the
// platform's existing stable-id scheme rather than a raw serial number.
type Sensor struct {
	ID   string `yaml:"id" json:"id"`
	Type string `yaml:"type" json:"type"`
	// AttachedTo is the link the sensor is bolted to. A sensor's pose is not
	// constant — it hangs off something that moves — so naming the link rather
	// than a world position is what makes one rule cover every sensor on every
	// articulated robot: pose = FK(link, live joints) ∘ nominal mount ∘
	// calibration correction.
	AttachedTo  string       `yaml:"attached_to,omitempty" json:"attached_to,omitempty"`
	NominalPose *PoseXYZWXYZ `yaml:"nominal_pose,omitempty" json:"nominal_pose,omitempty"`
}

// PoseXYZWXYZ is a nominal mounting pose: translation and a quaternion, in the
// frame named by AttachedTo.
type PoseXYZWXYZ struct {
	XYZ  [3]float64 `yaml:"xyz" json:"xyz"`
	Quat [4]float64 `yaml:"quat" json:"quat"`
}

// Procedure is one entry in requires_calibration: a calibration this robot
// needs, which method measures it, and what counts as good enough.
type Procedure struct {
	ID     string   `yaml:"id" json:"id"`
	Title  string   `yaml:"title,omitempty" json:"title,omitempty"`
	Method MethodID `yaml:"method" json:"method"`
	// AppliesTo names what is being calibrated — a sensor id, or a joint group.
	// Calibration is per sensor and per group, never one global boolean: a
	// wrist camera and a head camera are separate measurements with separate
	// residuals.
	AppliesTo string `yaml:"applies_to,omitempty" json:"applies_to,omitempty"`
	// Class raises the safety class above the method's floor for a robot where
	// the procedure is more dangerous than it usually is. It can never lower it.
	Class SafetyClass `yaml:"class,omitempty" json:"class,omitempty"`
	// Budget is the gate. A procedure with no budget can never qualify, because
	// completing a procedure is not the same as passing it.
	Budget Quantity `yaml:"budget" json:"budget"`
	// Joints narrows a joint method to a subset — one arm rather than the whole
	// robot. Empty means every joint in Joints.Order.
	Joints []string `yaml:"joints,omitempty" json:"joints,omitempty"`
	// Params are the selected method's own parameters. Each method decodes this
	// into a typed struct it declares, so a profile can supply a parameter but
	// cannot invent a step.
	Params map[string]string `yaml:"params,omitempty" json:"params,omitempty"`
}

// reservedProcedureIDs are the words a procedure id may not be, because
// `calibrate <id>` shares its argument position with these subcommands and a
// procedure called "status" would be unreachable.
//
// The CLI has a test asserting every subcommand it registers under `calibrate`
// appears here, so adding one there and forgetting this cannot silently make a
// profile id unreachable.
var reservedProcedureIDs = map[string]bool{"status": true, "clear": true, "profiles": true}

// ReservedProcedureIDs lists the ids a profile may not use, sorted.
func ReservedProcedureIDs() []string {
	out := make([]string, 0, len(reservedProcedureIDs))
	for id := range reservedProcedureIDs {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Validate refuses a profile the platform cannot act on, naming what is wrong.
// Everything it checks is something the platform later relies on; anything it
// does not check is opaque by design.
func (p *Profile) Validate() error {
	if strings.TrimSpace(p.Kind) == "" {
		return fmt.Errorf("profile has no kind")
	}
	if len(p.Joints.Order) == 0 {
		return fmt.Errorf("profile %q declares no joints.order: the joint order is the contract a "+
			"policy driving joints by index depends on, and without it a sweep cannot verify the joint map", p.Kind)
	}
	if strings.TrimSpace(p.Joints.Unit) == "" {
		return fmt.Errorf("profile %q declares no joints.unit: a joint position without a unit "+
			"cannot be compared with a budget", p.Kind)
	}
	seen := make(map[string]bool, len(p.Joints.Order))
	for _, j := range p.Joints.Order {
		if seen[j] {
			return fmt.Errorf("profile %q lists joint %q twice in joints.order", p.Kind, j)
		}
		seen[j] = true
	}
	for name := range p.Joints.Declared {
		if !seen[name] {
			return fmt.Errorf("profile %q declares limits for joint %q, which is not in joints.order", p.Kind, name)
		}
	}
	if p.Joints.Source.Backend == "" {
		return fmt.Errorf("profile %q names no joints.source.backend: nothing can read this robot's joints", p.Kind)
	}

	sensors := make(map[string]bool, len(p.Sensors))
	for _, s := range p.Sensors {
		if s.ID == "" {
			return fmt.Errorf("profile %q has a sensor with no id", p.Kind)
		}
		if sensors[s.ID] {
			return fmt.Errorf("profile %q declares sensor %q twice", p.Kind, s.ID)
		}
		sensors[s.ID] = true
	}

	ids := make(map[string]bool, len(p.RequiresCalibration))
	for i := range p.RequiresCalibration {
		proc := &p.RequiresCalibration[i]
		if proc.ID == "" {
			return fmt.Errorf("profile %q has a requires_calibration entry with no id", p.Kind)
		}
		if reservedProcedureIDs[proc.ID] {
			return fmt.Errorf("profile %q uses the reserved calibration id %q: "+
				"`wendy device robot calibrate %s` is a subcommand, so a procedure by that name could never be run",
				p.Kind, proc.ID, proc.ID)
		}
		if ids[proc.ID] {
			return fmt.Errorf("profile %q declares calibration %q twice", p.Kind, proc.ID)
		}
		ids[proc.ID] = true

		desc, err := Method(proc.Method)
		if err != nil {
			return fmt.Errorf("profile %q, calibration %q: %w", p.Kind, proc.ID, err)
		}
		if _, err := Resolve(desc.ClassFloor, proc.Class); err != nil {
			return fmt.Errorf("profile %q, calibration %q: %w", p.Kind, proc.ID, err)
		}
		if proc.Budget.Value <= 0 || proc.Budget.Unit == "" {
			return fmt.Errorf("profile %q, calibration %q declares no budget: "+
				"the residual is the gate, so a calibration with nothing to pass could only ever be 'it ran'", p.Kind, proc.ID)
		}
		for _, j := range proc.Joints {
			if !seen[j] {
				return fmt.Errorf("profile %q, calibration %q names joint %q, which is not in joints.order", p.Kind, proc.ID, j)
			}
		}
		if proc.AppliesTo != "" && len(sensors) > 0 && !sensors[proc.AppliesTo] && !seen[proc.AppliesTo] {
			// applies_to may name a sensor, a joint, or a group the profile
			// does not enumerate; only refuse when it names something that
			// looks like a sensor id and is not one.
			if isSensorMethod(proc.Method) {
				return fmt.Errorf("profile %q, calibration %q applies to sensor %q, which the profile does not declare", p.Kind, proc.ID, proc.AppliesTo)
			}
		}
	}
	return nil
}

// isSensorMethod reports whether a method measures a sensor rather than joints,
// so applies_to can be checked against the declared sensors.
func isSensorMethod(id MethodID) bool {
	return id == MethodFiducialExtrinsics || id == MethodHandEyeExtrinsics
}

// Procedure returns the named calibration, or an error listing the ones this
// robot does declare — the ids come from the profile, so the robot itself says
// what it needs.
func (p *Profile) Procedure(id string) (Procedure, error) {
	for _, proc := range p.RequiresCalibration {
		if proc.ID == id {
			return proc, nil
		}
	}
	have := make([]string, 0, len(p.RequiresCalibration))
	for _, proc := range p.RequiresCalibration {
		have = append(have, proc.ID)
	}
	sort.Strings(have)
	if len(have) == 0 {
		return Procedure{}, fmt.Errorf("profile %q declares no calibrations at all", p.Kind)
	}
	return Procedure{}, fmt.Errorf("profile %q has no calibration %q; it declares %s", p.Kind, id, strings.Join(have, ", "))
}

// JointIndex returns the position of a joint in the canonical order, or -1.
func (p *Profile) JointIndex(name string) int {
	for i, j := range p.Joints.Order {
		if j == name {
			return i
		}
	}
	return -1
}

// ProcedureJoints resolves the joints a procedure covers, defaulting to every
// joint the robot has.
func (p *Profile) ProcedureJoints(proc Procedure) []string {
	if len(proc.Joints) > 0 {
		return proc.Joints
	}
	return p.Joints.Order
}

// ParseProfile decodes and validates a profile.
func ParseProfile(data []byte) (*Profile, error) {
	var p Profile
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parsing robot profile: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}
