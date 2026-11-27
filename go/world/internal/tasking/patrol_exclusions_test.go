package tasking

import (
	"encoding/json"
	"fmt"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"testing"
)

func exclusionPolicy() *PatrolExclusions {
	p := &PatrolExclusions{Frame: world.FrameRef{ID: "office", Revision: "1"}, Revision: 1, Rectangles: []PatrolRectangle{{ID: "area-a", MinMM: []int{0, 0}, MaxMM: []int{1000, 1000}}}}
	p.SHA256 = p.Digest()
	return p
}
func TestRestrictedAreasFrameDigestAndGeometry(t *testing.T) {
	p := exclusionPolicy()
	area := Bounds{Min: [3]float64{-9, -1, -7}, Max: [3]float64{9, 2, 7}}
	if err := p.Validate(p.Frame, area); err != nil {
		t.Fatal(err)
	}
	for _, test := range []string{"digest", "frame", "missing-corner", "reversed", "outside", "duplicate", "revision"} {
		t.Run(test, func(t *testing.T) {
			p := exclusionPolicy()
			switch test {
			case "digest":
				p.Rectangles[0].MaxMM[0] = 1200
			case "frame":
				p.Frame.Revision = "2"
			case "missing-corner":
				p.Rectangles[0].MinMM = nil
			case "reversed":
				p.Rectangles[0].MinMM[0] = 2000
			case "outside":
				p.Rectangles[0].MaxMM[1] = 8000
			case "duplicate":
				p.Rectangles = append(p.Rectangles, p.Rectangles[0])
			case "revision":
				p.Revision = 2
			}
			if test != "digest" {
				p.SHA256 = p.Digest()
			}
			if p.Validate(world.FrameRef{ID: "office", Revision: "1"}, area) == nil {
				t.Fatal("invalid restriction accepted")
			}
		})
	}
}
func TestRestrictedAreasUseWholeGo2FootprintAndWorldAxes(t *testing.T) {
	p := exclusionPolicy()
	r := PatrolExclusionRadius()
	if p.Clear(-r, 0.5) || !p.Clear(-r-.001, .5) {
		t.Fatal("footprint boundary not conservative")
	}
	if p.Clear(1+r/2, 1+r/2) {
		t.Fatal("corner clips bounding footprint")
	}
	s := Spec{Patrol: &PatrolPolicy{Exclusions: p}}
	if patrolPositionClear(s, Telemetry{Position: [3]float64{.5, .3, -.5}}) {
		t.Fatal("World Z sign ignored")
	}
	if !patrolPositionClear(s, Telemetry{Position: [3]float64{.5, .3, 2}}) {
		t.Fatal("clear world position refused")
	}
}

func TestPatrolEvaluationHoldsRestrictedPosition(t *testing.T) {
	r, s, tele := patrolRig(t)
	s.ID = "restricted-patrol"
	p := exclusionPolicy()
	p.Frame = s.Frame
	p.SHA256 = p.Digest()
	s.Patrol.Exclusions = p
	if _, err := r.s.Accept(s, r.auth()); err != nil {
		t.Fatal(err)
	}
	tele.Position = [3]float64{-2, 0, 0}
	if d := patrolEvaluate(t, r, s, tele); d.Action != mission.Continue {
		t.Fatal(d)
	}
	tele.Position = [3]float64{.5, 0, -.5}
	if d := patrolEvaluate(t, r, s, tele); d.Action != mission.Hold || d.Record.Status.Phase == mission.Succeeded {
		t.Fatal("restriction did not hold motion", d)
	}
	// Mutating a caller's copy does not remove the retained policy.
	p.Rectangles = nil
	if d := patrolEvaluate(t, r, s, tele); d.Action != mission.Hold {
		t.Fatal("caller removed retained restriction", d)
	}
}

func TestRestrictedRectangleRejectsUnknownCoordinates(t *testing.T) {
	for _, corner := range []string{"min_mm", "max_mm"} {
		for _, value := range []string{`[null,0]`, `[0,null]`, `null`, `[]`, `[0]`, `[0,0,0]`, `[0.5,0]`, `[false,0]`} {
			t.Run(corner+value, func(t *testing.T) {
				low, high := "[0,0]", "[1000,1000]"
				if corner == "min_mm" {
					low = value
				} else {
					high = value
				}
				raw := fmt.Sprintf(`{"id":"area","min_mm":%s,"max_mm":%s}`, low, high)
				var rectangle PatrolRectangle
				if json.Unmarshal([]byte(raw), &rectangle) == nil {
					t.Fatal("unknown coordinate became usable geometry", raw)
				}
			})
		}
	}
	for _, raw := range []string{`{"id":"area","max_mm":[1000,1000]}`, `{"id":"area","min_mm":[0,0]}`, `{"id":"area","min_mm":[0,0],"max_mm":[1000,1000],"extra":0}`} {
		var rectangle PatrolRectangle
		if json.Unmarshal([]byte(raw), &rectangle) == nil {
			t.Fatal("missing/extra geometry field accepted", raw)
		}
	}
	var rectangle PatrolRectangle
	if err := json.Unmarshal([]byte(`{"id":"area","min_mm":[0,0],"max_mm":[1000,1000]}`), &rectangle); err != nil || rectangle.MinMM[0] != 0 {
		t.Fatal("explicit zero refused", err)
	}
}
