package robotinspect

// Unknown is why a property has no value. The rule this enforces is that a property
// never carries a default in place of a measurement: an operator reading a number must
// be able to tell "nobody looked" from "we looked and it is this".
//
// Reason is machine-readable and part of the document's contract, so renaming one breaks
// any caller switching on it — the same discipline as the shared streamreason package.
type Unknown struct {
	Reason string `json:"reason"`
	Detail string `json:"detail,omitempty"`
}

// Reasons a property or a whole probe can come back empty.
const (
	// ReasonRequirementUnmet: the probe needs something this robot does not offer —
	// no ROS 2 graph, no CAN interface, no vendor backend.
	ReasonRequirementUnmet = "REQUIREMENT_UNMET"
	// ReasonSourceAbsent: the requirement was met but the artefact was missing, such as
	// a robot with a ROS 2 graph but no CameraInfo topic.
	ReasonSourceAbsent = "SOURCE_ABSENT"
	// ReasonNeverMeasured: a declaration exists and nothing has ever checked it. The
	// camera extrinsics case: read from a URDF, never validated against the built robot.
	ReasonNeverMeasured = "NEVER_MEASURED"
	// ReasonHeldByOther: another process owns the device. Reporting the holder is the
	// useful answer; failing is not.
	ReasonHeldByOther = "HELD_BY_OTHER"
	// ReasonProbeFailed: the probe ran and errored. Detail carries the error.
	ReasonProbeFailed = "PROBE_FAILED"
	// ReasonNotPassive: the only probe that could answer may actuate the robot, so
	// inspection will not run it.
	ReasonNotPassive = "NOT_PASSIVE"
)

// NewUnknown returns an Unknown with a reason and optional detail.
func NewUnknown(reason, detail string) Unknown {
	return Unknown{Reason: reason, Detail: detail}
}
