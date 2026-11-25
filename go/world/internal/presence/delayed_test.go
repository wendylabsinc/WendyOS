package presence

import (
	"github.com/wendylabsinc/wendy/go/world/internal/tracking"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func detectionAt(seconds, x float64) tracking.Detection {
	d := track("sample", seconds, x).Members[0]
	d.PositionXZ = [2]float64{x, 0}
	d.Covariance = [4]float64{.04, 0, 0, .04}
	d.ModelID = "model"
	d.ModelVersion = "1"
	d.ModelSHA256 = strings.Repeat("a", 64)
	return d
}
func TestTrackerDelayedInside(t *testing.T) {
	e, err := New([]Zone{{"room", "Room", [2]float64{-5, -5}, [2]float64{1, 5}, .5}}, "run", "simulation:run", frame)
	if err != nil {
		t.Fatal(err)
	}
	tr, err := tracking.New(tracking.Config{TrackerID: "r", RunID: "run", ClockDomain: "simulation:run", Frame: frame, MaxMiss: 1250 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	var id string
	for i, x := range []float64{-2, -1.4, -.8, -.2, .4, .55} {
		sample := float64(i) * .2
		now := sample
		if i == 5 {
			now += .9
		}
		tracks, err := tr.Step(anchor.Add(time.Duration(now*1e9)), []tracking.Detection{detectionAt(sample, x)})
		if err != nil || len(tracks) != 1 {
			t.Fatalf("tracks=%+v err=%v", tracks, err)
		}
		if i == 0 {
			id = tracks[0].ID
		}
		if tracks[0].ID != id {
			t.Fatal("identity changed")
		}
		out := step(t, e, now, tracks...)
		if i == 5 {
			a := tracks[0]
			if a.ObservedPositionXZ[0]+2*math.Sqrt(a.ObservedCovariance[0]) >= 1 || a.PositionXZ[0]-2*math.Sqrt(a.Covariance[0]) <= 1 {
				t.Fatalf("fixture must observe inside and predict outside: %+v", a)
			}
			if !reflect.DeepEqual(kinds(out), []string{"entered", "dwell"}) {
				t.Fatalf("prediction invented transition: %v", kinds(out))
			}
			z := out.Zones[0]
			if z.State != "uncertain" || z.Observed != 0 || z.Possible != 1 || !z.Occupants[0].LastObserved.Equal(a.LastObserved) {
				t.Fatalf("false current occupancy: %+v", z)
			}
		}
	}
	// A later measured outside pose still establishes a genuine exit, even delayed.
	d := detectionAt(1.2, 1.8)
	tracks, err := tr.Step(anchor.Add(2100*time.Millisecond), []tracking.Detection{d})
	if err != nil || len(tracks) != 1 || tracks[0].ID != id {
		t.Fatalf("outside association: %+v %v", tracks, err)
	}
	out := step(t, e, 2.1, tracks...)
	if !reflect.DeepEqual(kinds(out), []string{"entered", "dwell", "exited"}) {
		t.Fatalf("missing observed exit: %v", kinds(out))
	}
	event := out.Events[2]
	if !event.At.Equal(d.SampleTime) || !reflect.DeepEqual(event.Inputs, tracks[0].Members) {
		t.Fatalf("exit rewrote evidence: %+v", event)
	}
}
func TestDelayedOutsidePrediction(t *testing.T) {
	e := engine(t)
	a := track("a", 0, 2)
	a.PositionXZ = [2]float64{0, 0}
	a.Time = anchor.Add(500 * time.Millisecond)
	out := step(t, e, .5, a)
	if len(out.Events) != 0 || out.Zones[0].Observed != 0 || out.Zones[0].Possible != 1 {
		t.Fatalf("prediction invented inside evidence: %+v", out)
	}
}
func TestDelayedInsideDwell(t *testing.T) {
	e := engine(t)
	a := track("a", 0, 0)
	a.Time = anchor.Add(500 * time.Millisecond)
	out := step(t, e, .5, a)
	if len(out.Events) != 1 || !out.Events[0].At.Equal(anchor) || out.Zones[0].Observed != 1 {
		t.Fatal(out)
	}
	a = track("a", 1, 0)
	a.Time = anchor.Add(1100 * time.Millisecond)
	out = step(t, e, 1.1, a)
	if !reflect.DeepEqual(kinds(out), []string{"entered", "dwell"}) || out.Events[1].DwellSeconds != 1 || !out.Events[1].At.Equal(a.LastObserved) {
		t.Fatal(out)
	}
}
func TestOutsideAfterExpiry(t *testing.T) {
	e := engine(t)
	step(t, e, 0, track("a", 0, 0))
	out := step(t, e, 2, track("a", 2, 2))
	if !reflect.DeepEqual(kinds(out), []string{"entered", "lost"}) {
		t.Fatal(kinds(out))
	}
}
