package presence

import (
	"encoding/json"
	"github.com/wendylabsinc/wendy/go/world/internal/sensing"
	"github.com/wendylabsinc/wendy/go/world/internal/tracking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

var anchor = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
var frame = world.FrameRef{ID: "floor", Revision: "1"}

func engine(t *testing.T) *Engine {
	t.Helper()
	e, err := New([]Zone{{"desk", "Desk", [2]float64{-1, -1}, [2]float64{1, 1}, 1}}, "run", "simulation:run", frame)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func track(id string, seconds, x float64) tracking.Track {
	at := anchor.Add(time.Duration(seconds * 1e9))
	until := at.Add(1250 * time.Millisecond)
	member := tracking.Detection{Source: "camera-a", SampleID: id + at.Format(time.RFC3339Nano), Version: world.Version{Epoch: 1, Sequence: uint64(seconds*1000) + 1}, SampleTime: at, ValidUntil: until, RunID: "run", ClockDomain: "simulation:run", Frame: frame, Evidence: []sensing.EvidenceRef{{SHA256: strings.Repeat("a", 64), ContentType: "image/png", Size: 10}}}
	return tracking.Track{ID: id, State: "observed", Time: at, LastObserved: at, ValidUntil: until, RunID: "run", ClockDomain: "simulation:run", Frame: frame, ObservedPositionXZ: [2]float64{x, 0}, ObservedCovariance: [4]float64{.01, 0, 0, .01}, PositionXZ: [2]float64{x, 0}, Covariance: [4]float64{.01, 0, 0, .01}, Members: []tracking.Detection{member}}
}
func step(t *testing.T, e *Engine, seconds float64, tracks ...tracking.Track) Output {
	t.Helper()
	v, err := e.Step(anchor.Add(time.Duration(seconds*1e9)), tracks)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func kinds(v Output) []string {
	out := []string{}
	for _, e := range v.Events {
		out = append(out, e.Kind)
	}
	return out
}
func TestEntryDwellExitRetainsOriginalEvidence(t *testing.T) {
	e := engine(t)
	first := track("anon/1", 0, 0)
	v := step(t, e, 0, first)
	if v.Zones[0].Observed != 1 || !reflect.DeepEqual(v.Events[0].Inputs, first.Members) {
		t.Fatal(v)
	}
	v = step(t, e, .5, track("anon/1", .5, .1))
	v = step(t, e, 1, track("anon/1", 1, .2))
	if !reflect.DeepEqual(kinds(v), []string{"entered", "dwell"}) || v.Zones[0].Occupants[0].DwellSeconds != 1 {
		t.Fatal(v)
	}
	outside := track("anon/1", 1.5, 2)
	v = step(t, e, 1.5, outside)
	if !reflect.DeepEqual(kinds(v), []string{"entered", "dwell", "exited"}) || v.Zones[0].State != "unknown" || !reflect.DeepEqual(v.Events[2].Inputs, outside.Members) {
		t.Fatal(v)
	}
	v.Events[0].Inputs[0].Evidence[0].SHA256 = "mutated"
	if step(t, e, 2).Events[0].Inputs[0].Evidence[0].SHA256 == "mutated" {
		t.Fatal("output aliases evidence")
	}
}
func TestMissingCoastingBoundaryAndAmbiguityNeverExit(t *testing.T) {
	for _, reason := range []string{"missing", "expired", "coasting", "boundary", "ambiguous"} {
		t.Run(reason, func(t *testing.T) {
			e := engine(t)
			step(t, e, 0, track("a", 0, 0))
			at := .5
			next := track("a", at, 0)
			batch := []tracking.Track{next}
			switch reason {
			case "missing":
				batch = nil
			case "expired":
				at = 1.25
				batch = nil
			case "coasting":
				batch[0].State = "coasting"
			case "boundary":
				batch[0].PositionXZ[0] = .9
				batch[0].ObservedPositionXZ[0] = .9
			case "ambiguous":
				batch[0].Ambiguous = true
			}
			v := step(t, e, at, batch...)
			want := "lost"
			if reason == "boundary" {
				want = "uncertain"
			}
			if !reflect.DeepEqual(kinds(v), []string{"entered", want}) || v.Zones[0].Observed != 0 {
				t.Fatal(v)
			}
			// Recovery starts new anonymous dwell rather than bridging a gap.
			at += .25
			v = step(t, e, at, track("a", at, 0))
			if v.Zones[0].Occupants[0].DwellSeconds != 0 || v.Events[len(v.Events)-1].Kind != "entered" {
				t.Fatal(v)
			}
		})
	}
}
func TestDuplicateDoesNotExtendDwellAndRestartLosesHistory(t *testing.T) {
	e := engine(t)
	tr := track("a", 0, 0)
	step(t, e, 0, tr)
	tr.Time = anchor.Add(time.Second)
	v := step(t, e, 1, tr)
	if len(v.Events) != 1 || v.Zones[0].Occupants[0].DwellSeconds != 0 || !v.Zones[0].Occupants[0].ValidUntil.Equal(anchor.Add(1250*time.Millisecond)) {
		t.Fatal(v)
	}
	tr.Time = anchor.Add(1250 * time.Millisecond)
	v = step(t, e, 1.25, tr)
	if v.Zones[0].Observed != 0 || v.Events[1].Kind != "lost" {
		t.Fatal(v)
	}
	v = step(t, engine(t), 2, track("a", 2, 0))
	if len(v.Events) != 1 || v.Zones[0].Occupants[0].DwellSeconds != 0 {
		t.Fatal("restart fabricated history")
	}
}
func TestInvalidBatchAtomicAndDeterministic(t *testing.T) {
	for _, problem := range []string{"frame", "nan", "observed-covariance", "duplicate", "future", "member"} {
		t.Run(problem, func(t *testing.T) {
			e := engine(t)
			first := step(t, e, 0, track("a", 0, 0))
			bad := track("a", .5, 0)
			batch := []tracking.Track{bad}
			switch problem {
			case "frame":
				batch[0].Frame.Revision = "2"
			case "nan":
				batch[0].Covariance[0] = math.NaN()
			case "observed-covariance":
				batch[0].ObservedCovariance = [4]float64{}
			case "duplicate":
				batch = append(batch, bad)
			case "future":
				batch[0].LastObserved = anchor.Add(2 * time.Second)
			case "member":
				batch[0].Members[0].Frame.Revision = "2"
			}
			if _, err := e.Step(anchor.Add(time.Second/2), batch); err == nil {
				t.Fatal("invalid batch accepted")
			}
			if !reflect.DeepEqual(kinds(first), kinds(step(t, e, 0, track("a", 0, 0)))) {
				t.Fatal("invalid batch mutated state")
			}
		})
	}
	a, b := engine(t), engine(t)
	one, two := track("a", 0, -.4), track("b", 0, .4)
	av := step(t, a, 0, one, two)
	bv := step(t, b, 0, two, one)
	aj, _ := json.Marshal(av)
	bj, _ := json.Marshal(bv)
	if string(aj) != string(bj) {
		t.Fatal("input order changed output")
	}
}
func TestRetentionBoundsAndConfig(t *testing.T) {
	e := engine(t)
	for i := 0; i < 200; i++ {
		at := float64(i) / 10
		if i%2 == 0 {
			step(t, e, at, track("a", at, 0))
		} else {
			step(t, e, at)
		}
	}
	if len(e.events) != MaxEvents {
		t.Fatal(len(e.events))
	}
	if len(step(t, e, 100).Events) != 0 {
		t.Fatal("history retained past bound")
	}
	for _, zones := range [][]Zone{nil, {{ID: "x", MinXZ: [2]float64{1, 0}, MaxXZ: [2]float64{0, 1}, DwellSeconds: 1}}, {{ID: "x", MinXZ: [2]float64{0, 0}, MaxXZ: [2]float64{1, 1}, DwellSeconds: math.NaN()}}} {
		if _, err := New(zones, "run", "clock", frame); err == nil {
			t.Fatal("invalid zones accepted")
		}
	}
}

func TestObservedBoundaryCrossingExitsButCannotAccumulateDwell(t *testing.T) {
	e := engine(t)
	step(t, e, 0, track("a", 0, 0))
	v := step(t, e, .5, track("a", .5, .9))
	if v.Events[1].Kind != "uncertain" {
		t.Fatal(v)
	}
	v = step(t, e, 1, track("a", 1, 1.4))
	if v.Events[len(v.Events)-1].Kind != "exited" {
		t.Fatal(v)
	}
	for _, event := range v.Events {
		if event.Kind == "dwell" {
			t.Fatal("boundary counted as dwell")
		}
	}
}
