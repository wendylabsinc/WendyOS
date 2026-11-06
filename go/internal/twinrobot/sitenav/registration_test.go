package sitenav

import (
	"context"
	"math"
	"testing"
	"time"
)

func semanticFixture(t *testing.T) ([]Triangle, []Triangle) {
	t.Helper()
	arch := []Triangle{{{0, 0, 0}, {6, 0, 0}, {6, 0, -8}}, {{0, 0, 0}, {6, 0, -8}, {0, 0, -8}}}
	wall := func(mesh *[]Triangle, a, b Point, height float64) {
		*mesh = append(*mesh, Triangle{{a.X, 0, -a.Y}, {b.X, 0, -b.Y}, {b.X, height, -b.Y}}, Triangle{{a.X, 0, -a.Y}, {b.X, height, -b.Y}, {a.X, height, -a.Y}})
	}
	for _, ends := range [][2]Point{{{0, 0}, {6, 0}}, {{6, 0}, {6, 8}}, {{6, 8}, {0, 8}}, {{0, 8}, {0, 0}}, {{4, 4}, {6, 4}}, {{4, 4}, {4, 6}}, {{0, 1}, {1, 2}}} {
		wall(&arch, ends[0], ends[1], 2)
	}
	var furniture []Triangle
	for _, ends := range [][2]Point{{{1, 5}, {2.5, 5}}, {{2.5, 5}, {2.5, 6}}, {{2.5, 6}, {1, 6}}, {{1, 6}, {1, 5}}} {
		wall(&furniture, ends[0], ends[1], .75)
	}
	furniture = append(furniture, Triangle{{1, .75, -5}, {2.5, .75, -5}, {2.5, .75, -6}}, Triangle{{1, .75, -5}, {2.5, .75, -6}, {1, .75, -6}})
	return arch, furniture
}
func fixtureSurfaceCloud(t *testing.T, arch []Triangle, position Point, rotation float64) SurfaceCloud {
	t.Helper()
	room, err := RoomFromMesh(arch)
	if err != nil {
		t.Fatal(err)
	}
	s, c := math.Sincos(-rotation)
	var points []Vertex
	for _, p := range room.walls {
		x, y := p.X-position.X, p.Y-position.Y
		for _, z := range []float64{.15, .20, .25, .45, .50, .55} {
			points = append(points, Vertex{c*x - s*y, s*x + c*y, z})
		}
	}
	return ExtractSurfaces(points, 0)
}
func TestMultilevelMatchingRecoversPoseFromPartialHeightWalls(t *testing.T) {
	arch, furniture := semanticFixture(t)
	model, err := NewLayeredRoom(arch, furniture)
	if err != nil {
		t.Fatal(err)
	}
	position, rotation := Point{2.1, 3.2}, .7
	cloud := fixtureSurfaceCloud(t, arch, position, rotation)
	if len(cloud.Surfaces) < 24 {
		t.Fatalf("short wall neighborhoods lost: %d", len(cloud.Surfaces))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	fits, err := RegisterSurfaces(ctx, model, RegistrationInput{Cloud: cloud})
	if err != nil {
		t.Fatal(err)
	}
	tracker := PoseTracker{}
	result := tracker.Update(Pose{}, cloud, fits, time.Now())
	if result.Selected < 0 {
		t.Fatalf("partial height walls failed: %s %+v", result.Message, fits)
	}
	fit := result.Fits[result.Selected]
	if fit.Pose.Position.Distance(position) > .15 || math.Abs(math.Remainder(fit.Pose.Rotation-rotation, 2*math.Pi)) > .08 {
		t.Fatalf("wrong registered pose: %+v", fit.Pose)
	}
	if fit.Directions < 2 || fit.Architecture < 12 {
		t.Fatal("pose lacks architectural support")
	}
}
func TestAccumulatedPacketReplaysDoNotResolveAmbiguity(t *testing.T) {
	cloud := SurfaceCloud{}
	for i := 0; i < 40; i++ {
		cloud.Surfaces = append(cloud.Surfaces, Surface{Position: Vertex{float64(i) * .1, 1, .5}})
	}
	fits := []RegistrationFit{{Pose: PoseCandidate{Position: Point{1, 2}}, Cost: .01, Valid: true}, {Pose: PoseCandidate{Position: Point{4, 5}, Rotation: math.Pi}, Cost: .011, Valid: true}}
	tracker := PoseTracker{}
	now := time.Now()
	for i := 0; i < 100; i++ {
		result := tracker.Update(Pose{}, cloud, fits, now.Add(time.Duration(i)*100*time.Millisecond))
		if result.Selected != -1 || result.IndependentViews != 1 {
			t.Fatalf("duplicate packets created confidence: %+v", result)
		}
	}
}
func TestNewGeometryRequiresChangedViewpoint(t *testing.T) {
	cloud := SurfaceCloud{}
	for i := 0; i < 40; i++ {
		cloud.Surfaces = append(cloud.Surfaces, Surface{Position: Vertex{float64(i) * .1, 1, .5}})
	}
	tracker := PoseTracker{}
	now := time.Now()
	tracker.Update(Pose{}, cloud, nil, now)
	changed := cloud
	changed.Surfaces = append([]Surface(nil), cloud.Surfaces...)
	for i := 0; i < 30; i++ {
		changed.Surfaces = append(changed.Surfaces, Surface{Position: Vertex{float64(i) * .1, 3, .5}})
	}
	result := tracker.Update(Pose{}, changed, nil, now.Add(time.Second))
	if result.IndependentViews != 1 {
		t.Fatal("stationary map changes counted as new viewpoints")
	}
	result = tracker.Update(Pose{Position: Point{.3, 0}}, changed, nil, now.Add(2*time.Second))
	if result.IndependentViews != 2 {
		t.Fatal("new viewpoint and geometry were not retained")
	}
}
func TestReferenceMapTransformDoesNotAlterNavigation(t *testing.T) {
	cloud := SurfaceCloud{Floor: -.1, Surfaces: []Surface{{Position: Vertex{1, 0, .4}, Direction: Point{1, 0}}}}
	reference := SiteReference(cloud, Pose{}, PoseCandidate{Position: Point{2, 3}, Rotation: math.Pi / 2}, -1)
	if len(reference) != 1 || math.Abs(reference[0].Position.X-2) > 1e-6 || math.Abs(reference[0].Position.Y-4) > 1e-6 || math.Abs(reference[0].Position.Z+.5) > 1e-6 {
		t.Fatalf("reference frame transform: %+v", reference)
	}
	if cloud.Surfaces[0].Position != (Vertex{1, 0, .4}) {
		t.Fatal("saving reference changed live observations")
	}
}

func TestFurnitureCannotEstablishAnArchitecturalAnchor(t *testing.T) {
	arch, furniture := semanticFixture(t)
	model, err := NewLayeredRoom(arch, furniture)
	if err != nil {
		t.Fatal(err)
	}
	// A scan containing only a movable desk cannot anchor itself to a room.
	// Keep the real architecture far enough from those returns to avoid a
	// fortuitous wall correspondence at the desk's correct location.
	position := Point{2.1, 3.2}
	var points []Vertex
	for i := 0; i < 25; i++ {
		for _, z := range []float64{.15, .20, .25, .45, .50, .55} {
			x := 1 + float64(i)*.05
			points = append(points, Vertex{x - position.X, 5 - position.Y, z})
			y := 5 + float64(i)*.04
			points = append(points, Vertex{2.5 - position.X, y - position.Y, z})
		}
	}
	cloud := ExtractSurfaces(points, 0)
	if len(cloud.Surfaces) < 24 {
		t.Fatal("fixture has no furniture surfaces")
	}
	fits, err := RegisterSurfaces(context.Background(), model, RegistrationInput{Cloud: cloud})
	if err != nil {
		t.Fatal(err)
	}
	tracker := PoseTracker{}
	result := tracker.Update(Pose{}, cloud, fits, time.Now())
	if result.Selected >= 0 {
		t.Fatalf("movable object alone established room pose: %+v", result.Fits[result.Selected].Pose)
	}
}

func TestSavedMapRejectsNonfiniteCoordinates(t *testing.T) {
	arch, furniture := semanticFixture(t)
	model, err := NewLayeredRoom(arch, furniture)
	if err != nil {
		t.Fatal(err)
	}
	cloud := fixtureSurfaceCloud(t, arch, Point{2.1, 3.2}, .7)
	_, err = RegisterSurfaces(context.Background(), model, RegistrationInput{Cloud: cloud, Reference: []Surface{{Position: Vertex{math.NaN(), 0, 0}}}})
	if err == nil {
		t.Fatal("invalid saved reference entered matching")
	}
}
