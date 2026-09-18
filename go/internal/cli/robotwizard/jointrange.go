package robotwizard

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/robotcal"
)

// jointRangeSweep is the nothing-powered procedure: an operator moves each
// joint end to end by hand and the platform records what it saw.
//
// It is the cheapest useful calibration on any robot, and it is the one that
// pays for the joint-map check. A policy driving joints by index needs index 31
// to be the joint it was during training; a hand replaced or a firmware
// revision that reorders the array makes it drive the wrong limb at full
// confidence. Asking the operator to move one named joint and watching which
// one moves is the entire defence, and it comes out of a sweep that was being
// done anyway. No torque, no entitlement, no risk.
type jointRangeSweep struct {
	params jointRangeParams
	joints []string
	// results are the per-joint outcomes, rebuilt from the session on resume so
	// an interrupted sweep is indistinguishable from an uninterrupted one.
	results map[string]jointResult
	// mismatches are joints where a different joint moved than the one asked
	// for.
	mismatches  []jointMapMismatch
	orderNote   string
	sourceOrder []string
	explain     string
}

// jointRangeParams are this method's own parameters. A profile supplies them;
// it does not get to add new ones, and an unknown key is a refusal rather than
// a silently ignored line — that is the boundary between parameterising a
// method and describing one.
type jointRangeParams struct {
	// MinTravel is how far a joint must move for the sweep to count, in the
	// profile's joint unit. It is only consulted for joints the profile
	// declares no vendor limits for: on a robot whose travel is a datasheet
	// fact the declared span is the requirement, and on one whose travel is
	// measured per unit (the SO-101) there is nothing else to compare against.
	MinTravel float64
	// MinTravelPerJoint overrides it for joints that legitimately travel less.
	// The SO-101's gripper is the case: 300 ticks against the arm's 1000.
	MinTravelPerJoint map[string]float64
	// MinSamples is how many readings a step must collect before its extremes
	// mean anything. Two readings can span a wide range and have measured
	// nothing.
	MinSamples int
	// SampleInterval is how often the source is polled while the operator moves
	// a joint.
	SampleInterval time.Duration
}

const (
	defaultMinSamples     = 20
	defaultSampleInterval = 20 * time.Millisecond
)

func parseJointRangeParams(in map[string]string) (jointRangeParams, error) {
	p := jointRangeParams{MinSamples: defaultMinSamples, SampleInterval: defaultSampleInterval}
	for key, raw := range in {
		var err error
		switch key {
		case "min_travel":
			p.MinTravel, err = strconv.ParseFloat(raw, 64)
		case "min_travel_per_joint":
			p.MinTravelPerJoint, err = parseJointFloats(raw)
		case "min_samples":
			p.MinSamples, err = strconv.Atoi(raw)
		case "sample_interval_ms":
			var ms int
			if ms, err = strconv.Atoi(raw); err == nil {
				p.SampleInterval = time.Duration(ms) * time.Millisecond
			}
		default:
			return p, fmt.Errorf("unknown parameter %q for method %s: it takes min_travel, "+
				"min_travel_per_joint, min_samples and sample_interval_ms. A profile parameterises a "+
				"method; it does not describe one", key, robotcal.MethodJointRangeSweep)
		}
		if err != nil {
			return p, fmt.Errorf("parameter %q for method %s: %w", key, robotcal.MethodJointRangeSweep, err)
		}
	}
	if p.MinSamples < 2 {
		p.MinSamples = 2
	}
	if p.SampleInterval <= 0 {
		p.SampleInterval = defaultSampleInterval
	}
	return p, nil
}

// parseJointFloats decodes "gripper=300,wrist_roll=500".
func parseJointFloats(raw string) (map[string]float64, error) {
	out := make(map[string]float64)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("expected joint=value pairs, got %q", part)
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return nil, fmt.Errorf("value for %q: %w", name, err)
		}
		out[strings.TrimSpace(name)] = v
	}
	return out, nil
}

// jointResult is one joint's outcome. Measured is explicit because a skipped
// joint must never be storable as a zero-travel one.
type jointResult struct {
	Name     string `json:"name"`
	Index    int    `json:"index"`
	Measured bool   `json:"measured"`
	// SkipReason is why a joint has no numbers: the operator skipped it.
	SkipReason    string   `json:"skip_reason,omitempty"`
	Min           *float64 `json:"min,omitempty"`
	Max           *float64 `json:"max,omitempty"`
	Travel        *float64 `json:"travel,omitempty"`
	Required      *float64 `json:"required,omitempty"`
	Shortfall     *float64 `json:"shortfall,omitempty"`
	DirectionSign int      `json:"direction_sign"`
	Samples       int      `json:"samples"`
	// RequiredFrom says where the requirement came from, because a shortfall
	// against a vendor datasheet and one against a swept-far-enough floor are
	// different claims.
	RequiredFrom string `json:"required_from,omitempty"`
}

type jointMapMismatch struct {
	Asked       string  `json:"asked"`
	AskedIndex  int     `json:"asked_index"`
	Moved       string  `json:"moved"`
	MovedIndex  int     `json:"moved_index"`
	AskedTravel float64 `json:"asked_travel"`
	MovedTravel float64 `json:"moved_travel"`
}

type jointRangePayload struct {
	Unit   string        `json:"unit"`
	Joints []jointResult `json:"joints"`
	// SourceOrder is the order the joint source reported names in. It is
	// recorded rather than enforced: a driver's publish order is not
	// necessarily the command order, so a difference is worth seeing and is not
	// by itself a fault. A joint the source does not report at all is a
	// different matter, and is refused before the sweep starts.
	SourceOrder []string `json:"source_order,omitempty"`
	OrderNote   string   `json:"order_note,omitempty"`
	// Unmeasured names the joints with no numbers — skipped, never reached, or
	// too few readings. It is the reason a procedure can finish and still
	// qualify nothing.
	Unmeasured   []string           `json:"unmeasured,omitempty"`
	JointMap     []jointMapMismatch `json:"joint_map_mismatches,omitempty"`
	JointMapNote string             `json:"joint_map_note,omitempty"`
}

func (m *jointRangeSweep) Steps(env *Env) []string {
	if m.joints == nil {
		m.joints = env.Profile.ProcedureJoints(env.Procedure)
	}
	return m.joints
}

// Preconditions refuses before the operator is asked to touch anything: a
// budget in the wrong unit, a joint with nothing to compare against, a source
// that cannot be opened, or a robot that does not report the joints the profile
// names.
func (m *jointRangeSweep) Preconditions(ctx context.Context, env *Env) error {
	params, err := parseJointRangeParams(env.Procedure.Params)
	if err != nil {
		return err
	}
	m.params = params
	m.results = make(map[string]jointResult)
	m.joints = env.Profile.ProcedureJoints(env.Procedure)
	if len(m.joints) == 0 {
		return fmt.Errorf("the procedure covers no joints")
	}

	unit := env.Profile.Joints.Unit
	if env.Procedure.Budget.Unit != unit {
		return fmt.Errorf("the budget is in %q and this robot reports joints in %q: "+
			"a travel shortfall can only be judged against a budget in the same unit",
			env.Procedure.Budget.Unit, unit)
	}
	if env.Procedure.Budget.Axis != "" || env.Procedure.Budget.Frame != "" {
		return fmt.Errorf("the budget is qualified by an axis or frame, and a joint travel " +
			"shortfall has neither")
	}

	// Every joint must have something to be measured against, or a sweep of it
	// could only ever report "it moved".
	for _, j := range m.joints {
		if _, _, err := m.requirement(env, j); err != nil {
			return err
		}
	}

	if env.OpenJointSource == nil {
		return fmt.Errorf("this build can open no joint sources, so nothing can read this robot's joints")
	}
	source, err := env.OpenJointSource(ctx, env.Profile.Joints.Source)
	if err != nil {
		return err
	}
	reading, err := source.Read(ctx)
	if err != nil {
		source.Close()
		return fmt.Errorf("reading joints from %s: %w", source.Describe(), err)
	}
	var missing []string
	for _, j := range m.joints {
		if _, ok := reading.Positions[j]; !ok {
			missing = append(missing, j)
		}
	}
	if len(missing) > 0 {
		source.Close()
		sort.Strings(missing)
		return fmt.Errorf("%s does not report %d of the joints this profile names (%s): "+
			"the profile and the robot disagree about what joints exist, which is not something a "+
			"sweep can measure past", source.Describe(), len(missing), strings.Join(missing, ", "))
	}
	env.Source = source

	m.noteSourceOrder(env, reading.Order)
	m.restore(env)
	return nil
}

// noteSourceOrder records how the source's reported order compares with the
// profile's canonical one.
func (m *jointRangeSweep) noteSourceOrder(env *Env, order []string) {
	m.sourceOrder = order
	if len(order) == 0 {
		m.orderNote = "the joint source reports no joint order, so only the per-joint check applies"
		return
	}
	var differs []string
	for _, j := range m.joints {
		want := env.Profile.JointIndex(j)
		got := indexOf(order, j)
		if want != got {
			differs = append(differs, fmt.Sprintf("%s: profile index %d, source index %d", j, want, got))
		}
	}
	if len(differs) > 0 {
		m.orderNote = "the source reports these joints at different indices than the profile's command " +
			"order (" + strings.Join(differs, "; ") + "). A driver's publish order need not be the " +
			"command order, so this is reported rather than refused — but if a policy indexes the " +
			"command array, check which of the two it was trained against"
	}
}

func indexOf(names []string, want string) int {
	for i, n := range names {
		if n == want {
			return i
		}
	}
	return -1
}

// restore rebuilds already-measured joints from the session, so a resumed sweep
// solves over everything rather than only what this run collected.
func (m *jointRangeSweep) restore(env *Env) {
	for _, j := range m.joints {
		if env.Session.WasSkipped(j) {
			m.results[j] = jointResult{
				Name:       j,
				Index:      env.Profile.JointIndex(j),
				Measured:   false,
				SkipReason: "skipped by the operator",
			}
			continue
		}
		raw, ok := env.Session.Steps[j]
		if !ok {
			continue
		}
		var r jointResult
		if err := json.Unmarshal(raw, &r); err == nil {
			m.results[j] = r
		}
	}
}

// requirement returns how far a joint has to travel, and where that number came
// from.
func (m *jointRangeSweep) requirement(env *Env, joint string) (float64, string, error) {
	if span, ok := env.Profile.Joints.Declared[joint]; ok && span.Travel() > 0 {
		return span.Travel(), "declared", nil
	}
	if v, ok := m.params.MinTravelPerJoint[joint]; ok && v > 0 {
		return v, "min_travel_per_joint", nil
	}
	if m.params.MinTravel > 0 {
		return m.params.MinTravel, "min_travel", nil
	}
	return 0, "", fmt.Errorf("joint %q has no declared travel in the profile and the procedure sets no "+
		"min_travel for it: there would be nothing to judge the sweep against, and 'the joint moved' is "+
		"not a calibration", joint)
}

// Run sweeps each joint the operator has not already answered for, checkpoints
// after every one, and solves over all of them.
func (m *jointRangeSweep) Run(ctx context.Context, env *Env) (Outcome, error) {
	for step, joint := range m.joints {
		if env.Session.Done(joint) {
			continue
		}
		idx := env.Profile.JointIndex(joint)
		env.Prompt.Announce(fmt.Sprintf("%d/%d  %s (index %d)", step+1, len(m.joints), joint, idx),
			"Move it slowly to one extreme, hold, then to the other.")

		window, action, err := m.sweepOne(ctx, env, joint)
		if err != nil {
			return Outcome{}, err
		}
		switch action {
		case StepAbort:
			return Outcome{}, ErrAborted
		case StepSkip:
			// Rule 3: a skip records "not measured", never a zero.
			env.Session.Skip(joint)
			m.results[joint] = jointResult{
				Name: joint, Index: idx, Measured: false,
				SkipReason: "skipped by the operator",
			}
			env.Prompt.Infof("not measured")
			if err := env.Checkpoint(ctx); err != nil {
				return Outcome{}, err
			}
			continue
		}

		result, err := m.solveOne(env, joint, idx, window)
		if err != nil {
			return Outcome{}, err
		}
		m.results[joint] = result
		encoded, err := json.Marshal(result)
		if err != nil {
			return Outcome{}, err
		}
		env.Session.Record(joint, encoded)
		if err := env.Checkpoint(ctx); err != nil {
			return Outcome{}, err
		}
		m.reportOne(env, result)
	}
	return m.solve(env)
}

// sweepOne samples the source for as long as the operator is moving the joint.
// The sampling runs beside the prompt rather than after it, because the thing
// being measured happens while they are moving it.
func (m *jointRangeSweep) sweepOne(ctx context.Context, env *Env, joint string) (*SampleWindow, StepAction, error) {
	window := NewSampleWindow()
	var mu sync.Mutex
	sampleCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	var sampleErr error

	go func() {
		defer close(done)
		ticker := time.NewTicker(m.params.SampleInterval)
		defer ticker.Stop()
		for {
			reading, err := env.Source.Read(sampleCtx)
			if err != nil {
				if sampleCtx.Err() == nil {
					mu.Lock()
					sampleErr = err
					mu.Unlock()
				}
				return
			}
			mu.Lock()
			window.Add(reading)
			mu.Unlock()
			select {
			case <-sampleCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	action, err := env.Prompt.Step(fmt.Sprintf("Press enter when %s has been moved end to end.", joint))
	stop()
	<-done
	if err != nil {
		return nil, StepAbort, err
	}
	mu.Lock()
	defer mu.Unlock()
	if sampleErr != nil {
		return nil, StepAbort, fmt.Errorf("reading joints from %s: %w", env.Source.Describe(), sampleErr)
	}
	return window, action, nil
}

// solveOne turns one joint's window into a result, and records a joint-map
// mismatch when a different joint moved than the one asked for.
func (m *jointRangeSweep) solveOne(env *Env, joint string, idx int, window *SampleWindow) (jointResult, error) {
	required, from, err := m.requirement(env, joint)
	if err != nil {
		return jointResult{}, err
	}
	result := jointResult{
		Name: joint, Index: idx,
		Samples:      window.Samples(),
		Required:     &required,
		RequiredFrom: from,
	}
	span, ok := window.Span(joint)
	if !ok || window.Samples() < m.params.MinSamples {
		// Too few readings is not a travel of zero: nothing was measured.
		result.Measured = false
		result.Required = &required
		result.RequiredFrom = from
		result.SkipReason = fmt.Sprintf("only %d readings arrived, %d are needed before extremes mean anything",
			window.Samples(), m.params.MinSamples)
		return result, nil
	}
	travel := span.Travel()
	shortfall := required - travel
	if shortfall < 0 {
		shortfall = 0
	}
	result.Measured = true
	result.SkipReason = ""
	result.Min, result.Max, result.Travel, result.Shortfall = &span.Min, &span.Max, &travel, &shortfall
	result.DirectionSign = window.DirectionSign(joint)

	// The joint-map check, free from the same sweep: the operator was asked to
	// move exactly one joint, so that joint should be the one that moved most.
	if moved, movedTravel := window.MostMoved(); moved != "" && moved != joint && movedTravel > travel {
		m.mismatches = append(m.mismatches, jointMapMismatch{
			Asked: joint, AskedIndex: idx,
			Moved: moved, MovedIndex: env.Profile.JointIndex(moved),
			AskedTravel: travel, MovedTravel: movedTravel,
		})
	}
	return result, nil
}

func (m *jointRangeSweep) reportOne(env *Env, r jointResult) {
	unit := env.Profile.Joints.Unit
	if !r.Measured {
		env.Prompt.Infof("not measured — %s", r.SkipReason)
		return
	}
	env.Prompt.Infof("recorded  %.4g … %.4g %s  (travel %.4g, %d samples)", *r.Min, *r.Max, unit, *r.Travel, r.Samples)
	env.Prompt.Infof("required  %.4g %s (%s)", *r.Required, unit, r.RequiredFrom)
	if *r.Shortfall > 0 {
		env.Prompt.Infof("! %.4g %s less travel than required", *r.Shortfall, unit)
	}
}

// solve reduces every joint to the one number the gate judges: the worst
// shortfall across the joints that were actually measured.
//
// A procedure where every joint was skipped has no residual at all — not a
// residual of zero. That is the difference between "we looked and it is fine"
// and "nobody looked", and collapsing them is how an uncalibrated robot ends up
// marked calibrated.
func (m *jointRangeSweep) solve(env *Env) (Outcome, error) {
	payload := jointRangePayload{
		Unit:        env.Profile.Joints.Unit,
		OrderNote:   m.orderNote,
		SourceOrder: m.sourceOrder,
	}

	var worst *float64
	var worstJoint string
	measured := 0
	var unmeasured []string
	for _, joint := range m.joints {
		r, ok := m.results[joint]
		if !ok {
			r = jointResult{Name: joint, Index: env.Profile.JointIndex(joint), SkipReason: "never reached"}
		}
		payload.Joints = append(payload.Joints, r)
		if !r.Measured || r.Shortfall == nil {
			unmeasured = append(unmeasured, joint)
			continue
		}
		measured++
		if worst == nil || *r.Shortfall > *worst {
			v := *r.Shortfall
			worst, worstJoint = &v, joint
		}
	}
	payload.JointMap = m.mismatches
	payload.Unmeasured = unmeasured

	var outcome Outcome
	if len(m.mismatches) > 0 {
		names := make([]string, 0, len(m.mismatches))
		for _, mm := range m.mismatches {
			names = append(names, fmt.Sprintf("asked for %s (index %d) and %s (index %d) moved instead",
				mm.Asked, mm.AskedIndex, mm.Moved, mm.MovedIndex))
		}
		payload.JointMapNote = strings.Join(names, "; ")
		outcome.Refusal = "the joint map does not match the profile: " + payload.JointMapNote +
			". A calibration taken on joints that are not the ones the profile names is worse than none, " +
			"and a policy that indexes this array would drive the wrong joint"
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		return Outcome{}, err
	}
	outcome.Payload = encoded

	// A procedure whose claim is "these seven joints travel as far as they
	// should" cannot be answered from four of them. The per-joint numbers are
	// still recorded — an interrupted sweep keeps its work — but the procedure
	// produces no residual at all, so it lands as NOT_MEASURED rather than as a
	// pass earned by skipping the awkward joint.
	if len(unmeasured) > 0 {
		m.explain = fmt.Sprintf("%d of %d joints measured; not measured: %s. "+
			"Nothing qualifies until every joint in the procedure has been swept",
			measured, len(m.joints), strings.Join(unmeasured, ", "))
		return outcome, nil
	}
	outcome.Residual = &robotcal.Quantity{Value: *worst, Unit: env.Profile.Joints.Unit}
	m.explain = fmt.Sprintf("%d of %d joints measured; worst shortfall on %s",
		measured, len(m.joints), worstJoint)
	return outcome, nil
}

// Explain renders a stored record, including one written by an earlier run.
func (m *jointRangeSweep) Explain(rec robotcal.Record) string {
	var payload jointRangePayload
	if len(rec.Payload) == 0 || json.Unmarshal(rec.Payload, &payload) != nil {
		return m.explain
	}
	var measured, skipped int
	var worst string
	for _, j := range payload.Joints {
		if j.Measured {
			measured++
		} else {
			skipped++
		}
	}
	if m.explain != "" {
		worst = m.explain
	} else {
		worst = fmt.Sprintf("%d joints measured, %d not measured", measured, skipped)
	}
	if payload.OrderNote != "" {
		worst += "\n\n" + payload.OrderNote
	}
	return worst
}
