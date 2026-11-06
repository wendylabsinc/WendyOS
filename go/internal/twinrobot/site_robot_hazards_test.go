package twinrobot

import (
	"math"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
)

func TestSiteRobotHazardsUseConfirmedAlignmentAndFreshNavigationData(t *testing.T) {
	for _, fault := range []string{"none", "unaligned", "pose", "capture", "cloud", "floor", "closed"} {
		t.Run(fault, func(t *testing.T) {
			r, _ := readySiteRobot()
			r.observed.Grid.Obstacles[sitenav.Cell{X: 10, Y: 20}] = true
			r.alignment = sitenav.Alignment{Origin: sitenav.Point{X: 1, Y: 2}, Site: sitenav.Point{X: 4, Y: 5}, Rotation: math.Pi / 2}
			r.floorY = 3
			switch fault {
			case "unaligned":
				r.frame = ""
			case "pose":
				r.observed.Pose.Received = time.Now().Add(-time.Second)
			case "capture":
				r.observed.Pose.Captured = time.Time{}
			case "cloud":
				r.observed.CloudAt = time.Now().Add(-time.Second)
			case "floor":
				r.observed.FloorFault = "unknown floor"
			case "closed":
				r.closed = true
			}
			s := r.Snapshot()
			if fault != "none" {
				if s.HazardsReady || len(s.Hazards) != 0 {
					t.Fatalf("displayed invalid hazards: %+v", s.Hazards)
				}
				return
			}
			if !s.HazardsReady || len(s.Hazards) != 1 {
				t.Fatal("missing hazards")
			}
			h := s.Hazards[0]
			if math.Abs(h.X-3.95) > 1e-9 || math.Abs(h.Z+5.05) > 1e-9 || h.Yaw != math.Pi/2 || h.Size != .1 || !h.Drop || s.FloorY != 3 {
				t.Fatalf("wrong site transform: %+v", h)
			}
			delete(r.observed.Grid.Obstacles, sitenav.Cell{X: 10, Y: 20})
			if len(r.Snapshot().Hazards) != 0 {
				t.Fatal("removed obstacle remained visible")
			}
		})
	}
}
