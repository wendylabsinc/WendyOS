package robotwizard

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotcal"
)

// testProfileYAML is a robot that is neither of the two WendyOS ships: three
// joints, encoder ticks, no declared vendor limits, and a per-joint travel
// floor. If the engine needed to know what robot it was driving, this profile
// would not work.
const testProfileYAML = `
kind: test-arm
control:
  transport: fake-bus
  unpowered:
    name: torque disabled
    instruction: Leave the arm limp so every joint is back-drivable by hand.
    hazard: A powered joint can drive against your hand.
joints:
  unit: tick
  source: {backend: fake}
  order: [a, b, c]
requires_calibration:
  - id: joint-range
    title: Joint range
    method: joint-range-sweep
    budget: {value: 10, unit: tick}
    params:
      min_travel: "1000"
      min_travel_per_joint: "c=300"
      min_samples: "4"
      sample_interval_ms: "1"
  - id: handeye
    method: hand-eye-extrinsics
    budget: {value: 0.01, unit: m}
`

type harness struct {
	deps   Deps
	prompt *fakePrompter
	source *fakeSource
	store  *robotcal.FileStore
}

func newHarness(t *testing.T, steps []step) *harness {
	t.Helper()
	profile, err := robotcal.ParseProfile([]byte(testProfileYAML))
	if err != nil {
		t.Fatalf("the test profile must be valid: %v", err)
	}
	source := newFakeSource(profile.Joints.Order)
	prompt := &fakePrompter{t: t, source: source, confirm: true, steps: steps}
	store := robotcal.NewFileStore(t.TempDir())
	h := &harness{prompt: prompt, source: source, store: store}
	h.deps = Deps{
		Profile:     profile,
		Store:       store,
		Unit:        "default",
		Prompt:      prompt,
		Interactive: true,
		OpenJointSource: func(context.Context, robotcal.Joints) (JointSource, error) {
			return source, nil
		},
	}
	return h
}

// fullSweep is the operator doing what they were asked, on every joint.
func fullSweep() []step {
	return []step{
		{sweeps: "a", min: -600, max: 600, samples: 8, action: StepDone},
		{sweeps: "b", min: -600, max: 600, samples: 8, action: StepDone},
		{sweeps: "c", min: -200, max: 200, samples: 8, action: StepDone},
	}
}

func TestWizardQualifiesAFullSweep(t *testing.T) {
	h := newHarness(t, fullSweep())
	rec, err := Run(context.Background(), h.deps, "joint-range")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rec.Qualified {
		t.Fatalf("a full sweep inside budget must qualify; verdict %q, residual %v", rec.Verdict(), rec.Residual)
	}
	if rec.Method != "joint-range-sweep/1.0" {
		t.Errorf("method = %q, want the method and its version recorded", rec.Method)
	}
	if rec.Residual == nil || rec.Residual.Unit != "tick" {
		t.Fatalf("residual = %v, want one in the robot's own unit", rec.Residual)
	}

	// Rule 1: the operator is told what will physically happen, in the robot's
	// own words, before being asked for anything.
	if !h.prompt.announcedContains("torque disabled") {
		t.Error("the announcement must quote the profile's name for the unpowered state")
	}
	if !h.prompt.announcedContains("Nothing is powered during this procedure.") {
		t.Error("a nothing-powered procedure must say so up front")
	}
	if !h.prompt.announcedContains("drive against your hand") {
		t.Error("the profile's hazard text must be shown before the operator acts")
	}

	// The record is on the robot, and the finished session is gone.
	stored, err := h.store.Load(context.Background(), "default")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stored.Calibrations["joint-range"]; !ok {
		t.Fatal("the result was not stored")
	}
	if _, ok := stored.Sessions["joint-range"]; ok {
		t.Error("a finished procedure must not leave a resumable session behind")
	}
	if stored.ProfileKind != "test-arm" {
		t.Errorf("profile kind = %q, want the unit to record what it was calibrated as", stored.ProfileKind)
	}
}

// TestSkippedJointRecordsNotMeasuredNeverZero is rule 3. "Did not touch" and
// "cannot tell" are different answers, and a skip that recorded a zero
// shortfall would be the best possible result.
func TestSkippedJointRecordsNotMeasuredNeverZero(t *testing.T) {
	steps := fullSweep()
	steps[1] = step{action: StepSkip}
	h := newHarness(t, steps)

	// The engine hands back a measured outcome and lets the caller decide what
	// to do with it; only a refusal or an abort is an error here.
	rec, err := Run(context.Background(), h.deps, "joint-range")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.Qualified {
		t.Fatal("a skipped joint must not qualify the procedure")
	}
	if rec.Residual != nil {
		t.Fatalf("residual = %v, want none at all: a skip is not a zero", rec.Residual)
	}
	if got := rec.Verdict(); got != robotcal.VerdictNotMeasured {
		t.Fatalf("verdict = %q, want %q", got, robotcal.VerdictNotMeasured)
	}

	var payload struct {
		Unmeasured []string `json:"unmeasured"`
		Joints     []struct {
			Name     string   `json:"name"`
			Measured bool     `json:"measured"`
			Travel   *float64 `json:"travel"`
		} `json:"joints"`
	}
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Unmeasured) != 1 || payload.Unmeasured[0] != "b" {
		t.Fatalf("unmeasured = %v, want exactly the skipped joint", payload.Unmeasured)
	}
	for _, j := range payload.Joints {
		if j.Name != "b" {
			continue
		}
		if j.Measured {
			t.Error("the skipped joint is recorded as measured")
		}
		if j.Travel != nil {
			t.Errorf("the skipped joint carries a travel of %v; it must carry none", *j.Travel)
		}
	}
}

// TestJointMapMismatchRefusesEvenWithinBudget is the worst failure the issue
// describes: a policy driving the wrong limb because the indices shifted. The
// numbers can be perfect and the calibration is still wrong.
func TestJointMapMismatchRefusesEvenWithinBudget(t *testing.T) {
	steps := fullSweep()
	// Asked for a, and b moves instead.
	steps[0] = step{sweeps: "b", min: -900, max: 900, samples: 8, action: StepDone}
	h := newHarness(t, steps)

	rec, err := Run(context.Background(), h.deps, "joint-range")
	if err == nil {
		t.Fatal("a joint-map mismatch must be a refusal")
	}
	if !strings.Contains(err.Error(), "joint map") {
		t.Fatalf("error = %v, want it to name the joint map", err)
	}
	if rec.Qualified {
		t.Fatal("a mismatched joint map must never qualify")
	}
	if rec.Residual != nil {
		t.Fatalf("residual = %v; a refusal must not leave a number the gate could pass", rec.Residual)
	}
	var payload struct {
		JointMap []struct {
			Asked string `json:"asked"`
			Moved string `json:"moved"`
		} `json:"joint_map_mismatches"`
	}
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.JointMap) == 0 || payload.JointMap[0].Asked != "a" || payload.JointMap[0].Moved != "b" {
		t.Fatalf("joint map report = %+v, want 'asked a, b moved'", payload.JointMap)
	}
}

// TestAJointMapMismatchSurvivesAResume is the crossing case, and the one that
// matters most: rule 2 (resumable) must not disarm the joint-map check.
//
// The failure it guards against: an arm whose servo ids were swapped during a
// bus repair. The operator sweeps the first joint, a different one moves, the
// wizard catches it — and then they stop part-way through and come back. If the
// refusal lived only in memory, the resumed run would qualify a robot whose
// joint map is wrong, and a policy would drive the wrong servo at full
// confidence. Each of the two behaviours is tested on its own elsewhere; only
// their intersection shows this.
func TestAJointMapMismatchSurvivesAResume(t *testing.T) {
	ctx := context.Background()

	// Run one: asked for a, b moves instead — then the operator stops at c.
	first := newHarness(t, []step{
		{sweeps: "b", min: -900, max: 900, samples: 8, action: StepDone},
		{sweeps: "b", min: -600, max: 600, samples: 8, action: StepDone},
		{action: StepAbort},
	})
	if _, err := Run(ctx, first.deps, "joint-range"); !errors.Is(err, ErrAborted) {
		t.Fatalf("err = %v, want ErrAborted", err)
	}

	// Run two sweeps only the joint that is left, correctly.
	second := newHarness(t, []step{
		{sweeps: "c", min: -200, max: 200, samples: 8, action: StepDone},
	})
	second.deps.Store = first.store
	rec, err := Run(ctx, second.deps, "joint-range")
	if err == nil {
		t.Fatal("the resumed run qualified a robot whose joint map does not match the profile")
	}
	if !strings.Contains(err.Error(), "joint map") {
		t.Fatalf("error = %v, want it to name the joint map", err)
	}
	if rec.Qualified {
		t.Fatal("a mismatch found before an interruption must still refuse after it")
	}
	if rec.Residual != nil {
		t.Fatalf("residual = %v; a refusal must not leave a number the gate could pass", rec.Residual)
	}
	var payload struct {
		JointMap []struct {
			Asked string `json:"asked"`
			Moved string `json:"moved"`
		} `json:"joint_map_mismatches"`
	}
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.JointMap) != 1 || payload.JointMap[0].Asked != "a" || payload.JointMap[0].Moved != "b" {
		t.Fatalf("joint map report = %+v, want the mismatch from before the interruption", payload.JointMap)
	}
}

// TestAnUnderSampledJointCanBeSweptAgain: a step that collected too few
// readings measured nothing, so it must not be checkpointed as answered. If it
// were, every rerun would skip past the joint that failed and the procedure
// would be pinned at NOT_MEASURED, with `calibrate clear` — which discards
// every other joint's work — the only way out.
func TestAnUnderSampledJointCanBeSweptAgain(t *testing.T) {
	ctx := context.Background()
	first := newHarness(t, []step{
		{sweeps: "a", min: -600, max: 600, samples: 8, action: StepDone},
		// One reading arrives before the operator answers: nothing measured.
		{sweeps: "b", min: -600, max: 600, samples: 1, action: StepDone},
		{action: StepAbort},
	})
	if _, err := Run(ctx, first.deps, "joint-range"); !errors.Is(err, ErrAborted) {
		t.Fatalf("err = %v, want ErrAborted", err)
	}
	stored, err := first.store.Load(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	session := stored.Sessions["joint-range"]
	if _, recorded := session.Steps["b"]; recorded {
		t.Fatal("an under-sampled joint was checkpointed as answered, so it can never be re-swept")
	}
	if session.WasSkipped("b") {
		t.Fatal("an under-sampled joint is not the same as one the operator skipped")
	}
	if _, recorded := session.Steps["a"]; !recorded {
		t.Fatal("the joint that was measured should still be checkpointed")
	}

	// The rerun asks for b again, and for c, and then qualifies.
	second := newHarness(t, []step{
		{sweeps: "b", min: -600, max: 600, samples: 8, action: StepDone},
		{sweeps: "c", min: -200, max: 200, samples: 8, action: StepDone},
	})
	second.deps.Store = first.store
	rec, err := Run(ctx, second.deps, "joint-range")
	if err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if second.prompt.at != 2 {
		t.Fatalf("the rerun asked for %d steps, want 2 — the under-sampled joint and the one never reached", second.prompt.at)
	}
	if !rec.Qualified {
		t.Fatalf("the re-swept joint should qualify the procedure; verdict %q", rec.Verdict())
	}
}

// TestJointMapMarginLetsALimpRobotPass is the judgement call made explicit:
// moving one joint by hand on a robot that hangs under gravity drags others
// through joint space, so a profile can say how much of that is expected.
func TestJointMapMarginLetsALimpRobotPass(t *testing.T) {
	// The operator sweeps the joint they were asked for, and a limp neighbour
	// flops slightly further as they do.
	dragged := func() []step {
		return []step{
			{sweeps: "a", min: -600, max: 600, also: "b", alsoMin: -680, alsoMax: 680, samples: 8, action: StepDone},
			{sweeps: "b", min: -600, max: 600, samples: 8, action: StepDone},
			{sweeps: "c", min: -200, max: 200, samples: 8, action: StepDone},
		}
	}

	// Strict by default: 1360 beats 1200, so this reads as a mismatch.
	strict := newHarness(t, dragged())
	if _, err := Run(context.Background(), strict.deps, "joint-range"); err == nil {
		t.Fatal("with no margin, a neighbour moving furthest is a mismatch")
	}

	// A profile whose robot hangs says how much of that to expect.
	lenient := newHarness(t, dragged())
	lenient.deps.Profile.RequiresCalibration[0].Params["joint_map_margin"] = "300"
	rec, err := Run(context.Background(), lenient.deps, "joint-range")
	if err != nil {
		t.Fatalf("a margin the profile declared should absorb this: %v", err)
	}
	if !rec.Qualified {
		t.Fatalf("verdict = %q, want the sweep to qualify", rec.Verdict())
	}
}

// TestAnInterruptedSweepResumes is rule 2. Two people and a gantry is not a
// thing to ask for twice.
func TestAnInterruptedSweepResumes(t *testing.T) {
	ctx := context.Background()
	first := newHarness(t, []step{
		{sweeps: "a", min: -600, max: 600, samples: 8, action: StepDone},
		{action: StepAbort},
	})
	if _, err := Run(ctx, first.deps, "joint-range"); !errors.Is(err, ErrAborted) {
		t.Fatalf("err = %v, want ErrAborted", err)
	}
	stored, err := first.store.Load(ctx, "default")
	if err != nil {
		t.Fatal(err)
	}
	session, ok := stored.Sessions["joint-range"]
	if !ok {
		t.Fatal("an aborted procedure must leave a checkpoint to resume from")
	}
	if _, done := session.Steps["a"]; !done {
		t.Fatalf("the finished joint was not checkpointed: %+v", session.Steps)
	}

	// The second run continues where the first stopped: it asks only for the
	// two joints that are left.
	second := newHarness(t, []step{
		{sweeps: "b", min: -600, max: 600, samples: 8, action: StepDone},
		{sweeps: "c", min: -200, max: 200, samples: 8, action: StepDone},
	})
	second.deps.Store = first.store
	rec, err := Run(ctx, second.deps, "joint-range")
	if err != nil {
		t.Fatalf("resumed run: %v", err)
	}
	if !rec.Qualified {
		t.Fatalf("the resumed sweep should qualify; verdict %q", rec.Verdict())
	}
	if second.prompt.at != 2 {
		t.Fatalf("the resumed run asked for %d steps, want 2 — the first joint was already measured", second.prompt.at)
	}
	var resumed bool
	for _, info := range second.prompt.infos {
		if strings.Contains(info, "Resuming") {
			resumed = true
		}
	}
	if !resumed {
		t.Error("a resumed run must say what it is skipping rather than silently jumping ahead")
	}
}

// TestShortfallOutsideBudgetDoesNotQualify: the residual is the gate, and
// finishing every step is not.
func TestShortfallOutsideBudgetDoesNotQualify(t *testing.T) {
	steps := fullSweep()
	steps[0] = step{sweeps: "a", min: -100, max: 100, samples: 8, action: StepDone} // 200 of 1000
	h := newHarness(t, steps)

	rec, err := Run(context.Background(), h.deps, "joint-range")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.Qualified {
		t.Fatal("every step was completed and the travel is 800 ticks short; completion is not the gate")
	}
	if rec.Residual == nil || rec.Residual.Value != 800 {
		t.Fatalf("residual = %v, want the worst shortfall (800 tick)", rec.Residual)
	}
	if got := rec.Verdict(); got != robotcal.VerdictOverBudget {
		t.Fatalf("verdict = %q, want %q", got, robotcal.VerdictOverBudget)
	}
}

// TestPerJointFloorLetsAGripperTravelLess: the SO-101's gripper travels far
// less than its arm joints, and that is a parameter rather than a second
// procedure.
func TestPerJointFloorLetsAGripperTravelLess(t *testing.T) {
	steps := fullSweep()
	// c only has to reach 300 (min_travel_per_joint), and does.
	steps[2] = step{sweeps: "c", min: -160, max: 160, samples: 8, action: StepDone}
	h := newHarness(t, steps)
	rec, err := Run(context.Background(), h.deps, "joint-range")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !rec.Qualified {
		t.Fatalf("320 ticks clears c's floor of 300; verdict %q, residual %v", rec.Verdict(), rec.Residual)
	}
}

func TestRefusalsHappenBeforeTheOperatorIsAskedForAnything(t *testing.T) {
	tests := []struct {
		name      string
		procedure string
		mutate    func(*harness)
		errSubstr string
	}{
		{
			// Rule 6.
			name:      "no person at the terminal",
			procedure: "joint-range",
			mutate:    func(h *harness) { h.deps.Interactive = false },
			errSubstr: "needs a person at the terminal",
		},
		{
			// The motion entitlement does not exist, so this cannot be gated
			// honestly and therefore is not offered.
			name:      "a powered-motion procedure",
			procedure: "handeye",
			errSubstr: "motion entitlement",
		},
		{
			name:      "a calibration this robot does not declare",
			procedure: "wheel-odometry",
			errSubstr: "has no calibration",
		},
		{
			name:      "a backend this build cannot open",
			procedure: "joint-range",
			mutate: func(h *harness) {
				h.deps.OpenJointSource = func(_ context.Context, joints robotcal.Joints) (JointSource, error) {
					return nil, &UnsupportedBackendError{Backend: joints.Source.Backend, Available: []string{"ros2-joint-states"}}
				}
			},
			errSubstr: "no joint source backend",
		},
		{
			name:      "a robot that does not report the joints the profile names",
			procedure: "joint-range",
			mutate: func(h *harness) {
				h.source.rest = map[string]float64{"a": 0, "b": 0}
				h.source.order = []string{"a", "b"}
			},
			errSubstr: "does not report 1 of the joints",
		},
		{
			name:      "a budget in a unit the robot does not report",
			procedure: "joint-range",
			mutate: func(h *harness) {
				h.deps.Profile.RequiresCalibration[0].Budget = robotcal.Quantity{Value: 1, Unit: "rad"}
			},
			errSubstr: "can only be judged against a budget in the same unit",
		},
		{
			name:      "a parameter the method does not define",
			procedure: "joint-range",
			mutate: func(h *harness) {
				h.deps.Profile.RequiresCalibration[0].Params["then_do"] = "a backflip"
			},
			errSubstr: "unknown parameter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, fullSweep())
			if tt.mutate != nil {
				tt.mutate(h)
			}
			_, err := Run(context.Background(), h.deps, tt.procedure)
			if err == nil {
				t.Fatalf("want a refusal mentioning %q, got none", tt.errSubstr)
			}
			if !strings.Contains(err.Error(), tt.errSubstr) {
				t.Fatalf("error = %v, want it to mention %q", err, tt.errSubstr)
			}
			if h.prompt.at != 0 {
				t.Errorf("the operator was asked to do %d things before the refusal; "+
					"preconditions are checked before anything physical is requested", h.prompt.at)
			}
		})
	}
}

// held is a joint something is driving, as a source that can tell reports it.
func held(reason, remedy string) JointStatus {
	return JointStatus{Mobility: MobilityHeld, HoldReason: reason, HoldRemedy: remedy}
}

// free is a joint a source has established nobody is driving.
func free() JointStatus { return JointStatus{Mobility: MobilityFree} }

// namesJoint reports whether a message names this joint as a word of its own.
func namesJoint(msg, joint string) bool {
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(joint) + `\b`).MatchString(msg)
}

// TestASweptJointMustBeFreeToMove is the five minutes this check exists to
// save. On unitree-g1-nx-2 an operator moved seven joints end to end against
// arm motors that were energised the whole time; the encoders correctly
// reported 0.0003 rad of travel and the procedure refused for want of travel —
// after the sweep rather than before it.
//
// The refusal must name the joints, must come before the operator is asked for
// anything (rule 7 and rule 1), and must stay distinct from the joint the robot
// does not have at all.
func TestASweptJointMustBeFreeToMove(t *testing.T) {
	const (
		energised = "still energised (motor mode 1)"
		remedy    = "Put the robot into a true zero-torque state — not merely damping — and run this again."
	)

	tests := []struct {
		name string
		// status is what the robot says about each of its joints.
		status map[string]JointStatus
		// reports, when set, replaces the joints the robot reports at all.
		reports   []string
		errSubstr string
		// wantNamed and wantUnnamed are joints the refusal must and must not
		// name, which is what keeps a mixed robot from being described as
		// entirely stuck.
		wantNamed   []string
		wantUnnamed []string
	}{
		{
			name:   "a source that cannot tell sweeps as it always did",
			status: nil,
		},
		{
			name:   "every joint free",
			status: map[string]JointStatus{"a": free(), "b": free(), "c": free()},
		},
		{
			name:        "every joint energised",
			status:      map[string]JointStatus{"a": held(energised, remedy), "b": held(energised, remedy), "c": held(energised, remedy)},
			errSubstr:   "a, b, c are still energised (motor mode 1) and will resist you, reporting no travel. " + remedy,
			wantNamed:   []string{"a", "b", "c"},
			wantUnnamed: nil,
		},
		{
			// The mixed case: one motor left energised among limp ones is the
			// likeliest way this happens, and naming all three would send the
			// operator looking at the wrong two.
			name:        "one energised joint among free ones",
			status:      map[string]JointStatus{"a": free(), "b": held(energised, remedy), "c": free()},
			errSubstr:   "b is still energised (motor mode 1) and will resist you",
			wantNamed:   []string{"b"},
			wantUnnamed: []string{"a", "c"},
		},
		{
			// Two joints, two reasons: each keeps its own words rather than
			// being flattened into whichever was seen first.
			name: "joints held for different reasons",
			status: map[string]JointStatus{
				"a": held(energised, remedy),
				"b": free(),
				"c": held("under a parking brake", "Release the brake."),
			},
			errSubstr:   "a is still energised (motor mode 1); c is under a parking brake and will resist you",
			wantNamed:   []string{"a", "c"},
			wantUnnamed: []string{"b"},
		},
		{
			// A joint that is not fitted is a different failure with its own
			// refusal, and it keeps it. Telling this operator to power
			// something down would send them after a motor that is not there.
			name:        "a joint the robot does not have",
			reports:     []string{"a", "b"},
			status:      map[string]JointStatus{"a": free(), "b": free()},
			errSubstr:   "does not report 1 of the joints",
			wantUnnamed: []string{"energised"},
		},
		{
			// Both at once: the profile names a joint this robot does not have
			// *and* the ones it does have are energised. The absent joint is
			// the one that cannot be measured at all, so it is what the
			// operator hears about first.
			name:      "an absent joint outranks an energised one",
			reports:   []string{"a", "b"},
			status:    map[string]JointStatus{"a": held(energised, remedy), "b": free()},
			errSubstr: "does not report 1 of the joints",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, fullSweep())
			h.source.status = tt.status
			if tt.reports != nil {
				h.source.rest = map[string]float64{}
				for _, j := range tt.reports {
					h.source.rest[j] = 0
				}
				h.source.order = tt.reports
			}

			_, err := Run(context.Background(), h.deps, "joint-range")
			if tt.errSubstr == "" {
				if err != nil {
					t.Fatalf("Run: %v — a robot whose joints are free must sweep", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want a refusal mentioning %q, got none", tt.errSubstr)
			}
			if !strings.Contains(err.Error(), tt.errSubstr) {
				t.Fatalf("error = %v, want it to contain %q", err, tt.errSubstr)
			}
			// Which joints were named is read off the clause that names them,
			// not off the whole message: the remedy is a sentence of English
			// and this robot's joints are called a, b and c.
			named, _, _ := strings.Cut(err.Error(), " and will resist you")
			for _, joint := range tt.wantNamed {
				if !namesJoint(named, joint) {
					t.Errorf("error = %v, want it to name %q", err, joint)
				}
			}
			for _, joint := range tt.wantUnnamed {
				if namesJoint(named, joint) {
					t.Errorf("error = %v, must not name %q", err, joint)
				}
			}
			// The point of the whole check: nothing was asked of the operator,
			// and the announcement to support a limp robot never happened.
			if h.prompt.at != 0 {
				t.Errorf("the operator was asked to move %d joints before the refusal; "+
					"this check exists precisely to happen first", h.prompt.at)
			}
			if h.prompt.announcedContains("Nothing is powered during this procedure.") {
				t.Error("the safety announcement was made before the refusal; a precondition " +
					"refusal must land before the operator is told to support the robot")
			}
		})
	}
}

// TestASweptJointCarriesWhatTheMotorSaid: the three numbers that distinguish
// not-fitted from energised from free are recorded with the sweep, because
// reconstructing them afterwards is what cost an hour of live-robot time.
func TestASweptJointCarriesWhatTheMotorSaid(t *testing.T) {
	h := newHarness(t, fullSweep())
	volts := 48.5
	h.source.status = map[string]JointStatus{
		"a": {
			Mobility:      MobilityFree,
			TemperaturesC: []float64{49, 48},
			Volts:         &volts,
			Vendor:        map[string]string{"mode": "0"},
		},
		"b": free(),
		"c": free(),
	}

	rec, err := Run(context.Background(), h.deps, "joint-range")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var payload struct {
		Joints []struct {
			Name   string       `json:"name"`
			Status *JointStatus `json:"status"`
		} `json:"joints"`
	}
	if err := json.Unmarshal(rec.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	var got *JointStatus
	for _, j := range payload.Joints {
		if j.Name == "a" {
			got = j.Status
		}
	}
	if got == nil {
		t.Fatal("the stored record carries nothing about what joint a was doing while it was swept")
	}
	if got.Mobility != MobilityFree {
		t.Errorf("mobility = %v, want it recorded as free", got.Mobility)
	}
	if got.Volts == nil || *got.Volts != volts {
		t.Errorf("volts = %v, want %v", got.Volts, volts)
	}
	if len(got.TemperaturesC) != 2 {
		t.Errorf("temperatures = %v, want both sensors", got.TemperaturesC)
	}
	if got.Vendor["mode"] != "0" {
		t.Errorf("vendor = %v, want the robot's own mode word carried verbatim", got.Vendor)
	}
	// And the operator saw it while they were standing at the robot.
	var shown bool
	for _, line := range h.prompt.infos {
		if strings.Contains(line, "48.5 V") {
			shown = true
		}
	}
	if !shown {
		t.Error("the joint's condition was recorded but never shown")
	}
}

func TestDecliningTheAnnouncementStopsBeforeAnythingHappens(t *testing.T) {
	h := newHarness(t, fullSweep())
	h.prompt.confirm = false
	if _, err := Run(context.Background(), h.deps, "joint-range"); !errors.Is(err, ErrAborted) {
		t.Fatalf("err = %v, want ErrAborted", err)
	}
	if h.prompt.at != 0 {
		t.Error("saying no to the announcement must not run any step")
	}
}

// TestRunningAgainstADifferentModelIsRefused: joint order is per model, so a
// record from another profile is a measurement of different joints, not a stale
// one.
func TestRunningAgainstADifferentModelIsRefused(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, fullSweep())
	if err := h.store.SetProfileKind(ctx, "default", "some-other-robot"); err != nil {
		t.Fatal(err)
	}
	_, err := Run(ctx, h.deps, "joint-range")
	if err == nil || !strings.Contains(err.Error(), "different joints") {
		t.Fatalf("error = %v, want a refusal about mixing models", err)
	}
}

func TestPlanListsEveryDeclaredCalibration(t *testing.T) {
	h := newHarness(t, nil)
	statuses, _, err := Plan(context.Background(), h.deps)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 {
		t.Fatalf("statuses = %d, want one per declared calibration", len(statuses))
	}
	byID := map[string]robotcal.Status{}
	for _, st := range statuses {
		byID[st.ID] = st
	}
	if !byID["joint-range"].Runnable {
		t.Error("joint-range-sweep is implemented and should be runnable")
	}
	if byID["handeye"].Runnable {
		t.Error("hand-eye is not implemented and must not be offered as runnable")
	}
	if byID["handeye"].Reason == "" {
		t.Error("a procedure that cannot be run must say why")
	}
	if byID["joint-range"].Class != string(robotcal.ClassNothingPowered) {
		t.Errorf("class = %q, want nothing-powered", byID["joint-range"].Class)
	}
}
