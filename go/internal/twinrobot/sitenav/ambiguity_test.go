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

func TestFragilePoseNeedsNewArchitecturalEvidenceFromAnotherView(t *testing.T) {
	now := time.Now()
	first := SurfaceCloud{}
	for i := 0; i < 40; i++ {
		first.Surfaces = append(first.Surfaces, Surface{Position: Vertex{float64(i) * .1, 0, .5}, Direction: Point{1, 0}})
	}
	fit := RegistrationFit{Pose: PoseCandidate{Position: Point{2, 3}}, Cost: .01, Valid: true, Plausible: true, NeedsCorroboration: true}
	for _, kind := range []string{"replayed", "door-only", "furniture", "inconsistent", "new-walls"} {
		t.Run(kind, func(t *testing.T) {
			tracker := PoseTracker{}
			if r := tracker.Update(Pose{}, first, []RegistrationFit{fit}, now); r.Selected >= 0 {
				t.Fatal("single fragile view was suggested")
			}
			cloud := SurfaceCloud{Surfaces: append([]Surface(nil), first.Surfaces...)}
			next := fit
			if kind != "replayed" {
				for i := 0; i < 12; i++ {
					cloud.Surfaces = append(cloud.Surfaces, Surface{Position: Vertex{float64(i) * .15, 2, .5}, Direction: Point{1, 0}})
					if kind != "door-only" {
						cloud.Surfaces = append(cloud.Surfaces, Surface{Position: Vertex{3, float64(i) * .15, .5}, Direction: Point{0, 1}})
					}
				}
			}
			next.Classes = make([]int, len(cloud.Surfaces))
			for i := range next.Classes {
				next.Classes[i] = 1
				if kind == "furniture" {
					next.Classes[i] = 2
				}
			}
			if kind == "inconsistent" {
				next.Pose.Position.X += 1
			}
			r := tracker.Update(Pose{Yaw: .5}, cloud, []RegistrationFit{next}, now.Add(time.Second))
			if (r.Selected >= 0) != (kind == "new-walls") {
				t.Fatalf("unexpected corroboration for %s: %+v", kind, r)
			}
		})
	}
}

func TestSupportPlaneAliasesAreNotIndependent(t *testing.T) {
	if !sameSupportPlane(Point{1, 0}, 1, Point{-1, 0}, -1.1) {
		t.Fatal("opposite normals split one plane")
	}
	if sameSupportPlane(Point{1, 0}, 1, Point{0, 1}, 1) || sameSupportPlane(Point{1, 0}, 1, Point{1, 0}, 2) {
		t.Fatal("distinct planes merged")
	}
}

func TestSingleCornerNeedsCorroborationUnlessCameraAgrees(t *testing.T) {
	arch, furniture := semanticFixture(t)
	model, err := NewLayeredRoom(arch, furniture)
	if err != nil {
		t.Fatal(err)
	}
	position, rotation := Point{2.1, 3.2}, .7
	// Only two observed planes, such as a wall plus an inward-open door.
	cloud := fixtureSurfaceCloud(t, arch[:6], position, rotation)
	for _, camera := range []bool{false, true} {
		input := RegistrationInput{Cloud: cloud}
		if camera {
			input.Visual = &VisualHint{Position: position, Yaw: rotation, Inliers: 30, Residual: 1}
		}
		fits, err := RegisterSurfaces(context.Background(), model, input)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, fit := range fits {
			if !fit.Valid {
				continue
			}
			if !camera {
				found = true
				if !fit.NeedsCorroboration {
					t.Fatal("two-plane fit bypassed corroboration")
				}
			}
			if camera && fit.Pose.Position.Distance(position) < .2 && !fit.NeedsCorroboration {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing usable corner fit, camera=%v", camera)
		}
	}
}
