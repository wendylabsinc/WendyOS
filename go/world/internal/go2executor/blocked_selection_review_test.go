package go2executor

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
)

func TestBlockedSelectionReviewUnsafeZeroStillRefuses(t *testing.T) {
	c, _, a, _ := exactRController(t)
	// A synthetic faster measured velocity makes the zero envelope include
	// R's original occupied reserve. No observed cell or prior is removed.
	s := snapshotState(a.state)
	s.BodyVelocity[0] = .3
	grid, err := c.buildGrid(s)
	if err != nil {
		t.Fatal(err)
	}
	prior, err := c.gaitPriors(s)
	if err != nil {
		t.Fatal(err)
	}
	limits := selectionLimits(c)
	measured := go2nav.BodyMotion{ForwardMPS: s.BodyVelocity[0], LateralMPS: s.BodyVelocity[1], YawRateRPS: s.BodyVelocity[2]}
	if !errors.Is(grid.CheckContinuation(s.Pose, measured, prior, go2nav.Command{}, limits), go2nav.ErrBlocked) {
		t.Fatal("fixture does not independently establish an unsafe zero")
	}
	before := c.gait
	_, err = c.selectCandidate(grid, s, prior, go2nav.Command{ForwardMPS: .3}, limits)
	d, ok := c.SelectionObservation()
	if !errors.Is(err, go2nav.ErrBlocked) || !ok || d.CandidateResult != "blocked" || d.ZeroResult != "blocked" || d.Outcome != "refused" || d.Selected != nil {
		t.Fatalf("unsafe zero selected: err=%v diagnostic=%+v", err, d)
	}
	if !reflect.DeepEqual(before, c.gait) || len(a.commands) != 0 {
		t.Fatal("failed geometric selection changed motion history")
	}
}

func TestBlockedSelectionReviewExcludedKindsAndModels(t *testing.T) {
	for _, kind := range []string{"nonfinite", "combined-axis", "too-many-priors", "bounds", "already-zero", "isotropic", "directional"} {
		t.Run(kind, func(t *testing.T) {
			c, _, a, _ := exactRController(t)
			s := snapshotState(a.state)
			prior, err := c.gaitPriors(s)
			if err != nil {
				t.Fatal(err)
			}
			candidate := go2nav.Command{ForwardMPS: .3}
			want := go2nav.ErrInvalid
			switch kind {
			case "nonfinite":
				candidate.ForwardMPS = math.NaN()
			case "combined-axis":
				candidate.YawRateRPS = .1
			case "too-many-priors":
				prior = append(prior, go2nav.Command{})
			case "bounds":
				want = go2nav.ErrBounds
			case "already-zero":
				candidate = go2nav.Command{}
				s.BodyVelocity[0] = .3
				want = go2nav.ErrBlocked
			case "isotropic":
				c.profile.MotionEnvelope = go2nav.IsotropicV1
			case "directional":
				c.profile.MotionEnvelope = go2nav.DirectionalV1
			}
			grid, err := c.buildGrid(s)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "bounds" {
				s.Pose.X = 4
			}
			limits := selectionLimits(c)
			measured := go2nav.BodyMotion{ForwardMPS: s.BodyVelocity[0], LateralMPS: s.BodyVelocity[1], YawRateRPS: s.BodyVelocity[2]}
			if kind == "isotropic" || kind == "directional" {
				want = grid.CheckContinuation(s.Pose, measured, prior, candidate, limits)
			}
			selected, err := c.selectCandidate(grid, s, prior, candidate, limits)
			if !errors.Is(err, want) {
				t.Fatalf("expected original refusal %v, got %v", want, err)
			}
			d, ok := c.SelectionObservation()
			if kind == "isotropic" || kind == "directional" {
				if ok || selected != candidate {
					t.Fatal("gait-only alternative changed another motion model")
				}
			} else if !ok || d.ZeroResult != "not_attempted" || d.Selected != nil || d.Outcome != "refused" {
				t.Fatalf("excluded failure reached alternative selection: %+v", d)
			}
		})
	}
}

func TestBlockedSelectionReviewEarlierProposalFailureNeverReachesFallback(t *testing.T) {
	c, _, a, _ := exactRController(t)
	// The measured-plus-proposed envelope now fails even without any prior.
	// Final joint selection must not turn that earlier refusal into a command.
	a.state.BodyVelocity[0] = .3
	d, err := c.Tick(context.Background())
	if !errors.Is(err, go2nav.ErrBlocked) || d.Action != mission.Stop || c.fault == nil || len(a.commands) != 0 || a.stops == 0 {
		t.Fatalf("earlier proposal refusal escaped: %v %+v", err, d)
	}
	shot, ok := c.FaultSnapshot()
	if !ok || shot.Stage != "measured-motion" || shot.Selection != nil {
		t.Fatalf("earlier fault became final selection: %+v", shot.Selection)
	}
}

func TestBlockedSelectionReviewFinalGatesDoNotDispatchSelectedZero(t *testing.T) {
	for _, gate := range []string{"exact-original-age", "fixed-deadline", "clock-divergence"} {
		t.Run(gate, func(t *testing.T) {
			c, _, a, clock := exactRController(t)
			oldNow := c.cfg.Now
			fired := false
			c.cfg.Now = func() time.Time {
				stage := "final-freshness"
				if gate == "fixed-deadline" {
					stage = "final-deadline"
				}
				if !fired && c.selectionDiagnostic != nil && c.diagnosticStage == stage {
					fired = true
					switch gate {
					case "exact-original-age":
						limit := time.Unix(0, a.state.OdomCapturedUnixNS).Add(250 * time.Millisecond)
						clock.advance(limit.Sub(clock.wall))
					case "fixed-deadline":
						clock.advance(501 * time.Millisecond)
					case "clock-divergence":
						clock.wall = clock.wall.Add(60 * time.Millisecond)
					}
				}
				return oldNow()
			}
			prior, expiry := c.gait.prior, c.gait.priorNotAfter
			d, err := c.Tick(context.Background())
			if !fired || err == nil || d.Action != mission.Stop || len(a.commands) != 0 || a.stops == 0 {
				t.Fatalf("final gate did not refuse: fired=%v err=%v", fired, err)
			}
			shot, ok := c.FaultSnapshot()
			if !ok || shot.Selection == nil || shot.Selection.Outcome != "zero_after_blocked" || shot.Selection.CommandSequence != 0 {
				t.Fatal("selected but unattempted zero lost its evidence")
			}
			if c.gait.prior != prior || c.gait.priorNotAfter != expiry || c.gait.firstZero != nil {
				t.Fatal("undispatched selection discharged prior or invented receipt")
			}
		})
	}
}

type blockedReviewReceiptAdapter struct {
	*selectionAdapter
	mutate func(*BridgeCommandReceipt)
}

func (a *blockedReviewReceiptAdapter) LastCommandReceipt() (BridgeCommandReceipt, bool) {
	r, ok := a.selectionAdapter.LastCommandReceipt()
	if ok && a.mutate != nil {
		a.mutate(&r)
	}
	return r, ok
}

func TestBlockedSelectionReviewUncertainZeroACKCannotResume(t *testing.T) {
	for _, kind := range []string{"missing", "wrong-capture", "rejected", "command-error"} {
		t.Run(kind, func(t *testing.T) {
			c, _, a, _ := exactRController(t)
			wrapped := &blockedReviewReceiptAdapter{selectionAdapter: a}
			c.adapter = wrapped
			switch kind {
			case "missing":
				a.omitReceipt = true
			case "wrong-capture":
				wrapped.mutate = func(r *BridgeCommandReceipt) { r.Request.CaptureSequence++ }
			case "rejected":
				wrapped.mutate = func(r *BridgeCommandReceipt) { r.Acknowledgment.Accepted = false }
			case "command-error":
				a.commandErr = errors.New("test uncertain local write")
			}
			prior, expiry := c.gait.prior, c.gait.priorNotAfter
			d, err := c.Tick(context.Background())
			if err == nil || d.Action != mission.Stop || c.fault == nil || len(a.commands) != 1 || a.commands[0].BodyVX != 0 || a.commands[0].YawRate != 0 || a.stops == 0 {
				t.Fatal("uncertain zero did not stop and latch")
			}
			if c.gait.prior != prior || c.gait.priorNotAfter != expiry || c.gait.firstZero != nil {
				t.Fatal("unmatched zero receipt discharged retained motion")
			}
			shot, ok := c.FaultSnapshot()
			if !ok || shot.Selection == nil || shot.Selection.Outcome != "zero_after_blocked" || shot.Selection.CommandSequence != a.commands[0].Sequence {
				t.Fatal("uncertain dispatch lost its original selected attempt")
			}
			wrapped.mutate, a.commandErr, a.omitReceipt = nil, nil, false
			_, _ = c.Tick(context.Background())
			if len(a.commands) != 1 || c.Start(context.Background()) == nil {
				t.Fatal("later receipt recovery retried or rearmed the session")
			}
		})
	}
}

func TestBlockedSelectionReviewRepeatedZeroPreservesOriginalBarrier(t *testing.T) {
	c, _, a, _ := exactRController(t)
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	prior, expiry, first := c.gait.prior, c.gait.priorNotAfter, *c.gait.firstZero
	for i := 0; i < 3; i++ {
		// Synthetic later tuples exercise the command-state barrier only. They
		// retain R geometry and uncertainty and do not assert observed dynamics.
		a.fresh(40 * time.Millisecond)
		a.state.Cloud.Sequence = a.state.CaptureSequence
		a.state.Cloud.CapturedUnixNS = a.state.ScanCapturedUnixNS
		if _, err := c.Tick(context.Background()); err != nil {
			t.Fatalf("later zero stage=%s: %v", c.diagnosticStage, err)
		}
		last := a.commands[len(a.commands)-1]
		if last.BodyVX != 0 || last.YawRate != 0 || c.gait.prior != prior || c.gait.priorNotAfter != expiry || !reflect.DeepEqual(*c.gait.firstZero, first) || c.gaitBarrierSatisfied(a.state) {
			t.Fatal("repeated zero replaced the first receipt or renewed/discharged prior")
		}
	}
}
