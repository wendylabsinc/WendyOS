package cslam

import (
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/posegraph"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// wallDistance is the distance from p to the nearest wall.
func wallDistance(p geom.Vec2, walls []geom.Segment) float64 {
	best := math.Inf(1)
	for _, w := range walls {
		best = math.Min(best, w.DistanceTo(p))
	}
	return best
}

// ownFrame is where a robot's own map frame really is: the true base pose at
// its first keyframe, where its Local pose is the identity.
func ownFrame(run simRun, r Robot) geom.Pose2 {
	return run.truthAt(r.Keyframes[0].Time).Compose(r.Keyframes[0].Local.Inverse())
}

func deg(rad float64) float64 { return rad * 180 / math.Pi }

// The planar convention, on a case small enough to check by hand: a robot
// that drives along +X and then turns toward +Z (clockwise seen from above)
// must come out of the pipeline unmirrored. Its poses are compared with the
// truth directly in its own frame, with no alignment that could hide a flip.
func TestHandedness(t *testing.T) {
	// The test's own lidar first: a wall 2 m to the robot's right (+Z) is hit
	// by the beam at yaw +π/2.
	wall := []geom.Segment{{A: geom.Vec2{X: -5, Z: 2}, B: geom.Vec2{X: 5, Z: 2}}}
	s := castScan(spatial.Header{Device: "r", Sensor: "lidar", Time: simEpoch}, geom.Pose2{}, wall, nil)
	if _, r, state := s.Beam(simBeams * 3 / 4); state != spatial.BeamHit || math.Abs(r-2) > 1e-9 {
		t.Fatalf("beam at +π/2 reads %v (%v), want a hit at 2 m", r, state)
	}

	robot := simRobot{
		dev:   "robot-a",
		route: []geom.Vec2{{X: 1.5, Z: 4.2}, {X: 10.5, Z: 4.2}, {X: 10.5, Z: 7}, {X: 6, Z: 7}},
		mount: geom.Pose2{X: 0.15, Z: 0.04, Yaw: 0.1}, odomOrigin: geom.Pose2{X: 5, Z: -3, Yaw: 1},
		odomScale: 1.01, odomYawPerMetre: 0.005, clockOffset: 30 * time.Millisecond,
	}
	run := simulate(robot, hall, 1)
	m := mapperFor(t, testConfig(), run)
	ingest(t, m, run.meas)
	sol := solve(t, m)
	r := sol.Robots[0]
	if sol.Reference != robot.dev || sol.FrameID != LocalFrameID(robot.dev) || !r.Shared || r.Alignment != nil {
		t.Fatalf("a lone robot must be its own shared frame: %+v", sol)
	}
	origin := run.truthAt(r.Keyframes[0].Time)
	worst, turned := 0.0, false
	for i, p := range r.Trajectory() {
		want := origin.Between(run.truthAt(r.Keyframes[i].Time))
		worst = math.Max(worst, p.Pos().Dist(want.Pos()))
		if math.Abs(geom.AngleDiff(p.Yaw, want.Yaw)) > 0.03 {
			t.Fatalf("keyframe %d: yaw %.3f, want %.3f", i, p.Yaw, want.Yaw)
		}
		turned = turned || p.Yaw > 1.5 // the first turn is toward +Z: positive yaw
	}
	if worst > 0.08 || !turned {
		t.Fatalf("own-frame trajectory is off by %.3f m (turned toward +Z: %v)", worst, turned)
	}
	// The map: occupied cells, carried to the walls' frame, lie on walls.
	g, err := sol.Grid(testFloor, grid.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	occupied, onWall := 0, 0
	g.Each(func(c grid.Cell, _ float64) bool {
		if g.State(c) == grid.Occupied {
			occupied++
			if wallDistance(origin.Apply(g.Center(c)), hall) < 0.12 {
				onWall++
			}
		}
		return true
	})
	if occupied < 200 || float64(onWall) < 0.95*float64(occupied) {
		t.Fatalf("%d of %d occupied cells lie on a wall", onWall, occupied)
	}
}

// Two robots in unrelated odometry frames circle the hall in opposite
// directions: they must end in one frame, with the relative transform of
// their own frames right and a trajectory that odometry alone cannot match.
func TestTwoRobotsShareAFrame(t *testing.T) {
	a, b := simulate(robotA, hall, 1), simulate(robotB, hall, 2)
	runs := map[spatial.DeviceID]simRun{robotA.dev: a, robotB.dev: b}
	m := mapperFor(t, testConfig(), a, b)
	ingest(t, m, merge(a, b))
	sol := solve(t, m)
	if sol.Reference != robotA.dev || sol.FrameID != LocalFrameID(robotA.dev) {
		t.Fatalf("shared frame %q of %q, want the first robot's", sol.FrameID, sol.Reference)
	}
	if !sol.Converged || sol.Closures < 10 || sol.DOF <= 0 {
		t.Fatalf("closures %d, removed %d, DOF %d, converged %v", sol.Closures, sol.Removed, sol.DOF, sol.Converged)
	}
	var est, want []geom.Vec2
	for _, r := range sol.Robots {
		if !r.Shared {
			t.Fatalf("%s is not in the shared frame", r.Device)
		}
		for i, p := range r.Trajectory() {
			est, want = append(est, p.Pos()), append(want, runs[r.Device].truthAt(r.Keyframes[i].Time).Pos())
		}
		if r.Pose.FrameID != sol.FrameID || r.Pose.Floor != testFloor || r.Pose.Validate() != nil {
			t.Fatalf("device pose of %s: %+v", r.Device, r.Pose)
		}
		// The device pose is the latest odometry instant, in the shared frame.
		ra, _ := sol.Robot(sol.Reference)
		real := ownFrame(a, ra).Between(runs[r.Device].truthAt(r.Time))
		if d := r.Pose.Pose2().Pos().Dist(real.Pos()); d > 0.1 || r.Pose.UncertaintyM <= 0 {
			t.Fatalf("device pose of %s is %.3f m off (σ %.3f)", r.Device, d, r.Pose.UncertaintyM)
		}
	}
	_, rmse, err := geom.AlignPoints2(est, want, nil)
	if err != nil {
		t.Fatal(err)
	}
	ra, _ := sol.Robot(robotA.dev)
	rb, _ := sol.Robot(robotB.dev)
	if ra.Alignment != nil || rb.Alignment == nil {
		t.Fatalf("alignments: reference %v, other %v", ra.Alignment, rb.Alignment)
	}
	al := *rb.Alignment
	real := ownFrame(a, ra).Between(ownFrame(b, rb))
	dm, dyaw := al.ChildToParent.Pos().Dist(real.Pos()), math.Abs(geom.AngleDiff(al.ChildToParent.Yaw, real.Yaw))
	sigma := geom.PositionCov(al.Covariance).MaxSigma()
	t.Logf("trajectory RMSE %.3f m; alignment %.3f m, %.2f° (σ %.3f m, %.2f°), %d closures, RMSE %.3f m", rmse, dm, deg(dyaw), sigma, deg(math.Sqrt(al.Covariance[8])), al.Inliers, al.RMSE)
	if rmse > 0.05 {
		t.Errorf("trajectory RMSE %.3f m", rmse)
	}
	if dm > 0.08 || deg(dyaw) > 1 {
		t.Errorf("frame alignment off by %.3f m, %.2f°", dm, deg(dyaw))
	}
	if al.ChildFrameID != LocalFrameID(robotB.dev) || al.ParentFrameID != sol.FrameID || al.Inliers < 3 || al.Validate() != nil {
		t.Errorf("alignment record %+v", al)
	}
	// The reported uncertainty is neither zero nor absurd, and covers the error.
	if !(sigma > 0 && sigma < 0.2) || dm > 4*sigma {
		t.Errorf("alignment error %.3f m against a reported σ of %.3f m", dm, sigma)
	}

	// The merged map in the shared frame lies on the hall's walls.
	g, err := sol.Grid(testFloor, grid.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	origin := ownFrame(a, ra)
	occupied, onWall := 0, 0
	g.Each(func(c grid.Cell, _ float64) bool {
		if g.State(c) == grid.Occupied {
			occupied++
			if wallDistance(origin.Apply(g.Center(c)), hall) < 0.12 {
				onWall++
			}
		}
		return true
	})
	if occupied < 500 || float64(onWall) < 0.95*float64(occupied) {
		t.Errorf("%d of %d occupied cells of the merged map lie on a wall", onWall, occupied)
	}
	if _, err := sol.Grid("cellar", grid.DefaultConfig()); !errors.Is(err, geom.ErrInsufficient) {
		t.Errorf("grid of a floor nobody mapped: %v", err)
	}
}

// The joint estimate is a function of the measurements, not of the order in
// which different robots' measurements arrive.
func TestArrivalOrderDoesNotMatter(t *testing.T) {
	// Three legs each are enough: the robots pass each other in the hall.
	ra, rb := robotA, robotB
	ra.route, rb.route = ra.route[:4], rb.route[:4]
	a, b := simulate(ra, hall, 1), simulate(rb, hall, 2)
	run := func(feed func(m *Mapper)) *Solution {
		m := mapperFor(t, testConfig(), a, b)
		feed(m)
		return solve(t, m)
	}
	want := run(func(m *Mapper) { ingest(t, m, merge(a, b)) })
	if other, _ := want.Robot(robotB.dev); want.Closures == 0 || !other.Shared {
		t.Fatalf("the robots did not meet: %d closures", want.Closures)
	}
	orders := map[string]func(m *Mapper){
		"b then a": func(m *Mapper) { ingest(t, m, b.meas); ingest(t, m, a.meas) },
		// A Solve in between has already processed robot a's keyframes when
		// robot b's earlier ones arrive: the back end must replay.
		"a, solve, b": func(m *Mapper) { ingest(t, m, a.meas); solve(t, m); ingest(t, m, b.meas) },
		"in chunks": func(m *Mapper) {
			for i, j := 0, 0; i < len(a.meas) || j < len(b.meas); {
				n := min(700, len(a.meas)-i)
				ingest(t, m, a.meas[i:i+n])
				i += n
				n = min(300, len(b.meas)-j)
				ingest(t, m, b.meas[j:j+n])
				j += n
			}
		},
	}
	for name, feed := range orders {
		got := run(feed)
		if got.Closures != want.Closures || got.Chi2 != want.Chi2 {
			t.Errorf("%s: %d closures, χ² %v; in time order %d, %v", name, got.Closures, got.Chi2, want.Closures, want.Chi2)
		}
		for k := range want.Robots {
			w, g := want.Robots[k], got.Robots[k]
			if !reflect.DeepEqual(w.Trajectory(), g.Trajectory()) || !reflect.DeepEqual(w.Alignment, g.Alignment) || !reflect.DeepEqual(w.Pose, g.Pose) {
				t.Errorf("%s: the estimate of %s differs from the one in time order", name, w.Device)
			}
		}
	}
}

// The streaming path: Solve as the measurements come in. The horizon keeps
// the back end from being rebuilt whenever one robot's keyframe arrives a
// moment after a later one of another robot, and the estimate ends where the
// one-shot estimate does.
func TestSolveWhileStreaming(t *testing.T) {
	ra, rb := robotA, robotB
	ra.route, rb.route = ra.route[:4], rb.route[:4]
	a, b := simulate(ra, hall, 1), simulate(rb, hall, 2)
	stream := merge(a, b)
	batch := mapperFor(t, testConfig(), a, b)
	ingest(t, batch, stream)
	want := solve(t, batch)

	m := mapperFor(t, testConfig(), a, b)
	graph, solves := m.back.g, 0
	for i, meas := range stream {
		if err := m.Ingest(meas); err != nil {
			t.Fatal(err)
		}
		// 7 is coprime with the 10 measurements of one scan period, so Solve
		// falls between every pair of the two robots' measurements in turn.
		if i%7 == 0 {
			if _, err := m.Solve(); err == nil {
				solves++
			} else if !errors.Is(err, geom.ErrInsufficient) {
				t.Fatal(err)
			}
		}
	}
	got := solve(t, m)
	if m.back.g != graph || solves < len(stream)/8 {
		t.Errorf("the back end was rebuilt while streaming in time order (%d solves)", solves)
	}
	for k := range want.Robots {
		w, g := want.Robots[k], got.Robots[k]
		if !g.Shared || len(g.Keyframes) != len(w.Keyframes) {
			t.Fatalf("%s: shared %v, %d keyframes, want %d", g.Device, g.Shared, len(g.Keyframes), len(w.Keyframes))
		}
		p, q := w.Trajectory(), g.Trajectory()
		for i := range p {
			if d := p[i].Pos().Dist(q[i].Pos()); d > 0.03 {
				t.Fatalf("keyframe %d of %s: streaming and one-shot estimates are %.3f m apart", i, w.Device, d)
			}
		}
	}
}

// Robots that never see the same place stay in their own frames: no
// alignment is invented.
func TestDisjointRobotsStayApart(t *testing.T) {
	// Robot b drives a different building: an L-shaped room.
	v := func(x, z float64) geom.Vec2 { return geom.Vec2{X: x, Z: z} }
	var other []geom.Segment
	outline := []geom.Vec2{v(0, 0), v(9, 0), v(9, 3), v(4, 3), v(4, 10), v(0, 10), v(0, 0)}
	for i := 1; i < len(outline); i++ {
		other = append(other, geom.Segment{A: outline[i-1], B: outline[i]})
	}
	ra, rb := robotA, robotB
	ra.route = ra.route[:3]
	rb.route = []geom.Vec2{v(7.5, 1.5), v(2, 1.5), v(2, 8.5)}
	a, b := simulate(ra, hall, 1), simulate(rb, other, 2)
	m := mapperFor(t, testConfig(), a, b)
	ingest(t, m, merge(a, b))
	sol := solve(t, m)
	first, _ := sol.Robot(robotA.dev)
	got, _ := sol.Robot(robotB.dev)
	if !first.Shared || got.Shared || got.Alignment != nil || got.Pose.FrameID != LocalFrameID(robotB.dev) {
		t.Fatalf("robot b: shared %v, alignment %v, pose frame %q", got.Shared, got.Alignment, got.Pose.FrameID)
	}
	// Its own estimate is still sound, in its own frame (compared without an
	// alignment, so the first keyframe's heading error has a lever of metres).
	origin := b.truthAt(got.Keyframes[0].Time)
	for i, p := range got.Trajectory() {
		if d := p.Pos().Dist(origin.Between(b.truthAt(got.Keyframes[i].Time)).Pos()); d > 0.2 {
			t.Fatalf("keyframe %d of the unaligned robot is %.3f m off in its own frame", i, d)
		}
	}
	for _, rec := range sol.Records() {
		if al, ok := rec.Payload.(spatial.FrameAlignment); ok {
			t.Fatalf("published alignment %+v", al)
		}
	}

	// With an absolute prior each, both are in the priors' frame although
	// they never meet. (The two buildings overlap in it; nothing forbids that.)
	m = mapperFor(t, testConfig(), a, b)
	at := simEpoch.Add(2 * time.Second)
	for _, meas := range merge(a, b) {
		if !at.IsZero() && !meas.When().Before(at) {
			for _, run := range []simRun{a, b} {
				p := run.truthAt(at)
				err := m.AddPrior(at, spatial.DevicePose{
					Device: run.robot.dev, Floor: testFloor, FrameID: "site", Location: spatial.At(p.Pos().Lift(0), 0.02), Yaw: p.Yaw,
					Covariance: geom.Mat3{4e-4, 0, 0, 0, 4e-4, 0, 0, 0, 1e-4}, Method: "test", Score: 1,
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			at = time.Time{}
		}
		if err := m.Ingest(meas); err != nil {
			t.Fatal(err)
		}
	}
	sol = solve(t, m)
	for _, run := range []simRun{a, b} {
		r, _ := sol.Robot(run.robot.dev)
		if !r.Shared || r.Alignment == nil || sol.FrameID != "site" {
			t.Fatalf("%s with a prior: shared %v, alignment %v, frame %q", r.Device, r.Shared, r.Alignment, sol.FrameID)
		}
		for i, p := range r.Trajectory() {
			if d := p.Pos().Dist(run.truthAt(r.Keyframes[i].Time).Pos()); d > 0.2 {
				t.Fatalf("keyframe %d of %s is %.3f m off in the site frame", i, r.Device, d)
			}
		}
	}
}

// One false closure cannot bend the map: alone it never enters the graph,
// and forced into the graph it is down-weighted and then removed.
func TestFalseClosure(t *testing.T) {
	a, b := simulate(robotA, hall, 1), simulate(robotB, hall, 2)
	m := mapperFor(t, testConfig(), a, b)
	ingest(t, m, merge(a, b))
	before := solve(t, m)
	ra, rb := m.robot(robotA.dev), m.robot(robotB.dev)
	from, to := 5, len(rb.nodes)-3
	// A match that puts robot b 1.5 m and 0.3 rad from where it is.
	truthful := m.pose(ra.nodes[from]).Between(m.pose(rb.nodes[to]))
	info, err := m.closureInfo(geom.Mat3{})
	if err != nil {
		t.Fatal(err)
	}
	lie := &candidate{a: ra, b: rb, from: from, to: to, z: truthful.Compose(geom.Pose2{X: 1.5, Yaw: 0.3}), info: info}

	// Geometric consistency: no other match agrees, so it only waits.
	edges := m.back.g.NumEdges()
	if err := m.consider(lie); err != nil {
		t.Fatal(err)
	}
	if lie.inGraph || m.back.g.NumEdges() != edges {
		t.Fatal("an unsupported closure entered the graph")
	}

	// Past the gate anyway: the kernel and the χ² test take it out again.
	e := posegraph.Relative(ra.nodes[from], rb.nodes[to], lie.z, lie.info)
	e.Robust = true
	id, err := m.back.g.AddEdge(e)
	if err != nil {
		t.Fatal(err)
	}
	m.back.closures = append(m.back.closures, closure{id: id, a: ra, b: rb})
	m.back.dirty = true
	after := solve(t, m)
	if after.Removed != before.Removed+1 || after.Closures != before.Closures {
		t.Fatalf("removed %d closures (before %d), kept %d (before %d)", after.Removed, before.Removed, after.Closures, before.Closures)
	}
	for k := range before.Robots {
		p, q := before.Robots[k].Trajectory(), after.Robots[k].Trajectory()
		for i := range p {
			if d := p[i].Pos().Dist(q[i].Pos()); d > 0.005 {
				t.Fatalf("keyframe %d of %s moved %.3f m", i, before.Robots[k].Device, d)
			}
		}
	}
}

// An absolute prior fixes the gauge: the shared frame is the prior's, for
// the robot that has it and for the robot aligned to it by closures alone.
func TestAbsolutePriorFixesGauge(t *testing.T) {
	a, b := simulate(robotA, hall, 1), simulate(robotB, hall, 2)
	m := mapperFor(t, testConfig(), a, b)
	stream := merge(a, b)
	priorAt := []time.Duration{2 * time.Second, 12 * time.Second, 25 * time.Second}
	cov := geom.Mat3{0.02 * 0.02, 0, 0, 0, 0.02 * 0.02, 0, 0, 0, 0.01 * 0.01}
	for _, meas := range stream {
		if len(priorAt) > 0 && !meas.When().Before(simEpoch.Add(priorAt[0])) {
			// Robot b is localized in the hall's frame, 2 cm and 0.5° off.
			at := simEpoch.Add(priorAt[0])
			p := b.truthAt(at).Compose(geom.Pose2{X: 0.02, Z: -0.01, Yaw: 0.008})
			err := m.AddPrior(at, spatial.DevicePose{
				Device: robotB.dev, Floor: testFloor, FrameID: "site", Location: spatial.At(p.Pos().Lift(0), 0.02),
				Yaw: p.Yaw, Covariance: cov, Method: "test", Score: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			priorAt = priorAt[1:]
		}
		if err := m.Ingest(meas); err != nil {
			t.Fatal(err)
		}
	}
	sol := solve(t, m)
	if sol.FrameID != "site" || sol.Reference != "" {
		t.Fatalf("shared frame %q (reference %q), want the priors' frame", sol.FrameID, sol.Reference)
	}
	runs := map[spatial.DeviceID]simRun{robotA.dev: a, robotB.dev: b}
	for _, r := range sol.Robots {
		if !r.Shared || r.Alignment == nil || r.Alignment.ParentFrameID != "site" || r.Alignment.Validate() != nil || r.Pose.FrameID != "site" {
			t.Fatalf("%s: shared %v, alignment %+v", r.Device, r.Shared, r.Alignment)
		}
		worst := 0.0
		for i, p := range r.Trajectory() {
			worst = math.Max(worst, p.Pos().Dist(runs[r.Device].truthAt(r.Keyframes[i].Time).Pos()))
		}
		real := ownFrame(runs[r.Device], r)
		dm := r.Alignment.ChildToParent.Pos().Dist(real.Pos())
		t.Logf("%s: worst keyframe %.3f m from the truth in the site frame; own frame %.3f m off", r.Device, worst, dm)
		if worst > 0.1 || dm > 0.1 {
			t.Errorf("%s is not in the site frame: keyframes up to %.3f m off, own frame %.3f m", r.Device, worst, dm)
		}
	}
	// A prior in another frame is refused.
	err := m.AddPrior(simEpoch.Add(30*time.Second), spatial.DevicePose{
		Device: robotA.dev, Floor: testFloor, FrameID: "elsewhere", Location: spatial.At(geom.Vec3{}, 0.02), Covariance: cov, Method: "test",
	})
	if err == nil {
		t.Error("a prior in a second frame was accepted")
	}
}

// Submaps, closures and maps never span two floors.
func TestFloorsStaySeparate(t *testing.T) {
	a := simulate(robotA, hall, 1)
	m := mapperFor(t, testConfig(), a)
	half := len(a.meas) / 2
	ingest(t, m, a.meas[:half])
	// "Upstairs" is the same hall again: were floors not kept apart, the
	// second lap would close loops against the first.
	if err := m.SetFloor(robotA.dev, "upper", 3.5); err != nil {
		t.Fatal(err)
	}
	ingest(t, m, a.meas[half:])
	sol := solve(t, m)
	r := sol.Robots[0]
	rb := m.robot(robotA.dev)
	for _, c := range m.back.closures {
		e, _ := m.back.g.Edge(c.id) // one robot: node ids are keyframe indices
		if from, to := rb.kfs[e.From].floor, rb.kfs[e.To].floor; from != to {
			t.Errorf("closure from keyframe %d on %q to keyframe %d on %q", e.From, from, e.To, to)
		}
	}
	if r.Pose.Floor != "upper" || r.Pose.PositionM[1] != 3.5 {
		t.Errorf("device pose on %q at elevation %v", r.Pose.Floor, r.Pose.PositionM[1])
	}
	count := map[spatial.FloorID]int{}
	for _, kf := range r.Keyframes {
		count[kf.Floor]++
	}
	if count[testFloor] < 10 || count["upper"] < 10 || len(count) != 2 {
		t.Errorf("keyframes per floor: %v", count)
	}
	for floor := range count {
		if _, err := sol.Grid(floor, grid.DefaultConfig()); err != nil {
			t.Errorf("grid of %q: %v", floor, err)
		}
	}
	if err := m.SetFloor(robotA.dev, "", 0); err == nil {
		t.Error("an empty floor id was accepted")
	}
}

// The mapper refuses what it cannot use instead of guessing.
func TestRefusals(t *testing.T) {
	if _, err := New(Config{SubmapKeyframes: 5}); !errors.Is(err, ErrConfig) {
		t.Errorf("odd SubmapKeyframes: %v", err)
	}
	if _, err := New(Config{ClosureMinScore: -1}); !errors.Is(err, ErrConfig) {
		t.Errorf("negative ClosureMinScore: %v", err)
	}
	a := simulate(robotA, hall, 1)
	m, err := New(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Solve(); !errors.Is(err, geom.ErrInsufficient) {
		t.Errorf("Solve without keyframes: %v", err)
	}
	var odom spatial.Odometry
	var scan spatial.Scan
	for _, meas := range a.meas[:12] {
		switch v := meas.(type) {
		case spatial.Odometry:
			odom = v
			if err := m.Ingest(v); err != nil {
				t.Fatal(err)
			}
		case spatial.Scan:
			scan = v
			if err := m.Ingest(v); !errors.Is(err, ErrUncalibrated) {
				t.Errorf("scan of an uncalibrated lidar: %v", err)
			}
		}
	}
	// Odometry alone constrains no map.
	if _, err := m.Solve(); !errors.Is(err, geom.ErrInsufficient) {
		t.Errorf("Solve with odometry only: %v", err)
	}
	if err := m.Ingest(odom); !errors.Is(err, ErrOutOfOrder) {
		t.Errorf("repeated odometry: %v", err)
	}
	// A calibration that is not a lidar mount on the device's own base.
	bad := a.calibration()
	bad.Kind = spatial.KindCamera
	if err := m.SetCalibration(bad); err == nil {
		t.Error("a camera calibration was accepted")
	}
	if err := m.SetCalibration(a.calibration()); err != nil {
		t.Fatal(err)
	}
	ingest(t, m, a.meas[12:40])
	if err := m.Ingest(scan); !errors.Is(err, ErrOutOfOrder) {
		t.Errorf("an old scan: %v", err)
	}
	// Other kinds are not this package's business.
	if err := m.Ingest(spatial.Imu{Header: spatial.Header{Device: robotA.dev, Sensor: "imu", Time: simEpoch}}); err != nil {
		t.Errorf("an IMU sample: %v", err)
	}
	if sol := solve(t, m); len(sol.Robots) != 1 || len(sol.Robots[0].Keyframes) == 0 {
		t.Errorf("solution after calibration: %+v", sol)
	}
}

// Every record of a solution becomes a valid component under this feature's
// model id, with the scans it rests on as ancestry.
func TestRecordsPublish(t *testing.T) {
	a, b := simulate(robotA, hall, 1), simulate(robotB, hall, 2)
	m := mapperFor(t, testConfig(), a, b)
	ingest(t, m, merge(a, b))
	sol := solve(t, m)
	scans := map[string]bool{}
	for _, meas := range merge(a, b) {
		if _, ok := meas.(spatial.Scan); ok {
			scans[spatial.InputID(meas)] = true
		}
	}
	sim := world.SimulationContext{RunID: "hall-1", ScenarioVersion: "hall/1", Seed: 1, ClockDomain: "test"}
	scope := spatial.Scope{Tenant: "tenant", Site: "hall", Source: world.Source{Device: "hub", App: "wendy", Service: "spatial", Stream: Feature}}
	tiles, err := sol.MapRecords(testFloor, grid.DefaultConfig(), 64)
	if err != nil || len(tiles) == 0 {
		t.Fatalf("%d map tiles: %v", len(tiles), err)
	}
	count := map[string]int{}
	for i, rec := range append(sol.Records(), tiles...) {
		var inputs []world.Input
		for _, id := range rec.Inputs {
			if !scans[id] {
				t.Fatalf("record %d names input %q, which is not a scan that was ingested", i, id)
			}
			inputs = append(inputs, world.Input{ID: id, Origin: world.Simulated, Simulation: &sim})
		}
		prov, err := spatial.InferredProvenance(Feature, "1", "test", spatial.ReduceInputs(inputs))
		if err != nil || len(inputs) == 0 {
			t.Fatalf("record %d: %d inputs: %v", i, len(inputs), err)
		}
		if prov.ModelID != "wendy.spatial.cslam" {
			t.Fatalf("model id %q", prov.ModelID)
		}
		c, err := spatial.ToComponent(scope, world.Version{Epoch: 1, Sequence: uint64(i + 1)}, world.FrameRef{ID: rec.FrameID, Revision: "r1"},
			world.Timing{SampleTime: rec.Time, ClockDomain: "test"}, time.Minute, prov, rec.Payload)
		if err != nil {
			t.Fatalf("record %d (%s): %v", i, rec.Payload.Type(), err)
		}
		if _, err := spatial.DecodePayload(c); err != nil {
			t.Fatalf("record %d (%s) does not decode: %v", i, rec.Payload.Type(), err)
		}
		count[rec.Payload.Type()]++
	}
	keyframes := len(sol.Robots[0].Keyframes) + len(sol.Robots[1].Keyframes)
	if count[spatial.TypeDevicePose] != 2 || count[spatial.TypeFrameAlignment] != 1 || count[spatial.TypeKeyframe] != keyframes {
		t.Errorf("records by type: %v, want 2 poses, 1 alignment, %d keyframes", count, keyframes)
	}
	if tile := tiles[0].Payload.(spatial.MapTile); count[spatial.TypeMapTile] != len(tiles) || tile.Versions[string(robotA.dev)] != uint64(len(sol.Robots[0].Keyframes)) || tiles[0].FrameID != sol.FrameID {
		t.Errorf("map tiles: %d published, versions %v, frame %q", count[spatial.TypeMapTile], tile.Versions, tiles[0].FrameID)
	}
}
