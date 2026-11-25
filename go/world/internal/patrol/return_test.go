package patrol

import (
	"errors"
	"testing"
)

type returnAdapter struct {
	fake
	goalError error
	plans     int
}

func (f *returnAdapter) Plan(p Point) (Route, error) { f.plans++; return f.fake.Plan(p) }
func (f *returnAdapter) Goal(id string, p Point) error {
	if f.goalError != nil {
		return f.goalError
	}
	return f.fake.Goal(id, p)
}
func returnRig(t *testing.T) (*Engine, *returnAdapter) {
	t.Helper()
	f := &returnAdapter{fake: fake{unreachable: map[float64]bool{}}}
	e, err := New("mission", 100000, []Room{room("office", 2)}, f)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.SetReturn(Point{X: 1}, 10); err != nil {
		t.Fatal(err)
	}
	return e, f
}
func rn(f *returnAdapter, stamp float64) Navigation { n := nav(&f.fake); n.Stamp = stamp; return n }
func TestRecallSettledArrival(t *testing.T) {
	e, f := returnRig(t)
	selectRoom(t, e, &f.fake, 1000)
	if err := e.Control("recall", 2000); err != nil {
		t.Fatal(err)
	}
	if e.State.CurrentRoom != "" || e.State.Return.Trigger != "operator" || e.State.Return.RequestedAtMS != 2000 {
		t.Fatal(e.State)
	}
	_ = e.Tick(2100, rn(f, 1))
	if e.State.State != "returning" || f.p.X != 1 {
		t.Fatal(e.State)
	}
	n := rn(f, 2)
	n.Goal.State = "succeeded"
	n.Speed = .2
	_ = e.Tick(3000, n)
	if _, _, _, ok := e.Arrival(); ok {
		t.Fatal("moving robot completed")
	}
	n.Speed = 0
	n.Stamp = 3
	_ = e.Tick(4000, n)
	// Repeated samples and wall time alone cannot establish settling.
	_ = e.Tick(7000, n)
	if _, _, _, ok := e.Arrival(); ok {
		t.Fatal("duplicate sample completed")
	}
	n.Stamp = 4
	_ = e.Tick(7100, n)
	if _, _, _, ok := e.Arrival(); ok {
		t.Fatal("one simulation second completed")
	}
	n.Stamp = 5
	_ = e.Tick(8100, n)
	if _, _, _, ok := e.Arrival(); ok {
		t.Fatal("gap bridged into settled arrival")
	}
	n.Stamp = 6
	_ = e.Tick(9100, n)
	since, first, last, ok := e.Arrival()
	if !ok || since != 7100 || first != 4 || last != 6 || f.captures != 0 {
		t.Fatal(since, first, last, ok)
	}
	if e.State.Rooms[0].Visits != 0 {
		t.Fatal("return invented room coverage")
	}
}
func TestReturnHoldsAndExplicitRetry(t *testing.T) {
	for _, failure := range []string{"blocked", "sensors", "dispatch", "incorrect-arrival"} {
		t.Run(failure, func(t *testing.T) {
			e, f := returnRig(t)
			_ = e.Control("recall", 1000)
			n := rn(f, 1)
			switch failure {
			case "blocked":
				f.unreachable[1] = true
			case "sensors":
				n.Ready = false
			case "dispatch":
				f.goalError = errors.New("revoked")
			}
			_ = e.Tick(1100, n)
			if failure == "incorrect-arrival" {
				n = rn(f, 2)
				n.Goal.State = "succeeded"
				n.Pose.X = 5
				_ = e.Tick(1200, n)
			}
			if e.State.State != "return_held" || e.State.Reason == "" {
				t.Fatal(e.State)
			}
			f.unreachable[1] = false
			f.goalError = nil
			_ = e.Tick(3000, rn(f, 3))
			if e.State.State != "return_held" {
				t.Fatal("hold auto resumed")
			}
			if err := e.Control("resume", 3100); err != nil {
				t.Fatal(err)
			}
			_ = e.Tick(3200, rn(f, 4))
			if e.State.State != "returning" {
				t.Fatal(e.State)
			}
		})
	}
}
func TestReturnWindowPauseAndDeadline(t *testing.T) {
	e, f := returnRig(t)
	_ = e.Control("pause", 1000)
	_ = e.Tick(91000, rn(f, 2))
	if e.State.Return.Requested {
		t.Fatal("automatic return bypassed pause")
	}
	_ = e.Control("resume", 92000)
	_ = e.Tick(92100, rn(f, 3))
	if !e.State.Return.Requested || e.State.Return.Trigger != "scheduled" {
		t.Fatal(e.State)
	}
	_ = e.Control("stop", 93000)
	_ = e.Tick(94000, rn(f, 4))
	if !e.Terminal() || e.State.State != "stopped" || e.Control("resume", 94001) == nil {
		t.Fatal(e.State)
	}
	e, f = returnRig(t)
	_ = e.Control("recall", 99000)
	_ = e.Tick(100000, rn(f, 5))
	if e.State.State != "expired" {
		t.Fatal("return extended deadline")
	}
}
func TestRecallRequiresSignedPoint(t *testing.T) {
	e, _ := rig(t)
	if e.Control("recall", 1000) == nil {
		t.Fatal("recall without admitted policy")
	}
	e, f := returnRig(t)
	_ = e.Control("pause", 1000)
	_ = e.Control("recall", 2000)
	_ = e.Tick(3000, rn(f, 1))
	if e.State.State != "paused" || f.goal != "" {
		t.Fatal("recall resumed pause")
	}
	if e.SetReturn(Point{X: 2}, 0) == nil {
		t.Fatal("point changed")
	}
}

func TestReturnSampleGap(t *testing.T) {
	for _, gap := range []string{"wall", "simulation"} {
		t.Run(gap, func(t *testing.T) {
			e, f := returnRig(t)
			_ = e.Control("recall", 1000)
			_ = e.Tick(1100, rn(f, 1))
			n := rn(f, 2)
			n.Goal.State = "succeeded"
			_ = e.Tick(2000, n)
			at := int64(2200)
			n.Stamp = 2.2
			if gap == "wall" {
				at = 5000
			} else {
				n.Stamp = 5
			}
			_ = e.Tick(at, n)
			if _, _, _, ok := e.Arrival(); ok {
				t.Fatal("unobserved interval became dwell")
			}
			first := n.Stamp
			n.Stamp = first + 1
			_ = e.Tick(at+1000, n)
			n.Stamp = first + 2
			_ = e.Tick(at+2000, n)
			since, observed, _, ok := e.Arrival()
			if !ok || since != at || observed != first {
				t.Fatal(since, observed, ok)
			}
		})
	}
}
