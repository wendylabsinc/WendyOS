package sitenav

import (
	"encoding/binary"
	"encoding/json"
	"github.com/pierrec/lz4/v4"
	"math"
	"testing"
	"time"
)

func TestCloudMeasuredFloorOffset(t *testing.T) {
	now := time.Now()
	for _, height := range []float64{-.125, -.075, 0, .1} {
		o := Observations{Pose: Pose{Position: Point{2, 2}, Height: .3, Received: now, Captured: now}}
		if err := o.Receive(CloudTopic, cloudFrame(t, now, "odom", []float64{0, 0, height}), true, now, now); err != nil {
			t.Fatalf("height %v: %v", height, err)
		}
		if math.Abs(o.FloorHeight-height) > 1e-6 || !o.Grid.Clear(Point{2, 2}) {
			t.Fatalf("floor reference not measured at %v", height)
		}
	}
}

func TestIrregularBuriedVolumeIsUnknownNotConfirmedDrop(t *testing.T) {
	var points []Vertex
	// A measured level floor surrounds an occluded body-sized patch. Occupied
	// volume below the patch has an uneven bowl-shaped top, like the captured
	// Go2 map. The previous three-cell drop rule falsely blocked departure.
	for x := -20; x <= 20; x++ {
		for y := -20; y <= 20; y++ {
			px, py := float64(x)*.1+.05, float64(y)*.1+.05
			if math.Abs(px) < .3 && math.Abs(py) < .2 {
				top := -.35 + .18*(math.Abs(px)/.3+math.Abs(py)/.2)
				for z := -.5; z <= top; z += .05 {
					points = append(points, Vertex{px, py, z})
				}
			} else {
				points = append(points, Vertex{px, py, 0})
			}
		}
	}
	g := navigationGrid(points, 0)
	if g.ClearPose(Point{}, 0) {
		t.Fatal("invented observed floor under robot")
	}
	allowed := g.WithDeparture(&Departure{Position: Point{}})
	if !allowed.ClearPose(Point{}, 0) {
		t.Fatal("irregular volume became a measured drop")
	}
	if _, err := allowed.PlanPose(Point{}, 0, Point{1, 0}); err != nil {
		t.Fatal(err)
	}
	// The very same uncertainty away from departure is never authorized.
	if _, err := g.WithDeparture(&Departure{Position: Point{1, 0}}).PlanPose(Point{1, 0}, math.Pi, Point{}); err == nil {
		t.Fatal("departure allowance erased unknown destination")
	}
}

func TestLowerSurfaceRequiresPlaneAndKeepsSmallDrops(t *testing.T) {
	flat := []Vertex{{0, 0, -.2}, {.1, 0, -.2}, {0, .1, -.2}, {.1, .1, -.2}}
	if !observedLowerSurface(flat) {
		t.Fatal("small measured drop was rejected")
	}
	g := navigationGrid(flat, 0).WithDeparture(&Departure{Position: Point{}})
	if !g.Obstacles[Cell{0, 0}] || g.ClearPose(Point{}, 0) {
		t.Fatal("departure erased measured drop")
	}
	for _, points := range [][]Vertex{
		flat[:3],
		{{0, 0, -.2}, {.1, 0, -.2}, {.2, 0, -.2}, {.3, 0, -.2}},
		{{0, 0, -.2}, {.1, 0, -.3}, {0, .1, -.1}, {.1, .1, -.4}},
	} {
		if observedLowerSurface(points) {
			t.Fatal("insufficient/nonplanar volume became a confirmed drop")
		}
	}
	// Raised obstacles remain blocked even if the floor beneath them is hidden.
	g = navigationGrid([]Vertex{{0, 0, .15}}, 0).WithDeparture(&Departure{Position: Point{}})
	if g.ClearPose(Point{}, 0) {
		t.Fatal("departure erased raised obstacle")
	}
}

func TestBuriedShelvesAreNotIndependentDropSurfaces(t *testing.T) {
	var points []Vertex
	for x := -15; x <= 15; x++ {
		for y := -15; y <= 15; y++ {
			z := 0.0
			if x >= -8 && x <= 8 && y >= -8 && y <= 8 {
				// One occluded region has broad, locally flat shelves at
				// different depths, as in the captured accumulated voxel map.
				z = -.4
				if x > 2 {
					z = -.2
				}
			}
			points = append(points, Vertex{float64(x)*CellSize + .05, float64(y)*CellSize + .05, z})
		}
	}
	g := navigationGrid(points, 0)
	if g.Obstacles[Cell{0, 0}] || g.Floor[Cell{0, 0}] {
		t.Fatal("buried shelf must remain unknown, not a confirmed drop or free floor")
	}
	if g.ClearPose(Point{}, 0) {
		t.Fatal("unknown floor permitted by default")
	}
	allowed := g.WithDeparture(&Departure{Position: Point{}})
	if !allowed.ClearPose(Point{}, 0) {
		t.Fatal("buried shelves prevented supervised departure")
	}
	if allowed.ClearPose(Point{.7, 0}, 0) {
		t.Fatal("departure allowance expanded into unknown floor ahead")
	}
	// A separate, genuinely planar lower surface remains blocked.
	points = append(points, Vertex{2, 2, -.2}, Vertex{2.1, 2, -.2}, Vertex{2, 2.1, -.2}, Vertex{2.1, 2.1, -.2})
	g = navigationGrid(points, 0)
	if !g.Obstacles[Cell{20, 20}] {
		t.Fatal("independent lower plane was lost")
	}
}

func TestDepartureMatchesOrientedClearanceAndStaysFixed(t *testing.T) {
	for _, yaw := range []float64{0, .37, math.Pi / 2} {
		g := Grid{Floor: map[Cell]bool{}, Obstacles: map[Cell]bool{}}
		p := Point{.023, -.017}
		allowed := g.WithDeparture(&Departure{Position: p, Yaw: yaw})
		if !allowed.ClearPose(p, yaw) {
			t.Fatal("departure footprint did not cover navigation envelope")
		}
		if allowed.ClearPose(Point{p.X + math.Cos(yaw), p.Y + math.Sin(yaw)}, yaw) {
			t.Fatal("allowance moved with robot")
		}
	}
}

func TestFloorHeightRequiresLevelSurroundingEvidence(t *testing.T) {
	var flat []Vertex
	for x := -15; x <= 15; x++ {
		for y := -15; y <= 15; y++ {
			flat = append(flat, Vertex{float64(x) * .1, float64(y) * .1, -.1})
		}
	}
	for _, mode := range []string{"level", "sparse", "one-side", "step", "ramp", "table"} {
		t.Run(mode, func(t *testing.T) {
			points := append([]Vertex(nil), flat...)
			switch mode {
			case "sparse":
				points = points[:20]
			case "one-side":
				points = points[:len(points)/2]
			case "step":
				for i := range points {
					if points[i].X > 0 {
						points[i].Z += .2
					}
				}
			case "ramp":
				for i := range points {
					points[i].Z += points[i].X * .3
				}
			case "table":
				for i := range points {
					points[i].Z = .4
				}
			}
			height, err := observedFloorHeight(points, Point{})
			if mode == "level" {
				if err != nil || math.Abs(height+.1) > 1e-6 {
					t.Fatalf("valid plane: %v %v", height, err)
				}
			} else if err == nil {
				t.Fatal("invalid floor accepted")
			}
		})
	}
}

func TestDepartureAllowanceIsBoundedAndPreservesHazards(t *testing.T) {
	g := floorGrid()
	for x := -3; x <= 2; x++ {
		for y := -2; y <= 1; y++ {
			delete(g.Floor, Cell{x, y})
		}
	}
	start, goal := Point{}, Point{2, 0}
	if _, err := g.Plan(start, goal); err == nil {
		t.Fatal("default filled hidden floor")
	}
	departure := &Departure{Position: start, Yaw: 0}
	allowed := g.WithDeparture(departure)
	if _, err := allowed.Plan(start, goal); err != nil {
		t.Fatal(err)
	}
	if g.Clear(start) {
		t.Fatal("allowance mutated sensor grid")
	}
	// A gap ahead remains unknown even though departure is possible.
	for x := 5; x < 10; x++ {
		for y := -30; y < 50; y++ {
			delete(g.Floor, Cell{x, y})
		}
	}
	if _, err := g.WithDeparture(departure).Plan(start, goal); err == nil {
		t.Fatal("allowance filled route ahead")
	}
	// Occupied cells represent both obstacles and measured drops.
	g.Obstacles[Cell{}] = true
	if g.WithDeparture(departure).Clear(start) {
		t.Fatal("allowance erased a hazard")
	}
	// The initial allowance cannot follow the robot to a new blind spot.
	g = floorGrid()
	delete(g.Floor, Cell{10, 0})
	if g.WithDeparture(departure).Clear(Point{1, 0}) {
		t.Fatal("departure footprint moved")
	}
}

// Full decoder regression: an offset floor must not absorb raised obstacles,
// measured drops or absent cells when the floor reference changes.
func TestCloudOffsetFloorPreservesHazardsAndUnknown(t *testing.T) {
	now := time.Now()
	for _, kind := range []string{"obstacle", "drop", "missing", "buried"} {
		t.Run(kind, func(t *testing.T) {
			voxels := make([]byte, 2048*9)
			for i := 2048 * 2; i < 2048*3; i++ {
				voxels[i] = 255
			}
			for y := 38; y < 42; y++ {
				for x := 38; x < 42; x++ {
					index, mask := y*16+x/8, byte(1<<uint(7-x%8))
					if kind == "missing" {
						voxels[2048*2+index] &^= mask
					} else {
						layer := 6
						if kind == "drop" {
							layer = 0
							voxels[2048*2+index] &^= mask
						}
						if kind == "buried" {
							layer = 0
						}
						voxels[layer*2048+index] |= mask
					}
				}
			}
			compressed := make([]byte, lz4.CompressBlockBound(len(voxels)))
			n, err := lz4.CompressBlock(voxels, compressed, nil)
			if err != nil || n == 0 {
				t.Fatal(err)
			}
			meta, _ := json.Marshal(map[string]any{"topic": CloudTopic, "data": map[string]any{"src_size": len(voxels), "origin": []float64{0, 0, -.2}, "resolution": .05, "frame_id": "odom", "stamp": float64(now.UnixNano()) / 1e9}})
			header := make([]byte, 4)
			binary.LittleEndian.PutUint16(header, uint16(len(meta)))
			packet := append(append(header, meta...), compressed[:n]...)
			o := Observations{Pose: Pose{Position: Point{2, 2}, Height: .3, Received: now, Captured: now}}
			if err := o.Receive(CloudTopic, packet, true, now, now); err != nil {
				t.Fatal(err)
			}
			if kind == "buried" {
				if math.Abs(o.FloorHeight+.1) > 1e-6 || !o.Grid.Clear(Point{2, 2}) {
					t.Fatal("occupied volume beneath observed floor became a false drop")
				}
				return
			}
			if math.Abs(o.FloorHeight+.1) > 1e-6 || o.Grid.Clear(Point{2, 2}) {
				t.Fatal("height correction erased hazard/gap")
			}
			if kind != "missing" && o.Grid.WithDeparture(&Departure{Position: Point{2, 2}}).Clear(Point{2, 2}) {
				t.Fatal("departure allowance erased measured hazard")
			}
		})
	}
}

func TestCloudLocalizationDoesNotRequireNavigableFloor(t *testing.T) {
	now := time.Now()
	voxels := make([]byte, 2048*30)
	set := func(x, y, z int) { voxels[z*2048+y*16+x/8] |= byte(1 << uint(7-x%8)) }
	// Two competing ground heights, so the level-floor check must fail.
	for x := 0; x < 128; x++ {
		for y := 0; y < 128; y++ {
			z := 0
			if x >= 40 {
				z = 4
			}
			set(x, y, z)
		}
	}
	// Vertical structure remains usable independently of the ground.
	for y := 0; y < 128; y++ {
		for z := 10; z < 24; z++ {
			set(20, y, z)
			set(90, y, z)
		}
	}
	compressed := make([]byte, lz4.CompressBlockBound(len(voxels)))
	n, err := lz4.CompressBlock(voxels, compressed, nil)
	if err != nil || n == 0 {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{"topic": CloudTopic, "data": map[string]any{"src_size": len(voxels), "origin": []float64{0, 0, -.2}, "resolution": .05, "frame_id": "odom", "stamp": float64(now.UnixNano()) / 1e9}})
	header := make([]byte, 4)
	binary.LittleEndian.PutUint16(header, uint16(len(meta)))
	packet := append(append(header, meta...), compressed[:n]...)
	o := Observations{Pose: Pose{Position: Point{2, 2}, Height: .3, Received: now, Captured: now}}
	if err := o.Receive(CloudTopic, packet, true, now, now); err != nil {
		t.Fatal(err)
	}
	if o.LocalizationReady(now) != nil || len(o.Landmarks) < 40 {
		t.Fatal("floor rejection disabled valid structural matching")
	}
	if o.Ready(now) == nil || len(o.Grid.Floor) > 0 {
		t.Fatal("localization enabled navigation over uncertain floor")
	}
	if o.LocalizationReady(now.Add(MaxAge+time.Nanosecond)) == nil {
		t.Fatal("stale localization scan accepted")
	}
	if o.Receive(CloudTopic, packet, true, now, now) == nil || o.LocalizationReady(now) == nil {
		t.Fatal("replayed packet retained localization readiness")
	}
	o.SetArrivalTiming(true)
	if len(o.Landmarks) != 0 || o.LocalizationReady(now) == nil {
		t.Fatal("mode change retained old structural scan")
	}
}

func TestStructuralLandmarksRejectIsolatedClutter(t *testing.T) {
	var points []Vertex
	for x := 0; x < 50; x++ {
		for z := 0; z < 16; z++ {
			points = append(points, Vertex{float64(x) * .1, 0, .3 + float64(z)*.05})
		}
	}
	// Multiple flat surfaces at separate heights are not continuous structure.
	for x := 0; x < 50; x++ {
		points = append(points, Vertex{float64(x) * .1, 1, .4}, Vertex{float64(x) * .1, 1, 1.1})
	}
	// A short but dense chair side is not a useful wall landmark either.
	for x := 0; x < 50; x++ {
		for z := 0; z < 9; z++ {
			points = append(points, Vertex{float64(x) * .1, 2, .3 + float64(z)*.05})
		}
	}
	landmarks := structuralLandmarks(points, .3)
	if len(landmarks) < 40 {
		t.Fatal("vertical structure lost")
	}
	for _, p := range landmarks {
		if p.Y > .5 {
			t.Fatal("isolated clutter used for wall matching")
		}
	}
}

func TestFloorSurfaceIgnoresBuriedOccupancyAndKeepsClutterBlocked(t *testing.T) {
	var points []Vertex
	for x := -20; x <= 20; x++ {
		for y := -20; y <= 20; y++ {
			px, py := float64(x)*.1+.025, float64(y)*.1+.025
			points = append(points, Vertex{px, py, -.025})
			// Uneven accumulated occupancy underneath the visible top surface.
			for z := -10; z < 0; z++ {
				if (x+y+z)%3 != 0 {
					points = append(points, Vertex{px, py, -.025 + float64(z)*.05})
				}
			}
			// Low clutter occupies part of the neighborhood, not its walking surface.
			if x > 7 && y > 0 {
				points = append(points, Vertex{px, py, .125})
			}
		}
	}
	height, err := observedFloorHeight(points, Point{})
	if err != nil || math.Abs(height+.025) > 1e-6 {
		t.Fatalf("failed to find upper floor: %v %v", height, err)
	}
	g := navigationGrid(points, height)
	if !g.Clear(Point{}) {
		t.Fatal("buried occupancy blocked level floor")
	}
	if g.Clear(Point{1.2, .6}) {
		t.Fatal("raised clutter was accepted as traversable floor")
	}
	// Isolated lower occupancy does not demonstrate a walkable surface.
	g = navigationGrid([]Vertex{{1, 0, -.3}}, 0)
	if g.Clear(Point{1, 0}) || g.WithDeparture(&Departure{Position: Point{}}).Clear(Point{1, 0}) {
		t.Fatal("lower occupancy or departure allowance invented floor ahead")
	}
}

func TestLowerWallLandmarksKeepOccludedWallsAndRejectTableLegs(t *testing.T) {
	var points []Vertex
	// Walls visible from 10 to 70 cm above the measured floor. Their upper
	// portion alone is too short for the original body-height filter.
	for i := 0; i < 24; i++ {
		for z := 0; z < 13; z++ {
			h := .125 + float64(z)*.05
			points = append(points, Vertex{1.025 + float64(i)*.1, -1.025, h}, Vertex{1.025, -1.025 + float64(i)*.1, h})
		}
	}
	// Four dense legs plus a large flat tabletop must not become a wall.
	for _, p := range []Point{{-2, -2}, {-2, -1}, {-1, -2}, {-1, -1}} {
		for z := 0; z < 15; z++ {
			points = append(points, Vertex{p.X, p.Y, .125 + float64(z)*.05})
		}
	}
	for x := -20; x <= -10; x++ {
		for y := -20; y <= -10; y++ {
			points = append(points, Vertex{float64(x) * .1, float64(y) * .1, .825})
		}
	}
	if localizationStructure(structuralLandmarks(points, .32)) == nil {
		t.Fatal("fixture no longer exercises occluded upper walls")
	}
	landmarks := lowerWallLandmarks(points, .025, .32)
	if err := localizationStructure(landmarks); err != nil {
		t.Fatalf("lower walls rejected: %v (%d)", err, len(landmarks))
	}
	for _, p := range landmarks {
		if p.X < 0 {
			t.Fatalf("table clutter became a wall: %+v", p)
		}
	}
	if len(lowerWallLandmarks(points, math.NaN(), .32)) != 0 || len(lowerWallLandmarks(points, 1, .32)) != 0 {
		t.Fatal("invalid floor admitted lower walls")
	}
}

func TestLowerWallLandmarksDoNotAcceptShortOrFlatStructure(t *testing.T) {
	var points []Vertex
	for i := 0; i < 30; i++ {
		for z := 0; z < 9; z++ {
			points = append(points, Vertex{float64(i) * .1, 1, .125 + float64(z)*.05})
		}
	}
	for i := 0; i < 30; i++ {
		points = append(points, Vertex{float64(i) * .1, 2, .125}, Vertex{float64(i) * .1, 2, .825})
	}
	if len(lowerWallLandmarks(points, .025, .32)) != 0 {
		t.Fatal("short furniture or separated flat surfaces became walls")
	}
}

func TestLowerWallLandmarksStillNeedMoreThanOneWall(t *testing.T) {
	var points []Vertex
	for x := 0; x < 30; x++ {
		for z := 0; z < 13; z++ {
			points = append(points, Vertex{.025 + float64(x)*.1, 1.025, .125 + float64(z)*.05})
		}
	}
	landmarks := lowerWallLandmarks(points, .025, .32)
	if len(landmarks) < 24 {
		t.Fatal("fixture should contain a long observed wall")
	}
	if localizationStructure(landmarks) == nil {
		t.Fatal("single lower wall enabled localization")
	}
}

func TestSparseWallStripsProvideDistributedLandmarks(t *testing.T) {
	var points []Vertex
	for i := 0; i < 30; i++ {
		// A wall is tall but each individual column has only four samples.
		// Neighboring columns sample different heights in the same surface.
		for _, z := range []int{0, 3, 7, 10} {
			h := .325 + float64(z+i%3)*.05
			points = append(points, Vertex{1.025 + float64(i)*.1, -1.025, h}, Vertex{1.025, -1.025 + float64(i)*.1, h})
		}
	}
	if len(verticalColumns(points, .27, 1.52)) != 0 {
		t.Fatal("fixture must be too sparse for independent dense columns")
	}
	landmarks := structuralLandmarks(points, .32)
	if err := localizationStructure(landmarks); err != nil {
		t.Fatalf("distributed sparse walls lost: %v (%d landmarks)", err, len(landmarks))
	}
	// A long strip with only three repeated height layers is still clutter.
	points = nil
	for i := 0; i < 30; i++ {
		for _, h := range []float64{.325, .625, .925} {
			points = append(points, Vertex{1.025 + float64(i)*.1, -1.025, h})
		}
	}
	if len(structuralLandmarks(points, .32)) != 0 {
		t.Fatal("separated horizontal layers became a wall")
	}
}

func TestCloudIncludesLowerWallsWhenUpperWallsAlreadyPass(t *testing.T) {
	now := time.Now()
	voxels := make([]byte, 2048*24)
	set := func(x, y, z int) { voxels[z*2048+y*16+x/8] |= 1 << uint(7-x%8) }
	for x := 0; x < 128; x++ {
		for y := 0; y < 128; y++ {
			set(x, y, 0)
		}
	}
	// Two tall walls already provide enough spread to pass the minimum.
	var upper []Vertex
	for i := 10; i < 70; i++ {
		for z := 6; z <= 20; z++ {
			set(10, i, z)
			set(i, 10, z)
			upper = append(upper, Vertex{.5, float64(i) * .05, float64(z) * .05}, Vertex{float64(i) * .05, .5, float64(z) * .05})
		}
		// An obscured wall is visible from 10 to 65 cm above the floor.
		for z := 2; z <= 13; z++ {
			set(80, i, z)
		}
	}
	if err := localizationStructure(structuralLandmarks(upper, .32)); err != nil {
		t.Fatalf("upper-wall fixture must already pass: %v", err)
	}
	compressed := make([]byte, lz4.CompressBlockBound(len(voxels)))
	n, err := lz4.CompressBlock(voxels, compressed, nil)
	if err != nil || n == 0 {
		t.Fatalf("compress fixture: %v", err)
	}
	meta, _ := json.Marshal(map[string]any{"topic": CloudTopic, "data": map[string]any{"src_size": len(voxels), "origin": []float64{0, 0, 0}, "resolution": .05, "frame_id": "odom", "stamp": float64(now.UnixNano()) / 1e9}})
	header := make([]byte, 4)
	binary.LittleEndian.PutUint16(header, uint16(len(meta)))
	packet := append(append(header, meta...), compressed[:n]...)
	o := Observations{Pose: Pose{Position: Point{2, 2}, Height: .32, Received: now, Captured: now}}
	if err := o.Receive(CloudTopic, packet, true, now, now); err != nil {
		t.Fatal(err)
	}
	for _, p := range o.Landmarks {
		if p.X >= 4 && p.Y > 1 && p.Y < 3 {
			return
		}
	}
	t.Fatal("passing upper-wall readiness discarded the lower wall")
}

func TestOccludedBuriedLayerIsNotAnIndependentDrop(t *testing.T) {
	var points []Vertex
	for x := -10; x <= 10; x++ {
		for y := -10; y <= 10; y++ {
			px, py := float64(x)*CellSize+.025, float64(y)*CellSize+.025
			// The captured failure had continuous layers at -10, -15 and
			// -20 cm, also underneath neighboring measured floor cells.
			for _, z := range []float64{-.2, -.15, -.1} {
				points = append(points, Vertex{px, py, z})
			}
			if x < -3 || x > 2 || y < -2 || y > 1 {
				points = append(points, Vertex{px, py, 0})
			}
		}
	}
	g := navigationGrid(points, 0)
	if g.Obstacles[Cell{}] || g.Floor[Cell{}] {
		t.Fatal("buried layer must remain unknown")
	}
	if g.ClearPose(Point{}, 0) {
		t.Fatal("unknown floor allowed without supervision")
	}
	if !g.WithUnobservedFloor(Point{}).Turn(Point{}, 0, 1) {
		t.Fatal("buried volume blocked supervised turn")
	}
	// Distinct lower surfaces and raised returns retain their blocking meaning.
	points = append(points, Vertex{2.025, 2.025, -.2}, Vertex{2.125, 2.025, -.2}, Vertex{2.025, 2.125, -.2}, Vertex{2.125, 2.125, -.2}, Vertex{.025, .025, .15})
	g = navigationGrid(points, 0)
	if !g.Obstacles[Cell{20, 20}] || !g.Obstacles[Cell{}] {
		t.Fatal("drop or raised obstacle was erased")
	}
}

func TestDistinctLowerSurfaceBesideBuriedFloorStillBlocks(t *testing.T) {
	var points []Vertex
	for x := -5; x <= 5; x++ {
		for y := -5; y <= 5; y++ {
			px, py := float64(x)*CellSize+.025, float64(y)*CellSize+.025
			if x >= 0 && x <= 1 && y >= 0 && y <= 1 {
				points = append(points, Vertex{px, py, -.2})
			} else {
				points = append(points, Vertex{px, py, 0}, Vertex{px, py, -.1})
			}
		}
	}
	g := navigationGrid(points, 0)
	if !g.Obstacles[Cell{}] || g.WithUnobservedFloor(Point{}).ClearPose(Point{}, 0) {
		t.Fatal("a distinct lower surface was erased by nearby buried occupancy")
	}
}
