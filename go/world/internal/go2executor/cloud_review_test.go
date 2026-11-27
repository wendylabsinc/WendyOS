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

func TestCloudReviewExactOriginalAgeBoundary(t *testing.T) {
	for _, age := range []time.Duration{250*time.Millisecond - time.Nanosecond, 250 * time.Millisecond} {
		t.Run(age.String(), func(t *testing.T) {
			c, _, a, clock := cloudController(t)
			s, err := a.State(context.Background(), c.session)
			if err != nil {
				t.Fatal(err)
			}
			clock.advance(age)
			err = c.validateState(s)
			if age < 250*time.Millisecond && err != nil {
				t.Fatal(err)
			}
			if age == 250*time.Millisecond && err == nil {
				t.Fatalf("exclusive cloud age boundary not enforced: %v", err)
			}
		})
	}
}

func TestCloudReviewRepeatedCaptureDoesNotRenewCommand(t *testing.T) {
	c, g, a, _ := cloudController(t)
	if d, err := c.Tick(context.Background()); err != nil || d.Action != mission.Continue || len(a.commands) != 1 {
		t.Fatalf("initial tick: %v %+v", err, d)
	}
	served := snapshotState(c.observedState)
	lastRequest := a.commands[0]
	prior, priorExpiry := c.gait.prior, c.gait.priorNotAfter
	firstZero := *c.gait.firstZero
	a.cloudHook = func(s *State) {
		sequence, epoch, previous := s.Sequence, s.ExecutorEpoch, s.PreviousCommand
		*s = snapshotState(served)
		s.Sequence, s.ExecutorEpoch, s.PreviousCommand = sequence, epoch, previous
	}
	before := g.evaluations
	if d, err := c.Tick(context.Background()); err != nil || d.Action != mission.Continue {
		t.Fatalf("unchanged fresh tuple refused: %v %+v", err, d)
	}
	if len(a.commands) != 1 || a.commands[0] != lastRequest || c.usedCapture != served.CaptureSequence {
		t.Fatal("repeated capture renewed or redispatched command")
	}
	if c.gait.prior != prior || c.gait.priorNotAfter != priorExpiry || *c.gait.firstZero != firstZero {
		t.Fatal("skipped duplicate changed retained motion or zero receipt")
	}
	if g.evaluations != before+1 {
		t.Fatal("duplicate bypassed current authority evaluation")
	}
}

func TestCloudReviewZeroRetainsUnexpiredPrior(t *testing.T) {
	c, _, a, clock := cloudController(t)
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Model an acknowledged target still inside its original expiry while a
	// previous zero receipt is awaiting the later original-sample barrier.
	c.gait.prior = go2nav.Command{ForwardMPS: .1}
	c.gait.priorNotAfter = clock.wall.Add(400 * time.Millisecond).UnixMilli()
	c.gait.axis = axisForward
	prior, expiry, zero := c.gait.prior, c.gait.priorNotAfter, *c.gait.firstZero
	if _, err := c.Tick(context.Background()); err != nil {
		t.Fatalf("stage=%s: %v", c.diagnosticStage, err)
	}
	last := a.commands[len(a.commands)-1]
	if last.BodyVX != 0 || last.YawRate != 0 {
		t.Fatal("zero barrier did not hold")
	}
	if c.gait.prior != prior || c.gait.priorNotAfter != expiry || *c.gait.firstZero != zero {
		t.Fatal("cloud decision erased or renewed retained prior/first zero")
	}
}

func TestCloudReviewMissingInputStopsWithoutLegacyFallbackOrRestart(t *testing.T) {
	c, g, a, _ := cloudController(t)
	clearEvaluations := 0
	g.hook = func(value tasking.Telemetry) {
		if value.ObstacleClear {
			clearEvaluations++
		}
	}
	a.cloudHook = func(s *State) { s.Cloud = nil }
	if _, err := c.Tick(context.Background()); err == nil {
		t.Fatal("missing required cloud admitted")
	}
	if len(a.commands) != 0 || a.stops == 0 || clearEvaluations != 0 {
		t.Fatal("missing cloud reached authority or motion")
	}
	beforeSessions := a.sessionCalls
	a.cloudHook = nil
	if decision, _ := c.Tick(context.Background()); decision.Action != mission.Stop {
		t.Fatal("collector recovery silently resumed faulted controller")
	}
	if err := c.Start(context.Background()); err == nil {
		t.Fatal("controller rearmed after missing cloud")
	}
	if a.sessionCalls != beforeSessions || len(a.commands) != 0 {
		t.Fatal("fault caused session or command retry")
	}
}

func TestCloudReviewMissingReturnsRemainUnknown(t *testing.T) {
	c, g, a, _ := cloudController(t)
	clearEvaluations := 0
	g.hook = func(value tasking.Telemetry) {
		if value.ObstacleClear {
			clearEvaluations++
		}
	}
	a.cloudHook = func(s *State) {
		for i := range s.Cloud.Ranges {
			s.Cloud.Ranges[i] = math.Inf(1)
		}
		for i := range s.Scan.Ranges {
			s.Scan.Ranges[i] = math.Inf(1)
		}
	}
	s, err := a.State(context.Background(), c.session)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeState(cloudStateJSON(t, s))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range decoded.Cloud.Ranges {
		if !math.IsInf(r, 1) {
			t.Fatal("null cloud invented finite return")
		}
	}
	for _, r := range decoded.Scan.Ranges {
		if !math.IsInf(r, 1) {
			t.Fatal("null companion invented finite return")
		}
	}
	if _, err = c.Tick(context.Background()); err == nil {
		t.Fatal("missing observations created traversable route")
	}
	if len(a.commands) != 0 || clearEvaluations != 0 || a.stops == 0 {
		t.Fatal("unknown cloud route progressed to motion")
	}
}

func TestCloudReviewPreviousTupleOwnsItsRanges(t *testing.T) {
	c, _, a, _ := cloudController(t)
	s, err := a.State(context.Background(), c.session)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.validateState(s); err != nil {
		t.Fatal(err)
	}
	old := c.cloudPrevious.Ranges[0]
	s.Cloud.Ranges[0] = 7
	if c.cloudPrevious.Ranges[0] != old {
		t.Fatal("caller changed prior validated tuple")
	}
	if err = c.validateState(s); err == nil {
		t.Fatal("same served ID admitted changed original range")
	}
}
