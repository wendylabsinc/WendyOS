package robotinspect

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// stubProbe stands in for a transport-specific probe. Observe returns whatever the test
// hands it, so the core's plan, merge and verdict logic is exercised without a robot.
type stubProbe struct {
	id       string
	class    Class
	requires []Requirement
	provides []string
	found    []Property
	err      error
}

func (s stubProbe) ID() string              { return s.id }
func (s stubProbe) Class() Class            { return s.class }
func (s stubProbe) Requires() []Requirement { return s.requires }
func (s stubProbe) Provides() []string      { return s.provides }
func (s stubProbe) Observe(context.Context, *Env) ([]Property, error) {
	return s.found, s.err
}

func TestNewQuantityRequiresAnAxisForAngles(t *testing.T) {
	if _, err := NewQuantity(75, Degrees); err == nil {
		t.Fatal("an angle with no axis was accepted; that substitution is the bug this prevents")
	}
	q, err := NewQuantity(75, Degrees, WithAxis(AxisHorizontal))
	if err != nil {
		t.Fatalf("qualified angle rejected: %v", err)
	}
	if got, want := q.String(), "75 deg horizontal"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestNewQuantityRequiresAFrameForTranslations(t *testing.T) {
	if _, err := NewQuantity(510, Millimetres, WithAxis("z")); err == nil {
		t.Fatal("a translation with no frame was accepted")
	}
	if _, err := NewQuantity(510, Millimetres, WithAxis("z"), WithFrame("base_link")); err != nil {
		t.Errorf("qualified translation rejected: %v", err)
	}
}

func TestNewQuantityLeavesScalarUnitsUnqualified(t *testing.T) {
	if _, err := NewQuantity(43, Count); err != nil {
		t.Errorf("a count should need no axis: %v", err)
	}
	if _, err := NewQuantity(30, Hertz); err != nil {
		t.Errorf("a rate should need no axis: %v", err)
	}
}

func TestNewObservationRequiresSamplingForAMeasurement(t *testing.T) {
	q := MustQuantity(5, Hertz)
	source := Source{Probe: "stream-sample", Origin: "topic:/camera/color/image_raw"}

	if _, err := NewObservation(q, Measured, source); err == nil {
		t.Fatal("a measurement with no sampling window was accepted")
	}
	if _, err := NewObservation(q, Measured, source, WithSampling(0, 0)); err == nil {
		t.Fatal("a measurement with an empty sampling window was accepted")
	}
	if _, err := NewObservation(q, Measured, source, WithSampling(5*time.Second, 27)); err != nil {
		t.Errorf("sampled measurement rejected: %v", err)
	}
	// A declaration has no window to report, and must not be forced to invent one.
	if _, err := NewObservation(MustQuantity(30, Hertz), Declared, Source{Probe: "urdf", Origin: "datasheet"}); err != nil {
		t.Errorf("declaration rejected: %v", err)
	}
}

func TestNewObservationRequiresQuantityAndProvenance(t *testing.T) {
	source := Source{Probe: "urdf", Origin: "urdf:base.urdf"}
	if _, err := NewObservation(Quantity{}, Declared, source); err == nil {
		t.Error("an observation with no quantity was accepted")
	}
	if _, err := NewObservation(MustQuantity(1, Count), Declared, Source{Probe: "urdf"}); err == nil {
		t.Error("an observation with no origin was accepted")
	}
	if _, err := NewObservation(MustQuantity(1, Count), Kind("guessed"), source); err == nil {
		t.Error("an unknown observation kind was accepted")
	}
}

// The case that cost a day on the real robot: 75 deg was the declared horizontal field
// of view and 43.4 deg the measured vertical one. Both numbers are right. Reporting them
// as a disagreement invites exactly the axis substitution that produced the error, so the
// verdict has to say they are not the same measurement.
func TestAssessReportsIncomparableAcrossAxesRatherThanDisagreement(t *testing.T) {
	declared, err := NewObservation(
		MustQuantity(75, Degrees, WithAxis(AxisHorizontal)),
		Declared, Source{Probe: "camerainfo", Origin: "datasheet"})
	if err != nil {
		t.Fatal(err)
	}
	measured, err := NewObservation(
		MustQuantity(43.4, Degrees, WithAxis(AxisVertical)),
		Measured, Source{Probe: "checkerboard", Origin: "topic:/camera/color/camera_info"},
		WithSampling(time.Minute, 27), WithConditions(map[string]string{"resolution": "640x480"}))
	if err != nil {
		t.Fatal(err)
	}

	assessment := Property{ID: "camera.color.fov", Observations: []Observation{declared, measured}}.Assess()
	if assessment.Verdict != VerdictIncomparable {
		t.Fatalf("verdict = %q, want %q", assessment.Verdict, VerdictIncomparable)
	}
	for _, want := range []string{AxisHorizontal, AxisVertical, "declared", "measured"} {
		if !strings.Contains(assessment.Detail, want) {
			t.Errorf("detail %q does not name %q", assessment.Detail, want)
		}
	}
}

func TestAssessReportsDisagreementAndNamesTheConditions(t *testing.T) {
	declared, err := NewObservation(MustQuantity(30, Hertz), Declared,
		Source{Probe: "camerainfo", Origin: "datasheet"})
	if err != nil {
		t.Fatal(err)
	}
	measured, err := NewObservation(MustQuantity(5, Hertz), Measured,
		Source{Probe: "stream-sample", Origin: "topic:/camera/color/image_raw"},
		WithSampling(5*time.Second, 25), WithConditions(map[string]string{"resolution": "848x480"}))
	if err != nil {
		t.Fatal(err)
	}

	assessment := Property{ID: "camera.color.rate", Observations: []Observation{declared, measured}}.Assess()
	if assessment.Verdict != VerdictDisagree {
		t.Fatalf("verdict = %q, want %q", assessment.Verdict, VerdictDisagree)
	}
	if !strings.Contains(assessment.Detail, "848x480") {
		t.Errorf("detail %q does not name the conditions the measurement was taken under", assessment.Detail)
	}
}

func TestAssessAgreesInsideTheUnitTolerance(t *testing.T) {
	declared, _ := NewObservation(MustQuantity(500, Hertz), Declared, Source{Probe: "vendor-sdk", Origin: "datasheet"})
	measured, _ := NewObservation(MustQuantity(487, Hertz), Measured,
		Source{Probe: "topic-hz", Origin: "dds:rt/lowstate"}, WithSampling(5*time.Second, 2435))

	if got := (Property{ID: "control.rate", Observations: []Observation{declared, measured}}).Assess().Verdict; got != VerdictAgree {
		t.Errorf("verdict = %q, want %q (487 Hz is inside the 5%% rate tolerance of 500 Hz)", got, VerdictAgree)
	}
}

func TestAssessSingleObservationIsNotAPass(t *testing.T) {
	only, _ := NewObservation(MustQuantity(43, Count), Declared, Source{Probe: "urdf", Origin: "urdf:g1.urdf"})
	assessment := Property{ID: "joints.count", Observations: []Observation{only}}.Assess()
	if assessment.Verdict != VerdictSingle {
		t.Errorf("verdict = %q, want %q", assessment.Verdict, VerdictSingle)
	}
}

func TestAssessUnknownCarriesItsReason(t *testing.T) {
	unknown := NewUnknown(ReasonNeverMeasured, "read from the URDF, never checked against the built robot")
	assessment := Property{ID: "camera.color.extrinsics", Unknown: &unknown}.Assess()
	if assessment.Verdict != VerdictUnknown {
		t.Fatalf("verdict = %q, want %q", assessment.Verdict, VerdictUnknown)
	}
	if !strings.Contains(assessment.Detail, ReasonNeverMeasured) {
		t.Errorf("detail %q does not carry the reason code", assessment.Detail)
	}
}

func TestPassivePlanNeverRunsAProbeThatMayActuate(t *testing.T) {
	registry := NewRegistry()
	registry.MustRegister(stubProbe{id: "self-test", class: ClassMayActuate})
	registry.MustRegister(stubProbe{id: "dds-graph", class: ClassPassive})

	plan := registry.PassivePlan(NewEnv())
	for _, p := range plan.Run {
		if p.ID() == "self-test" {
			t.Fatal("a may-actuate probe was scheduled; read-only must hold by construction")
		}
	}
	if got := plan.Skipped["self-test"].Reason; got != ReasonNotPassive {
		t.Errorf("skip reason = %q, want %q", got, ReasonNotPassive)
	}
}

func TestPassivePlanSkipsProbesWhoseTransportIsAbsent(t *testing.T) {
	registry := NewRegistry()
	registry.MustRegister(stubProbe{
		id: "serial-servo", class: ClassPassive,
		requires: []Requirement{RequirementSerialPort},
	})
	registry.MustRegister(stubProbe{
		id: "ros2-control", class: ClassPassive,
		requires: []Requirement{RequirementROS2ControllerManager},
	})

	// An arm on a USB cable: serial is there, the ROS 2 graph is not.
	plan := registry.PassivePlan(NewEnv().Offer(RequirementSerialPort, "/dev/ttyUSB0"))
	if len(plan.Run) != 1 || plan.Run[0].ID() != "serial-servo" {
		t.Fatalf("planned %v, want only serial-servo", probeIDs(plan.Run))
	}
	skipped := plan.Skipped["ros2-control"]
	if skipped.Reason != ReasonRequirementUnmet {
		t.Errorf("skip reason = %q, want %q", skipped.Reason, ReasonRequirementUnmet)
	}
	if !strings.Contains(skipped.Detail, string(RequirementROS2ControllerManager)) {
		t.Errorf("skip detail %q does not name the missing requirement", skipped.Detail)
	}
}

func TestInspectFoldsDeclaredAndMeasuredIntoOneProperty(t *testing.T) {
	declared, _ := NewObservation(MustQuantity(120, Degrees, WithAxis("yaw")), Declared,
		Source{Probe: "urdf", Origin: "urdf:g1.urdf"})
	measured, _ := NewObservation(MustQuantity(86, Degrees, WithAxis("yaw")), Measured,
		Source{Probe: "vendor-sdk", Origin: "sdk:joint_limits"}, WithSampling(2*time.Second, 80))

	registry := NewRegistry()
	registry.MustRegister(stubProbe{
		id: "urdf", class: ClassPassive, provides: []string{"joint.waist_yaw.limit"},
		found: []Property{{ID: "joint.waist_yaw.limit", Observations: []Observation{declared}}},
	})
	registry.MustRegister(stubProbe{
		id: "vendor-sdk", class: ClassPassive, provides: []string{"joint.waist_yaw.limit"},
		found: []Property{{ID: "joint.waist_yaw.limit", Observations: []Observation{measured}}},
	})

	doc := Inspect(context.Background(), registry, NewEnv(), Target{Device: "g1", VendorKind: "unitree-g1"})
	if len(doc.Properties) != 1 {
		t.Fatalf("got %d properties, want the two probes folded into one", len(doc.Properties))
	}
	if got := len(doc.Properties[0].Observations); got != 2 {
		t.Fatalf("got %d observations, want 2", got)
	}
	// Only once both answers live on one property does the gate become a finding.
	if got := doc.Properties[0].Assess().Verdict; got != VerdictDisagree {
		t.Errorf("verdict = %q, want %q", got, VerdictDisagree)
	}
	if !doc.PassiveOnly {
		t.Error("PassiveOnly = false after a plan of passive probes")
	}
}

func TestInspectRecordsAWantedPropertyNoProbeCanAnswer(t *testing.T) {
	registry := NewRegistry()
	registry.MustRegister(stubProbe{id: "compute", class: ClassPassive, provides: []string{"compute.board"}})

	doc := Inspect(context.Background(), registry, NewEnv(), Target{
		Device: "so101",
		Want:   []string{"compute.board", "safety.estop"},
	})

	var estop *Property
	for i := range doc.Properties {
		if doc.Properties[i].ID == "safety.estop" {
			estop = &doc.Properties[i]
		}
	}
	if estop == nil {
		t.Fatal("a wanted property with no probe was omitted; it must be reported as unknown")
	}
	if got := estop.Assess().Verdict; got != VerdictUnknown {
		t.Errorf("verdict = %q, want %q", got, VerdictUnknown)
	}
}

func TestInspectKeepsPartialAnswersFromAFailingProbe(t *testing.T) {
	found, _ := NewObservation(MustQuantity(31, Count), Declared, Source{Probe: "dds-graph", Origin: "dds:domain0"})
	registry := NewRegistry()
	registry.MustRegister(stubProbe{
		id: "dds-graph", class: ClassPassive,
		found: []Property{{ID: "ros2.topics", Observations: []Observation{found}}},
		err:   errors.New("participant left mid-sweep"),
	})

	doc := Inspect(context.Background(), registry, NewEnv(), Target{Device: "go2"})
	if len(doc.Properties) != 1 {
		t.Fatalf("got %d properties, want the partial answer kept", len(doc.Properties))
	}
	if got := doc.Failed["dds-graph"].Reason; got != ReasonProbeFailed {
		t.Errorf("failure reason = %q, want %q", got, ReasonProbeFailed)
	}
	// It ran. Filing it as skipped would tell an operator the opposite of what
	// happened, and the report prints those under a heading that says "not run".
	if _, skipped := doc.Skipped["dds-graph"]; skipped {
		t.Error("a probe that ran and errored was recorded as never run")
	}
	if len(doc.ProbesRun) != 1 || doc.ProbesRun[0] != "dds-graph" {
		t.Errorf("ProbesRun = %v, want the probe that ran", doc.ProbesRun)
	}
}

func TestInspectOrdersPropertiesForAStableDiff(t *testing.T) {
	observation, _ := NewObservation(MustQuantity(1, Count), Declared, Source{Probe: "p", Origin: "o"})
	registry := NewRegistry()
	registry.MustRegister(stubProbe{
		id: "p", class: ClassPassive,
		found: []Property{
			{ID: "ros2.topics", Observations: []Observation{observation}},
			{ID: "camera.color.fov", Observations: []Observation{observation}},
			{ID: "joints.count", Observations: []Observation{observation}},
		},
	})

	doc := Inspect(context.Background(), registry, NewEnv(), Target{Device: "g1"})
	want := []string{"camera.color.fov", "joints.count", "ros2.topics"}
	for i, id := range want {
		if doc.Properties[i].ID != id {
			t.Fatalf("property %d = %q, want %q (documents are diffed, so order is part of the contract)", i, doc.Properties[i].ID, id)
		}
	}
}

// The measurement campaign on unitree-g1-nx-2 is the acceptance fixture for the core:
// its four known-true rows must come out as the right verdicts, with the two that were
// never measured reading unknown rather than wrong.
func TestSummariseReproducesTheG1Campaign(t *testing.T) {
	doc := Document{Schema: Schema, Device: "unitree-g1-nx-2", VendorKind: "unitree-g1", Properties: []Property{
		g1FieldOfView(t),
		g1FrameRate(t),
		g1WaistGate(t),
		g1Extrinsics(),
	}}

	summary := doc.Summarise()
	if summary.Disagree != 2 {
		t.Errorf("Disagree = %d, want 2 (frame rate and the waist gate)", summary.Disagree)
	}
	if summary.Incomparable != 1 {
		t.Errorf("Incomparable = %d, want 1 (the field of view, quoted on two axes)", summary.Incomparable)
	}
	if summary.Unknown != 1 {
		t.Errorf("Unknown = %d, want 1 (extrinsics were never measured)", summary.Unknown)
	}
	if got := summary.Findings(); got != 3 {
		t.Errorf("Findings = %d, want 3", got)
	}
}

func g1FieldOfView(t *testing.T) Property {
	t.Helper()
	declared, err := NewObservation(MustQuantity(75, Degrees, WithAxis(AxisHorizontal)), Declared,
		Source{Probe: "camerainfo", Origin: "datasheet"})
	if err != nil {
		t.Fatal(err)
	}
	measured, err := NewObservation(MustQuantity(43.4, Degrees, WithAxis(AxisVertical)), Measured,
		Source{Probe: "checkerboard", Origin: "topic:/camera/color/camera_info"},
		WithSampling(time.Minute, 27), WithConditions(map[string]string{"resolution": "640x480"}))
	if err != nil {
		t.Fatal(err)
	}
	return Property{ID: "camera.color.fov", Observations: []Observation{declared, measured}}
}

func g1FrameRate(t *testing.T) Property {
	t.Helper()
	declared, _ := NewObservation(MustQuantity(40, Hertz), Declared, Source{Probe: "camerainfo", Origin: "datasheet"})
	measured, err := NewObservation(MustQuantity(5, Hertz), Measured,
		Source{Probe: "stream-sample", Origin: "topic:/camera/color/image_raw"},
		WithSampling(5*time.Second, 25), WithConditions(map[string]string{"resolution": "848x480"}))
	if err != nil {
		t.Fatal(err)
	}
	return Property{ID: "camera.color.rate", Observations: []Observation{declared, measured}}
}

func g1WaistGate(t *testing.T) Property {
	t.Helper()
	declared, _ := NewObservation(MustQuantity(120, Degrees, WithAxis("yaw")), Declared,
		Source{Probe: "urdf", Origin: "urdf:g1.urdf"})
	measured, err := NewObservation(MustQuantity(86, Degrees, WithAxis("yaw")), Measured,
		Source{Probe: "vendor-sdk", Origin: "sdk:safety_gate"}, WithSampling(2*time.Second, 80))
	if err != nil {
		t.Fatal(err)
	}
	return Property{ID: "joint.waist_yaw.limit", Observations: []Observation{declared, measured}}
}

func g1Extrinsics() Property {
	unknown := NewUnknown(ReasonNeverMeasured, "read from the URDF, never checked against the built robot")
	return Property{ID: "camera.color.extrinsics", Unknown: &unknown}
}

func probeIDs(probes []Probe) []string {
	ids := make([]string, 0, len(probes))
	for _, p := range probes {
		ids = append(ids, p.ID())
	}
	return ids
}
