package occupancy

import (
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/spatialsim"
)

type probeKnobs struct {
	name      string
	exact     bool          // exact reference instead of default noise
	shift     geom.Vec2     // pose offset applied to shiftDev ("" = all)
	yaw       float64       // yaw offset
	shiftDev  spatial.DeviceID
	noMount   bool          // identity lidar mount
	noOffset  bool          // ignore clock offset
	window    time.Duration // 0 = acceptanceWindow
	devices   []spatial.DeviceID
	noActors  bool
	emptyMap  bool // score an all-free map on the observed cells
	dropHalf  bool // drop every second layer cell block (keep only tiles with even ix+iz)
}

type probeResult struct {
	agree, observed  int
	perFloor         map[spatial.FloorID][2]int
	digest           string
	seenTotal        int // layer cells with evidence (unique per floor)
	seenOutside      int // outside the truth raster
	seenTruthUnknown int
	allFree          int // agreement of an all-free map on observed cells
	truthOcc, occHit int // truth occupied observed cells, and those the map calls occupied
	mapOcc           int // map occupied among observed
	wrong            map[string]int
	furnFree         map[string]int
}

func probeBuild(t *testing.T, seed uint64, k probeKnobs) (*Replica, *spatialsim.Site) {
	t.Helper()
	s, err := spatialsim.Office(seed, spatialsim.Options{NoActors: k.noActors})
	if err != nil {
		t.Fatal(err)
	}
	rc := spatialsim.DefaultReferenceConfig()
	if k.exact {
		rc = spatialsim.ReferenceConfig{}
	}
	ref, err := s.Reference(rc)
	if err != nil {
		t.Fatal(err)
	}
	ids := k.devices
	if ids == nil {
		ids = []spatial.DeviceID{spatialsim.Rover1, spatialsim.Rover2, spatialsim.Rover3, spatialsim.Climber, spatialsim.Ranger}
	}
	window := k.window
	if window == 0 {
		window = acceptanceWindow
	}
	robots := map[spatial.DeviceID]*robot{}
	fleet := make([]*robot, len(ids))
	for i, id := range ids {
		r := &robot{id: id, replica: mustReplica(t, DefaultConfig())}
		r.mapper = mustMapper(t, r.replica, id)
		cal, ok := ref.Calibration(id, spatialsim.SensorLidar, s.At(0))
		if !ok {
			t.Fatalf("%s: no cal", id)
		}
		if k.noMount {
			cal.SensorToParent = geom.IdentityPose3()
		}
		if k.noOffset {
			cal.TimeOffset = 0
		}
		if err := r.mapper.Calibrate(cal); err != nil {
			t.Fatal(err)
		}
		r.offset = cal.TimeOffset
		robots[id], fleet[i] = r, r
	}
	sync := func() {
		for _, r := range fleet {
			mustCommit(t, r.mapper)
		}
		for i, to := range fleet {
			from := fleet[(i+1)%len(fleet)]
			for _, l := range from.replica.Delta(to.replica.Summary()) {
				if _, err := to.replica.Apply(l); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	nextSync := syncEvery
	for m := range s.Stream(s.At(0), s.At(window), spatialsim.Filter{Devices: ids, Kinds: []spatial.SensorKind{spatial.KindLidar2D}}) {
		scan, ok := m.(spatial.Scan)
		if !ok {
			continue
		}
		r := robots[scan.Device]
		at := scan.Time.Add(-r.offset).Sub(s.Epoch())
		for ; r.nextPose <= at+poseStep; r.nextPose += poseStep {
			if pose, ok := ref.DevicePose(r.id, s.At(r.nextPose)); ok {
				if k.shiftDev == "" || k.shiftDev == r.id {
					pose.PositionM[0] += k.shift.X
					pose.PositionM[2] += k.shift.Z
					pose.Yaw += k.yaw
				}
				if err := r.mapper.Locate(s.At(r.nextPose), pose); err != nil {
					t.Fatal(err)
				}
			}
		}
		if err := r.mapper.Ingest(scan, s.Input(scan)); err != nil {
			t.Fatal(err)
		}
		if at >= nextSync {
			sync()
			nextSync += syncEvery
		}
	}
	for range fleet {
		sync()
	}
	return fleet[0].replica, s
}

func probeFleet(t *testing.T, seed uint64, k probeKnobs) probeResult {
	t.Helper()
	fused, s := probeBuild(t, seed, k)
	window := k.window
	if window == 0 {
		window = acceptanceWindow
	}
	res := probeResult{digest: fused.Digest(), perFloor: map[spatial.FloorID][2]int{}, wrong: map[string]int{}, furnFree: map[string]int{}}
	tr := s.Truth()
	cell := fused.Config().Grid.CellSize
	objs := tr.Objects(s.At(window))
	for _, floor := range fused.Floors() {
		g := mustGrid(t, fused, floor)
		raster, err := tr.Occupancy(floor, s.At(window), cell)
		if err != nil {
			t.Fatal(err)
		}
		seen := make([]bool, raster.Width*raster.Height)
		side := int32(fused.Config().TileSide)
		uniq := map[grid.Cell]bool{}
		for _, l := range fused.Delta(nil) {
			if l.Floor != floor {
				continue
			}
			for i, b := range l.Cells {
				if b == 0 {
					continue
				}
				c := grid.Cell{IX: l.IX*side + int32(i)%side, IZ: l.IZ*side + int32(i)/side}
				p := g.Center(c)
				ix, iz := int((p.X-raster.Origin.X)/cell), int((p.Z-raster.Origin.Z)/cell)
				in := p.X >= raster.Origin.X && p.Z >= raster.Origin.Z && ix < raster.Width && iz < raster.Height
				if !uniq[c] {
					uniq[c] = true
					res.seenTotal++
					if !in {
						res.seenOutside++
					} else if raster.Cells[iz*raster.Width+ix] == spatialsim.CellUnknown {
						res.seenTruthUnknown++
					}
				}
				if in {
					seen[iz*raster.Width+ix] = true
				}
			}
		}
		fa, fo := 0, 0
		for iz := 0; iz < raster.Height; iz++ {
			for ix := 0; ix < raster.Width; ix++ {
				want := raster.Cells[iz*raster.Width+ix]
				if want == spatialsim.CellUnknown || !seen[iz*raster.Width+ix] {
					continue
				}
				got := g.StateAt(raster.Centre(ix, iz))
				if k.emptyMap {
					got = grid.Free
				}
				if k.dropHalf {
					tx, tz := grid.TileOf(g.CellAt(raster.Centre(ix, iz)), int(side))
					if (tx+tz)%2 != 0 {
						got = grid.Unknown
					}
				}
				ok := (want == spatialsim.CellOccupied && got == grid.Occupied) || (want == spatialsim.CellFree && got == grid.Free)
				fo++
				if ok {
					fa++
				} else {
					cls := raster.Classes[iz*raster.Width+ix]
					res.wrong[fmt.Sprintf("%s: truth %s, map %v", floor, cls, got)]++
					if cls == spatialsim.ClassFurniture && got == grid.Free {
						for _, o := range objs {
							if o.Floor == floor && o.Shape.Contains(raster.Centre(ix, iz)) {
								res.furnFree[fmt.Sprintf("%s(h=%.2f)", o.ID, o.Shape.Height)]++
							}
						}
					}
				}
				if want == spatialsim.CellFree {
					res.allFree++
				}
				if want == spatialsim.CellOccupied {
					res.truthOcc++
					if got == grid.Occupied {
						res.occHit++
					}
				}
				if got == grid.Occupied {
					res.mapOcc++
				}
			}
		}
		res.perFloor[floor] = [2]int{fa, fo}
		res.agree, res.observed = res.agree+fa, res.observed+fo
	}
	return res
}

func (r probeResult) log(t *testing.T, name string) {
	keys := make([]string, 0, len(r.wrong))
	for k := range r.wrong {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var floors []string
	for f, v := range r.perFloor {
		floors = append(floors, fmt.Sprintf("%s=%.4f(%d)", f, float64(v[0])/float64(v[1]), v[1]))
	}
	sort.Strings(floors)
	t.Logf("PROBE %s: agreement %.4f on %d; floors %v; seen %d (outside raster %d, truth unknown %d); all-free baseline %.4f; occ recall %.4f (%d/%d) map-occ %d; digest %s",
		name, float64(r.agree)/float64(r.observed), r.observed, floors, r.seenTotal, r.seenOutside, r.seenTruthUnknown,
		float64(r.allFree)/float64(r.observed), float64(r.occHit)/float64(r.truthOcc), r.occHit, r.truthOcc, r.mapOcc, r.digest[:12])
	for _, k := range keys {
		t.Logf("PROBE %s:   wrong %-40s %d", name, k, r.wrong[k])
	}
	if len(r.furnFree) > 0 {
		t.Logf("PROBE %s:   furniture-free by object %v", name, r.furnFree)
	}
}

func TestZZProbeFleet(t *testing.T) {
	which := os.Getenv("PROBE")
	if which == "" {
		t.Skip()
	}
	seed := uint64(1)
	fmt.Sscanf(os.Getenv("PROBE_SEED"), "%d", &seed)
	all := []probeKnobs{
		{name: "baseline"},
		{name: "baseline-again"},
		{name: "exact", exact: true},
		{name: "exact-noactors", exact: true, noActors: true},
		{name: "allfree", emptyMap: true},
		{name: "shift-all-0.5", shift: geom.Vec2{X: 0.5}},
		{name: "shift-all-0.2", shift: geom.Vec2{X: 0.2}},
		{name: "shift-all-0.1", shift: geom.Vec2{X: 0.1}},
		{name: "shift-rover1-0.5", shift: geom.Vec2{X: 0.5}, shiftDev: spatialsim.Rover1},
		{name: "yaw-all-0.1", yaw: 0.1},
		{name: "nomount", noMount: true},
		{name: "nooffset", noOffset: true},
		{name: "drophalf", dropHalf: true},
	}
	for _, k := range all {
		if which != "" && which != k.name && which != "all" {
			continue
		}
		if which == "" && k.name != "baseline" {
			continue
		}
		probeFleet(t, seed, k).log(t, k.name)
	}
}
