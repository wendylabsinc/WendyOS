package go2executor

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

const rFaultPath = "../../docs/verification/go2-cloud-live/trials/r/first-fault.json.gz"
const rFaultSHA = "755db2aa20e3eaa6c8922f3bf3f50d1347520cdc2ffe356ce320c81f9edfd814"

// The compressed archive is R's original failed capture, not a regenerated
// clear grid. No original ranges, source stamps, uncertainty or limits change.
func exactRController(t *testing.T) (*Controller, *fakeGateway, *selectionAdapter, *testClock) {
	t.Helper()
	file, err := os.Open(rFaultPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	raw, err := io.ReadAll(io.LimitReader(reader, MaxFaultJSONBytes+1))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > MaxFaultJSONBytes || SHA256(raw) != rFaultSHA {
		t.Fatal("archived R input changed")
	}
	var shot FaultSnapshot
	if err = json.Unmarshal(raw, &shot); err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	state, err := DecodeState(fields["state"])
	if err != nil {
		t.Fatal(err)
	}
	if shot.Stage != "joint-continuation" || shot.Failure != go2nav.ErrBlocked.Error() || state.CaptureSequence != 66 || state.Cloud == nil || state.Binding.WorldEpoch != 30 || shot.Spec.ID != "go2-cloud-calibration-r" || tasking.SpecDigest(shot.Spec) != shot.SpecSHA256 || shot.LastAcknowledged.CaptureSequence != 65 {
		t.Fatal("wrong R snapshot")
	}
	clock := &testClock{wall: time.UnixMilli(shot.DecisionStartedUnixMS)}
	adapter := &selectionAdapter{state: state, clock: clock}
	record := tasking.Record{Spec: shot.Spec, Status: mission.Status{Phase: mission.Running, Version: 2}, AuthorizationUntilUnixMS: shot.Spec.NotAfterUnixMS}
	gateway := &fakeGateway{record: record, epoch: shot.ExecutorEpoch}
	// Restore only a test-local controller at the consumed decision. This is not
	// a production restart path, signed authority test or retroactive R success.
	controller := &Controller{
		cfg:     Config{TaskID: shot.Spec.ID, Site: shot.Spec.Site, Binding: *shot.Spec.Go2, Now: func() time.Time { return clock.wall }, Monotonic: func() time.Duration { return clock.mono }},
		gateway: gateway, adapter: adapter, profile: shot.Profile, alignment: shot.Alignment,
		started: true, bound: true, epoch: shot.ExecutorEpoch, session: state.Session, boot: state.BridgeBootID,
		odomPublisher: state.OdomPublisher, scanPublisher: state.ScanPublisher, commandPublisher: state.CommandPublisher,
		sequence: state.Sequence, capture: state.CaptureSequence, usedCapture: shot.LastAcknowledged.CaptureSequence,
		record: record, lastState: state, lastCommand: shot.PreviousCommand, path: shot.Route, waypoint: shot.Waypoint,
		lastAttempt: copyCommand(shot.LastAttempt), lastAcknowledged: copyCommand(shot.LastAcknowledged), lastAcknowledgedReceipt: copyReceipt(shot.LastAcknowledgedReceipt),
	}
	controller.gait = gaitTransition{startupSettled: shot.Gait.StartupSettled, axis: axisForward, prior: go2nav.Command{ForwardMPS: shot.Gait.RetainedTarget[0]}, priorNotAfter: shot.Gait.RetainedNotAfterUnixMS, clearedCapture: shot.Gait.ClearedAtCaptureSequence}
	return controller, gateway, adapter, clock
}

func TestBlockedZeroSelectionExactRFullPriorAndFixedReceipt(t *testing.T) {
	c, g, a, _ := exactRController(t)
	state := a.state
	grid, err := c.buildGrid(state)
	if err != nil {
		t.Fatal(err)
	}
	limits := selectionLimits(c)
	measured := go2nav.BodyMotion{ForwardMPS: state.BodyVelocity[0], LateralMPS: state.BodyVelocity[1], YawRateRPS: state.BodyVelocity[2]}
	candidate, err := grid.CommandObserved(state.Pose, c.path[c.waypoint], measured, limits)
	if err != nil || candidate != (go2nav.Command{ForwardMPS: .3}) {
		t.Fatalf("R proposal changed: %+v %v", candidate, err)
	}
	prior, err := c.gaitPriors(state)
	if err != nil {
		t.Fatal(err)
	}
	if err = grid.CheckContinuation(state.Pose, measured, prior, candidate, limits); err != go2nav.ErrBlocked {
		t.Fatalf("R blocked positive no longer reproduced: %v", err)
	}
	if err = grid.CheckContinuation(state.Pose, measured, prior, go2nav.Command{}, limits); err != nil {
		t.Fatalf("R full-prior zero is unsafe: %v", err)
	}
	originalPrior, priorDeadline, spec := c.gait.prior, c.gait.priorNotAfter, tasking.SpecDigest(c.record.Spec)
	d, err := c.Tick(context.Background())
	if err != nil || d.Action != mission.Continue || c.fault != nil || c.finished || c.record.Status.Phase != mission.Running || g.record.Status.Phase != mission.Running {
		t.Fatalf("tick %s: %v", c.diagnosticStage, err)
	}
	if len(a.commands) != 1 {
		t.Fatalf("commands=%d", len(a.commands))
	}
	request := a.commands[0]
	if request.BodyVX != 0 || request.BodyVY != 0 || request.YawRate != 0 || request.CaptureSequence != 66 || request.Sequence != 42 || request.NotAfterUnixMS != c.decisionStartedUnixMS+500 || tasking.SpecDigest(c.record.Spec) != spec {
		t.Fatalf("changed attempt identity/deadline: %+v", request)
	}
	if c.gait.prior != originalPrior || c.gait.priorNotAfter != priorDeadline || c.gait.firstZero == nil {
		t.Fatal("zero discarded original retained motion or lacks matched ACK")
	}
	diagnostic, ok := c.CommandObservation()
	if !ok || diagnostic.ReceiptState != "matched" || diagnostic.Receipt == nil || diagnostic.Receipt.Request != request {
		t.Fatal("zero attempt/ACK evidence missing")
	}
	selection := diagnostic.Selection
	if selection == nil || selection.Candidate != [3]float64{.3, 0, 0} || selection.CandidateResult != "blocked" || selection.ZeroResult != "clear" || selection.Outcome != "zero_after_blocked" || selection.Selected == nil || *selection.Selected != [3]float64{} || selection.CommandSequence != 42 || selection.CaptureSequence != 66 || selection.Priors[0] != [3]float64{.3, 0, 0} {
		t.Fatalf("incorrect selected/attempted join: %+v", selection)
	}
}

func TestBlockedZeroSelectionOriginalAuthorityAndCaptureLimits(t *testing.T) {
	for _, kind := range []string{"task-deadline", "grant-deadline", "used-capture"} {
		t.Run(kind, func(t *testing.T) {
			c, g, a, clock := exactRController(t)
			deadline := clock.wall.UnixMilli() + 120
			switch kind {
			case "task-deadline":
				// Synthetic tighter authority exercises the cap, not a modified R replay.
				c.record.Spec.NotAfterUnixMS = deadline
				g.record.Spec.NotAfterUnixMS = deadline
			case "grant-deadline":
				c.record.AuthorizationUntilUnixMS = deadline
				g.record.AuthorizationUntilUnixMS = deadline
			case "used-capture":
				c.usedCapture = a.state.CaptureSequence
			}
			prior, priorDeadline := c.gait.prior, c.gait.priorNotAfter
			d, err := c.Tick(context.Background())
			if err != nil || d.Action != mission.Continue {
				t.Fatalf("tick: %v", err)
			}
			selection, ok := c.SelectionObservation()
			if !ok || selection.Outcome != "zero_after_blocked" {
				t.Fatalf("blocked zero not selected: %+v", selection)
			}
			if kind == "used-capture" {
				if len(a.commands) != 0 || selection.CommandSequence != 0 || c.gait.firstZero != nil {
					t.Fatal("used capture renewed a command or invented ACK")
				}
			} else if len(a.commands) != 1 || a.commands[0].NotAfterUnixMS != deadline || a.commands[0].DecisionRemainingMS != 120 {
				t.Fatal("zero extended original authority deadline")
			}
			if c.gait.prior != prior || c.gait.priorNotAfter != priorDeadline {
				t.Fatal("selection/zero shortened retained prior")
			}
		})
	}
}
