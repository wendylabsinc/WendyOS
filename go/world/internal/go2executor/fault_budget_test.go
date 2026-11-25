package go2executor

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/tasking"
	"testing"
	"time"
)

type budgetAdapter struct {
	*fakeAdapter
	outer       time.Time
	hadDeadline bool
}

func (a *budgetAdapter) State(ctx context.Context, session string) (State, error) {
	a.outer, a.hadDeadline = ctx.Deadline()
	return a.fakeAdapter.State(ctx, session)
}

type budgetGateway struct {
	*fakeGateway
	inner       time.Time
	remaining   time.Duration
	hadDeadline bool
}

func (g *budgetGateway) Evaluate(ctx context.Context, id string, epoch uint64, tele tasking.Telemetry) (tasking.Decision, error) {
	g.inner, g.hadDeadline = ctx.Deadline()
	g.remaining = time.Until(g.inner)
	return g.fakeGateway.Evaluate(ctx, id, epoch, tele)
}
func TestRunUsesCommissionedParentAndBoundedEvaluate(t *testing.T) {
	for _, budget := range []int64{125, 500} {
		t.Run(time.Duration(budget*time.Millisecond.Nanoseconds()).String(), func(t *testing.T) {
			old, g, a, _ := fixture(t)
			cfg := old.cfg
			var p Profile
			_ = json.Unmarshal(cfg.ProfileJSON, &p)
			p.DecisionMaxMS = budget
			cfg.ProfileJSON, _ = json.Marshal(p)
			cfg.Binding.ProfileSHA256 = SHA256(cfg.ProfileJSON)
			g.record.Spec.Go2 = &cfg.Binding
			a.state.Binding = cfg.Binding
			ba := &budgetAdapter{fakeAdapter: a}
			bg := &budgetGateway{fakeGateway: g}
			c, e := New(bg, ba, cfg)
			if e != nil {
				t.Fatal(e)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			reports := 0
			var tickErr error
			e = c.Run(ctx, func(_ tasking.Decision, err error) { reports++; tickErr = err; cancel() })
			if !errors.Is(e, context.Canceled) || tickErr != nil || reports != 1 || !ba.hadDeadline || !bg.hadDeadline {
				t.Fatalf("run setup failed: %v tick=%v reports=%d", e, tickErr, reports)
			}
			if bg.inner.After(ba.outer) || bg.remaining > 250*time.Millisecond {
				t.Fatal("Evaluate exceeded parent or250ms ceiling")
			}
			if budget == 125 && !bg.inner.Equal(ba.outer) {
				t.Fatal("short parent budget was widened")
			}
			if budget == 500 && !ba.outer.After(bg.inner) {
				t.Fatal("outer tick still uses Evaluate's250ms ceiling")
			}
			if len(a.commands) != 1 || a.commands[0].DecisionRemainingMS > budget {
				t.Fatal("dispatch exceeded commissioned fixed budget")
			}
		})
	}
}
func TestLocalFaultReportPreservesObservationAndLatches(t *testing.T) {
	c, g, a, _ := fixture(t)
	if e := c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e := c.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	captured := c.lastState.SampleUnixMS
	position := c.lastState.Pose
	c.fault = context.DeadlineExceeded
	seen := 0
	g.hook = func(tele tasking.Telemetry) {
		seen++
		if !tele.LocalFault || tele.SampleUnixMS != captured || tele.Position != [3]float64{position.X, 0, -position.Y} {
			t.Fatal("fault report rewrote source observation")
		}
	}
	count := len(a.commands)
	if _, e := c.Tick(context.Background()); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
	if seen != 1 || len(a.commands) != count {
		t.Fatal("fault report absent or dispatched motion")
	}
}

func TestLatchedFaultEvaluationRetains250msChildBudget(t *testing.T) {
	c, g, _, _ := fixture(t)
	bg := &budgetGateway{fakeGateway: g}
	c.gateway = bg
	if e := c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, e := c.Tick(context.Background()); e != nil {
		t.Fatal(e)
	}
	c.fault = context.DeadlineExceeded
	parent, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	outer, _ := parent.Deadline()
	if _, e := c.Tick(parent); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
	if !bg.hadDeadline || bg.remaining > 250*time.Millisecond || !outer.After(bg.inner) {
		t.Fatal("latched fault report inherited500ms outer budget")
	}
}

func TestFaultTimingUsesFixedLocalMonotonicPhases(t *testing.T) {
	c, g, a, clock := fixture(t)
	if e := c.Start(context.Background()); e != nil {
		t.Fatal(e)
	}
	a.stateHook = func(s *State) { clock.advance(5 * time.Millisecond) }
	g.hook = func(tasking.Telemetry) { clock.advance(260 * time.Millisecond) }
	g.changed = true // Deterministic post-Evaluate failure; this fake clock does not drive context timers.
	if _, e := c.Tick(context.Background()); e == nil {
		t.Fatal("changed immutable response admitted")
	}
	shot, ok := c.FaultSnapshot()
	if !ok {
		t.Fatal("missing fault")
	}
	v := shot.Timing
	if !v.ClockValid || v.ConfiguredBudgetMS != 500 || v.BeforeEvaluateMS == nil || *v.BeforeEvaluateMS != 5 || v.EvaluateElapsedMS == nil || *v.EvaluateElapsedMS != 260 || v.FaultElapsedMS == nil || *v.FaultElapsedMS < 265 || v.EffectiveEvaluateBudgetMS == nil || *v.EffectiveEvaluateBudgetMS > 250 {
		t.Fatalf("phase timing %+v", v)
	}
	old := *v.EvaluateElapsedMS
	*v.EvaluateElapsedMS = 99
	again, _ := c.FaultSnapshot()
	if *again.Timing.EvaluateElapsedMS != old {
		t.Fatal("timing aliases stored first fault")
	}
}
func TestFaultTimingUnknownBeforeStartAndOnClockFault(t *testing.T) {
	for _, mode := range []string{"before-decision", "clock-fault"} {
		t.Run(mode, func(t *testing.T) {
			c, g, _, clock := fixture(t)
			if mode == "before-decision" {
				g.beginErr = errors.New("durable begin failed")
				if e := c.Start(context.Background()); e == nil {
					t.Fatal("missing start fault")
				}
			} else {
				if e := c.Start(context.Background()); e != nil {
					t.Fatal(e)
				}
				g.hook = func(tasking.Telemetry) { clock.mono -= time.Second }
				if _, e := c.Tick(context.Background()); e == nil {
					t.Fatal("clock fault admitted")
				}
			}
			shot, ok := c.FaultSnapshot()
			if !ok {
				t.Fatal("missing first fault")
			}
			v := shot.Timing
			if v.ClockValid || v.BeforeEvaluateMS != nil || v.EvaluateElapsedMS != nil || v.EffectiveEvaluateBudgetMS != nil || v.FaultElapsedMS != nil || v.ConfiguredBudgetMS != 500 {
				t.Fatalf("invented timing %+v", v)
			}
		})
	}
}
