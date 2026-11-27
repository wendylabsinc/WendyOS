package patrol

import (
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/patroltour"
	"math"
)

type TourState struct {
	Policy  patroltour.Policy  `json:"policy"`
	Visits  []patroltour.Visit `json:"visits"`
	Outcome string             `json:"outcome"`
}

func (e *Engine) SetTour(p patroltour.Policy) error {
	if e.State.Tour != nil || e.goalNumber != 0 || e.State.State != "waiting" || e.State.Return == nil || p.Validate() != nil || len(p.Stops) != len(e.State.Rooms) {
		return errors.New("tour must be configured once with the signed rooms and return before dispatch")
	}
	for i, s := range p.Stops {
		if s.RoomID != e.State.Rooms[i].ID || !inside(e.State.Rooms[i].Polygon, Point{X: s.Position[0], Y: -s.Position[2]}) {
			return errors.New("tour destination must be inside its commissioned room")
		}
	}
	raw, _ := json.Marshal(p)
	var owned patroltour.Policy
	_ = json.Unmarshal(raw, &owned)
	e.State.Tour = &TourState{Policy: owned, Visits: []patroltour.Visit{}, Outcome: "pending"}
	return nil
}
func (e *Engine) holdTour(reason string) error {
	e.State.State = "tour_held"
	e.State.Reason = reason
	e.settledAt = 0
	e.State.Tour.Outcome = "incomplete"
	return e.adapter.Cancel()
}
func (e *Engine) chooseTour(now int64) error {
	t := e.State.Tour
	if len(t.Visits) == len(t.Policy.Stops) {
		t.Outcome = "returning"
		return e.recall(now, "inspection_complete")
	}
	s := t.Policy.Stops[len(t.Visits)]
	p := Point{X: s.Position[0], Y: -s.Position[2], Yaw: s.Yaw}
	route, err := e.adapter.Plan(p)
	if err != nil || !route.Reachable {
		return e.holdTour("Inspection incomplete: no current route to the signed observation point. Resume to retry.")
	}
	e.State.CurrentRoom = s.RoomID
	e.State.Destination = &p
	t.Outcome = "inspecting"
	return e.dispatch(now, p)
}
func (e *Engine) recordTourCapture(c Capture) error {
	t := e.State.Tour
	i := len(t.Visits)
	if i >= len(t.Policy.Stops) || e.State.CurrentRoom != t.Policy.Stops[i].RoomID || c.SampleCount != 5 || !finite(c.TimeS) || c.TimeS <= 0 || c.TimeS < e.tourLastStamp || c.TimeS > e.tourLastStamp+1 || c.TimeS-e.tourFirstStamp < 2 {
		return errors.New("Inspection incomplete: original five-sensor capture required")
	}
	visit := patroltour.Visit{RoomID: e.State.CurrentRoom, SimulationNS: int64(math.Round(c.TimeS * 1e9)), ManifestSHA256: c.ManifestSHA256, Evidence: append([]string(nil), c.Evidence...)}
	evidence := &patroltour.Evidence{Visits: append(append([]patroltour.Visit(nil), t.Visits...), visit)}
	prefix := t.Policy
	prefix.Stops = prefix.Stops[:i+1]
	if !prefix.Complete(evidence, visit.SimulationNS) {
		return errors.New("Inspection incomplete: capture identities did not advance or evidence is invalid")
	}
	t.Visits = evidence.Visits
	t.Outcome = "inspecting"
	return nil
}
func (e *Engine) TourEvidence() *patroltour.Evidence {
	if e.State.Tour == nil {
		return nil
	}
	raw, _ := json.Marshal(patroltour.Evidence{Visits: e.State.Tour.Visits})
	var v patroltour.Evidence
	_ = json.Unmarshal(raw, &v)
	return &v
}
func (e *Engine) TourComplete() bool {
	return e.State.Tour == nil || e.State.Tour.Policy.Complete(e.TourEvidence(), math.MaxInt64)
}

// Tour observations require bounded, strictly advancing original sensor time.
func (e *Engine) tourSettled(now int64, n Navigation) bool {
	if !finite(n.Stamp) || n.Stamp <= 0 || n.Stamp < e.tourLastStamp {
		e.tourFirstStamp = 0
		e.tourLastStamp = 0
		e.tourLastReceiptMS = 0
		e.settledAt = 0
		return false
	}
	if n.Stamp == e.tourLastStamp {
		return false
	}
	if e.settledAt == 0 || e.tourLastReceiptMS == 0 || now-e.tourLastReceiptMS > 1000 || n.Stamp-e.tourLastStamp > 1 {
		e.tourFirstStamp = n.Stamp
		e.settledAt = now
	}
	e.tourLastStamp = n.Stamp
	e.tourLastReceiptMS = now
	return now-e.settledAt >= 2000 && n.Stamp-e.tourFirstStamp >= 2
}
