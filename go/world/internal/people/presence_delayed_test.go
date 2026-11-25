package people

import (
	"github.com/wendylabsinc/wendy/go/world/internal/presence"
	"math"
	"reflect"
	"testing"
	"time"
)

func TestDelayedCaptureEvidence(t *testing.T) {
	s := new(Session)
	for i, x := range []float64{-2, -1.4, -.8, -.2, .4, .55} {
		ns := int64(float64(i) * .2 * 1e9)
		req := input(uint64(i+1), ns)
		req.Images = req.Images[:1]
		req.Zones = []presence.Zone{{ID: "room", MinXZ: [2]float64{-5, -5}, MaxXZ: [2]float64{1, 5}, DwellSeconds: .5}}
		im := &req.Images[0]
		c := im.Sample.Camera
		c.Width = 640
		c.Height = 480
		c.CX = 320
		c.CY = 240
		c.FX = 100
		c.FY = 100
		im.Sample.UncertaintyM = .19
		p := &im.Poses
		p.ImageWidth = 640
		p.ImageHeight = 480
		p.Detections[0].LeftAnkle.X = 320 + x*100/1.92 - 10
		p.Detections[0].RightAnkle.X = 320 + x*100/1.92 + 10
		p.Detections[0].LeftAnkle.Y = 260
		p.Detections[0].RightAnkle.Y = 260
		if i == 5 {
			req.NowNS += int64(900 * time.Millisecond)
		}
		out, err := s.Step(req)
		if err != nil {
			t.Fatal(err)
		}
		kinds := []string{}
		for _, e := range out.Presence.Events {
			kinds = append(kinds, e.Kind)
		}
		tr := out.Tracks[0]
		member := tr.Members[0]
		if tr.ID != "hub/000001" {
			t.Fatalf("identity changed: %s", tr.ID)
		}
		if i == 5 {
			if member.PositionXZ[0]+2*math.Sqrt(member.Covariance[0]) >= 1 || tr.PositionXZ[0]-2*math.Sqrt(tr.Covariance[0]) <= 1 {
				t.Fatal("fixture must measure inside and predict outside")
			}
			if !reflect.DeepEqual(kinds, []string{"entered", "dwell"}) {
				t.Fatalf("prediction invented transition: %v", kinds)
			}
			z := out.Presence.Zones[0]
			if z.State != "uncertain" || z.Observed != 0 || z.Possible != 1 || !z.Occupants[0].LastObserved.Equal(tr.LastObserved) {
				t.Fatalf("delayed occupancy: %+v", z)
			}
		}
	}
}
