package go2executor

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

func TestGaitProfileExplicitVersionBoundAndLegacyParity(t *testing.T) {
	c, _ := tiltedController(t)
	cases := []struct {
		name, model, raw   string
		legacy, absent, ok bool
	}{
		{name: "legacy-absent", model: go2nav.IsotropicV1, absent: true, ok: true},
		{name: "legacy-zero", model: go2nav.IsotropicV1, raw: "0", ok: true},
		{name: "legacy-nonzero", model: go2nav.IsotropicV1, raw: "0.1"},
		{name: "legacy-null", model: go2nav.IsotropicV1, raw: "null"},
		{name: "legacy-v1-null", model: go2nav.IsotropicV1, raw: "null", legacy: true},
		{name: "legacy-v1-zero", model: go2nav.IsotropicV1, raw: "0", legacy: true, ok: true},
		{name: "directional-override", model: go2nav.DirectionalV1, raw: "0.1"},
		{name: "gait-v1", model: go2nav.IsotropicGaitV1, raw: "0.15", legacy: true},
		{name: "gait-absent", model: go2nav.IsotropicGaitV1, absent: true},
		{name: "gait-zero", model: go2nav.IsotropicGaitV1, raw: "0"},
		{name: "gait-null", model: go2nav.IsotropicGaitV1, raw: "null"},
		{name: "gait-bool", model: go2nav.IsotropicGaitV1, raw: "true"},
		{name: "gait-negative", model: go2nav.IsotropicGaitV1, raw: "-0.01"},
		{name: "gait-over", model: go2nav.IsotropicGaitV1, raw: "0.200001"},
		{name: "gait-overflow", model: go2nav.IsotropicGaitV1, raw: "1e1000"},
		{name: "gait-nan", model: go2nav.IsotropicGaitV1, raw: "NaN"},
		{name: "gait-valid", model: go2nav.IsotropicGaitV1, raw: "0.15", ok: true},
		{name: "gait-bound", model: go2nav.IsotropicGaitV1, raw: "0.2", ok: true},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			cfg := c.cfg
			var m map[string]json.RawMessage
			_ = json.Unmarshal(cfg.ProfileJSON, &m)
			m["motion_envelope"], _ = json.Marshal(tt.model)
			if tt.legacy {
				m["schema"] = json.RawMessage(`"wendy.world.go2-profile.v1"`)
				delete(m, "tilted_scan")
				m["max_abs_roll_rad"] = json.RawMessage(`0.05`)
				m["max_abs_pitch_rad"] = json.RawMessage(`0.05`)
			}
			// Assemble the intentionally malformed/nonfinite numeric cases verbatim.
			delete(m, "max_measured_lateral_mps")
			raw, _ := json.Marshal(m)
			if !tt.absent {
				raw = append(append(raw[:len(raw)-1], []byte(`,"max_measured_lateral_mps":`)...), []byte(tt.raw+"}")...)
			}
			cfg.ProfileJSON = raw
			cfg.Binding.ProfileSHA256 = SHA256(raw)
			_, p, e := DecodeConfiguration(cfg)
			if (e == nil) != tt.ok {
				t.Fatalf("admitted=%v want=%v err=%v", e == nil, tt.ok, e)
			}
			if e == nil && tt.model != go2nav.IsotropicGaitV1 && p.measuredLateralLimit() != .05 {
				t.Fatal("legacy effective limit changed")
			}
			if e == nil && tt.model == go2nav.IsotropicGaitV1 && p.measuredLateralLimit() != *p.MaxMeasuredLateralMPS {
				t.Fatal("explicit limit lost")
			}
		})
	}
	// Unknown fields and exact bytes remain part of the commissioned digest.
	cfg := c.cfg
	cfg.ProfileJSON = append(cfg.ProfileJSON, ' ')
	if _, _, e := DecodeConfiguration(cfg); e == nil {
		t.Fatal("profile mutation escaped binding")
	}
}

// This adapter supplies matched bridge-publication receipts. It does not model
// robot receipt, DDS transport, physical dynamics or measured stopping.
type gaitAdapter struct {
	*fakeAdapter
	receipt       *BridgeCommandReceipt
	omitReceipt   bool
	changeReceipt func(*BridgeCommandReceipt)
}

func (a *gaitAdapter) withTilt(s State) State {
	t := *a.state.Scan.TiltedPose
	t.OdomCapturedUnixNS = s.OdomCapturedUnixNS
	t.OdometryEpochAgeS = s.OdometryEpochAgeS
	t.OdomPositionXYZ = [3]float64{s.Pose.X, s.Pose.Y, .33}
	t.QuaternionWXYZ = [4]float64{math.Cos(s.Pose.Yaw / 2), 0, 0, math.Sin(s.Pose.Yaw / 2)}
	s.Scan.TiltedPose = &t
	return s
}
func (a *gaitAdapter) Session(ctx context.Context, binding tasking.Go2Binding) (State, error) {
	s, e := a.fakeAdapter.Session(ctx, binding)
	return a.withTilt(s), e
}
func (a *gaitAdapter) State(ctx context.Context, session string) (State, error) {
	s, e := a.fakeAdapter.State(ctx, session)
	return a.withTilt(s), e
}
func (a *gaitAdapter) Command(ctx context.Context, r CommandRequest) error {
	a.receipt = nil
	start := a.clock.wall.UnixNano()
	if e := a.fakeAdapter.Command(ctx, r); e != nil {
		return e
	}
	a.receipt = &BridgeCommandReceipt{Request: r, Acknowledgment: BridgeCommandAcknowledgment{Accepted: true, Sequence: r.Sequence, NotAfterUnixMS: r.NotAfterUnixMS, SourceTimestampNS: min(start, (r.NotAfterUnixMS-450)*int64(time.Millisecond))}, RequestStartedUnixNS: start, ResponseCompletedUnixNS: a.clock.wall.UnixNano()}
	if a.changeReceipt != nil {
		a.changeReceipt(a.receipt)
	}
	return nil
}
func (a *gaitAdapter) LastCommandReceipt() (BridgeCommandReceipt, bool) {
	if a.omitReceipt || a.receipt == nil {
		return BridgeCommandReceipt{}, false
	}
	return *a.receipt, true
}
func gaitController(t *testing.T) (*Controller, *fakeGateway, *gaitAdapter, *testClock) {
	t.Helper()
	base, fa := tiltedController(t)
	g := base.gateway.(*fakeGateway)
	p := base.profile
	p.MotionEnvelope = go2nav.IsotropicGaitV1
	v := .15
	p.MaxMeasuredLateralMPS = &v
	p.MaxYawRateRPS = .1
	cfg := base.cfg
	cfg.ProfileJSON, _ = json.Marshal(p)
	cfg.Binding.ProfileSHA256 = SHA256(cfg.ProfileJSON)
	fa.state.Binding = cfg.Binding
	fa.state.Sequence = 0
	a := &gaitAdapter{fakeAdapter: fa}
	g.record.Spec.Go2 = &cfg.Binding
	g.record.Spec.SpeedMPS = .1
	g.record.Spec.Target = [3]float64{1, 0, 0}
	c, e := New(g, a, cfg)
	if e != nil {
		t.Fatal(e)
	}
	if e = c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	return c, g, a, fa.clock
}
func gaitTick(t *testing.T, c *Controller, a *gaitAdapter) CommandRequest {
	t.Helper()
	before := len(a.commands)
	d, e := c.Tick(context.Background())
	if e != nil {
		t.Fatalf("tick stage=%s: %v", c.diagnosticStage, e)
	}
	if d.Action != mission.Continue || len(a.commands) != before+1 {
		t.Fatalf("tick did not dispatch one continued decision: %+v", d)
	}
	return a.commands[len(a.commands)-1]
}
func prepareGaitForward(t *testing.T) (*Controller, *fakeGateway, *gaitAdapter, *testClock) {
	t.Helper()
	c, g, a, clock := gaitController(t)
	zero := gaitTick(t, c, a)
	if zero.BodyVX != 0 || zero.YawRate != 0 {
		t.Fatal("startup moved before zero/capture barrier")
	}
	// A straight retained route isolates command-axis transition from A* tie order.
	c.path = []go2nav.Point{{X: 1, Y: 0}}
	c.waypoint = 0
	clock.advance(51 * time.Millisecond)
	positive := gaitTick(t, c, a)
	if positive.BodyVX != .1 || positive.YawRate != 0 {
		t.Fatalf("settled startup failed %+v", positive)
	}
	return c, g, a, clock
}

func TestGaitStartupRequiresLaterOriginalSettledCaptures(t *testing.T) {
	c, _, a, clock := gaitController(t)
	r := gaitTick(t, c, a)
	first := *c.gait.firstZero
	if r.BodyVX != 0 || r.YawRate != 0 || c.gait.startupSettled {
		t.Fatal("startup skipped zero")
	}
	c.path = []go2nav.Point{{X: 1, Y: 0}}
	c.waypoint = 0
	clock.advance(50 * time.Millisecond)
	if r = gaitTick(t, c, a); r.BodyVX != 0 || r.YawRate != 0 {
		t.Fatal("equal lower bound crossed strict barrier")
	}
	if *c.gait.firstZero != first {
		t.Fatal("repeated zero slid barrier")
	}
	clock.advance(time.Nanosecond)
	a.state.BodyVelocity = [3]float64{0, .011, 0}
	if r = gaitTick(t, c, a); r.BodyVX != 0 || r.YawRate != 0 {
		t.Fatal("lateral motion treated as settled")
	}
	a.state.BodyVelocity = [3]float64{}
	clock.advance(time.Millisecond)
	if r = gaitTick(t, c, a); r.BodyVX != .1 || r.YawRate != 0 {
		t.Fatalf("later settled original capture did not clear: %+v", r)
	}
	if !c.gait.startupSettled || c.gait.clearedCapture == 0 {
		t.Fatal("clearance barrier evidence absent")
	}
}

func TestGaitAxisTransitionRetainsFirstZeroAndPriorDeadline(t *testing.T) {
	c, _, a, clock := prepareGaitForward(t)
	prior := c.gait.prior
	deadline := c.gait.priorNotAfter
	c.path = []go2nav.Point{{X: 0, Y: 1}}
	c.waypoint = 0
	clock.advance(100 * time.Millisecond)
	r := gaitTick(t, c, a)
	if r.BodyVX != 0 || r.YawRate != 0 {
		t.Fatal("forward changed directly to yaw")
	}
	first := *c.gait.firstZero
	if c.gait.axis != axisForward || c.gait.prior != prior {
		t.Fatal("zero erased prior axis or magnitude")
	}
	clock.advance(100 * time.Millisecond)
	if r = gaitTick(t, c, a); r.BodyVX != 0 || r.YawRate != 0 {
		t.Fatal("ACK completion alone released old live target")
	}
	if *c.gait.firstZero != first || c.gait.priorNotAfter != deadline {
		t.Fatal("renewed zero changed fixed transition boundary")
	}
	// Both original sample lower bounds must be strictly after the old deadline.
	clock.advance(time.Duration(deadline+50-clock.wall.UnixMilli()) * time.Millisecond)
	if r = gaitTick(t, c, a); r.BodyVX != 0 || r.YawRate != 0 {
		t.Fatal("exact prior deadline accepted")
	}
	clock.advance(time.Millisecond)
	if r = gaitTick(t, c, a); r.BodyVX != 0 || r.YawRate != .1 {
		t.Fatalf("expired old target with later settled sample did not turn: %+v", r)
	}
	if c.gait.axis != axisYaw || c.gait.firstZero != nil {
		t.Fatal("new acknowledged axis not retained")
	}
	// Gait oscillation is not a commanded axis switch, but still enters geometry.
	a.state.BodyVelocity = [3]float64{.04, .1, .05}
	clock.advance(100 * time.Millisecond)
	if r = gaitTick(t, c, a); r.YawRate != .1 || r.BodyVX != 0 {
		t.Fatal("same-axis gait oscillation silently restored legacy zero rule")
	}
}

func TestGaitBarrierChecksBothOriginalStampsAndCaptureIdentity(t *testing.T) {
	c, _, a, clock := gaitController(t)
	gaitTick(t, c, a)
	z := c.gait.firstZero
	boundary := z.ResponseCompletedUnixNS + 50*int64(time.Millisecond)
	base := a.state
	base.CaptureSequence = z.Request.CaptureSequence + 1
	base.OdomCapturedUnixNS = boundary + 1
	base.ScanCapturedUnixNS = boundary + 1
	if !c.gaitBarrierSatisfied(base) {
		t.Fatal("valid boundary rejected")
	}
	for _, kind := range []string{"odom", "scan", "capture", "lateral", "yaw", "nan"} {
		t.Run(kind, func(t *testing.T) {
			s := base
			switch kind {
			case "odom":
				s.OdomCapturedUnixNS = boundary
			case "scan":
				s.ScanCapturedUnixNS = boundary
			case "capture":
				s.CaptureSequence = z.Request.CaptureSequence
			case "lateral":
				s.BodyVelocity[1] = .011
			case "yaw":
				s.BodyVelocity[2] = .011
			case "nan":
				s.BodyVelocity[0] = math.NaN()
			}
			if c.gaitBarrierSatisfied(s) {
				t.Fatal("uncertain original evidence cleared transition")
			}
		})
	}
	_ = clock
}

func TestGaitUnexpectedBridgeCommandAndReceiptFailureLatch(t *testing.T) {
	for _, kind := range []string{"opposite-axis", "same-axis-magnitude", "missing-receipt", "mismatched-receipt", "future-receipt", "failed-publication"} {
		t.Run(kind, func(t *testing.T) {
			c, _, a, clock := prepareGaitForward(t)
			n := len(a.commands)
			switch kind {
			case "opposite-axis":
				a.state.PreviousCommand = [3]float64{0, 0, .1}
			case "same-axis-magnitude":
				a.state.PreviousCommand = [3]float64{.05, 0, 0}
			case "missing-receipt":
				a.omitReceipt = true
			case "mismatched-receipt":
				a.changeReceipt = func(r *BridgeCommandReceipt) { r.Request.CaptureSequence++ }
			case "future-receipt":
				a.changeReceipt = func(r *BridgeCommandReceipt) { r.ResponseCompletedUnixNS += int64(time.Second) }
			case "failed-publication":
				a.commandErr = errors.New("uncertain publication")
			}
			clock.advance(100 * time.Millisecond)
			d, e := c.Tick(context.Background())
			if e == nil || d.Action != mission.Stop || c.fault == nil || a.stops == 0 {
				t.Fatalf("uncertainty did not latch stop: %+v %v", d, e)
			}
			if strings.Contains(kind, "axis") && len(a.commands) != n {
				t.Fatal("unexpected target produced new motion")
			}
			shots, ok := c.FaultSnapshot()
			if !ok || shots.Gait == nil || shots.Gait.AcknowledgedAxis != "forward" {
				t.Fatal("missing copied first-fault gait evidence")
			}
			// Restoring adapter data cannot reopen a latched process.
			a.state.PreviousCommand = c.lastCommand
			a.omitReceipt = false
			a.changeReceipt = nil
			a.commandErr = nil
			n = len(a.commands)
			clock.advance(100 * time.Millisecond)
			_, _ = c.Tick(context.Background())
			if len(a.commands) != n || a.sessionCalls != 1 {
				t.Fatal("recovered adapter resumed after uncertain command")
			}
		})
	}
}

func TestGaitFinalJointEnvelopeRetainsOldTargetThroughZeros(t *testing.T) {
	c, _, a, clock := prepareGaitForward(t)
	c.path = []go2nav.Point{{X: 0, Y: 1}}
	c.waypoint = 0
	clock.advance(100 * time.Millisecond)
	gaitTick(t, c, a)
	priors, e := c.gaitPriors(a.state)
	if e != nil || priors[0].ForwardMPS != .1 || priors[1].YawRateRPS != 0 {
		t.Fatalf("zero erased prior reserve: %+v %v", priors, e)
	}
	// Unknown observations remain fatal despite a transition requesting only zero.
	for i := range a.state.Scan.Ranges {
		a.state.Scan.Ranges[i] = math.NaN()
	}
	clock.advance(100 * time.Millisecond)
	before := len(a.commands)
	if _, e = c.Tick(context.Background()); e == nil || c.fault == nil || len(a.commands) != before {
		t.Fatal("unknown geometry became permitted zero/transition")
	}
}

func TestGaitFaultEvidenceIsCopiedAndProfileOverrideIsNotMutable(t *testing.T) {
	c, _, a, _ := gaitController(t)
	gaitTick(t, c, a)
	c.freezeFault(errors.New("test fault"))
	one, _ := c.FaultSnapshot()
	two, _ := c.FaultSnapshot()
	one.Gait.FirstZeroReceipt.ResponseCompletedUnixNS = 1
	*one.Profile.MaxMeasuredLateralMPS = .2
	if two.Gait.FirstZeroReceipt.ResponseCompletedUnixNS == 1 || c.gait.firstZero.ResponseCompletedUnixNS == 1 || c.profile.measuredLateralLimit() != .15 {
		t.Fatal("diagnostic aliases live state")
	}
	raw, e := json.Marshal(two)
	if e != nil || len(raw) > MaxFaultJSONBytes || !strings.Contains(string(raw), `"gait"`) {
		t.Fatal("bounded fault evidence unavailable")
	}
	cfg := c.cfg
	fresh, e := New(c.gateway, a, cfg)
	if e != nil {
		t.Fatal(e)
	}
	expected := append([]byte(nil), fresh.cfg.ProfileJSON...)
	for i := range cfg.ProfileJSON {
		cfg.ProfileJSON[i] = ' '
	}
	if fresh.profile.measuredLateralLimit() != .15 || string(fresh.cfg.ProfileJSON) != string(expected) {
		t.Fatal("external bytes changed copied commissioned configuration")
	}
}

func TestGaitRepeatedTargetFlipCannotReuseOldZeroBarrier(t *testing.T) {
	c, _, a, clock := prepareGaitForward(t)
	c.path = []go2nav.Point{{X: 0, Y: 1}}
	c.waypoint = 0
	clock.advance(100 * time.Millisecond)
	gaitTick(t, c, a)
	firstZero := *c.gait.firstZero
	// Returning to the retained forward axis is continuation, not an axis switch.
	c.path = []go2nav.Point{{X: 1, Y: 0}}
	c.waypoint = 0
	a.state.BodyVelocity = [3]float64{.1, .1, .05}
	clock.advance(100 * time.Millisecond)
	r := gaitTick(t, c, a)
	if r.BodyVX != .1 || r.YawRate != 0 {
		t.Fatal("same-axis target recovery unexpectedly changed axis")
	}
	if c.gait.firstZero != nil || c.gait.priorNotAfter != r.NotAfterUnixMS {
		t.Fatal("new nonzero failed to invalidate old zero/deadline")
	}
	c.path = []go2nav.Point{{X: 0, Y: 1}}
	c.waypoint = 0
	a.state.BodyVelocity = [3]float64{}
	clock.advance(100 * time.Millisecond)
	r = gaitTick(t, c, a)
	if r.BodyVX != 0 || r.YawRate != 0 || c.gait.firstZero.Request.Sequence == firstZero.Request.Sequence {
		t.Fatal("opposite target reused old cleared transition")
	}
}

func TestGaitLateralBoundAdmitsOnlyExplicitModel(t *testing.T) {
	c, _, a, _ := gaitController(t)
	s, e := a.State(context.Background(), c.session)
	if e != nil {
		t.Fatal(e)
	}
	s.BodyVelocity[1] = .15
	if e = c.validateState(s); e != nil {
		t.Fatalf("commissioned boundary refused: %v", e)
	}
	s.BodyVelocity[1] = math.Nextafter(.15, 1)
	if e = c.validateState(s); e == nil {
		t.Fatal("lateral bound enlarged")
	}
	legacy, old := tiltedController(t)
	old.state.BodyVelocity[1] = .051
	if e = legacy.validateState(old.state); e == nil {
		t.Fatal("legacy profile inherited gait bound")
	}
}

func TestGaitStartupMissingMatchedReceiptNeverProducesPositive(t *testing.T) {
	c, _, a, clock := gaitController(t)
	a.omitReceipt = true
	if _, e := c.Tick(context.Background()); e == nil || c.fault == nil || len(a.commands) != 1 || a.commands[0].BodyVX != 0 || a.commands[0].YawRate != 0 {
		t.Fatal("unknown zero acknowledgment did not latch before positive")
	}
	a.omitReceipt = false
	clock.advance(time.Second)
	_, _ = c.Tick(context.Background())
	if len(a.commands) != 1 || c.gait.firstZero != nil || c.gait.startupSettled {
		t.Fatal("missing startup acknowledgment became a barrier")
	}
}

func TestGaitSettledZeroDischargesExpiredPriorWithoutErasingAxis(t *testing.T) {
	c, _, a, clock := prepareGaitForward(t)
	c.path = []go2nav.Point{{X: 0, Y: 1}}
	c.waypoint = 0
	clock.advance(100 * time.Millisecond)
	gaitTick(t, c, a)
	deadline := c.gait.priorNotAfter
	s := a.state
	s.CaptureSequence++
	s.OdomCapturedUnixNS = (deadline + 50) * int64(time.Millisecond)
	s.ScanCapturedUnixNS = s.OdomCapturedUnixNS
	c.gaitProposal(s, go2nav.Command{})
	if c.gait.prior.ForwardMPS != .1 {
		t.Fatal("equal lower bound discharged old target")
	}
	s.OdomCapturedUnixNS++
	s.ScanCapturedUnixNS++
	if got := c.gaitProposal(s, go2nav.Command{}); got != (go2nav.Command{}) || c.gait.prior != (go2nav.Command{}) || c.gait.priorNotAfter != 0 || c.gait.axis != axisForward {
		t.Fatal("settled zero did not discharge only expired target")
	}
	// No new nonzero was published: the same historical zero remains valid for
	// a later switch after the complete observed barrier.
	if got := c.gaitProposal(s, go2nav.Command{YawRateRPS: .1}); got.YawRateRPS != .1 {
		t.Fatal("valid stopped history could not change axis")
	}
}
