package sitenav

import (
	"context"
	"errors"
	"math"
	"testing"
)

func testRoom(t *testing.T, asymmetric bool) RoomMap {
	t.Helper()
	mesh := []Triangle{
		{{0, 0, 0}, {6, 0, 0}, {6, 0, -8}}, {{0, 0, 0}, {6, 0, -8}, {0, 0, -8}},
	}
	wall := func(a, b Point) {
		mesh = append(mesh, Triangle{{a.X, 0, -a.Y}, {b.X, 0, -b.Y}, {b.X, 2, -b.Y}}, Triangle{{a.X, 0, -a.Y}, {b.X, 2, -b.Y}, {a.X, 2, -a.Y}})
	}
	wall(Point{0, 0}, Point{6, 0})
	wall(Point{6, 0}, Point{6, 8})
	wall(Point{6, 8}, Point{0, 8})
	wall(Point{0, 8}, Point{0, 0})
	if asymmetric {
		wall(Point{4, 4}, Point{6, 4})
		wall(Point{4, 4}, Point{4, 6})
		wall(Point{0, 1}, Point{1, 2})
	}
	r, err := RoomFromMesh(mesh)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRoomMatchingRecoversOffsetAndYaw(t *testing.T) {
	r := testRoom(t, true)
	position, rotation, origin := Point{2.1, 3.2}, .7, Point{-.2, .3}
	var scan []Point
	s, c := math.Sincos(-rotation)
	for i, p := range r.walls {
		if i%3 != 0 {
			continue
		}
		x, y := p.X-position.X, p.Y-position.Y
		scan = append(scan, Point{origin.X + c*x - s*y, origin.Y + s*x + c*y})
	}
	results, err := MatchRoom(context.Background(), r, scan, origin)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 || results[0].Position.Distance(position) > .15 || math.Abs(math.Remainder(results[0].Rotation-rotation, 2*math.Pi)) > .06 {
		t.Fatalf("failed to recover pose: %+v", results)
	}
	if results[0].Coverage < .9 {
		t.Fatalf("poor fit: %+v", results[0])
	}
}

func TestRoomMatchingRejectsAmbiguousAlternatives(t *testing.T) {
	r := testRoom(t, false)
	var scan []Point
	for i, p := range r.walls {
		if i%3 == 0 {
			scan = append(scan, Point{p.X - 2, p.Y - 3})
		}
	}
	results, err := MatchRoom(context.Background(), r, scan, Point{})
	if !errors.Is(err, ErrAmbiguousRoom) || len(results) != 0 {
		t.Fatalf("symmetric room must reject competing poses: %+v, %v", results, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = MatchRoom(ctx, r, scan, Point{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
	if _, err = MatchRoom(context.Background(), r, scan[:10], Point{}); err == nil {
		t.Fatal("sparse scan accepted")
	}
}

func TestRoomMatchingRejectsInvalidGeometry(t *testing.T) {
	if _, err := RoomFromMesh(nil); err == nil {
		t.Fatal("empty mesh accepted")
	}
	mesh := make([]Triangle, 4)
	mesh[0][0].X = math.NaN()
	if _, err := RoomFromMesh(mesh); err == nil {
		t.Fatal("nonfinite mesh accepted")
	}
}

// The room walls stay in place while furniture returns have no counterpart in
// the architectural model. Those outliers must not translate the wall fit.
func TestRoomMatchingIgnoresUnmodeledFurniture(t *testing.T) {
	r := testRoom(t, true)
	position, rotation, origin := Point{2.1, 3.2}, .7, Point{-.2, .3}
	var scan []Point
	s, c := math.Sincos(-rotation)
	add := func(p Point) {
		x, y := p.X-position.X, p.Y-position.Y
		scan = append(scan, Point{origin.X + c*x - s*y, origin.Y + s*x + c*y})
	}
	for i, p := range r.walls {
		if i%3 == 0 {
			add(p)
		}
	}
	// A dense collection of chair/desk surfaces offset from every wall.
	for x := 1.; x < 3.; x += .15 {
		for y := 2.; y < 4.; y += .2 {
			add(Point{x, y})
		}
	}
	results, err := MatchRoom(context.Background(), r, scan, origin)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 || results[0].Position.Distance(position) > .15 || math.Abs(math.Remainder(results[0].Rotation-rotation, 2*math.Pi)) > .06 {
		t.Fatalf("clutter displaced wall fit: %+v", results)
	}
	if results[0].Coverage > .9 {
		t.Fatal("unmatched furniture was reported as wall support")
	}
}

func TestRoomMatchingUsesSparseDistributedWalls(t *testing.T) {
	r := testRoom(t, true)
	position, rotation := Point{2.1, 3.2}, .7
	var scan []Point
	s, c := math.Sincos(-rotation)
	for i := 0; i < 36; i++ {
		p := r.walls[i*len(r.walls)/36]
		x, y := p.X-position.X, p.Y-position.Y
		scan = append(scan, Point{c*x - s*y, s*x + c*y})
	}
	results, err := MatchRoom(context.Background(), r, scan, Point{})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Position.Distance(position) > .15 || math.Abs(math.Remainder(results[0].Rotation-rotation, 2*math.Pi)) > .06 {
		t.Fatalf("sparse wall fit failed: %+v", results)
	}
	var line []Point
	for i := 0; i < 100; i++ {
		line = append(line, Point{float64(i) * .1, 1})
	}
	if localizationStructure(line) == nil {
		t.Fatal("single wall passed structural check")
	}
	var duplicates []Point
	for i := 0; i < 100; i++ {
		duplicates = append(duplicates, Point{1, 1})
	}
	if localizationStructure(duplicates) == nil {
		t.Fatal("duplicate columns counted as structure")
	}
}

func TestRoomFloorIgnoresWallOverhangAndSmallLowerFragments(t *testing.T) {
	mesh := []Triangle{
		{{0, 0, 0}, {4, 0, 0}, {4, 0, -4}}, {{0, 0, 0}, {4, 0, -4}, {0, 0, -4}},
		{{0, -1, 0}, {4, -1, 0}, {4, 2, 0}}, {{0, -1, 0}, {4, 2, 0}, {0, 2, 0}},
		{{0, -1, 0}, {0, -1, -4}, {0, 2, -4}}, {{0, -1, 0}, {0, 2, -4}, {0, 2, 0}},
		{{0, -2, 0}, {.1, -2, 0}, {0, -2, -.1}},
	}
	room, err := RoomFromMesh(mesh)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(room.FloorY) > 1e-6 || len(room.floor) != 2 {
		t.Fatalf("wrong floor: height=%f triangles=%d", room.FloorY, len(room.floor))
	}
	// A ceiling without a floor must not become a usable room.
	for i := 0; i < 2; i++ {
		for j := range mesh[i] {
			mesh[i][j].Y = 2
		}
	}
	if _, err := RoomFromMesh(mesh); err == nil {
		t.Fatal("ceiling accepted as floor")
	}
}

func TestRoomFloorMergesHeightBinBoundaryWithoutInventingArea(t *testing.T) {
	mesh := []Triangle{
		{{0, .024, 0}, {1.2, .024, 0}, {0, .024, -1}},
		{{1.2, .026, -1}, {0, .026, -1}, {1.2, .026, 0}},
	}
	y, err := roomFloorHeight(mesh)
	if err != nil || math.Abs(y-.025) > 1e-6 {
		t.Fatalf("floor across bin boundary: y=%f err=%v", y, err)
	}
	if _, err := roomFloorHeight(mesh[:1]); err == nil {
		t.Fatal("undersized floor accepted")
	}
	if _, err := roomFloorHeight(nil); err == nil {
		t.Fatal("missing floor accepted")
	}
}

func TestRoomFloorAcceptsLevelToleranceWithoutLowestVertexCutoff(t *testing.T) {
	// The normal passes the matcher's level tolerance, but both triangle
	// centroids are more than 25 cm above the lowest corner.
	mesh := []Triangle{
		{{0, 0, 0}, {8, 1.2, 0}, {8, 1.2, -8}},
		{{0, 0, 0}, {8, 1.2, -8}, {0, 0, -8}},
	}
	y, err := roomFloorHeight(mesh)
	if err != nil || math.Abs(y-.4) > 1e-6 {
		t.Fatalf("valid floor tolerance rejected: y=%f err=%v", y, err)
	}
}
