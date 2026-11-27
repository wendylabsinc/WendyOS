package people

import (
	"github.com/wendylabsinc/wendy/go/world/internal/presence"
	"testing"
	"time"
)

func TestPeopleZoneTimelineAndCameraLoss(t *testing.T) {
	s := new(Session)
	req := input(1, 0)
	req.Zones = []presence.Zone{{ID: "room", Label: "Room", MinXZ: [2]float64{-3, -3}, MaxXZ: [2]float64{3, 3}, DwellSeconds: .5}}
	out, err := s.Step(req)
	if err != nil || out.Presence == nil || out.Presence.Zones[0].Observed != 1 {
		t.Fatalf("%+v %v", out, err)
	}
	out, err = s.Step(input(2, int64(time.Second/2)))
	if err != nil || len(out.Presence.Events) != 2 || out.Presence.Events[1].Kind != "dwell" {
		t.Fatalf("%+v %v", out, err)
	}
	req = input(3, int64(time.Second))
	req.Images = nil
	out, err = s.Step(req)
	if err != nil || out.Presence.Events[len(out.Presence.Events)-1].Kind != "lost" || out.Presence.Zones[0].Observed != 0 {
		t.Fatalf("%+v %v", out, err)
	}
	req.NowNS = int64(2 * time.Second)
	out, err = s.Step(req)
	if err != nil || out.Presence.Zones[0].State != "unknown" || len(out.Tracks) != 0 {
		t.Fatalf("%+v %v", out, err)
	}
	req.Zones = []presence.Zone{{ID: "changed", MinXZ: [2]float64{-1, -1}, MaxXZ: [2]float64{1, 1}, DwellSeconds: 1}}
	if _, err = s.Step(req); err == nil {
		t.Fatal("zone changed within session")
	}
}
