package sitenav

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/pierrec/lz4/v4"
)

func TestObstacleVoxelClearanceUsesBodyAndMeasuredSize(t *testing.T) {
	for _, test := range []struct {
		name  string
		point Point
		clear bool
	}{
		{"beside body in same coarse cell", Point{0, .19}, true},
		{"front of body in same coarse cell", Point{.39, 0}, true},
		{"voxel edge intersects side", Point{0, .17}, false},
		{"voxel edge intersects front", Point{.37, 0}, false},
		{"center", Point{}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			g := navigationGrid([]Vertex{{test.point.X, test.point.Y, .2}}, 0)
			g.Floor = floorGrid().Floor
			if g.ClearPose(Point{}, 0) != test.clear {
				t.Fatal("incorrect body/voxel overlap")
			}
			if (g.PoseClearanceError(Point{}, 0, "Go2") == nil) != test.clear {
				t.Fatal("readiness disagrees with collision check")
			}
			if g.WithUnobservedFloor(Point{}).ClearPose(Point{}, 0) != test.clear {
				t.Fatal("supervised policy lost voxel detail")
			}
		})
	}
	g := navigationGrid([]Vertex{{0, .19, .2}}, 0)
	g.Floor = floorGrid().Floor
	if !g.Sweep(Point{}, Point{1, 0}, 0) || g.Turn(Point{}, 0, math.Pi/2) {
		t.Fatal("side clearance must allow translation but block a turn into the voxel")
	}
	// A small voxel between endpoints still blocks the continuous sweep.
	g = navigationGrid([]Vertex{{1, 0, .2}}, 0)
	g.Floor = floorGrid().Floor
	if !g.ClearPose(Point{}, 0) || !g.ClearPose(Point{2, 0}, 0) || g.Sweep(Point{}, Point{2, 0}, 0) {
		t.Fatal("missed intermediate voxel collision")
	}
}

func TestOverheadVoxelsDoNotBecomeFloorObstacles(t *testing.T) {
	for _, floor := range []float64{0, -.075} {
		for _, height := range []float64{.2, .4, .42, .45, .60, .65} {
			g := navigationGrid([]Vertex{{-.23, .17, floor + height}}, floor).WithUnobservedFloor(Point{})
			clear := height > StandingHeight+.025
			if g.ClearPose(Point{}, 0) != clear || g.Turn(Point{}, 0, math.Pi/3) != clear {
				t.Fatalf("height %.2f above floor %.3f: wrong clearance", height, floor)
			}
		}
	}
	// Removing overhead occupancy must not erase the floor's independent drop.
	points := []Vertex{{0, 0, -.2}, {.1, 0, -.2}, {0, .1, -.2}, {.1, .1, -.2}, {0, 0, .65}}
	g := navigationGrid(points, 0).WithUnobservedFloor(Point{})
	if g.ClearPose(Point{}, 0) || g.Turn(Point{}, 0, 1) {
		t.Fatal("overhead filter erased a measured drop")
	}
}

func TestCloudBodyHeightControlsOverheadClearance(t *testing.T) {
	for _, test := range []struct {
		name       string
		baseHeight float64
		layer      int
		clear      bool
	}{
		{"table above normal stance", .31, 13, true},
		{"body height obstacle", .31, 7, false},
		{"raised stance reaches obstacle", .55, 12, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Now()
			voxels := make([]byte, 2048*(test.layer+1))
			for i := 0; i < 2048; i++ {
				voxels[i] = 255
			}
			// A return 25 cm behind and 15 cm left of the odometry base.
			voxels[test.layer*2048+43*16+35/8] |= 1 << uint(7-35%8)
			compressed := make([]byte, lz4.CompressBlockBound(len(voxels)))
			n, err := lz4.CompressBlock(voxels, compressed, nil)
			if err != nil || n == 0 {
				t.Fatal("compression failed", err)
			}
			meta := map[string]any{"src_size": len(voxels), "origin": []float64{0, 0, 0}, "resolution": .05, "frame_id": "odom", "stamp": float64(now.UnixNano()) / 1e9}
			header, err := json.Marshal(map[string]any{"topic": CloudTopic, "data": meta})
			if err != nil {
				t.Fatal(err)
			}
			raw := make([]byte, 12)
			binary.LittleEndian.PutUint16(raw, 2)
			binary.LittleEndian.PutUint32(raw[4:], uint32(len(header)))
			raw = append(append(raw, header...), compressed[:n]...)
			o := Observations{Pose: Pose{Position: Point{2, 2}, Height: test.baseHeight, Received: now, Captured: now}}
			if err := o.Receive(CloudTopic, raw, true, now, now); err != nil {
				t.Fatal(err)
			}
			if o.Grid.Turn(o.Pose.Position, 0, math.Pi/3) != test.clear {
				t.Fatal("decoded cloud used wrong body height")
			}
		})
	}
}
