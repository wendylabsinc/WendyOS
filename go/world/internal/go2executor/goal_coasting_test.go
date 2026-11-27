package go2executor

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
)

func prepareGoalForward(t *testing.T) (*Controller, *fakeGateway, *gaitAdapter, *testClock) {
	t.Helper()
	c, g, a, clock := gaitController(t)
	g.record.Spec.Area = tasking.Bounds{Min: [3]float64{-4, -.1, -4}, Max: [3]float64{4, .1, 4}}
	r := gaitTick(t, c, a)
	if r.BodyVX != 0 || r.YawRate != 0 {
		t.Fatal("startup moved")
	}
	c.path = []go2nav.Point{{X: 1}}
	c.waypoint = 0
	clock.advance(51 * time.Millisecond)
	r = gaitTick(t, c, a)
	if r.BodyVX != .1 || r.YawRate != 0 {
		t.Fatal("fixture did not establish forward prior")
	}
	return c, g, a, clock
}

func TestFinalGoalCoastsInsideSignedToleranceWithoutClaimingSuccess(t *testing.T) {
	c, g, a, clock := prepareGoalForward(t)
	c.path = []go2nav.Point{{X: 1, Y: 0}}
	c.waypoint = 0
	// .07 is inside signed .10 but outside the former fixed .04 waypoint stop.
	a.state.Pose.X = .93
	a.state.BodyVelocity = [3]float64{.1, 0, 0}
	oldPrior, oldDeadline := c.gait.prior, c.gait.priorNotAfter
	clock.advance(100 * time.Millisecond)
	r := gaitTick(t, c, a)
	if r.BodyVX != 0 || r.YawRate != 0 {
		t.Fatalf("inside final tolerance still requests motion: %+v", r)
	}
	if c.gait.prior != oldPrior || c.gait.priorNotAfter != oldDeadline {
		t.Fatal("coasting erased a live prior target")
	}
	if c.record.Status.Phase != mission.Running || c.finished || g.record.Status.Phase != mission.Running || a.stops != 0 {
		t.Fatal("coasting invented completion or terminated authority")
	}
	if r.SpecDigest != tasking.SpecDigest(g.record.Spec) || r.NotAfterUnixMS > clock.wall.UnixMilli()+500 {
		t.Fatal("coasting changed signed task or decision deadline")
	}
}

func TestFinalGoalCoastingDoesNotApplyToIntermediateOrOutsideGoal(t *testing.T) {
	for _, kind := range []string{"outside-final", "near-intermediate"} {
		t.Run(kind, func(t *testing.T) {
			c, _, a, clock := prepareGoalForward(t)
			if kind == "outside-final" {
				a.state.Pose.X = .89
				c.path = []go2nav.Point{{X: 1}}
			} else {
				a.state.Pose.X = .50
				c.path = []go2nav.Point{{X: .57}, {X: 1}}
			}
			c.waypoint = 0
			a.state.BodyVelocity = [3]float64{.05, 0, 0}
			clock.advance(100 * time.Millisecond)
			r := gaitTick(t, c, a)
			if r.BodyVX != .1 || r.YawRate != 0 {
				t.Fatalf("fixed path tolerance changed outside final goal: %+v", r)
			}
			if c.waypoint != 0 {
				t.Fatal("intermediate tolerance silently enlarged")
			}
		})
	}
}

func TestFinalGoalCoastingUsesFreshPostAuthorityPose(t *testing.T) {
	for _, startInside := range []bool{false, true} {
		t.Run(map[bool]string{false: "enters", true: "leaves"}[startInside], func(t *testing.T) {
			c, g, a, clock := prepareGoalForward(t)
			c.path = []go2nav.Point{{X: 1}}
			c.waypoint = 0
			a.state.Pose.X = .89
			if startInside {
				a.state.Pose.X = .93
			}
			a.state.BodyVelocity = [3]float64{.05, 0, 0}
			g.hook = func(tasking.Telemetry) {
				if startInside {
					a.state.Pose.X = .89
				} else {
					a.state.Pose.X = .93
				}
			}
			clock.advance(100 * time.Millisecond)
			r := gaitTick(t, c, a)
			if (r.BodyVX == 0) == startInside {
				t.Fatal("coasting used stale pre-authority position")
			}
		})
	}
}

func TestFinalGoalZeroStillRefusesUnsafeRetainedPrior(t *testing.T) {
	c, _, a, clock := prepareGoalForward(t)
	c.path = []go2nav.Point{{X: 1}}
	c.waypoint = 0
	a.state.Pose.X = .93
	a.state.BodyVelocity = [3]float64{.1, 0, 0}
	clock.advance(100 * time.Millisecond)
	// Find a finite-return synthetic local scan that covers measured zero travel
	// but not the additional live prior target. No unknown cell becomes free.
	sample, _ := a.State(context.Background(), c.session)
	limits := go2nav.MotionLimits{MaxForwardMPS: .1, MaxYawRateRPS: .1, HorizonS: 1, ExtraStopM: .1, ExtraStopRad: .1, WaypointToleranceM: .04, MaxMeasuredLateralMPS: .15}
	measured := go2nav.BodyMotion{ForwardMPS: .1}
	found := false
	for i := 80; i <= 250; i++ {
		scan := sample.Scan
		scan.Ranges = make([]float64, 360)
		for j := range scan.Ranges {
			scan.Ranges[j] = float64(i) / 100
		}
		grid, e := go2nav.Build(scan, c.bounds(), c.profile.navigation())
		if e != nil {
			continue
		}
		if grid.CheckContinuation(sample.Pose, measured, nil, go2nav.Command{}, limits) == nil && grid.CheckContinuation(sample.Pose, measured, []go2nav.Command{{ForwardMPS: .1}}, go2nav.Command{}, limits) != nil {
			a.state.Scan.Ranges = scan.Ranges
			found = true
			break
		}
	}
	if !found {
		t.Fatal("fixture did not isolate retained-prior clearance")
	}
	before := len(a.commands)
	d, e := c.Tick(context.Background())
	if e == nil || d.Action != mission.Stop || c.fault == nil || len(a.commands) != before || c.diagnosticStage != "joint-continuation" {
		t.Fatalf("unsafe zero/prior escaped final check: stage=%s err=%v", c.diagnosticStage, e)
	}
	if c.gait.prior.ForwardMPS != .1 {
		t.Fatal("coasting discarded unsafe prior")
	}
}

func TestFinalGoalCoastingPreservesAuthorityAndFreshnessRefusals(t *testing.T) {
	for _, kind := range []string{"stop", "changed-spec", "stale-final"} {
		t.Run(kind, func(t *testing.T) {
			c, g, a, clock := prepareGoalForward(t)
			c.path = []go2nav.Point{{X: 1}}
			c.waypoint = 0
			a.state.Pose.X = .93
			a.state.BodyVelocity = [3]float64{.1, 0, 0}
			clock.advance(100 * time.Millisecond)
			if kind == "stop" {
				g.stop = true
			} else if kind == "changed-spec" {
				g.changed = true
			} else {
				g.hook = func(tasking.Telemetry) {
					a.stateHook = func(s *State) {
						s.ScanCapturedUnixNS -= int64(time.Second)
						s.Scan.CapturedUnixNS = s.ScanCapturedUnixNS
						s.SampleUnixMS = s.ScanCapturedUnixNS / int64(time.Millisecond)
					}
				}
			}
			before := len(a.commands)
			d, e := c.Tick(context.Background())
			if len(a.commands) != before || d.Action != mission.Stop || a.stops == 0 {
				t.Fatal("coasting bypassed authority/freshness")
			}
			if kind != "stop" && e == nil {
				t.Fatal("invalid final input admitted")
			}
		})
	}
}

func TestFinalGoalCoastingAlsoUsesLegacySignedTolerance(t *testing.T) {
	c, g, a, clock := fixture(t)
	g.record.Spec.Target = [3]float64{1, 0, 0}
	if e := c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e := c.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	c.path = []go2nav.Point{{X: 1}}
	c.waypoint = 0
	a.state.Pose.X = .93
	a.state.Pose.Yaw = 0
	a.state.BodyVelocity = [3]float64{.05, 0, 0}
	clock.advance(100 * time.Millisecond)
	if _, e := c.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	r := a.commands[len(a.commands)-1]
	if math.Abs(r.BodyVX)+math.Abs(r.YawRate) != 0 || c.record.Status.Phase != mission.Running {
		t.Fatal("legacy target coasting changed completion semantics")
	}
}
