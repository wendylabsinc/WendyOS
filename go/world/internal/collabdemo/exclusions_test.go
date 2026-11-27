package collabdemo

import (
	"math"
	"reflect"
	"testing"
)

func exclusionFixture() Scene {
	return Scene{Grid: Grid{Origin: Vec{0, 0}, Resolution: .2, Width: 50, Height: 40}, Robots: []Robot{{ID: "atlas", Start: Vec{1, 4}}}, Jobs: []Job{{ID: "inspect", Label: "Inspection", Position: Vec{9, 4}}}}
}
func TestExclusionPreviewDetoursAndMatchesAuthorizedRoute(t *testing.T) {
	scene := exclusionFixture()
	c := coordinator(t, scene)
	in := initial(scene)
	in.Authorized = false
	in.Preview = &PreviewRequest{RobotID: "atlas", JobID: "inspect"}
	base := c.Tick(in)
	zero(t, base)
	if base.Preview == nil || !base.Preview.Reachable {
		t.Fatal(base)
	}
	advance(&in)
	in.Exclusions = []Exclusion{{ID: "maintenance", Min: Vec{4, 3}, Max: Vec{6, 5}}}
	o := c.Tick(in)
	zero(t, o)
	if !o.Preview.Reachable || o.Preview.Length <= base.Preview.Length || o.Preview.BaselineLength != base.Preview.Length {
		t.Fatal("missing alternative route", o.Preview)
	}
	for i := 1; i < len(o.Preview.Path); i++ {
		if !scene.Grid.clearSegment(o.Preview.Path[i-1], o.Preview.Path[i], func(cell Cell) bool { return scene.Grid.inside(cell) && !c.blocked[cell] }) {
			t.Fatal("preview cut padded exclusion")
		}
	}
	planned := append([]Vec(nil), o.Preview.Path...)
	advance(&in)
	in.Authorized = true
	o = c.Tick(in)
	if !o.ExclusionsFrozen || !reflect.DeepEqual(c.workers["atlas"].reserved, planned) || !reflect.DeepEqual(o.Robots[0].Path, planned[1:]) {
		t.Fatal("execution differs from reviewed one-robot route", o)
	}
	if len(scene.Grid.Blocked) != 0 {
		t.Fatal("operator exclusion rewrote base map")
	}
}
func TestExclusionsCanBlockDestinationAndBeRemovedBeforeStart(t *testing.T) {
	s := exclusionFixture()
	c := coordinator(t, s)
	in := initial(s)
	in.Authorized = false
	in.Preview = &PreviewRequest{"atlas", "inspect"}
	in.Exclusions = []Exclusion{{ID: "closed", Min: Vec{8.5, 3.5}, Max: Vec{9.5, 4.5}}}
	o := c.Tick(in)
	if o.Preview.Reachable || o.Preview.Reason == "" || len(o.Preview.BaselinePath) == 0 {
		t.Fatal(o)
	}
	advance(&in)
	in.Exclusions = nil
	o = c.Tick(in)
	if !o.Preview.Reachable || o.Preview.Length != o.Preview.BaselineLength || len(o.Exclusions) != 0 {
		t.Fatal("removal did not restore baseline", o)
	}
}
func TestExclusionValidationIsAtomicAndFrozenAfterStart(t *testing.T) {
	s := exclusionFixture()
	c := coordinator(t, s)
	in := initial(s)
	in.Authorized = false
	good := []Exclusion{{ID: "closed", Min: Vec{4, 3}, Max: Vec{6, 5}}}
	in.Exclusions = good
	if c.Tick(in).Phase != "awaiting_authorization" {
		t.Fatal("valid edit failed")
	}
	previous := c.blocked
	for _, invalid := range [][]Exclusion{
		{{ID: "bad", Min: Vec{1, 4}, Max: Vec{2, 5}}},
		{{ID: "bad", Min: Vec{-1, 0}, Max: Vec{1, 1}}},
		{{ID: "bad", Min: Vec{2, 2}, Max: Vec{2, 3}}},
		{{ID: "bad", Min: Vec{math.NaN(), 2}, Max: Vec{3, 3}}},
		{good[0], good[0]},
	} {
		advance(&in)
		in.Exclusions = invalid
		o := c.Tick(in)
		if o.Phase != "refused" || !sameExclusions(c.exclusions, good) || !reflect.DeepEqual(c.blocked, previous) {
			t.Fatal("bad edit mutated planner", o)
		}
		zero(t, o)
	}
	advance(&in)
	in.Exclusions = good
	in.Authorized = true
	if c.Tick(in).Phase != "running" {
		t.Fatal("valid mission did not start")
	}
	deadline := c.deadline
	reservation := append([]Vec(nil), c.workers["atlas"].reserved...)
	advance(&in)
	in.Exclusions = nil
	in.Robots[0].Online = false
	o := c.Tick(in)
	if o.Phase != "refused" || !reflect.DeepEqual(c.workers["atlas"].reserved, reservation) || c.deadline != deadline {
		t.Fatal("editing frozen exclusions changed cached mission", o)
	}
	zero(t, o)
	advance(&in)
	in.Exclusions = good
	o = c.Tick(in)
	if o.Robots[0].State != "offline_cached" {
		t.Fatal("offline autonomy changed", o)
	}
	in.Time = deadline
	advance(&in)
	o = c.Tick(in)
	if o.Phase != "expired" {
		t.Fatal("preview/edit extended deadline", o)
	}
}
func TestPreviewRespectsPeerReservations(t *testing.T) {
	s := exclusionFixture()
	s.Robots = append(s.Robots, Robot{ID: "peer", Start: Vec{5, 4}})
	c := coordinator(t, s)
	in := initial(s)
	in.Authorized = false
	in.Preview = &PreviewRequest{"atlas", "inspect"}
	o := c.Tick(in)
	if o.Preview == nil || !o.Preview.Reachable {
		t.Fatal(o)
	}
	for i := 1; i < len(o.Preview.Path); i++ {
		if segmentDistance(Vec{5, 4}, o.Preview.Path[i-1], o.Preview.Path[i]) < Separation {
			t.Fatal("preview ignored peer")
		}
	}
}
