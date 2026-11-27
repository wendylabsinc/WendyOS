package patrol

import (
	"errors"
	"testing"
)

type fake struct {
	goal        string
	p           Point
	canceled    int
	captures    int
	bad         bool
	unreachable map[float64]bool
	captureErr  bool
}

func (f *fake) Plan(p Point) (Route, error) {
	return Route{Reachable: !f.unreachable[p.X], Length: p.X}, nil
}
func (f *fake) Goal(id string, p Point) error { f.goal = id; f.p = p; return nil }
func (f *fake) Cancel() error                 { f.canceled++; return nil }
func (f *fake) Capture() (Capture, error) {
	f.captures++
	if f.captureErr {
		return Capture{}, errors.New("camera lost")
	}
	return Capture{TimeS: float64(f.captures * 10), Evidence: []string{"camera-a", "camera-b", "audio"}, Findings: []Finding{{ID: "observation", Text: "gas"}}}, nil
}
func room(id string, x float64) Room {
	return Room{ID: id, Name: id, Polygon: [][2]float64{{0, -1}, {10, -1}, {10, 1}, {0, 1}}, Candidates: []Point{{X: x}}}
}
func rig(t *testing.T) (*Engine, *fake) {
	t.Helper()
	f := &fake{unreachable: map[float64]bool{}}
	e, err := New("mission", 100000, []Room{room("far", 4), room("near", 2)}, f)
	if err != nil {
		t.Fatal(err)
	}
	return e, f
}
func nav(f *fake) Navigation {
	n := Navigation{Ready: true, Pose: f.p}
	n.Goal.ID = f.goal
	n.Goal.State = "navigating"
	n.Goal.Distance = 4
	return n
}
func selectRoom(t *testing.T, e *Engine, f *fake, now int64) {
	t.Helper()
	for i := 0; i < 100 && e.State.State != "navigating"; i++ {
		if err := e.Tick(now, nav(f)); err != nil {
			t.Fatal(err)
		}
	}
}
func complete(t *testing.T, e *Engine, f *fake, now int64) {
	t.Helper()
	n := nav(f)
	n.Goal.State = "succeeded"
	if err := e.Tick(now, n); err != nil {
		t.Fatal(err)
	}
	if err := e.Tick(now+2100, n); err != nil {
		t.Fatal(err)
	}
}
func TestCoverageRequiresFreshCaptureAndOldestFirst(t *testing.T) {
	e, f := rig(t)
	selectRoom(t, e, f, 1000)
	if e.State.CurrentRoom != "near" {
		t.Fatal(e.State)
	}
	n := nav(f)
	n.Goal.State = "succeeded"
	_ = e.Tick(2000, n)
	if e.State.Rooms[1].Visits != 0 || f.captures != 0 {
		t.Fatal("arrival marked coverage")
	}
	_ = e.Tick(4100, n)
	if e.State.Rooms[1].Visits != 1 {
		t.Fatal(e.State)
	}
	selectRoom(t, e, f, 4200)
	if e.State.CurrentRoom != "far" {
		t.Fatal("did not prioritize unobserved")
	}
	complete(t, e, f, 5000)
	selectRoom(t, e, f, 7200)
	if e.State.CurrentRoom != "near" {
		t.Fatal("did not choose oldest")
	}
}
func TestBlockedRoomDeferralAndRetry(t *testing.T) {
	e, f := rig(t)
	f.unreachable[2] = true
	selectRoom(t, e, f, 1000)
	if e.State.CurrentRoom != "far" || e.State.Rooms[1].BlockedUntilMS != 31000 {
		t.Fatal(e.State)
	}
	n := nav(f)
	_ = e.Tick(2000, n)
	_ = e.Tick(17001, n)
	if e.State.Rooms[0].BlockedUntilMS != 47001 {
		t.Fatal(e.State)
	}
	f.unreachable[2] = false
	selectRoom(t, e, f, 32000)
	if e.State.CurrentRoom != "near" {
		t.Fatal(e.State)
	}
}
func TestPauseRetainsDestinationAndStopEnds(t *testing.T) {
	e, f := rig(t)
	selectRoom(t, e, f, 1000)
	p := *e.State.Destination
	_ = e.Control("pause", 1100)
	_ = e.Tick(9000, nav(f))
	if e.State.State != "paused" || *e.State.Destination != p {
		t.Fatal(e.State)
	}
	_ = e.Control("resume", 10000)
	_ = e.Tick(10100, nav(f))
	if *e.State.Destination != p || e.State.State != "navigating" {
		t.Fatal(e.State)
	}
	_ = e.Control("stop", 10200)
	if e.Control("resume", 10300) == nil {
		t.Fatal("resumed stopped mission")
	}
}
func TestExpiryWhilePaused(t *testing.T) {
	e, f := rig(t)
	_ = e.Control("pause", 1000)
	_ = e.Tick(100000, nav(f))
	if e.State.State != "expired" || f.canceled < 2 {
		t.Fatal(e.State)
	}
}
func TestCaptureFailureCannotMarkCoverage(t *testing.T) {
	e, f := rig(t)
	f.captureErr = true
	selectRoom(t, e, f, 1000)
	complete(t, e, f, 2000)
	if e.State.Rooms[1].Visits != 0 || e.State.Rooms[1].BlockedReason == "" {
		t.Fatal(e.State)
	}
}
func TestSensorLossCancelsAndRetainsProgress(t *testing.T) {
	e, f := rig(t)
	selectRoom(t, e, f, 1000)
	p := *e.State.Destination
	_ = e.Tick(2000, Navigation{})
	if f.canceled != 1 || e.State.State != "waiting" {
		t.Fatal(e.State)
	}
	_ = e.Tick(3000, nav(f))
	if *e.State.Destination != p {
		t.Fatal(e.State)
	}
}
func TestFindingsDoNotRedirect(t *testing.T) {
	e, f := rig(t)
	selectRoom(t, e, f, 1000)
	complete(t, e, f, 2000)
	if len(e.State.Findings) != 1 {
		t.Fatal(e.State)
	}
	selectRoom(t, e, f, 5000)
	if e.State.CurrentRoom != "far" {
		t.Fatal("finding redirected passive patrol")
	}
}
func TestMovingCannotCapture(t *testing.T) {
	e, f := rig(t)
	selectRoom(t, e, f, 1000)
	n := nav(f)
	n.Goal.State = "succeeded"
	n.Speed = .55
	_ = e.Tick(2000, n)
	_ = e.Tick(5000, n)
	if f.captures != 0 {
		t.Fatal("captured while moving")
	}
}
