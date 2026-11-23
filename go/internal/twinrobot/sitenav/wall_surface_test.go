package sitenav

import (
	"context"
	"math"
	"os"
	"testing"
	"time"
)

func TestWallExtractionRejectsOccupiedVolumes(t *testing.T) {
	for _, angle := range []float64{0, .37, 1.1} {
		var points []Vertex
		s, c := math.Sincos(angle)
		for x := 0; x < 20; x++ {
			for y := 0; y < 16; y++ {
				for z := 0; z < 15; z++ {
					px, py := float64(x)*.05, float64(y)*.05
					points = append(points, Vertex{2 + c*px - s*py, 2 + s*px + c*py, .125 + float64(z)*.05})
				}
			}
		}
		if got := structuralLandmarks(points, .32); len(got) > 0 {
			t.Fatalf("solid occupied volume became %d upper-wall landmarks at angle %v", len(got), angle)
		}
		if got := lowerWallLandmarks(points, .025, .32); len(got) > 0 {
			t.Fatalf("solid occupied volume became %d lower-wall landmarks at angle %v", len(got), angle)
		}
	}
}

func TestWallExtractionPreservesRotatedThinSurfaces(t *testing.T) {
	for _, angle := range []float64{0, .37, 1.1} {
		var points []Vertex
		s, c := math.Sincos(angle)
		for i := 0; i < 50; i++ {
			for z := 0; z < 15; z++ {
				x, y := float64(i)*.05, 0.0
				points = append(points, Vertex{2 + c*x - s*y, -1 + s*x + c*y, .125 + float64(z)*.05})
				x, y = 0, float64(i)*.05
				points = append(points, Vertex{2 + c*x - s*y, -1 + s*x + c*y, .125 + float64(z)*.05})
			}
		}
		got := lowerWallLandmarks(points, .025, .32)
		if err := localizationStructure(got); err != nil {
			t.Fatalf("thin corner lost at rotation %v: %d points: %v", angle, len(got), err)
		}
	}
}

func TestRoomMatchingRejectsUnconstrainedParallelWalls(t *testing.T) {
	room := testRoom(t, false)
	var scan []Point
	for i := 0; i < 50; i++ {
		x := .5 + float64(i)*.1
		scan = append(scan, Point{x - 2, -3}, Point{x - 2, 5})
	}
	if results, err := MatchRoom(context.Background(), room, scan, Point{}); err == nil {
		t.Fatalf("parallel walls produced a pose: %+v", results)
	}
}

func TestRecordedOccupiedCloudDoesNotSuggestHeading(t *testing.T) {
	raw, err := os.ReadFile("testdata/occupied-wall-cloud.bin")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 125000000)
	o := Observations{Pose: Pose{Position: Point{-.034937, .01939}, Height: .31861, Yaw: -.59125239859, Received: now, Captured: now}}
	if err := o.Receive(CloudTopic, raw, true, now, now); err != nil {
		t.Fatal(err)
	}
	// This packet previously produced 288 landmarks and competing headings
	// with 59–63% coverage. Its occupied blocks are not thin wall surfaces.
	if err := o.LocalizationReady(now); err == nil {
		t.Fatalf("recorded occupied patches admitted localization: %d landmarks", len(o.Landmarks))
	}
	if len(o.Landmarks) == 0 {
		t.Fatal("the remaining thin surface should still be retained")
	}
	if err := o.Ready(now); err != nil {
		t.Fatalf("wall classification changed floor readiness: %v", err)
	}
}

func TestRoomMatchingIgnoresDuplicateReturnDensity(t *testing.T) {
	r := testRoom(t, true)
	position, rotation := Point{2.1, 3.2}, .7
	var scan []Point
	s, c := math.Sincos(-rotation)
	for i, p := range r.walls {
		if i%3 != 0 {
			continue
		}
		x, y := p.X-position.X, p.Y-position.Y
		q := Point{c*x - s*y, s*x + c*y}
		scan = append(scan, q)
		if p.Y == 0 {
			for j := 0; j < 50; j++ {
				scan = append(scan, q)
			}
		}
	}
	results, err := MatchRoom(context.Background(), r, scan, Point{})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Position.Distance(position) > .15 || math.Abs(math.Remainder(results[0].Rotation-rotation, 2*math.Pi)) > .06 {
		t.Fatalf("duplicate density changed pose: %+v", results)
	}
}

func TestExtractedWallSurfacesRecoverPose(t *testing.T) {
	r := testRoom(t, true)
	position, rotation := Point{2.1, 3.2}, .7
	var cloud []Vertex
	s, c := math.Sincos(-rotation)
	for _, p := range r.walls {
		x, y := p.X-position.X, p.Y-position.Y
		for z := 0; z < 15; z++ {
			cloud = append(cloud, Vertex{c*x - s*y, s*x + c*y, .125 + float64(z)*.05})
		}
	}
	landmarks := lowerWallLandmarks(cloud, .025, .32)
	results, err := MatchRoom(context.Background(), r, landmarks, Point{})
	if err != nil {
		t.Fatalf("extracted thin surfaces failed matching: %v (%d landmarks)", err, len(landmarks))
	}
	if len(results) != 1 || results[0].Position.Distance(position) > .15 || math.Abs(math.Remainder(results[0].Rotation-rotation, 2*math.Pi)) > .06 {
		t.Fatalf("extraction changed recovered pose: %+v", results)
	}
}
