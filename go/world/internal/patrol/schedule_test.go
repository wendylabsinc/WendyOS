package patrol

import "testing"

func scheduledRig(t *testing.T) (*Engine, *fake) {
	t.Helper()
	e, f := rig(t)
	if err := e.SetSchedule(map[string]int{"near": 10, "far": 30}, 1000); err != nil {
		t.Fatal(err)
	}
	return e, f
}
func TestScheduledVisitsWaitUntilDueAndUseIndependentWallClock(t *testing.T) {
	e, f := scheduledRig(t)
	selectRoom(t, e, f, 1000)
	complete(t, e, f, 1100)
	if e.State.Rooms[1].NextDueMS != 13200 || *e.State.Rooms[1].LastObservedS != 10 {
		t.Fatal(e.State.Rooms)
	}
	selectRoom(t, e, f, 3300)
	if e.State.CurrentRoom != "far" {
		t.Fatal("initial survey skipped far room")
	}
	complete(t, e, f, 3400)
	for range 5 {
		_ = e.Tick(5600, nav(f))
	}
	if e.State.State != "scheduled" || e.State.CurrentRoom != "" {
		t.Fatal("early repeated capture", e.State)
	}
	selectRoom(t, e, f, 13200)
	if e.State.CurrentRoom != "near" {
		t.Fatal("short revisit interval not used")
	}
	complete(t, e, f, 13300)
	selectRoom(t, e, f, 35500)
	// Near was due at 25.4s; far at 35.5s. Earliest due remains first.
	if e.State.CurrentRoom != "near" {
		t.Fatal("overdue ordering lost")
	}
	complete(t, e, f, 35600)
	selectRoom(t, e, f, 37800)
	if e.State.CurrentRoom != "far" {
		t.Fatal("longer interval room starved by frequent room")
	}
}
func TestScheduleDoesNotAdvanceOnFailureOrPause(t *testing.T) {
	e, f := scheduledRig(t)
	selectRoom(t, e, f, 1000)
	f.captureErr = true
	complete(t, e, f, 1200)
	if e.State.Rooms[1].NextDueMS != 1000 || e.State.Rooms[1].Visits != 0 {
		t.Fatal("failed evidence cleared overdue coverage")
	}
	selectRoom(t, e, f, 3400)
	if e.State.CurrentRoom != "far" {
		t.Fatal("blocked due room starved accessible room")
	}
	if err := e.Control("pause", 3500); err != nil {
		t.Fatal(err)
	}
	_ = e.Tick(20000, nav(f))
	if e.State.Rooms[0].NextDueMS != 1000 || e.State.Rooms[1].NextDueMS != 1000 {
		t.Fatal("pause moved deadline")
	}
	if err := e.Control("resume", 21000); err != nil {
		t.Fatal(err)
	}
	_ = e.Tick(100000, nav(f))
	if e.State.State != "expired" {
		t.Fatal("schedule extended mission")
	}
}
func TestScheduleRejectsIncompleteOrChangedPolicy(t *testing.T) {
	for _, schedule := range []map[string]int{{"near": 10}, {"near": 10, "unknown": 20}, {"near": 9, "far": 20}, {"near": 10, "far": 86401}} {
		e, _ := rig(t)
		if e.SetSchedule(schedule, 1000) == nil {
			t.Fatal("accepted invalid schedule", schedule)
		}
		if e.State.Rooms[0].NextDueMS != 0 {
			t.Fatal("invalid policy partially mutated engine")
		}
	}
	e, f := scheduledRig(t)
	selectRoom(t, e, f, 1000)
	if e.SetSchedule(map[string]int{"near": 20, "far": 40}, 2000) == nil {
		t.Fatal("changed running policy")
	}
}
