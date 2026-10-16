package robotcal

import (
	"errors"
	"fmt"
)

// SafetyClass is what a procedure physically does, and the only thing the
// platform gates on. It is not a description of the procedure — it is the
// answer to "can this hurt someone or the robot".
type SafetyClass string

const (
	// ClassNothingPowered: no actuator is energised at any point. Moving a limp
	// arm by hand. Safe on a robot unboxed ten minutes ago.
	ClassNothingPowered SafetyClass = "nothing-powered"
	// ClassStaticObservation: the robot holds still and something is observed.
	// A fiducial in front of a camera. Nothing is commanded.
	ClassStaticObservation SafetyClass = "static-observation"
	// ClassPoweredMotion: the procedure commands the robot to move.
	ClassPoweredMotion SafetyClass = "powered-motion"
)

// severity orders the classes so a profile can raise one but never lower it.
func (c SafetyClass) severity() int {
	switch c {
	case ClassNothingPowered:
		return 0
	case ClassStaticObservation:
		return 1
	case ClassPoweredMotion:
		return 2
	default:
		return -1
	}
}

// Valid reports whether c is one of the three classes.
func (c SafetyClass) Valid() bool { return c.severity() >= 0 }

// Resolve returns the class a procedure actually runs at, given the floor its
// method can never go below and whatever the profile declared.
//
// The class belongs to the (profile, method) pair, not to the method alone. A
// joint-range sweep on a back-drivable arm energises nothing; the same sweep on
// a robot that must hold a brake open to be back-driven does. If the class were
// a property of the method, the arm would inherit the humanoid's gates for a
// procedure that never powers anything — and the whole point of the safe
// classes is that they run on day one.
//
// A profile may only raise the class. Lowering it is a refusal, not a warning:
// a profile that could declare a powered procedure unpowered is a profile that
// can switch the gate off.
func Resolve(floor, declared SafetyClass) (SafetyClass, error) {
	if !floor.Valid() {
		return "", fmt.Errorf("unknown safety class %q", floor)
	}
	if declared == "" {
		return floor, nil
	}
	if !declared.Valid() {
		return "", fmt.Errorf("unknown safety class %q: expected one of %s, %s, %s",
			declared, ClassNothingPowered, ClassStaticObservation, ClassPoweredMotion)
	}
	if declared.severity() < floor.severity() {
		return "", fmt.Errorf("a profile cannot declare %q for a method whose floor is %q: "+
			"the class may be raised for a robot where the procedure is more dangerous, never lowered",
			declared, floor)
	}
	return declared, nil
}

// GateInput is what the platform knows when it decides whether a procedure may
// run at all.
type GateInput struct {
	// Interactive is whether a person is at the terminal. Every method in the
	// fixed set has steps only a person can perform, so this is required for
	// all of them — see rule 6, no --non-interactive for human steps.
	Interactive bool
	// MotionEntitlementAvailable is whether the platform can actually enforce
	// the motion entitlement (WDY-3128). It is false in every build today,
	// because that entitlement does not exist: appconfig enumerates no `motion`
	// type and oci/entitlements.go has nothing to apply for it.
	//
	// It is a field rather than a constant so the gate is already written for
	// the day it lands, and so the refusal below is testable.
	MotionEntitlementAvailable bool
}

// Gate refuses a procedure whose class this build cannot safely run, naming
// what is missing. Refuse, do not warn: a warning on a powered procedure is a
// procedure that runs.
func (c SafetyClass) Gate(in GateInput) error {
	if !c.Valid() {
		return fmt.Errorf("unknown safety class %q", c)
	}
	if !in.Interactive {
		return fmt.Errorf("a %s calibration needs a person at the terminal: "+
			"every step of it is something only a human can do, so there is no "+
			"non-interactive mode. `calibrate status` and `calibrate clear` stay scriptable", c)
	}
	if c == ClassPoweredMotion && !in.MotionEntitlementAvailable {
		return errors.New("powered-motion calibrations are not available in this build: " +
			"they must be gated on the motion entitlement, and that entitlement does not " +
			"exist yet (WDY-3128). Gating in the CLI alone would be a gate any other gRPC " +
			"client walks straight past, so this refuses instead. " +
			"The nothing-powered and static-observation procedures are unaffected")
	}
	return nil
}
