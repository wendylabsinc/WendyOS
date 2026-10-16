package robotwizard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotcal"
)

// Deps is everything the procedure loop needs and nothing it can discover for
// itself. All of it is injected so the loop can be driven end to end in a test
// with no robot, no terminal and no device.
type Deps struct {
	Profile *robotcal.Profile
	Store   robotcal.Store
	Unit    string
	Prompt  Prompter
	// OpenJointSource resolves the backend the profile selected. nil means this
	// build can open none, which joint methods refuse on.
	OpenJointSource JointSourceOpener
	// Interactive is whether a person is at the terminal.
	Interactive bool
	// MotionEntitlementAvailable is whether the platform can enforce the motion
	// entitlement. False in every build today (WDY-3128).
	MotionEntitlementAvailable bool
	Now                        func() time.Time
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Outcome is what a method hands back: the one number the gate judges, plus
// whatever the method wants recorded alongside it.
type Outcome struct {
	// Residual is nil when nothing was measured — every step skipped, or a
	// procedure that collected no usable samples. A nil residual never
	// qualifies, and is not the same as a residual of zero.
	Residual *robotcal.Quantity
	Payload  json.RawMessage
	// Refusal, when set, is a failure the residual cannot excuse. A joint map
	// that does not match the profile is the case this exists for: the numbers
	// may be well inside budget and the calibration is still wrong, because it
	// was taken on joints that are not the ones the profile names.
	Refusal string
}

// Method is one of the fixed set, made runnable.
type Method interface {
	// Preconditions is checked before the operator is asked to do anything. It
	// refuses, naming what failed — it never warns and continues.
	Preconditions(ctx context.Context, env *Env) error
	// Steps are the units of work this procedure resumes at.
	Steps(env *Env) []string
	// Run performs the remaining steps and solves.
	Run(ctx context.Context, env *Env) (Outcome, error)
	// Explain renders a finished record for a person.
	Explain(rec robotcal.Record) string
}

// Env is what a method gets: the procedure it is running, the profile it is
// running against, the session it may resume, and the people-facing surfaces.
type Env struct {
	Deps
	Procedure  robotcal.Procedure
	Descriptor robotcal.Descriptor
	Class      robotcal.SafetyClass
	Session    *robotcal.Session
	// Source is opened by the engine when the method asked for one in its
	// preconditions.
	Source JointSource
}

// Checkpoint persists the session so an interrupted procedure resumes rather
// than restarts. Methods call it after every step, not at the end.
func (e *Env) Checkpoint(ctx context.Context) error {
	e.Session.UpdatedAt = e.now()
	return e.Store.PutSession(ctx, e.Unit, *e.Session)
}

// newMethod returns the runnable implementation of a method in the fixed set.
func newMethod(id robotcal.MethodID) (Method, error) {
	switch id {
	case robotcal.MethodJointRangeSweep:
		return &jointRangeSweep{}, nil
	default:
		desc, err := robotcal.Method(id)
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("calibration method %q: %s", id, desc.Unavailable)
	}
}

// Plan is what `calibrate status` shows and what `calibrate` picks from: every
// calibration the profile asks for, beside what the store has.
func Plan(ctx context.Context, d Deps) ([]robotcal.Status, robotcal.UnitRecord, error) {
	if d.Profile == nil {
		return nil, robotcal.UnitRecord{}, errors.New("no robot profile")
	}
	unit, err := d.Store.Load(ctx, d.Unit)
	if err != nil {
		return nil, robotcal.UnitRecord{}, err
	}
	if err := checkUnitProfile(unit, d.Profile.Kind); err != nil {
		return nil, unit, err
	}
	return robotcal.Statuses(d.Profile, unit), unit, nil
}

// checkUnitProfile refuses to mix records taken on one model with a run against
// another. Joint order is per model, so a record from a different profile is
// not a stale measurement — it is a measurement of different joints.
func checkUnitProfile(unit robotcal.UnitRecord, kind string) error {
	if unit.ProfileKind == "" || unit.ProfileKind == kind {
		return nil
	}
	return fmt.Errorf("unit %q holds calibrations taken as a %q, and this run is against a %q: "+
		"joint order and joint names are per model, so these are measurements of different joints. "+
		"Clear them (`wendy device robot calibrate clear <id> --unit %s`) or run against the profile they were taken with",
		unit.Unit, unit.ProfileKind, kind, unit.Unit)
}

// Run performs one calibration procedure end to end: preconditions, the
// announcement, the steps, the solve, the gate, the store.
//
// The ordering is the contract. Nothing physical is asked for until the
// preconditions have passed and the operator has been told what will happen,
// and nothing is qualified by having finished — only by fitting the budget.
//
// A measured outcome is returned with a nil error whatever its verdict: "we
// measured it and it does not fit" is a result, and the caller decides how to
// surface it. An error means the procedure did not produce one — a precondition
// refused, the operator stopped, or the result is refused outright because the
// joint map does not match the profile.
func Run(ctx context.Context, d Deps, procedureID string) (robotcal.Record, error) {
	var zero robotcal.Record
	if d.Profile == nil {
		return zero, errors.New("no robot profile: nothing says what this robot needs calibrated")
	}
	proc, err := d.Profile.Procedure(procedureID)
	if err != nil {
		return zero, err
	}
	desc, err := robotcal.Method(proc.Method)
	if err != nil {
		return zero, err
	}
	class, err := robotcal.Resolve(desc.ClassFloor, proc.Class)
	if err != nil {
		return zero, err
	}

	// Rule 7, and rule 6 inside it: refuse on precondition rather than warn,
	// and never offer a non-interactive path through a step only a person can
	// perform. This runs before the method is even constructed.
	if err := class.Gate(robotcal.GateInput{
		Interactive:                d.Interactive,
		MotionEntitlementAvailable: d.MotionEntitlementAvailable,
	}); err != nil {
		return zero, err
	}

	method, err := newMethod(proc.Method)
	if err != nil {
		return zero, err
	}

	unit, err := d.Store.Load(ctx, d.Unit)
	if err != nil {
		return zero, err
	}
	if err := checkUnitProfile(unit, d.Profile.Kind); err != nil {
		return zero, err
	}

	env := &Env{Deps: d, Procedure: proc, Descriptor: desc, Class: class}
	env.Session = resumeSession(unit, proc, d.Profile.Kind, d.now())

	if err := method.Preconditions(ctx, env); err != nil {
		return zero, fmt.Errorf("refusing to start %q: %w", proc.ID, err)
	}
	if env.Source != nil {
		defer env.Source.Close()
	}

	// Rule 1: every prompt states what will physically happen, before it
	// happens. This is the engine's job rather than each method's, so no method
	// can forget it.
	if err := announce(env); err != nil {
		return zero, err
	}

	// Rule 2: say what is being resumed, rather than silently skipping steps
	// the operator does not remember doing.
	if steps := method.Steps(env); env.Session.Progress(steps) > 0 {
		d.Prompt.Infof("Resuming: %d of %d steps already recorded (%d skipped).",
			env.Session.Progress(steps), len(steps), len(env.Session.Skipped))
	}
	if err := env.Checkpoint(ctx); err != nil {
		return zero, err
	}

	outcome, runErr := method.Run(ctx, env)
	if runErr != nil && !errors.Is(runErr, ErrAborted) {
		return zero, runErr
	}
	if errors.Is(runErr, ErrAborted) {
		// What was measured is already checkpointed; the session is what makes
		// the next run resume.
		return zero, fmt.Errorf("%w — %d steps recorded so far, rerun to continue",
			ErrAborted, env.Session.Progress(method.Steps(env)))
	}

	// Rules 4 and 5: the residual is the gate, and the method is recorded
	// because "measured by fiducial" and "measured by tape" are not the same
	// evidence.
	residual := outcome.Residual
	if outcome.Refusal != "" {
		// A refusal is not a large residual. Dropping the residual is what
		// keeps a well-inside-budget number from qualifying a calibration that
		// is wrong for a reason the budget cannot see.
		residual = nil
	}
	rec, verdict := robotcal.NewRecord(proc.ID, desc.Ref(), residual, proc.Budget, d.now(), outcome.Payload)

	if err := d.Store.PutRecord(ctx, d.Unit, rec); err != nil {
		return zero, err
	}
	if err := d.Store.SetProfileKind(ctx, d.Unit, d.Profile.Kind); err != nil {
		return zero, err
	}
	if err := d.Store.ClearSession(ctx, d.Unit, proc.ID); err != nil {
		return zero, err
	}

	d.Prompt.Announce(fmt.Sprintf("%s — %s", procTitle(proc), verdict), explainLines(method, rec, outcome, verdict)...)
	if outcome.Refusal != "" {
		return rec, fmt.Errorf("refusing to qualify %q: %s", proc.ID, outcome.Refusal)
	}
	return rec, nil
}

// explainLines renders the result for a person: the numbers the gate used, then
// whatever the method wants to add.
func explainLines(method Method, rec robotcal.Record, outcome Outcome, verdict robotcal.Verdict) []string {
	lines := []string{}
	if rec.Residual != nil {
		lines = append(lines, fmt.Sprintf("residual  %s", *rec.Residual))
	} else {
		lines = append(lines, "residual  not measured")
	}
	lines = append(lines, fmt.Sprintf("budget    %s", rec.Budget))
	lines = append(lines, fmt.Sprintf("method    %s", rec.Method))
	if outcome.Refusal != "" {
		lines = append(lines, "", "refused: "+outcome.Refusal)
	}
	if detail := method.Explain(rec); detail != "" {
		lines = append(lines, "", detail)
	}
	if verdict == robotcal.VerdictNotMeasured {
		lines = append(lines, "", "Nothing was measured, so nothing is qualified. A skipped step records "+
			"'not measured' rather than zero, on purpose.")
	}
	return lines
}

// resumeSession returns the checkpoint to carry on from, or a fresh one. A
// checkpoint from a different profile or a different method is discarded rather
// than resumed: it holds measurements of something else.
func resumeSession(unit robotcal.UnitRecord, proc robotcal.Procedure, kind string, now time.Time) *robotcal.Session {
	if s, ok := unit.Sessions[proc.ID]; ok && s.Method == proc.Method && s.ProfileKind == kind {
		return &s
	}
	return &robotcal.Session{
		ProcedureID: proc.ID,
		Method:      proc.Method,
		ProfileKind: kind,
		StartedAt:   now,
	}
}

// announce tells the operator what is about to physically happen and asks them
// to agree to it, before anything happens. What it says about the robot's state
// is quoted from the profile, because only the profile knows what "not powered"
// is called on this machine.
func announce(env *Env) error {
	lines := []string{}
	switch env.Class {
	case robotcal.ClassNothingPowered:
		lines = append(lines, "Nothing is powered during this procedure.")
		unpowered := env.Profile.Control.Unpowered
		if unpowered.Hazard != "" {
			lines = append(lines, "", "! "+unpowered.Hazard)
		}
		if unpowered.Instruction != "" {
			lines = append(lines, "", unpowered.Instruction)
		}
		if unpowered.Name != "" {
			lines = append(lines, "", fmt.Sprintf("Required state: %s", unpowered.Name))
		}
	case robotcal.ClassStaticObservation:
		lines = append(lines, "The robot holds still and is observed. Nothing is commanded.")
	case robotcal.ClassPoweredMotion:
		lines = append(lines, "! The robot will move under its own power.")
	}
	lines = append(lines,
		"",
		fmt.Sprintf("Safety class: %s", env.Class),
		fmt.Sprintf("Method:       %s", env.Descriptor.Ref()),
		fmt.Sprintf("Budget:       %s", env.Procedure.Budget),
	)
	if env.Source != nil {
		lines = append(lines, fmt.Sprintf("Joint source: %s", env.Source.Describe()))
	}
	env.Prompt.Announce(procTitle(env.Procedure), lines...)

	ok, err := env.Prompt.Confirm("Ready?")
	if err != nil {
		return err
	}
	if !ok {
		return ErrAborted
	}
	return nil
}

func procTitle(proc robotcal.Procedure) string {
	if proc.Title != "" {
		return proc.Title
	}
	return proc.ID
}
