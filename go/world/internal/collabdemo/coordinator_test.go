package collabdemo

import (
	"reflect"
	"strings"
	"testing"
)

func fixture() Scene {
	return Scene{Grid: Grid{Origin: Vec{-7.5, -7.5}, Resolution: .3, Width: 50, Height: 50},
		Robots: []Robot{{ID: "atlas", Start: Vec{-4.95, -4.95}}, {ID: "iris", Start: Vec{.15, -4.95}}, {ID: "echo", Start: Vec{4.95, -4.95}}},
		Jobs:   []Job{{ID: "a", Label: "Office A", Position: Vec{-4.95, 1.05}}, {ID: "b", Label: "Office B", Position: Vec{.15, 1.05}}, {ID: "c", Label: "Office C", Position: Vec{4.95, 1.05}}, {ID: "d", Label: "Office D", Position: Vec{-4.95, 4.95}}, {ID: "e", Label: "Office E", Position: Vec{.15, 4.95}}, {ID: "f", Label: "Office F", Position: Vec{4.95, 4.95}}}}
}
func initial(s Scene) Input {
	in := Input{RunID: "synthetic-test", Sequence: 1, Authorized: true}
	for _, r := range s.Robots {
		in.Robots = append(in.Robots, Observation{ID: r.ID, Position: r.Start, Online: true, Available: true})
	}
	return in
}
func advance(in *Input) { in.Sequence++; in.Time += .2 }
func coordinator(t *testing.T, s Scene) *Coordinator {
	t.Helper()
	c, e := New(s)
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func zero(t *testing.T, o Output) {
	t.Helper()
	for _, r := range o.Robots {
		if r.Velocity != (Vec{}) {
			t.Fatalf("unexpected motion: %+v", o)
		}
	}
}

func TestConcurrentAssignmentUniqueAndReserved(t *testing.T) {
	s := fixture()
	c := coordinator(t, s)
	o := c.Tick(initial(s))
	owners := map[string]string{}
	for _, r := range o.Robots {
		if r.JobID == "" {
			t.Fatalf("robot lacks concurrent assignment: %+v", o)
		}
		if old := owners[r.JobID]; old != "" {
			t.Fatalf("double owner %s %s", old, r.ID)
		}
		owners[r.JobID] = r.ID
	}
	for i, a := range o.Robots {
		for j, b := range o.Robots {
			if i >= j {
				continue
			}
			for _, p := range a.Path {
				if nearPath(p, b.Path, Separation) {
					t.Fatalf("conflicting paths %s %s", a.ID, b.ID)
				}
			}
		}
	}
}

func TestCompleteMeasuredMissionsExactlyOnce(t *testing.T) {
	s := fixture()
	c := coordinator(t, s)
	in := initial(s)
	events := map[string]int{}
	for tick := 0; tick < 1800; tick++ {
		o := c.Tick(in)
		for _, e := range o.Events {
			if strings.Contains(e, " completed ") {
				events[e]++
			}
		}
		if o.Phase == "completed" {
			if o.Completed != len(s.Jobs) || len(events) != len(s.Jobs) {
				t.Fatalf("incomplete results %+v events=%v", o, events)
			}
			for _, n := range events {
				if n != 1 {
					t.Fatalf("duplicate completion %v", events)
				}
			}
			for _, j := range o.Jobs {
				if j.Reading == nil || j.Reading.Provenance != "synthetic-analytic" {
					t.Fatalf("unlabeled result %+v", j)
				}
			}
			zero(t, o)
			return
		}
		for i, r := range o.Robots {
			in.Robots[i].Velocity = r.Velocity
			for axis := range 2 {
				in.Robots[i].Position[axis] += r.Velocity[axis] * .2
			}
		}
		for i, a := range in.Robots {
			for j, b := range in.Robots {
				if i < j && distance(a.Position, b.Position) < Separation {
					t.Fatalf("robots too close: %v %v", a, b)
				}
			}
		}
		advance(&in)
	}
	t.Fatal("missions did not finish")
}

func TestDuplicatesReorderingAndRunChangeHaveNoEffects(t *testing.T) {
	s := fixture()
	c := coordinator(t, s)
	in := initial(s)
	first := c.Tick(in)
	sequence := c.sequence
	for _, bad := range []Input{in, {RunID: "other", Sequence: 2, Time: 1, Authorized: true, Robots: in.Robots}, {RunID: in.RunID, Sequence: 2, Time: 0, Authorized: true, Robots: in.Robots}} {
		o := c.Tick(bad)
		if o.Phase != "refused" {
			t.Fatal(o)
		}
		zero(t, o)
		if c.sequence != sequence || !reflect.DeepEqual(first.Jobs, o.Jobs) {
			t.Fatal("rejected input mutated state")
		}
	}
	advance(&in)
	if c.Tick(in).Phase != "running" {
		t.Fatal("valid progress did not recover")
	}
}

func TestPartitionRetainsUnknownWorkAndReservations(t *testing.T) {
	s := fixture()
	c := coordinator(t, s)
	in := initial(s)
	first := c.Tick(in)
	w := c.workers["atlas"]
	job := w.job
	path := append([]Vec(nil), w.reserved...)
	known := w.position
	in.Robots[0].Online = false
	in.Robots[0].Available = false
	// Even supplied local data cannot advance coordinator state while isolated.
	in.Robots[0].Position = s.Jobs[job].Position
	for range 20 {
		advance(&in)
		out := c.Tick(in)
		if out.Robots[0].State != "offline_cached" || out.Robots[0].Velocity != (Vec{}) {
			t.Fatal(out)
		}
		if out.Jobs[job].State != "assigned" || out.Jobs[job].AssignedTo != "atlas" {
			t.Fatal("offline task reassigned")
		}
	}
	if !reflect.DeepEqual(w.reserved, path) || w.position != known || w.job != job {
		t.Fatal("offline knowledge changed")
	}
	in.Robots[0].Online = true
	in.Robots[0].Available = true
	advance(&in)
	out := c.Tick(in)
	if out.Jobs[job].State == "completed" {
		t.Fatal("offline elapsed time counted toward inspection")
	}
	for range 12 {
		advance(&in)
		out = c.Tick(in)
	}
	if out.Jobs[job].State != "completed" || first.Robots[0].JobID != out.Jobs[job].ID {
		t.Fatal("reconciliation failed", out)
	}
}

func TestPausedWorkerReleasesOnlyAfterObservedStop(t *testing.T) {
	s := fixture()
	c := coordinator(t, s)
	in := initial(s)
	first := c.Tick(in)
	job := c.workers["atlas"].job
	in.Robots[0].Available = false
	in.Robots[0].Velocity = Vec{.1, 0}
	advance(&in)
	out := c.Tick(in)
	if out.Jobs[job].AssignedTo != "atlas" || out.Robots[0].State != "stopping" || out.Robots[0].Velocity != (Vec{}) {
		t.Fatal(out)
	}
	in.Robots[0].Velocity = Vec{}
	advance(&in)
	out = c.Tick(in)
	if out.Jobs[job].AssignedTo == "atlas" || out.Robots[0].JobID != "" || first.Robots[0].JobID == "" {
		t.Fatal("stopped job not released", out)
	}
}

func TestStopExpiryAndFreshProcessRequireAuthorization(t *testing.T) {
	s := fixture()
	c := coordinator(t, s)
	in := initial(s)
	in.Authorized = false
	zero(t, c.Tick(in))
	if c.started {
		t.Fatal("unauthorized start")
	}
	advance(&in)
	in.Authorized = true
	c.Tick(in)
	advance(&in)
	in.Stop = true
	out := c.Tick(in)
	zero(t, out)
	if out.Phase != "stopped" {
		t.Fatal(out)
	}
	advance(&in)
	in.Stop = false
	out = c.Tick(in)
	zero(t, out)
	if out.Phase != "stopped" {
		t.Fatal("stop unlatches", out)
	}
	crashed := coordinator(t, s)
	fresh := initial(s)
	fresh.Authorized = false
	out = crashed.Tick(fresh)
	zero(t, out)
	for _, j := range out.Jobs {
		if j.AssignedTo != "" {
			t.Fatal("restart restored execution")
		}
	}
	expiring := coordinator(t, s)
	fresh.Authorized = true
	expiring.Tick(fresh)
	fresh.Time = 600
	fresh.Sequence = 2
	out = expiring.Tick(fresh)
	zero(t, out)
	if out.Phase != "expired" {
		t.Fatal(out)
	}
}

func TestBlockedGridAndPeerRoutesRefusePlans(t *testing.T) {
	s := fixture()
	for y := 0; y < 50; y++ {
		s.Grid.Blocked = append(s.Grid.Blocked, Cell{20, y})
	}
	c := coordinator(t, s)
	if p := c.plan(Vec{-2, -2}, Vec{2, -2}, nil); len(p) > 0 {
		t.Fatal("crossed blocked wall")
	}
	c = coordinator(t, fixture())
	barrier := [][]Vec{{{-7, -.5}, {7, -.5}}}
	if p := c.plan(Vec{0, -2}, Vec{0, 2}, barrier); len(p) > 0 {
		t.Fatal("crossed reserved route")
	}
	if c.scene.Grid.center(Cell{0, 0}) != (Vec{-7.35, -7.35}) {
		t.Fatal("grid origin is not a corner")
	}
}
