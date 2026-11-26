package semantic

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/spatialsim"
)

// Design targets of feature 8 (specs/superpowers/specs/2026-11-25-spatial-intelligence-design.md).
const (
	targetRoomIoU       = 0.8 // mean IoU of the best one-to-one room matching, with the correct room count
	targetCellAccuracy  = 0.9 // cell class accuracy on observed cells
	targetLabelAccuracy = 0.8 // share of scored rooms with the right label
)

// acceptanceWindow is the stretch of the Office scenario the maps and the
// object votes are built from: all of the static period before the scheduled
// changes, which is two or more loops of every robot.
const acceptanceWindow = spatialsim.OfficeChangesFrom

// seeds reads SPATIAL_SEEDS (comma-separated unsigned integers); the default
// is a single seed.
func seeds(t *testing.T) []uint64 {
	t.Helper()
	env := os.Getenv("SPATIAL_SEEDS")
	if env == "" {
		return []uint64{1}
	}
	var out []uint64
	for _, f := range strings.Split(env, ",") {
		v, err := strconv.ParseUint(strings.TrimSpace(f), 10, 64)
		if err != nil {
			t.Fatalf("SPATIAL_SEEDS: %v", err)
		}
		out = append(out, v)
	}
	return out
}

// officeScenes runs the streaming Segmenter on the Office scenario the way an
// engine would: per-floor maps, camera calibrations and device poses from the
// upstream stand-in (Site.Reference with its default noise), camera frames
// from the stream.
func officeScenes(t *testing.T, s *spatialsim.Site, window time.Duration) (map[spatial.FloorID]*Scene, *Segmenter) {
	t.Helper()
	ref, err := s.Reference(spatialsim.DefaultReferenceConfig())
	if err != nil {
		t.Fatal(err)
	}
	seg, err := NewSegmenter(Config{})
	if err != nil {
		t.Fatal(err)
	}
	// Floor elevations are what the multifloor feature publishes; the
	// stand-in is the elevation of the reference device poses on each floor.
	elevation := map[spatial.FloorID]float64{}
	for _, dev := range s.Devices() {
		for at := time.Duration(0); at < window; at += 5 * time.Second {
			if p, ok := ref.DevicePose(dev, s.At(at)); ok {
				elevation[p.Floor] = p.PositionM[1]
			}
		}
	}
	for floor, y := range elevation {
		g, err := ref.Grid(floor, s.At(0), s.At(window))
		if err != nil {
			t.Fatal(err)
		}
		if err := seg.SetMap(floor, y, g); err != nil {
			t.Fatal(err)
		}
	}
	// Camera calibrations as they stand at the end of the window, where
	// every camera of the window has started.
	offsets := map[spatial.DeviceID]time.Duration{}
	mounted := map[spatial.DeviceID]bool{}
	for _, m := range s.Manifests() {
		if cal, ok := ref.Calibration(m.Device, m.Sensor, s.At(window-time.Second)); ok && m.Modality == spatial.KindCamera {
			seg.SetCalibration(cal)
			offsets[m.Device], mounted[m.Device] = cal.TimeOffset, cal.Parent == spatial.ParentDeviceBase
		}
	}
	for m := range s.Stream(s.At(0), s.At(window), spatialsim.Filter{Kinds: []spatial.SensorKind{spatial.KindCamera}}) {
		frame, ok := m.(spatial.CameraFrame)
		if !ok {
			continue
		}
		if at := frame.Time.Add(-offsets[frame.Device]); mounted[frame.Device] {
			if p, ok := ref.DevicePose(frame.Device, at); ok {
				seg.SetDevicePose(p, at)
			}
		}
		if err := seg.Ingest(frame); err != nil {
			t.Fatal(err)
		}
	}
	scenes := map[spatial.FloorID]*Scene{}
	for _, floor := range seg.Floors() {
		sc, err := seg.Scene(floor)
		if err != nil {
			t.Fatalf("%s: %v", floor, err)
		}
		scenes[floor] = sc
	}
	return scenes, seg
}

// score is the tally of one or more floors against Truth.
type score struct {
	rooms, countOK, floors int     // scored truth rooms; floors with the right room count; floors
	iou                    float64 // sum over scored truth rooms
	worstIoU               float64
	labelled               int // scored rooms with the right label
	cells, cellsRight      int // observed cells with a truth class
	structure, structRight int // observed structure cells that are structure in truth
	notes                  []string
}

// scoreFloor compares one floor's scene with Truth at site time 0.
//
// A truth room is scored when a lidar was inside it while on this floor, so
// that its map shows the room from within. Rooms the map only looks into
// through a doorway are left out (the upper office and meeting room, and the
// yard and the ground corridor on each other's maps), as are the lift and
// stair shafts, which Truth lists as portal footprints and not as semantic
// rooms. Cells of rooms that are left out are don't-care for the room
// metrics, and a region that lies mostly on them is not counted.
func scoreFloor(t *testing.T, s *spatialsim.Site, sc *Scene, window time.Duration, total *score) {
	t.Helper()
	tr := s.Truth()
	// The floors on this floor's level: the yard is level with the ground
	// floor and both show up on each other's maps.
	var level []spatial.FloorID
	for _, f := range tr.Floors(s.At(0)) {
		if f.ElevationM == sc.Elevation {
			level = append(level, f.Floor)
		}
	}
	var lidars []spatial.DeviceID
	for _, m := range s.Manifests() {
		if m.Modality == spatial.KindLidar2D {
			lidars = append(lidars, m.Device)
		}
	}
	type truthRoom struct {
		spatialsim.Room
		scored bool
		why    string
	}
	var rooms []truthRoom
	for _, rm := range tr.Rooms() {
		onLevel := false
		for _, f := range level {
			onLevel = onLevel || f == rm.Floor
		}
		if !onLevel {
			continue
		}
		r := truthRoom{Room: rm}
		switch {
		case rm.Portal != "":
			r.why = "shaft of portal " + rm.Portal
		default:
			r.why = "no lidar inside it on this floor"
			for _, dev := range lidars {
				for at := time.Duration(0); at < window && !r.scored; at += 500 * time.Millisecond {
					st, ok := tr.Device(dev, s.At(at))
					r.scored = ok && st.Floor == sc.Floor && geom.PointInPolygon(st.Pose.Pos(), rm.Polygon)
				}
			}
		}
		rooms = append(rooms, r)
	}
	roomAt := func(p geom.Vec2) int {
		for i, r := range rooms {
			if geom.PointInPolygon(p, r.Polygon) {
				return i
			}
		}
		return -1
	}
	rasters := make([]spatialsim.Raster, len(level))
	for i, f := range level {
		var err error
		if rasters[i], err = tr.Occupancy(f, s.At(0), sc.layer.cell); err != nil {
			t.Fatal(err)
		}
	}

	// One pass over the lattice that covers the scene and the truth rooms.
	l := sc.layer
	lo, hi := l.lo, grid.Cell{IX: l.lo.IX + int32(l.w), IZ: l.lo.IZ + int32(l.h)}
	for _, r := range rooms {
		for _, p := range r.Polygon {
			lo.IX, lo.IZ = min(lo.IX, int32(p.X/l.cell)-1), min(lo.IZ, int32(p.Z/l.cell)-1)
			hi.IX, hi.IZ = max(hi.IX, int32(p.X/l.cell)+1), max(hi.IZ, int32(p.Z/l.cell)+1)
		}
	}
	est := len(sc.Rooms)
	estArea, estIgnored := make([]int, est), make([]int, est)
	truthArea := make([]int, len(rooms))
	inter := make([][]int, est)
	for k := range inter {
		inter[k] = make([]int, len(rooms))
	}
	var sum score
	for iz := lo.IZ; iz < hi.IZ; iz++ {
		for ix := lo.IX; ix < hi.IX; ix++ {
			c := grid.Cell{IX: ix, IZ: iz}
			p := geom.Vec2{X: (float64(ix) + 0.5) * l.cell, Z: (float64(iz) + 0.5) * l.cell}
			k, class := -1, ClassUnknown
			if i := l.index(c); i >= 0 {
				k, class = int(sc.region[i])-1, l.class[i]
			}
			j := roomAt(p)
			switch {
			case j >= 0 && !rooms[j].scored:
				if k >= 0 {
					estIgnored[k]++
				}
			default:
				if k >= 0 {
					estArea[k]++
				}
				if j >= 0 {
					truthArea[j]++
				}
				if k >= 0 && j >= 0 {
					inter[k][j]++
				}
			}
			if class == ClassUnknown {
				continue
			}
			truth := spatialsim.ClassNone
			for _, r := range rasters {
				if truth = r.ClassAt(p); truth != spatialsim.ClassNone {
					break
				}
			}
			if truth == spatialsim.ClassNone {
				continue
			}
			sum.cells++
			if class.String() == truth.String() {
				sum.cellsRight++
			}
			if class != ClassFree && truth != spatialsim.ClassFree {
				sum.structure++
				if class.String() == truth.String() {
					sum.structRight++
				}
			}
		}
	}

	// Regions that count, and the truth rooms they are matched with.
	var counted, scored []int
	for k := range sc.Rooms {
		if estIgnored[k] <= estArea[k] {
			counted = append(counted, k)
		} else {
			sum.notes = append(sum.notes, fmt.Sprintf("region %s not counted: mostly on rooms left out", sc.Rooms[k].Region.ID))
		}
	}
	for j, r := range rooms {
		if r.scored {
			scored = append(scored, j)
		} else {
			sum.notes = append(sum.notes, fmt.Sprintf("truth room %s left out: %s", r.ID, r.why))
		}
	}
	if len(counted) > 16 {
		t.Fatalf("%s: %d regions, too many to match", sc.Floor, len(counted))
	}
	iou := func(k, j int) float64 {
		if union := estArea[k] + truthArea[j] - inter[k][j]; union > 0 {
			return float64(inter[k][j]) / float64(union)
		}
		return 0
	}
	// Best one-to-one matching by total IoU: dynamic programme over the
	// scored truth rooms and the set of regions already used.
	best := make([][]float64, len(scored)+1)
	pick := make([][]int, len(scored)+1)
	for a := range best {
		best[a], pick[a] = make([]float64, 1<<len(counted)), make([]int, 1<<len(counted))
	}
	for a := len(scored) - 1; a >= 0; a-- {
		for used := 0; used < 1<<len(counted); used++ {
			best[a][used], pick[a][used] = best[a+1][used], -1
			for b, k := range counted {
				if used&(1<<b) == 0 {
					if v := iou(k, scored[a]) + best[a+1][used|1<<b]; v > best[a][used] {
						best[a][used], pick[a][used] = v, b
					}
				}
			}
		}
	}
	sum.floors, sum.rooms, sum.worstIoU = 1, len(scored), 1
	if len(counted) == len(scored) {
		sum.countOK = 1
	}
	used := 0
	for a, j := range scored {
		b := pick[a][used]
		v, label, id := 0.0, "", "-"
		if b >= 0 {
			used |= 1 << b
			rm := sc.Rooms[counted[b]]
			v, label, id = iou(counted[b], j), rm.Region.Label, rm.Region.ID
		}
		sum.iou += v
		sum.worstIoU = min(sum.worstIoU, v)
		if label == rooms[j].Label {
			sum.labelled++
		}
		t.Logf("  %-12s (%s) <- %-10s IoU %.3f label %q", rooms[j].ID, rooms[j].Label, id, v, label)
	}
	t.Logf("  %s: %d regions counted for %d scored rooms; %s", sc.Floor, len(counted), len(scored), strings.Join(sum.notes, "; "))

	total.floors += sum.floors
	total.countOK += sum.countOK
	total.rooms += sum.rooms
	total.iou += sum.iou
	total.worstIoU = min(total.worstIoU, sum.worstIoU)
	total.labelled += sum.labelled
	total.cells += sum.cells
	total.cellsRight += sum.cellsRight
	total.structure += sum.structure
	total.structRight += sum.structRight
}

// TestOfficeAcceptance scores the semantic scene of every floor of the
// standard Office scenario with default noise against Truth.
func TestOfficeAcceptance(t *testing.T) {
	for _, seed := range seeds(t) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			s, err := spatialsim.Office(seed, spatialsim.Options{})
			if err != nil {
				t.Fatal(err)
			}
			scenes, seg := officeScenes(t, s, acceptanceWindow)
			floors := make([]spatial.FloorID, 0, len(scenes))
			for f := range scenes {
				floors = append(floors, f)
			}
			sort.Slice(floors, func(i, j int) bool { return floors[i] < floors[j] })
			total := score{worstIoU: 1}
			for _, f := range floors {
				scoreFloor(t, s, scenes[f], acceptanceWindow, &total)
			}
			st := seg.Stats()
			t.Logf("camera frames %d, skipped %d, detections placed %d", st.Frames, st.Skipped, st.Placed)

			meanIoU := total.iou / float64(total.rooms)
			cellAcc := float64(total.cellsRight) / float64(total.cells)
			labelAcc := float64(total.labelled) / float64(total.rooms)
			t.Logf("METRIC semantic_room_count_floors_correct=%d floors (target == %d)", total.countOK, total.floors)
			t.Logf("METRIC semantic_room_iou_mean=%.3f ratio (target > %.1f)", meanIoU, targetRoomIoU)
			t.Logf("METRIC semantic_cell_class_accuracy=%.4f ratio (target > %.1f)", cellAcc, targetCellAccuracy)
			t.Logf("METRIC semantic_region_label_accuracy=%.3f ratio (target > %.1f)", labelAcc, targetLabelAccuracy)
			// Not targets, but what the targets hide: the worst room, and the
			// accuracy on structure alone (free cells are most of a map).
			t.Logf("INFO semantic_room_iou_worst=%.3f, structure cells %d of %d right (%.3f), %d rooms scored on %d floors",
				total.worstIoU, total.structRight, total.structure, float64(total.structRight)/float64(max(total.structure, 1)), total.rooms, total.floors)
			if total.countOK != total.floors {
				t.Errorf("room count right on %d of %d floors", total.countOK, total.floors)
			}
			if !(meanIoU > targetRoomIoU) {
				t.Errorf("mean room IoU %.3f, want > %.1f", meanIoU, targetRoomIoU)
			}
			if !(cellAcc > targetCellAccuracy) {
				t.Errorf("cell class accuracy %.4f, want > %.1f", cellAcc, targetCellAccuracy)
			}
			if !(labelAcc > targetLabelAccuracy) {
				t.Errorf("region label accuracy %.3f, want > %.1f", labelAcc, targetLabelAccuracy)
			}
		})
	}
}
