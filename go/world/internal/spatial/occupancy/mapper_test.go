package occupancy

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func testPose(dev spatial.DeviceID, floor spatial.FloorID, x, z, yaw float64) spatial.DevicePose {
	return spatial.DevicePose{
		Device: dev, Floor: floor, FrameID: "site", Location: spatial.At(geom.Vec3{X: x, Z: z}, 0.03), Yaw: yaw,
		Covariance: geom.Mat3{4e-4, 0, 0, 0, 4e-4, 0, 0, 0, 1e-4}, Method: "test", Score: 1,
	}
}

// quarterScan has four beams: forward, right, back, left of the scan frame.
// A range of 0 means the beam carries no information.
func quarterScan(dev spatial.DeviceID, at time.Time, forward, right, back, left float64) spatial.Scan {
	s := spatial.Scan{
		Header:   spatial.Header{Device: dev, Sensor: "lidar", Time: at},
		AngleMin: 0, AngleIncrement: math.Pi / 2, RangeMin: 0.05, RangeMax: 10, RangeSigma: 0.01,
		Ranges: []float64{forward, right, back, left},
	}
	for i, r := range s.Ranges {
		if r == 0 {
			s.Ranges[i] = spatial.RangeInvalid
		}
	}
	return s
}

// roomScan is the scan a lidar at pose would take of walls, by ray casting.
func roomScan(dev spatial.DeviceID, at time.Time, pose geom.Pose2, walls []geom.Segment) spatial.Scan {
	const beams = 720
	s := spatial.Scan{
		Header:   spatial.Header{Device: dev, Sensor: "lidar", Time: at},
		AngleMin: -math.Pi, AngleIncrement: 2 * math.Pi / beams, RangeMin: 0.05, RangeMax: 10, RangeSigma: 0.01,
		Ranges: make([]float64, beams),
	}
	for i := range s.Ranges {
		best := spatial.RangeNoReturn
		for _, w := range walls {
			if r, ok := geom.RaySegment(pose.Pos(), geom.Heading(pose.Yaw+s.Angle(i)), w); ok && (best < 0 || r < best) {
				best = r
			}
		}
		s.Ranges[i] = best
	}
	return s
}

func mustMapper(t testing.TB, r *Replica, source spatial.DeviceID) *Mapper {
	t.Helper()
	m, err := NewMapper(r, source)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func mustCommit(t testing.TB, m *Mapper) []Layer {
	t.Helper()
	layers, err := m.Commit()
	if err != nil {
		t.Fatal(err)
	}
	return layers
}

func mustGrid(t testing.TB, r *Replica, floor spatial.FloorID) *grid.Grid {
	t.Helper()
	g, err := r.Grid(floor)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// Positive yaw is clockwise seen from above and the body +Z axis is to the
// right: the convention every scan placement rests on, checked on single
// beams before any pipeline is trusted.
func TestScanPlacementConventions(t *testing.T) {
	for name, tc := range map[string]struct {
		pose  spatial.DevicePose
		mount geom.Pose2
		scan  spatial.Scan
		hit   geom.Vec2
		free  geom.Vec2
	}{
		"forward along +X": {testPose("r", "ground", 2.02, 3.02, 0), geom.Pose2{}, quarterScan("r", testEpoch, 1, 0, 0, 0), geom.Vec2{X: 3.02, Z: 3.02}, geom.Vec2{X: 2.52, Z: 3.02}},
		"right is +Z":      {testPose("r", "ground", 2.02, 3.02, 0), geom.Pose2{}, quarterScan("r", testEpoch, 0, 1, 0, 0), geom.Vec2{X: 2.02, Z: 4.02}, geom.Vec2{X: 2.02, Z: 3.52}},
		"yaw +90 faces +Z": {testPose("r", "ground", 2.02, 3.02, math.Pi/2), geom.Pose2{}, quarterScan("r", testEpoch, 1, 0, 0, 0), geom.Vec2{X: 2.02, Z: 4.02}, geom.Vec2{X: 2.02, Z: 3.52}},
		"right of +Z is -X": {testPose("r", "ground", 2.02, 3.02, math.Pi/2), geom.Pose2{}, quarterScan("r", testEpoch, 0, 1, 0, 0),
			geom.Vec2{X: 1.02, Z: 3.02}, geom.Vec2{X: 1.52, Z: 3.02}},
		"mount ahead and yawed": {testPose("r", "ground", 2.02, 3.02, math.Pi/2), geom.Pose2{X: 0.2, Yaw: math.Pi / 2}, quarterScan("r", testEpoch, 1, 0, 0, 0),
			geom.Vec2{X: 1.02, Z: 3.22}, geom.Vec2{X: 1.52, Z: 3.22}},
	} {
		r := mustReplica(t, DefaultConfig())
		m := mustMapper(t, r, "r")
		if err := m.Integrate(testEpoch, tc.pose, tc.mount, tc.scan, testInput("r", 1)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		mustCommit(t, m)
		g := mustGrid(t, r, "ground")
		if g.StateAt(tc.hit) != grid.Occupied || g.StateAt(tc.free) != grid.Free || g.Counts().Occupied != 1 {
			t.Errorf("%s: hit cell %v, cell on the beam %v, %d occupied cells", name, g.StateAt(tc.hit), g.StateAt(tc.free), g.Counts().Occupied)
		}
	}
}

// Two robots in the two halves of a walled space each map what they see;
// after one sync each replica holds the whole space.
func TestTwoRobotsShareAMap(t *testing.T) {
	// 8 x 4 m split by a wall at x = 4.02. Walls sit off the cell lattice.
	rect := func(x0, z0, x1, z1 float64) []geom.Segment {
		a, b, c, d := geom.Vec2{X: x0, Z: z0}, geom.Vec2{X: x1, Z: z0}, geom.Vec2{X: x1, Z: z1}, geom.Vec2{X: x0, Z: z1}
		return []geom.Segment{{A: a, B: b}, {A: b, B: c}, {A: c, B: d}, {A: d, B: a}}
	}
	walls := append(rect(0.02, 0.02, 8.02, 4.02), geom.Segment{A: geom.Vec2{X: 4.02, Z: 0.02}, B: geom.Vec2{X: 4.02, Z: 4.02}})
	ra, rb := mustReplica(t, DefaultConfig()), mustReplica(t, DefaultConfig())
	ma, mb := mustMapper(t, ra, "robot-a"), mustMapper(t, rb, "robot-b")
	mount := geom.Pose2{X: 0.1, Z: 0.03, Yaw: 0.1}
	for i := range 6 {
		at := testEpoch.Add(time.Duration(i) * time.Second)
		pa := testPose("robot-a", "ground", 1.5+0.2*float64(i), 2.1, 0.3*float64(i))
		pb := testPose("robot-b", "ground", 6.5-0.2*float64(i), 1.9, -0.4*float64(i))
		if err := ma.Integrate(at, pa, mount, roomScan("robot-a", at, pa.Pose2().Compose(mount), walls), testInput("robot-a", i)); err != nil {
			t.Fatal(err)
		}
		if err := mb.Integrate(at, pb, mount, roomScan("robot-b", at, pb.Pose2().Compose(mount), walls), testInput("robot-b", i)); err != nil {
			t.Fatal(err)
		}
	}
	if ga := ma.floors["ground"].g; ga.StateAt(geom.Vec2{X: 2, Z: 2}) != grid.Free {
		t.Fatal("robot-a did not map its own room")
	}
	if _, err := ra.Grid("ground"); !errors.Is(err, ErrNoEvidence) {
		t.Fatalf("scans reached the replica before Commit: %v", err)
	}
	la, lb := mustCommit(t, ma), mustCommit(t, mb)
	if len(la) == 0 || len(lb) == 0 {
		t.Fatal("nothing committed")
	}
	if g := mustGrid(t, ra, "ground"); g.StateAt(geom.Vec2{X: 6, Z: 2}) != grid.Unknown {
		t.Fatal("robot-a knows the far room before any sync")
	}
	// What a source committed is exactly what it mapped: no tile was missed
	// and, with the default sensor model, no cell was rounded.
	if !mustGrid(t, ra, "ground").Equal(ma.floors["ground"].g) || !mustGrid(t, rb, "ground").Equal(mb.floors["ground"].g) {
		t.Fatal("a replica differs from its mapper's own grid after Commit")
	}

	// One sync each way, as components.
	for _, pull := range []struct{ to, from *Replica }{{ra, rb}, {rb, ra}} {
		for _, l := range pull.from.Delta(pull.to.Summary()) {
			c, err := l.Component(testPublication(l.Source))
			if err != nil {
				t.Fatal(err)
			}
			if changed, err := pull.to.ApplyComponent(c); err != nil || !changed {
				t.Fatal(changed, err)
			}
		}
	}
	if ra.Digest() != rb.Digest() {
		t.Fatal("replicas differ after a two-way sync")
	}
	g := mustGrid(t, ra, "ground")
	for _, p := range []geom.Vec2{{X: 1, Z: 1}, {X: 3, Z: 3.5}, {X: 5, Z: 0.5}, {X: 7.5, Z: 3.5}} {
		if g.StateAt(p) != grid.Free {
			t.Errorf("%v is %v, want free", p, g.StateAt(p))
		}
	}
	for _, p := range []geom.Vec2{{X: 0.02, Z: 2}, {X: 8.02, Z: 2}, {X: 2, Z: 0.02}, {X: 6, Z: 4.02}, {X: 4.02, Z: 1}, {X: 4.02, Z: 3}} {
		if g.StateAt(p) != grid.Occupied {
			t.Errorf("wall at %v is %v, want occupied", p, g.StateAt(p))
		}
	}
	for _, p := range []geom.Vec2{{X: -1, Z: 2}, {X: 4, Z: 5}, {X: 9, Z: 2}} {
		if g.StateAt(p) != grid.Unknown {
			t.Errorf("%v behind a wall is %v, want unknown", p, g.StateAt(p))
		}
	}
	// The dividing wall was seen from both sides: its tile fuses two sources.
	tile, err := ra.Tile("ground", 1, 0)
	if err != nil || len(tile.Versions) != 2 {
		t.Fatalf("tile over the dividing wall: versions %v (%v)", tile.Versions, err)
	}
}

func TestCommitVersionsOnlyWhatChanged(t *testing.T) {
	r := mustReplica(t, DefaultConfig())
	m := mustMapper(t, r, "robot-a")
	pose := testPose("robot-a", "upper", 1.02, 1.02, 0)
	pose.PositionM[1] = 3.5
	scan := func(i int) (time.Time, spatial.Scan, world.Input) {
		at := testEpoch.Add(time.Duration(i) * 100 * time.Millisecond)
		return at, quarterScan("robot-a", at, 1, 0, 0, 0), testInput("robot-a", i)
	}
	at, s, in := scan(0)
	if err := m.Integrate(at, pose, geom.Pose2{}, s, in); err != nil {
		t.Fatal(err)
	}
	first := mustCommit(t, m)
	if len(first) != 1 || first[0].Version != 1 || first[0].LayerKey != (LayerKey{Floor: "upper", Source: "robot-a"}) ||
		first[0].ElevationM != 3.5 || !first[0].Time.Equal(at) || len(first[0].Inputs) != 1 || first[0].Inputs[0].ID != in.ID {
		t.Fatalf("first commit: %+v", first)
	}
	if again := mustCommit(t, m); len(again) != 0 {
		t.Fatalf("a commit without new scans wrote %d layers", len(again))
	}
	if tile, err := r.Tile("upper", 0, 0); err != nil || tile.PositionM[1] != 3.5 {
		t.Fatalf("fused tile elevation: %v (%v)", tile.PositionM, err)
	}

	// The same beam again and again: versions climb until the cells reach
	// their clamps, then the layer stops changing and keeps its version.
	version := uint64(1)
	for i := 1; i <= 3*spatial.MaxInputs; i++ {
		at, s, in := scan(i)
		if err := m.Integrate(at, pose, geom.Pose2{}, s, in); err != nil {
			t.Fatal(err)
		}
		layers := mustCommit(t, m)
		switch {
		case len(layers) == 1 && layers[0].Version == version+1:
			version++
		case len(layers) != 0:
			t.Fatalf("scan %d: commit wrote %+v after version %d", i, layers, version)
		}
	}
	held, _ := r.Layer(first[0].LayerKey)
	if version < 3 || version > 8 || held.Version != version {
		t.Fatalf("layer settled at version %d (replica holds %d); want a few rewrites, then none", version, held.Version)
	}
	if m.Stats().Integrated != 3*spatial.MaxInputs+1 {
		t.Fatalf("stats: %+v", m.Stats())
	}

	// A layer rests on every scan over its tile, reduced to a bounded list
	// that keeps the first and the newest of them.
	at, s, in = scan(1000)
	s.Ranges[1] = 1.5 // a new beam, so the layer changes
	if err := m.Integrate(at, pose, geom.Pose2{}, s, in); err != nil {
		t.Fatal(err)
	}
	last := mustCommit(t, m)
	if len(last) != 1 || len(last[0].Inputs) < 2 || len(last[0].Inputs) > spatial.MaxInputs {
		t.Fatalf("last commit: %+v", last)
	}
	ids := map[string]bool{}
	for _, in := range last[0].Inputs {
		ids[in.ID] = true
	}
	if !ids[testInput("robot-a", 0).ID] || !ids[in.ID] || !last[0].Time.Equal(at) {
		t.Fatalf("ancestry lost the first or the newest scan: %v", last[0].Inputs)
	}
}

// A source that restarts with nothing but what its peers hold continues its
// own layers: the versions go on and the earlier evidence stays.
func TestRestartedSourceContinuesItsLayers(t *testing.T) {
	peer := mustReplica(t, DefaultConfig())
	first := mustMapper(t, peer, "robot-a")
	pose := testPose("robot-a", "ground", 1.02, 1.02, 0)
	for i := range 2 {
		at := testEpoch.Add(time.Duration(i) * time.Second)
		if err := first.Integrate(at, pose, geom.Pose2{}, quarterScan("robot-a", at, 1, 0, 0, 0), testInput("robot-a", i)); err != nil {
			t.Fatal(err)
		}
		mustCommit(t, first)
	}
	old, _ := peer.Layer(LayerKey{Floor: "ground", Source: "robot-a"})

	restarted := mustReplica(t, DefaultConfig())
	second := mustMapper(t, restarted, "robot-a")
	at := testEpoch.Add(10 * time.Second)
	if err := second.Integrate(at, pose, geom.Pose2{}, quarterScan("robot-a", at, 0, 1, 0, 0), testInput("robot-a", 10)); err != nil {
		t.Fatal(err)
	}
	// Its own old layer arrives from the peer before it publishes.
	if n, err := restarted.Merge(peer); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	layers := mustCommit(t, second)
	if len(layers) != 1 || layers[0].Version != 3 || len(layers[0].Inputs) != 3 {
		t.Fatalf("commit after restart: %+v", layers)
	}
	if n, err := peer.Merge(restarted); err != nil || n != 1 || peer.Digest() != restarted.Digest() {
		t.Fatalf("the peer did not take the continued layer (%d, %v)", n, err)
	}
	g := mustGrid(t, peer, "ground")
	if g.StateAt(geom.Vec2{X: 2.02, Z: 1.02}) != grid.Occupied || g.StateAt(geom.Vec2{X: 1.02, Z: 2.02}) != grid.Occupied {
		t.Fatal("evidence from before or after the restart is missing")
	}
	if got, want := g.LogOddsAt(geom.Vec2{X: 2.02, Z: 1.02}), 2*grid.DefaultConfig().Hit; math.Abs(got-want) > 1e-9 {
		t.Fatalf("the old evidence was absorbed as %g, want %g", got, want)
	}
	if again := mustCommit(t, second); len(again) != 0 {
		t.Fatalf("the old layer was absorbed twice: %+v", again)
	}

	// The other order: the restarted source publishes version 1 first, then
	// its old version 2 arrives and wins the merge. The next Commit, with no
	// new scan, must republish this run's evidence on top of the old layer.
	late := mustReplica(t, DefaultConfig())
	third := mustMapper(t, late, "robot-a")
	if err := third.Integrate(at, pose, geom.Pose2{}, quarterScan("robot-a", at, 0, 1, 0, 0), testInput("robot-a", 10)); err != nil {
		t.Fatal(err)
	}
	if layers := mustCommit(t, third); len(layers) != 1 || layers[0].Version != 1 {
		t.Fatalf("first commit after restart: %+v", layers)
	}
	if changed, err := late.Apply(old); err != nil || !changed || old.Version != 2 {
		t.Fatal(changed, err, old.Version)
	}
	layers = mustCommit(t, third)
	if len(layers) != 1 || layers[0].Version != 3 || len(layers[0].Inputs) != 3 {
		t.Fatalf("commit after the old layer superseded this run's: %+v", layers)
	}
	g = mustGrid(t, late, "ground")
	if g.StateAt(geom.Vec2{X: 2.02, Z: 1.02}) != grid.Occupied || g.StateAt(geom.Vec2{X: 1.02, Z: 2.02}) != grid.Occupied {
		t.Fatal("evidence from before or after the restart is missing")
	}
	if again := mustCommit(t, third); len(again) != 0 {
		t.Fatalf("a settled tile was rewritten: %+v", again)
	}
}

// The streaming path places a scan at the pose interpolated at its site
// time: its timestamp less the calibrated clock offset.
func TestStreamInterpolatesPoseAtSiteTime(t *testing.T) {
	const offset = 40 * time.Millisecond
	r := mustReplica(t, DefaultConfig())
	m := mustMapper(t, r, "robot-a")
	cal := spatial.Calibration{
		Device: "robot-a", Sensor: "lidar", Kind: spatial.KindLidar2D, SensorToParent: geom.Pose3FromPose2(geom.Pose2{X: 0.1}, 0),
		Parent: spatial.ParentDeviceBase, ParentDevice: "robot-a", ResidualUnit: "m", Method: "test", Revision: "1", State: spatial.CalibrationValid, TimeOffset: offset,
	}
	if err := m.Calibrate(cal); err != nil {
		t.Fatal(err)
	}
	// The robot drives along +X at 1 m/s; poses arrive every 100 ms.
	locate := func(d time.Duration) {
		t.Helper()
		if err := m.Locate(testEpoch.Add(d), testPose("robot-a", "ground", 1+d.Seconds(), 2.02, 0)); err != nil {
			t.Fatal(err)
		}
	}
	locate(0)
	locate(100 * time.Millisecond)
	locate(200 * time.Millisecond)
	// Taken at site time 250 ms, base at x = 1.25, lidar at 1.35, wall at
	// 3.02: the lidar clock stamps it 290 ms.
	scan := quarterScan("robot-a", testEpoch.Add(250*time.Millisecond+offset), 3.02-1.35, 0, 0, 0)
	if err := m.Ingest(scan, testInput("robot-a", 1)); err != nil {
		t.Fatal(err)
	}
	if s := m.Stats(); s.Pending != 1 || s.Integrated != 0 {
		t.Fatalf("a scan ahead of the newest pose must wait: %+v", s)
	}
	if err := m.Ingest(spatial.Imu{Header: spatial.Header{Device: "robot-a", Sensor: "imu", Time: testEpoch}}, world.Input{}); err != nil {
		t.Fatalf("other measurements are ignored: %v", err)
	}
	locate(300 * time.Millisecond)
	if s := m.Stats(); s != (Stats{Integrated: 1}) {
		t.Fatalf("stats after the bracketing pose: %+v", s)
	}
	layers := mustCommit(t, m)
	if len(layers) != 1 || !layers[0].Time.Equal(testEpoch.Add(250*time.Millisecond)) {
		t.Fatalf("layer time is not the scan's site time: %+v", layers)
	}
	g := mustGrid(t, r, "ground")
	// Ignoring the offset would put the wall 4 cm further, in the next cell.
	if g.StateAt(geom.Vec2{X: 3.02, Z: 2.02}) != grid.Occupied || g.Known(g.CellAt(geom.Vec2{X: 3.07, Z: 2.02})) {
		t.Fatalf("wall cell %v, next cell known %v", g.StateAt(geom.Vec2{X: 3.02, Z: 2.02}), g.Known(g.CellAt(geom.Vec2{X: 3.07, Z: 2.02})))
	}
}

// Every scan the mapper cannot place is refused and counted, never guessed.
func TestStreamRefusesWhatItCannotPlace(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxPending = 2
	r := mustReplica(t, cfg)
	m := mustMapper(t, r, "robot-a")
	cal := spatial.Calibration{
		Device: "robot-a", Sensor: "lidar", Kind: spatial.KindLidar2D, SensorToParent: geom.IdentityPose3(),
		Parent: spatial.ParentDeviceBase, ParentDevice: "robot-a", ResidualUnit: "m", Method: "test", Revision: "1", State: spatial.CalibrationValid,
	}
	at := func(ms int) time.Time { return testEpoch.Add(time.Duration(ms) * time.Millisecond) }
	n := 0
	ingest := func(ms int) {
		t.Helper()
		n++
		if err := m.Ingest(quarterScan("robot-a", at(ms), 1, 0, 0, 0), testInput("robot-a", n)); err != nil {
			t.Fatal(err)
		}
	}
	locate := func(ms int, mutate func(*spatial.DevicePose)) {
		t.Helper()
		p := testPose("robot-a", "ground", 1.02, 1.02, 0)
		if mutate != nil {
			mutate(&p)
		}
		if err := m.Locate(at(ms), p); err != nil {
			t.Fatal(err)
		}
	}
	want := Stats{}
	check := func(what string) {
		t.Helper()
		if got := m.Stats(); got != want {
			t.Fatalf("%s: stats %+v, want %+v", what, got, want)
		}
	}

	ingest(0) // no calibration yet
	want.Uncalibrated++
	check("uncalibrated lidar")
	if err := m.Calibrate(cal); err != nil {
		t.Fatal(err)
	}

	ingest(10) // three scans wait for a pose, the queue holds two
	ingest(20)
	ingest(30)
	want.NoPose, want.Pending = 1, 2
	check("pending overflow")
	locate(100, nil) // the first pose is after them: nothing brackets them
	want.NoPose, want.Pending = 3, 0
	check("scans before the first pose")

	locate(200, nil)
	ingest(150)
	want.Integrated++
	check("bracketed scan")

	locate(900, nil) // 700 ms without a pose
	ingest(500)
	want.NoPose++
	check("pose gap")

	locate(1000, func(p *spatial.DevicePose) { p.UncertaintyM = 0.5 })
	ingest(950)
	want.Uncertain++
	check("uncertain pose")

	locate(1100, func(p *spatial.DevicePose) { p.FrameID = "odom/robot-a" })
	locate(1200, func(p *spatial.DevicePose) { p.FrameID = "odom/robot-a" })
	ingest(1150)
	want.WrongFrame++
	check("pose in the odometry frame")

	locate(1300, nil)
	locate(1400, func(p *spatial.DevicePose) { p.Floor = "upper" })
	ingest(1350)
	want.NoPose++
	check("floor change between samples")

	locate(5000, func(p *spatial.DevicePose) { p.Floor = "upper" })
	locate(5100, func(p *spatial.DevicePose) { p.Floor = "upper" })
	ingest(1450) // older than the pose history
	want.NoPose++
	check("scan older than the pose history")

	quarantined := cal
	quarantined.State = spatial.CalibrationQuarantined
	if err := m.Calibrate(quarantined); err != nil {
		t.Fatal(err)
	}
	ingest(5050)
	want.Uncalibrated++
	check("quarantined lidar")

	if got := m.Stats(); got.Integrated+got.Pending+got.Uncalibrated+got.NoPose+got.Uncertain+got.WrongFrame != n {
		t.Fatalf("%d scans in, stats account for %+v", n, got)
	}
	if layers := mustCommit(t, m); len(layers) != 1 {
		t.Fatalf("one scan was integrated, %d layers committed", len(layers))
	}

	// Errors, as opposed to refusals: input that is not this source's.
	if err := m.Ingest(quarterScan("robot-b", at(0), 1, 0, 0, 0), testInput("robot-b", 1)); !errors.Is(err, ErrForeign) {
		t.Errorf("foreign scan: %v", err)
	}
	if err := m.Locate(at(6000), testPose("robot-b", "ground", 0, 0, 0)); !errors.Is(err, ErrForeign) {
		t.Errorf("foreign pose: %v", err)
	}
	if err := m.Locate(at(5100), testPose("robot-a", "ground", 0, 0, 0)); !errors.Is(err, ErrInvalid) {
		t.Errorf("pose out of order: %v", err)
	}
	foreign := cal
	foreign.Device, foreign.ParentDevice = "robot-b", "robot-b"
	if err := m.Calibrate(foreign); !errors.Is(err, ErrForeign) {
		t.Errorf("foreign calibration: %v", err)
	}
	if err := m.Ingest(quarterScan("robot-a", at(0), 1, 0, 0, 0), world.Input{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("scan without ancestry: %v", err)
	}
	pose := testPose("robot-a", "ground", 1, 1, 0)
	scan := quarterScan("robot-a", at(0), 1, 0, 0, 0)
	uncertain, odom := pose, pose
	uncertain.UncertaintyM, odom.FrameID = 1, "odom/robot-a"
	for name, tc := range map[string]struct {
		pose spatial.DevicePose
		want error
	}{"uncertain": {uncertain, ErrUncertain}, "frame": {odom, ErrFrame}, "foreign": {testPose("robot-b", "ground", 1, 1, 0), ErrForeign}} {
		if err := m.Integrate(at(0), tc.pose, geom.Pose2{}, scan, testInput("robot-a", 99)); !errors.Is(err, tc.want) {
			t.Errorf("Integrate with a pose that is %s: %v", name, err)
		}
	}
	if _, err := NewMapper(r, "robot/a"); !errors.Is(err, ErrInvalid) {
		t.Errorf("source with a slash: %v", err)
	}
}
