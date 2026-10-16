package robotcal

import (
	"fmt"
	"sort"
	"strings"
)

// MethodID names one of the calibration procedures WendyOS itself implements.
//
// The set is small, fixed, and closed. A profile selects a method and supplies
// its parameters; it never describes one. That is the whole defence against the
// profile becoming a declarative procedure language, which would leave WendyOS
// maintaining an interpreter. Anything the set does not cover stays an ordinary
// Wendy app and reports its result as a Record like everything else — that
// escape hatch is deliberate, and adding a fifth method here is a design
// conversation rather than a config change.
type MethodID string

const (
	// MethodJointRangeSweep: an operator moves each joint end to end by hand
	// and the platform records what it saw. Produces per-joint travel, a
	// direction sign, and the joint-map verification that falls out of the same
	// sweep for free.
	MethodJointRangeSweep MethodID = "joint-range-sweep"
	// MethodHomingOffset: each joint's zero, against a pose the operator puts
	// the robot into.
	MethodHomingOffset MethodID = "homing-offset"
	// MethodFiducialExtrinsics: a rigid transform from a sensor to a robot
	// frame, from a target whose geometry is known rather than measured by hand.
	MethodFiducialExtrinsics MethodID = "fiducial-extrinsics"
	// MethodHandEyeExtrinsics: the same transform, better conditioned, from the
	// robot moving the sensor or the target itself.
	MethodHandEyeExtrinsics MethodID = "hand-eye-extrinsics"
)

// Descriptor is what the platform knows about a method before anything runs:
// what it is called when recorded, the class it can never go below, and whether
// this build can actually perform it.
type Descriptor struct {
	ID MethodID
	// Version is recorded with every result, because trust differs between
	// revisions of the same procedure as much as between procedures.
	Version string
	// ClassFloor is the least dangerous this method can ever be. A profile may
	// raise it for a robot where the procedure is more dangerous; see Resolve.
	ClassFloor SafetyClass
	// Implemented is false for a method named by the fixed set that this build
	// cannot run. Listing it is not a promise that it works — Unavailable says
	// what is missing, and the procedure refuses rather than pretending.
	Implemented bool
	Unavailable string
	Summary     string
}

// Ref is how a method is written into a record: id/version, so a result can
// always be traced to the procedure that produced it.
func (d Descriptor) Ref() string { return string(d.ID) + "/" + d.Version }

// descriptors is the fixed set. Nothing outside this file adds to it.
var descriptors = map[MethodID]Descriptor{
	MethodJointRangeSweep: {
		ID:          MethodJointRangeSweep,
		Version:     "1.0",
		ClassFloor:  ClassNothingPowered,
		Implemented: true,
		Summary:     "Per-joint travel and direction, swept by hand, with the joint map verified",
	},
	MethodHomingOffset: {
		ID:         MethodHomingOffset,
		Version:    "1.0",
		ClassFloor: ClassNothingPowered,
		Unavailable: "not implemented yet: it needs a joint source that can also read a raw " +
			"encoder value, which no shipped backend exposes",
		Summary: "Each joint's zero, against a pose the operator puts the robot into",
	},
	MethodFiducialExtrinsics: {
		ID:         MethodFiducialExtrinsics,
		Version:    "1.0",
		ClassFloor: ClassStaticObservation,
		Unavailable: "not implemented yet: it needs fiducial detection and a sensor binding, " +
			"neither of which is in this change",
		Summary: "A rigid transform from a sensor to a robot frame, from a target of known geometry",
	},
	MethodHandEyeExtrinsics: {
		ID:         MethodHandEyeExtrinsics,
		Version:    "1.0",
		ClassFloor: ClassPoweredMotion,
		Unavailable: "not implemented yet, and it could not run if it were: a powered-motion " +
			"procedure must be gated on the motion entitlement, which does not exist (WDY-3128)",
		Summary: "The same transform, better conditioned, from the robot moving the sensor",
	},
}

// Method returns the descriptor for id, or an error listing the fixed set. The
// error is what a profile naming a method that does not exist gets, so it has
// to say what does.
func Method(id MethodID) (Descriptor, error) {
	d, ok := descriptors[id]
	if !ok {
		return Descriptor{}, fmt.Errorf("unknown calibration method %q: WendyOS implements %s, "+
			"and a profile selects one of them rather than describing a new one. "+
			"A procedure outside that set stays an ordinary Wendy app and reports its result as a calibration record",
			id, strings.Join(MethodIDs(), ", "))
	}
	return d, nil
}

// MethodIDs lists the fixed set, sorted, for help text and error messages.
func MethodIDs() []string {
	ids := make([]string, 0, len(descriptors))
	for id := range descriptors {
		ids = append(ids, string(id))
	}
	sort.Strings(ids)
	return ids
}

// Methods returns the fixed set in a stable order.
func Methods() []Descriptor {
	out := make([]Descriptor, 0, len(descriptors))
	for _, id := range MethodIDs() {
		out = append(out, descriptors[MethodID(id)])
	}
	return out
}
