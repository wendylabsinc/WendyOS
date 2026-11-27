package tasking

import (
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/mission"
	"github.com/wendylabsinc/wendy/go/world/internal/patroltour"
	"strings"
	"testing"
	"time"
)

func TestTourCompletionRequiresExactOriginalCoverage(t *testing.T) {
	for _, bad := range []string{"", "missing", "wrong-room", "future", "missing-original", "expired", "revoked"} {
		t.Run(bad, func(t *testing.T) {
			r, s, tele := patrolRig(t)
			s.ID = "finite-tour"
			s.Patrol.Return = &PatrolReturn{Position: []float64{1, 0, 1}}
			s.Patrol.Tour = &patroltour.Policy{Method: patroltour.Method, Stops: []patroltour.Stop{{RoomID: "room", Position: []float64{1, .3, 1}}}}
			if _, err := r.s.Accept(s, r.auth()); err != nil {
				t.Fatal(err)
			}
			v := patroltour.Visit{RoomID: "room", SimulationNS: 1000000000, ManifestSHA256: strings.Repeat("a", 64), Evidence: []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64)}}
			tele.Position = [3]float64{1, 0, 1}
			tele.SampleSimulationNS = 5000000000
			tele.PatrolArrival = &PatrolArrival{MissionID: s.ID, SettledSinceUnixMS: r.wall.Add(-3 * time.Second).UnixMilli(), ObservedAtUnixMS: r.wall.UnixMilli(), FirstSimulationNS: 2000000000, LastSimulationNS: 5000000000, Tour: &patroltour.Evidence{Visits: []patroltour.Visit{v}}}
			switch bad {
			case "missing":
				tele.PatrolArrival.Tour = nil
			case "wrong-room":
				tele.PatrolArrival.Tour.Visits[0].RoomID = "other"
			case "future":
				tele.PatrolArrival.Tour.Visits[0].SimulationNS = 6000000000
			case "missing-original":
				tele.PatrolArrival.Tour.Visits[0].Evidence = nil
			case "revoked":
				r.s.cfg.ValidateAuthority = func(Record) error { return errors.New("revoked") }
			case "expired":
				r.wall = r.wall.Add(time.Hour)
				r.mono += time.Hour
			}
			d, _ := r.s.EvaluatePatrol(s.ID, "app:nav", r.epoch, tele)
			if bad == "" {
				if d.Record.Status.Phase != mission.Succeeded {
					t.Fatal(d)
				}
				tele.PatrolArrival.Tour.Visits[0].RoomID = "mutated"
				saved, _ := r.s.Get(s.ID)
				if saved.PatrolReturnResult.Arrival.Tour.Visits[0].RoomID != "room" {
					t.Fatal("terminal receipt aliases caller")
				}
			} else if d.Record.Status.Phase == mission.Succeeded {
				t.Fatal("incomplete tour completed", bad)
			}
		})
	}
}
func TestTourAbsentEncodingAndMalformedGeometry(t *testing.T) {
	r, s, _ := patrolRig(t)
	defer r.s.Close()
	raw, _ := json.Marshal(s)
	if strings.Contains(string(raw), `"tour"`) {
		t.Fatal("absent policy changed legacy bytes")
	}
	for _, raw := range []string{`{"method":"inspect-selected-rooms-once-and-return-v1","stops":[{"room_id":"room","position":[null,0,0],"yaw":0}]}`, `{"method":"inspect-selected-rooms-once-and-return-v1","stops":[{"room_id":"room","position":[0,0,0]}]}`, `{"method":"inspect-selected-rooms-once-and-return-v1","method":"other","stops":[]}`} {
		var p patroltour.Policy
		if json.Unmarshal([]byte(raw), &p) == nil {
			t.Fatal("accepted malformed tour", raw)
		}
	}
}
