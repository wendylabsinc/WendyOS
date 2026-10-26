package sitenav

import (
	"context"
	"testing"
	"time"
)

func TestBetterParallelFitPreventsFalseUniqueCorner(t *testing.T) {
	// Costs and positions from the stationary Go2 replay. The third fit used
	// to win solely because the better parallel-wall fits were not Valid.
	fits := []RegistrationFit{
		{Pose: PoseCandidate{Position: Point{-1.86, -.28}}, Cost: .0320, Directions: 1, Plausible: true},
		{Pose: PoseCandidate{Position: Point{-.12, -.28}}, Cost: .0326, Directions: 1, Plausible: true},
		{Pose: PoseCandidate{Position: Point{.36, .55}}, Cost: .0374, Directions: 2, Plausible: true, Valid: true},
	}
	tracker := PoseTracker{}
	result := tracker.Update(Pose{}, SurfaceCloud{}, fits, time.Now())
	if result.Selected >= 0 {
		t.Fatalf("discarding underconstrained alternatives selected a false unique pose: %+v", result.Fits[result.Selected])
	}
	// A clearly worse alternative must not prevent a distinctly better fit.
	fits[0].Cost, fits[1].Cost = .06, .06
	tracker = PoseTracker{}
	result = tracker.Update(Pose{}, SurfaceCloud{}, fits, time.Now())
	if result.Selected < 0 || result.Fits[result.Selected].Directions != 2 {
		t.Fatal("a clearly worse alternative blocked a distinct pose")
	}
}

func TestParallelWallRegistrationRetainsCompetingExplanation(t *testing.T) {
	arch, furniture := semanticFixture(t)
	model, err := NewLayeredRoom(arch, furniture)
	if err != nil {
		t.Fatal(err)
	}
	var points []Vertex
	for i := 0; i < 50; i++ {
		for _, y := range []float64{0, 8} {
			for _, z := range []float64{.15, .20, .25, .45, .50, .55} {
				points = append(points, Vertex{.5 + float64(i)*.1 - 3, y - 4, z})
			}
		}
	}
	cloud := ExtractSurfaces(points, 0)
	fits, err := RegisterSurfaces(context.Background(), model, RegistrationInput{Cloud: cloud})
	if err != nil {
		t.Fatal(err)
	}
	competing := false
	for _, fit := range fits {
		if fit.Plausible && !fit.Valid && fit.Directions == 1 {
			competing = true
		}
	}
	if !competing {
		t.Fatal("well-fitting parallel walls disappeared from competing hypotheses")
	}
}
