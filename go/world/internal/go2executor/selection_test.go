package go2executor

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

// The archived input is immutable evidence from N, not an invented free-space
// fixture. The hash binds its original scan, attitude, uncertainty and priors.
const nFaultPath = "../../docs/verification/go2-locomotion/missions/n/fault-event.json"
const nFaultSHA = "734c6a03efc13bfe28ff12231c47389e1e75161072f3eddf9941dcaf28346d1f"

type selectionAdapter struct {
	state       State
	commands    []CommandRequest
	receipt     *BridgeCommandReceipt
	clock       *testClock
	stops       int
	omitReceipt bool
	commandErr  error
}

func (a *selectionAdapter) Session(context.Context, tasking.Go2Binding) (State, error) {
	return a.state, nil
}
func (a *selectionAdapter) Begin(context.Context, BeginRequest) error { return nil }
func (a *selectionAdapter) State(context.Context, string) (State, error) {
	s, _ := diagnosticState(a.state)
	return s, nil
}
func (a *selectionAdapter) Command(_ context.Context, r CommandRequest) error {
	a.commands = append(a.commands, r)
	a.state.Sequence = r.Sequence
	a.state.PreviousCommand = [3]float64{r.BodyVX, r.BodyVY, r.YawRate}
	now := a.clock.wall.UnixNano()
	a.receipt = &BridgeCommandReceipt{Request: r, Acknowledgment: BridgeCommandAcknowledgment{Accepted: true, Sequence: r.Sequence, NotAfterUnixMS: r.NotAfterUnixMS, SourceTimestampNS: min(now, (r.NotAfterUnixMS-450)*int64(time.Millisecond))}, RequestStartedUnixNS: now, ResponseCompletedUnixNS: now}
	return a.commandErr
}
func (a *selectionAdapter) LastCommandReceipt() (BridgeCommandReceipt, bool) {
	if a.omitReceipt || a.receipt == nil {
		return BridgeCommandReceipt{}, false
	}
	return *a.receipt, true
}
func (a *selectionAdapter) Stop(context.Context, StopRequest) error { a.stops++; return nil }
func (a *selectionAdapter) fresh(d time.Duration) {
	a.clock.advance(d)
	a.state.CaptureSequence++
	a.state.Scan.Sequence = a.state.CaptureSequence
	a.state.OdomCapturedUnixNS += int64(d)
	a.state.ScanCapturedUnixNS += int64(d)
	a.state.Scan.CapturedUnixNS += int64(d)
	a.state.SampleUnixMS = min(a.state.OdomCapturedUnixNS, a.state.ScanCapturedUnixNS) / int64(time.Millisecond)
	a.state.Scan.TiltedPose.OdomCapturedUnixNS = a.state.OdomCapturedUnixNS
}

func exactNController(t *testing.T) (*Controller, *fakeGateway, *selectionAdapter, *testClock) {
	t.Helper()
	raw, e := os.ReadFile(nFaultPath)
	if e != nil {
		t.Fatal(e)
	}
	if SHA256(raw) != nFaultSHA {
		t.Fatal("archived N input changed")
	}
	var outer struct{ Snapshot json.RawMessage }
	if e = json.Unmarshal(raw, &outer); e != nil {
		t.Fatal(e)
	}
	var shot FaultSnapshot
	if e = json.Unmarshal(outer.Snapshot, &shot); e != nil {
		t.Fatal(e)
	}
	var fields map[string]json.RawMessage
	if e = json.Unmarshal(outer.Snapshot, &fields); e != nil {
		t.Fatal(e)
	}
	s, e := DecodeState(fields["state"])
	if e != nil {
		t.Fatal(e)
	}
	if shot.Stage != "joint-continuation" || s.CaptureSequence != 90 || shot.Spec.ID != "go2-calibration-n" || tasking.SpecDigest(shot.Spec) != shot.SpecSHA256 {
		t.Fatal("wrong N input")
	}
	clock := &testClock{wall: time.UnixMilli(shot.DecisionStartedUnixMS)}
	a := &selectionAdapter{state: s, clock: clock}
	record := tasking.Record{Spec: shot.Spec, Status: mission.Status{Phase: mission.Running, Version: 2}, AuthorizationUntilUnixMS: shot.Spec.NotAfterUnixMS}
	g := &fakeGateway{record: record, epoch: shot.ExecutorEpoch}
	// Resume only test-local process state from the consumed snapshot. This is
	// not a production restore path or a cryptographic mission authority test.
	c := &Controller{cfg: Config{TaskID: shot.Spec.ID, Site: shot.Spec.Site, Binding: *shot.Spec.Go2, Now: func() time.Time { return clock.wall }, Monotonic: func() time.Duration { return clock.mono }}, gateway: g, adapter: a, profile: shot.Profile, alignment: shot.Alignment, started: true, bound: true, epoch: shot.ExecutorEpoch, session: s.Session, boot: s.BridgeBootID, odomPublisher: s.OdomPublisher, scanPublisher: s.ScanPublisher, commandPublisher: s.CommandPublisher, sequence: s.Sequence, capture: s.CaptureSequence, usedCapture: shot.LastAcknowledged.CaptureSequence, record: record, lastState: s, lastCommand: shot.PreviousCommand, path: shot.Route, waypoint: shot.Waypoint, lastAttempt: copyCommand(shot.LastAttempt), lastAcknowledged: copyCommand(shot.LastAcknowledged), lastAcknowledgedReceipt: copyReceipt(shot.LastAcknowledgedReceipt)}
	c.gait = gaitTransition{startupSettled: true, axis: axisForward, prior: go2nav.Command{ForwardMPS: shot.Gait.RetainedTarget[0]}, priorNotAfter: shot.Gait.RetainedNotAfterUnixMS, clearedCapture: shot.Gait.ClearedAtCaptureSequence}
	return c, g, a, clock
}
func selectionLimits(c *Controller) go2nav.MotionLimits {
	return go2nav.MotionLimits{MaxForwardMPS: c.record.Spec.SpeedMPS, MaxYawRateRPS: c.profile.MaxYawRateRPS, HorizonS: c.profile.HorizonS, ExtraStopM: c.profile.ExtraStopM, ExtraStopRad: c.profile.ExtraStopRad, WaypointToleranceM: .04, MaxMeasuredLateralMPS: c.profile.measuredLateralLimit()}
}
func nGrid(t *testing.T, c *Controller, s State) *go2nav.Grid {
	t.Helper()
	g, e := go2nav.Build(s.Scan, c.bounds(), c.profile.navigation())
	if e != nil {
		t.Fatal(e)
	}
	return g
}

func TestZeroSelectionExactNRunningPriorAndReceipt(t *testing.T) {
	c, g, a, _ := exactNController(t)
	s := a.state
	grid := nGrid(t, c, s)
	limits := selectionLimits(c)
	measured := go2nav.BodyMotion{ForwardMPS: s.BodyVelocity[0], LateralMPS: s.BodyVelocity[1], YawRateRPS: s.BodyVelocity[2]}
	candidate, e := grid.CommandObserved(s.Pose, go2nav.Point{X: .5}, measured, limits)
	if e != nil || candidate != (go2nav.Command{ForwardMPS: .3}) {
		t.Fatalf("original proposal changed: %+v %v", candidate, e)
	}
	priors, e := c.gaitPriors(s)
	if e != nil {
		t.Fatal(e)
	}
	if e = grid.CheckContinuation(s.Pose, measured, priors, candidate, limits); e != go2nav.ErrUnknown {
		t.Fatalf("N no longer reproduces final unknown: %v", e)
	}
	if e = grid.CheckContinuation(s.Pose, measured, priors, go2nav.Command{}, limits); e != nil {
		t.Fatalf("N zero+all prior is not clear: %v", e)
	}
	prior, deadline := c.gait.prior, c.gait.priorNotAfter
	d, e := c.Tick(context.Background())
	if e != nil || d.Action != mission.Continue {
		t.Fatalf("tick %s: %v", c.diagnosticStage, e)
	}
	if len(a.commands) != 1 || a.commands[0].BodyVX != 0 || a.commands[0].YawRate != 0 || c.fault != nil || c.finished || c.record.Status.Phase != mission.Running || g.record.Status.Phase != mission.Running {
		t.Fatal("zero selection changed task outcome or dispatched positive")
	}
	if c.gait.prior != prior || c.gait.priorNotAfter != deadline || c.gait.firstZero == nil {
		t.Fatal("zero discarded prior or failed to establish matched zero receipt")
	}
	diag, _ := c.CommandObservation()
	if diag.Selection == nil || diag.Selection.Candidate != [3]float64{.3, 0, 0} || diag.Selection.Outcome != "zero_after_unknown" || diag.Selection.CandidateResult != "unknown" || diag.Selection.ZeroResult != "clear" || diag.Selection.CommandSequence != a.commands[0].Sequence || diag.ReceiptState != "matched" || diag.Receipt.Request != a.commands[0] {
		t.Fatalf("lost selection/attempt/ACK distinction: %+v", diag)
	}
	if diag.Selection.CaptureSequence != 90 || diag.Selection.Priors[0] != [3]float64{.3, 0, 0} || diag.Request.NotAfterUnixMS != c.decisionStartedUnixMS+500 {
		t.Fatal("changed capture/prior/fixed deadline")
	}
}

func TestZeroSelectionRepeatedZerosKeepPriorAndFirstBarrierThenClearCandidateProgresses(t *testing.T) {
	c, _, a, _ := exactNController(t)
	if _, e := c.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	prior, deadline, first := c.gait.prior, c.gait.priorNotAfter, *c.gait.firstZero
	for i := 0; i < 3; i++ {
		a.fresh(60 * time.Millisecond)
		if _, e := c.Tick(context.Background()); e != nil {
			t.Fatal(e)
		}
		if a.commands[len(a.commands)-1].BodyVX != 0 || c.gait.prior != prior || c.gait.priorNotAfter != deadline || *c.gait.firstZero != first {
			t.Fatal("renewed zeros changed prior/first barrier")
		}
	}
	// A new, synthetic later observation reduces measured speed. The original
	// N scan remains unchanged. This is a state-machine test, not dynamics proof.
	a.fresh(60 * time.Millisecond)
	a.state.BodyVelocity = [3]float64{}
	if _, e := c.Tick(context.Background()); e != nil {
		t.Fatalf("later clear candidate: %v", e)
	}
	if a.commands[len(a.commands)-1].BodyVX != .3 || c.fault != nil || c.gait.firstZero != nil {
		t.Fatal("fresh clear candidate failed to progress")
	}
}

func TestZeroSelectionRefusalKindsAndModelScope(t *testing.T) {
	for _, kind := range []string{"zero-unknown", "occupied", "invalid", "bounds", "already-zero", "legacy"} {
		t.Run(kind, func(t *testing.T) {
			c, _, a, _ := exactNController(t)
			s := a.state
			limits := selectionLimits(c)
			candidate := go2nav.Command{ForwardMPS: .3}
			prior, _ := c.gaitPriors(s)
			want := go2nav.ErrUnknown
			zeroAttempt := false
			switch kind {
			case "zero-unknown":
				for i := range s.Scan.Ranges {
					s.Scan.Ranges[i] = math.Inf(1)
				}
				zeroAttempt = true
			case "occupied":
				for i := range s.Scan.Ranges {
					s.Scan.Ranges[i] = .4
				}
				want = go2nav.ErrBlocked
				zeroAttempt = true
			case "invalid":
				candidate.ForwardMPS = math.NaN()
				want = go2nav.ErrInvalid
			case "bounds":
				// Change only the checked pose after constructing the original grid.
				want = go2nav.ErrBounds
			case "already-zero":
				candidate = go2nav.Command{}
				for i := range s.Scan.Ranges {
					s.Scan.Ranges[i] = math.Inf(1)
				}
			case "legacy":
				c.profile.MotionEnvelope = go2nav.IsotropicV1
			}
			grid := nGrid(t, c, s)
			if kind == "bounds" {
				s.Pose.X = 4
			}
			_, e := c.selectCandidate(grid, s, prior, candidate, limits)
			if !errors.Is(e, want) {
				t.Fatalf("expected %v, got %v", want, e)
			}
			d, ok := c.SelectionObservation()
			if kind == "legacy" {
				if ok {
					t.Fatal("legacy selection behavior changed")
				}
				return
			}
			if !ok || d.Selected != nil || d.Outcome != "refused" || (d.ZeroResult != "not_attempted") != zeroAttempt {
				t.Fatalf("unexpected alternate selection: %+v", d)
			}
		})
	}
}

func TestZeroSelectionUnknownACKLatchesAndNeverRetries(t *testing.T) {
	for _, kind := range []string{"missing-receipt", "command-error"} {
		t.Run(kind, func(t *testing.T) {
			c, _, a, _ := exactNController(t)
			if kind == "missing-receipt" {
				a.omitReceipt = true
			} else {
				a.commandErr = errors.New("uncertain test dispatch")
			}
			d, e := c.Tick(context.Background())
			if e == nil || d.Action != mission.Stop || c.fault == nil || len(a.commands) != 1 || a.commands[0].BodyVX != 0 || a.stops == 0 {
				t.Fatal("uncertain zero did not latch")
			}
			shot, _ := c.FaultSnapshot()
			if shot.Selection == nil || shot.Selection.CommandSequence == 0 || shot.Selection.Outcome != "zero_after_unknown" {
				t.Fatal("lost original selected attempt")
			}
			a.omitReceipt = false
			a.commandErr = nil
			a.fresh(100 * time.Millisecond)
			a.state.BodyVelocity = [3]float64{}
			_, _ = c.Tick(context.Background())
			if len(a.commands) != 1 || c.fault == nil {
				t.Fatal("later clear state resumed faulted session")
			}
		})
	}
}

func TestZeroSelectionAuthorityFreshnessExpiryAndCaptureRemainGates(t *testing.T) {
	for _, kind := range []string{"stop", "expired-authority", "stale", "used-capture", "late-final", "late-deadline"} {
		t.Run(kind, func(t *testing.T) {
			c, g, a, clock := exactNController(t)
			switch kind {
			case "stop":
				g.stop = true
			case "expired-authority":
				g.hook = func(tasking.Telemetry) { g.record.AuthorizationUntilUnixMS = clock.wall.UnixMilli() - 1 }
			case "stale":
				g.hook = func(tasking.Telemetry) { clock.advance(251 * time.Millisecond) }
			case "used-capture":
				c.usedCapture = a.state.CaptureSequence
			case "late-final", "late-deadline":
				calls := 0
				old := c.cfg.Now
				c.cfg.Now = func() time.Time {
					if c.selectionDiagnostic != nil {
						calls++
						if kind == "late-final" {
							clock.advance(251 * time.Millisecond)
						} else if calls == 2 {
							clock.advance(501 * time.Millisecond)
						}
					}
					return old()
				}
			}
			d, e := c.Tick(context.Background())
			if len(a.commands) != 0 {
				t.Fatal("selected zero bypassed later gate")
			}
			if kind == "used-capture" {
				sel, ok := c.SelectionObservation()
				if e != nil || d.Action != mission.Continue || !ok || sel.Outcome != "zero_after_unknown" || sel.CommandSequence != 0 {
					t.Fatalf("skip lost unattempted selection %v %+v", e, sel)
				}
				return
			}
			if d.Action != mission.Stop {
				t.Fatal("invalid authority/freshness continued")
			}
			if kind == "late-final" || kind == "late-deadline" {
				shot, _ := c.FaultSnapshot()
				if shot.Selection == nil || shot.Selection.CommandSequence != 0 || shot.Selection.Outcome != "zero_after_unknown" {
					t.Fatal("final gate failure mislabeled as dispatch")
				}
			}
		})
	}
}

func TestZeroSelectionCopiedExactCommandJoin(t *testing.T) {
	c, _, a, _ := exactNController(t)
	if _, e := c.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	original, _ := c.SelectionObservation()
	request := a.commands[0]
	for _, kind := range []string{"task", "spec", "epoch", "capture", "target"} {
		r := request
		switch kind {
		case "task":
			r.TaskID += "x"
		case "spec":
			r.SpecDigest += "0"
		case "epoch":
			r.ExecutorEpoch++
		case "capture":
			r.CaptureSequence++
		case "target":
			r.BodyVX = .3
		}
		if c.selectionForCommand(r) != nil {
			t.Fatalf("joined mismatched %s", kind)
		}
	}
	copied, _ := c.SelectionObservation()
	copied.Priors[0][0] = 9
	copied.Selected[0] = 9
	again, _ := c.SelectionObservation()
	if !reflect.DeepEqual(original, again) {
		t.Fatal("selection accessor aliases controller")
	}
	diag, _ := c.CommandObservation()
	diag.Selection.Selected[0] = 9
	diag.Selection.Candidate[0] = 9
	againCommand, _ := c.CommandObservation()
	if !reflect.DeepEqual(againCommand.Selection, original) {
		t.Fatal("command selection aliases diagnostic")
	}
	c.freezeFault(errors.New("test later failure"))
	shot, _ := c.FaultSnapshot()
	shot.Selection.Selected[0] = 9
	shot.Selection.Priors[0][0] = 9
	frozen, _ := c.FaultSnapshot()
	if !reflect.DeepEqual(frozen.Selection, original) {
		t.Fatal("fault selection aliases caller")
	}
	b, e := json.Marshal(frozen)
	if e != nil || len(b) > MaxFaultJSONBytes {
		t.Fatalf("selection broke fault bound %v", e)
	}
}

func TestZeroSelectionClearedOnLaterEarlyFailure(t *testing.T) {
	c, g, a, _ := exactNController(t)
	if _, e := c.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	previous, _ := c.CommandObservation()
	a.fresh(60 * time.Millisecond)
	g.hook = func(tasking.Telemetry) { a.state.Ready = false }
	if _, e := c.Tick(context.Background()); e == nil {
		t.Fatal("invalid fresh state accepted")
	}
	if _, ok := c.SelectionObservation(); ok {
		t.Fatal("new early failure inherited old selection")
	}
	shot, _ := c.FaultSnapshot()
	if shot.Selection != nil || shot.LastCommandDiagnostic.Selection.CaptureSequence != previous.Selection.CaptureSequence {
		t.Fatal("current selection confused with explicitly older attempt")
	}
}

func TestZeroSelectionRechecksOccupiedCandidateReserve(t *testing.T) {
	c, _, a, _ := exactNController(t)
	s := a.state
	limits := selectionLimits(c)
	priors, _ := c.gaitPriors(s)
	candidate := go2nav.Command{ForwardMPS: .3}
	measured := go2nav.BodyMotion{ForwardMPS: s.BodyVelocity[0], LateralMPS: s.BodyVelocity[1], YawRateRPS: s.BodyVelocity[2]}
	// Place one actual finite-return obstacle in the candidate-only reserve.
	// Search is over bounded geometric fixtures, never production clearance.
	for i := 110; i <= 220; i++ {
		sample, _ := diagnosticState(s)
		sample.Scan.Ranges[180] = float64(i) / 100
		grid := nGrid(t, c, sample)
		if grid.CheckContinuation(sample.Pose, measured, priors, candidate, limits) != go2nav.ErrBlocked || grid.CheckContinuation(sample.Pose, measured, priors, go2nav.Command{}, limits) != nil {
			continue
		}
		selected, e := c.selectCandidate(grid, sample, priors, candidate, limits)
		d, _ := c.SelectionObservation()
		if e != nil || selected != (go2nav.Command{}) || d.CandidateResult != "blocked" || d.ZeroResult != "clear" || d.Selected == nil || *d.Selected != [3]float64{} || d.Outcome != "zero_after_blocked" {
			t.Fatal("clear full-prior zero was not independently selected")
		}
		return
	}
	t.Fatal("fixture did not isolate candidate-only occupied reserve")
}

func TestZeroSelectionNeverSuppressesEarlierProposalRefusal(t *testing.T) {
	c, _, a, _ := exactNController(t)
	// Remove a real angular sector. Its unknown support stays unknown; this
	// cannot reach the final selection helper through CommandObserved.
	for i := 160; i <= 200; i++ {
		a.state.Scan.Ranges[i] = math.Inf(1)
	}
	d, e := c.Tick(context.Background())
	if e == nil || d.Action != mission.Stop || c.fault == nil || len(a.commands) != 0 {
		t.Fatal("earlier unknown escaped refusal")
	}
	if c.selectionDiagnostic != nil || c.diagnosticStage == "joint-continuation" {
		t.Fatal("final fallback swallowed earlier geometry refusal")
	}
}
