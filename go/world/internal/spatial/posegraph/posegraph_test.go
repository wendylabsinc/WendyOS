package posegraph

import (
	"errors"
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// The simulation below turns ground-truth paths into noisy measurements.
// Truth is otherwise used only to score the estimates.

func newRand(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)) }

// noise is the standard deviation of one relative-pose measurement: metres
// per axis and radians.
type noise struct{ trans, yaw float64 }

func (n noise) measure(r *rand.Rand, from, to geom.Pose2) geom.Pose2 {
	z := from.Between(to)
	return geom.Pose2{X: z.X + r.NormFloat64()*n.trans, Z: z.Z + r.NormFloat64()*n.trans, Yaw: geom.WrapAngle(z.Yaw + r.NormFloat64()*n.yaw)}
}

func (n noise) info() geom.Mat3 {
	return geom.Mat3{0: 1 / (n.trans * n.trans), 4: 1 / (n.trans * n.trans), 8: 1 / (n.yaw * n.yaw)}
}

// must fails the test on err and otherwise yields v: must(g.Pose(id))(t).
func must[T any](v T, err error) func(testing.TB) T {
	return func(t testing.TB) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

// drive adds a trajectory that dead-reckons along truth from noisy odometry,
// starting at start in its own frame, with one odometry edge per step.
func drive(t testing.TB, g *Graph, name Trajectory, r *rand.Rand, truth []geom.Pose2, start geom.Pose2, odo noise) []NodeID {
	ids := []NodeID{must(g.AddNode(name, start))(t)}
	local := start
	for k := 1; k < len(truth); k++ {
		z := odo.measure(r, truth[k-1], truth[k])
		local = local.Compose(z)
		ids = append(ids, must(g.AddNode(name, local))(t))
		must(g.AddEdge(Relative(ids[k-1], ids[k], z, odo.info())))(t)
	}
	return ids
}

// squarePath walks laps of a square, turning toward +Z at each corner, so the
// yaw passes through ±π.
func squarePath(side, step float64, laps int) []geom.Pose2 {
	path := []geom.Pose2{{}}
	perSide := int(math.Round(side / step))
	for i := 0; i < 4*laps*perSide; i++ {
		move := geom.Pose2{X: step}
		if (i+1)%perSide == 0 {
			move.Yaw = math.Pi / 2
		}
		path = append(path, path[len(path)-1].Compose(move))
	}
	return path
}

// manhattan is a random walk of one-metre steps on a size x size street
// grid, the layout of the classic synthetic pose-graph datasets.
func manhattan(r *rand.Rand, n, size int) []geom.Pose2 {
	x, z, dir := size/2, size/2, 0
	dx, dz := [4]int{1, 0, -1, 0}, [4]int{0, 1, 0, -1}
	path := []geom.Pose2{{X: float64(x), Z: float64(z)}}
	for len(path) < n {
		turn := 0
		switch p := r.Float64(); {
		case p < 0.2:
			turn = 1
		case p < 0.4:
			turn = 3
		}
		for {
			d := (dir + turn) % 4
			if nx, nz := x+dx[d], z+dz[d]; nx >= 0 && nx < size && nz >= 0 && nz < size {
				x, z, dir = nx, nz, d
				break
			}
			turn++
		}
		path = append(path, geom.Pose2{X: float64(x), Z: float64(z), Yaw: geom.WrapAngle(float64(dir) * math.Pi / 2)})
	}
	return path
}

type cell [2]int

func cellOf(p geom.Pose2) cell { return cell{int(math.Round(p.X)), int(math.Round(p.Z))} }

// revisits pairs each pose with the oldest pose at least gap steps earlier in
// the same grid cell: the places a scan matcher would close a loop.
func revisits(truth []geom.Pose2, gap int) [][2]int {
	first := map[cell]int{} // lookups only
	var pairs [][2]int
	for k, p := range truth {
		if j, ok := first[cellOf(p)]; !ok {
			first[cellOf(p)] = k
		} else if k-j >= gap {
			pairs = append(pairs, [2]int{j, k})
		}
	}
	return pairs
}

// positionRMSE scores node positions against truth expressed in frame.
func positionRMSE(t testing.TB, g *Graph, ids []NodeID, frame geom.Pose2, truth []geom.Pose2) float64 {
	sum := 0.0
	for k, id := range ids {
		sum += must(g.Pose(id))(t).Pos().Sub(frame.Between(truth[k]).Pos()).Norm2()
	}
	return math.Sqrt(sum / float64(len(ids)))
}

func poseError(got, want geom.Pose2) (metres, radians float64) {
	return got.Pos().Dist(want.Pos()), math.Abs(geom.AngleDiff(got.Yaw, want.Yaw))
}

func TestSquareLoopClosure(t *testing.T) {
	const (
		side, step = 10.0, 0.5
		laps       = 3
		closeEvery = 5 // on later laps every fifth pose is matched to the same place one lap earlier
		// The closures must cut the trajectory error to a third of dead
		// reckoning. What is left is mostly the drift inside the first lap.
		minGain = 3.0
		rmseTol = 0.25 // metres
	)
	odo, closure := noise{0.01, 0.01}, noise{0.01, 0.005}
	r := newRand(1)
	truth := squarePath(side, step, laps)
	perLap := (len(truth) - 1) / laps
	g := New(Config{})
	ids := drive(t, g, "robot", r, truth, truth[0], odo)
	must(0, g.SetFixed(ids[0], true))(t)
	deadReckoned := positionRMSE(t, g, ids, geom.Pose2{}, truth)

	closures := 0
	for k := perLap; k < len(ids); k += closeEvery {
		must(g.AddEdge(Relative(ids[k-perLap], ids[k], closure.measure(r, truth[k-perLap], truth[k]), closure.info())))(t)
		closures++
	}
	res, err := g.Optimize()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Converged || len(res.Gauge) != 0 || res.Chi2 >= res.InitialChi2 {
		t.Fatalf("converged=%v gauge=%v chi2 %.3g -> %.3g", res.Converged, res.Gauge, res.InitialChi2, res.Chi2)
	}
	optimized := positionRMSE(t, g, ids, geom.Pose2{}, truth)
	t.Logf("rmse: dead reckoning %.3f m, optimised %.3f m, %d iterations, chi2 %.2f / %d dof",
		deadReckoned, optimized, res.Iterations, res.Chi2, res.DOF)
	if optimized > rmseTol || optimized*minGain > deadReckoned {
		t.Errorf("optimised rmse %.3f m, dead reckoning %.3f m: want <= %v m and %vx better", optimized, deadReckoned, rmseTol, minGain)
	}
	if res.DOF != 3*closures {
		t.Errorf("dof = %d, want 3 per closure (%d closures)", res.DOF, closures)
	}
}

// buildManhattan returns a graph with the first node fixed, odometry edges
// and one robust loop-closure edge per revisit.
func buildManhattan(t testing.TB, cfg Config, r *rand.Rand, n, size int, odo, closure noise) (g *Graph, ids []NodeID, truth []geom.Pose2, closures []EdgeID) {
	truth = manhattan(r, n, size)
	g = New(cfg)
	ids = drive(t, g, "robot", r, truth, truth[0], odo)
	must(0, g.SetFixed(ids[0], true))(t)
	for _, p := range revisits(truth, 20) {
		e := Relative(ids[p[0]], ids[p[1]], closure.measure(r, truth[p[0]], truth[p[1]]), closure.info())
		e.Robust = true
		closures = append(closures, must(g.AddEdge(e))(t))
	}
	return g, ids, truth, closures
}

func TestManhattan(t *testing.T) {
	const (
		budget = 2 * time.Second // per Optimize, the design's "couple of seconds"
		// One fixed node anchors the whole graph, so the absolute error of
		// the statistically consistent solution grows with its extent.
		rmseTol      = 0.3 // metres
		minGain      = 5.0 // over dead reckoning
		chi2PerDOFLo = 0.8 // a consistent solution has χ²/dof near 1
		chi2PerDOFHi = 1.2
	)
	odo := noise{0.02, 0.01}
	for _, tc := range []struct {
		name    string
		n, size int
	}{{"1000", 1000, 20}, {"3000", 3000, 34}} {
		t.Run(tc.name, func(t *testing.T) {
			if raceEnabled && tc.n > 1000 {
				t.Skip("scale check only; too slow under the race detector")
			}
			g, ids, truth, closures := buildManhattan(t, Config{}, newRand(2), tc.n, tc.size, odo, odo)
			deadReckoned := positionRMSE(t, g, ids, geom.Pose2{}, truth)
			start := time.Now()
			res, err := g.Optimize()
			elapsed := time.Since(start)
			if err != nil {
				t.Fatal(err)
			}
			optimized := positionRMSE(t, g, ids, geom.Pose2{}, truth)
			perDOF := res.Chi2 / float64(res.DOF)
			t.Logf("%d nodes, %d closures: %v, %d iterations, rmse %.3f m (dead reckoning %.2f m), chi2/dof %.3f",
				tc.n, len(closures), elapsed, res.Iterations, optimized, deadReckoned, perDOF)
			if elapsed > budget && !raceEnabled {
				t.Errorf("optimisation took %v, budget %v", elapsed, budget)
			}
			if optimized > rmseTol || optimized*minGain > deadReckoned {
				t.Errorf("rmse %.3f m (dead reckoning %.3f m)", optimized, deadReckoned)
			}
			if perDOF < chi2PerDOFLo || perDOF > chi2PerDOFHi {
				t.Errorf("chi2/dof = %.3f, want within [%v, %v]", perDOF, chi2PerDOFLo, chi2PerDOFHi)
			}
		})
	}
}

func TestOutlierLoopClosures(t *testing.T) {
	const (
		outlierEvery   = 10    // 10 % of the closures are replaced by a random pose
		rmseTol        = 0.15  // metres, once the flagged closures are removed
		dcsSlack       = 1.25  // DCS with the outliers still in the graph, relative to that
		gateConfidence = 0.999 // χ²₃ gate on an edge
		// Fraction of true closures above the gate. DCS lets the residual of a
		// down-weighted inlier grow, so it exceeds the nominal 0.1 %.
		maxFalseAlarms = 0.03
		minDamage      = 5.0 // the same graph without a kernel is this much worse
	)
	odo := noise{0.02, 0.01}
	gate := must(geom.ChiSquareQuantile(3, gateConfidence))(t)
	build := func(kernel Kernel) (*Graph, []NodeID, []geom.Pose2, []EdgeID, map[EdgeID]bool) {
		r := newRand(3)
		g, ids, truth, closures := buildManhattan(t, Config{Kernel: kernel}, r, 500, 14, odo, odo)
		bad := map[EdgeID]bool{} // lookups only
		for i, id := range closures {
			if i%outlierEvery != outlierEvery/2 {
				continue
			}
			e, _ := g.Edge(id)
			e.Pose = geom.Pose2{X: r.Float64()*20 - 10, Z: r.Float64()*20 - 10, Yaw: r.Float64()*2*math.Pi - math.Pi}
			must(0, g.RemoveEdge(id))(t)
			bad[must(g.AddEdge(e))(t)] = true
		}
		return g, ids, truth, closures, bad
	}

	g, ids, truth, closures, bad := build(KernelDCS)
	res, err := g.Optimize()
	if err != nil {
		t.Fatal(err)
	}
	rmse := positionRMSE(t, g, ids, geom.Pose2{}, truth)
	missed, falseAlarms := 0, 0
	for id := range res.Edges {
		e, ok := g.Edge(EdgeID(id))
		if !ok || !e.Robust {
			continue
		}
		flagged := res.Edges[id].Chi2 > gate
		if bad[EdgeID(id)] && !flagged {
			missed++
		}
		if !bad[EdgeID(id)] && flagged {
			falseAlarms++
		}
	}
	t.Logf("DCS: %d closures, %d outliers, %d iterations, rmse %.3f m, missed %d, false alarms %d",
		len(closures), len(bad), res.Iterations, rmse, missed, falseAlarms)
	if len(bad)*outlierEvery < len(closures)-outlierEvery {
		t.Fatalf("only %d outliers among %d closures", len(bad), len(closures))
	}
	if missed != 0 {
		t.Errorf("%d of %d outliers are below the chi-square gate", missed, len(bad))
	}
	if float64(falseAlarms) > maxFalseAlarms*float64(len(closures)-len(bad)) {
		t.Errorf("%d true closures flagged", falseAlarms)
	}

	// Removing the flagged edges and dropping the kernel gives a clean fit.
	for id, s := range res.Edges {
		if e, ok := g.Edge(EdgeID(id)); ok && e.Robust && s.Chi2 > gate {
			must(0, g.RemoveEdge(EdgeID(id)))(t)
		}
	}
	if res, err = g.Optimize(); err != nil {
		t.Fatal(err)
	}
	clean := positionRMSE(t, g, ids, geom.Pose2{}, truth)
	if perDOF := res.Chi2 / float64(res.DOF); perDOF > 1.2 || clean > rmseTol {
		t.Errorf("after removing the flagged edges: chi2/dof %.3f, rmse %.3f m", perDOF, clean)
	}
	if rmse > dcsSlack*clean {
		t.Errorf("rmse %.3f m with DCS, %.3f m without the outliers", rmse, clean)
	}

	// Huber bounds an outlier's pull but keeps it; plain least squares is wrecked.
	g, ids, truth, _, _ = build(KernelNone)
	if _, err := g.Optimize(); err != nil && !errors.Is(err, geom.ErrNoConvergence) {
		t.Fatal(err)
	}
	plain := positionRMSE(t, g, ids, geom.Pose2{}, truth)
	g, ids, truth, _, _ = build(KernelHuber)
	if _, err := g.Optimize(); err != nil && !errors.Is(err, geom.ErrNoConvergence) {
		t.Fatal(err)
	}
	huber := positionRMSE(t, g, ids, geom.Pose2{}, truth)
	t.Logf("rmse without kernel %.3f m, Huber %.3f m", plain, huber)
	if plain < minDamage*rmse {
		t.Errorf("outliers barely hurt the plain fit (%.3f m vs %.3f m): the test is not exercising the kernel", plain, rmse)
	}
	if huber >= plain {
		t.Errorf("Huber rmse %.3f m is no better than plain %.3f m", huber, plain)
	}
}

func TestTwoRobotAlignment(t *testing.T) {
	const (
		falseMatches = 3
		// Targets of the collaborative SLAM feature for frame alignment.
		frameTol    = 0.15              // metres
		frameYawTol = 2 * math.Pi / 180 // radians
		coarseTol   = 0.5               // metres, Align alone
		rmseTol     = 0.15              // metres, robot b in robot a's frame
		covGate     = 0.999             // the error must be plausible under the reported covariance
		minSigma    = 0.001             // metres: the covariance must not be degenerate
		maxSigma    = 0.2               // metres: nor vacuous
		gridSize    = 12
		steps       = 400
		everyNth    = 4 // inter-robot matches are attempted at every 4th shared cell
	)
	odo := noise{0.02, 0.01}
	r := newRand(4)
	truthA, truthB := manhattan(r, steps, gridSize), manhattan(r, steps, gridSize)
	g := New(Config{Kernel: KernelDCS})
	// Each robot starts at the origin of its own frame; neither knows the other's.
	a := drive(t, g, "a", r, truthA, geom.Pose2{}, odo)
	b := drive(t, g, "b", r, truthB, geom.Pose2{}, odo)
	must(0, g.SetFixed(a[0], true))(t)
	for _, tr := range []struct {
		ids   []NodeID
		truth []geom.Pose2
	}{{a, truthA}, {b, truthB}} {
		for _, p := range revisits(tr.truth, 20) {
			e := Relative(tr.ids[p[0]], tr.ids[p[1]], odo.measure(r, tr.truth[p[0]], tr.truth[p[1]]), odo.info())
			e.Robust = true
			must(g.AddEdge(e))(t)
		}
	}
	// Single-robot SLAM first: two components, robot b floating.
	res, err := g.Optimize()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Gauge) != 1 || res.Gauge[0] != b[0] || g.Aligned("a", "b") {
		t.Fatalf("gauge = %v, aligned = %v; want robot b held and unaligned", res.Gauge, g.Aligned("a", "b"))
	}
	if _, _, err := g.FrameTransform("a", "b"); !errors.Is(err, ErrDisconnected) {
		t.Fatalf("FrameTransform before alignment: %v", err)
	}
	if _, err := g.Align("a", "b", AlignConfig{}); !errors.Is(err, geom.ErrInsufficient) {
		t.Fatalf("Align without inter-robot edges: %v", err)
	}

	// Inter-robot matches where both robots visited a cell, plus false ones.
	seen := map[cell]int{} // lookups only
	for k, p := range truthA {
		if _, ok := seen[cellOf(p)]; !ok {
			seen[cellOf(p)] = k
		}
	}
	var matches, falses []EdgeID
	shared := 0
	for k, p := range truthB {
		i, ok := seen[cellOf(p)]
		if !ok {
			continue
		}
		if shared++; shared%everyNth != 0 {
			continue
		}
		e := Relative(a[i], b[k], odo.measure(r, truthA[i], truthB[k]), odo.info())
		e.Robust = true
		matches = append(matches, must(g.AddEdge(e))(t))
	}
	for i := 0; i < falseMatches; i++ {
		e := Relative(b[r.IntN(steps)], a[r.IntN(steps)],
			geom.Pose2{X: r.Float64()*8 - 4, Z: r.Float64()*8 - 4, Yaw: r.Float64()*6 - 3}, odo.info())
		e.Robust = true
		falses = append(falses, must(g.AddEdge(e))(t))
	}

	want := truthA[0].Between(truthB[0]) // b's start frame in a's start frame
	al, err := g.Align("a", "b", AlignConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if len(al.Inliers) < len(matches)*9/10 || len(al.Outliers) < falseMatches {
		t.Errorf("Align: %d inliers of %d matches, %d outliers of %d false", len(al.Inliers), len(matches), len(al.Outliers), falseMatches)
	}
	for _, id := range falses {
		for _, in := range al.Inliers {
			if in == id {
				t.Errorf("false match %d accepted by Align", id)
			}
		}
	}
	coarse, _, err := g.FrameTransform("a", "b")
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := poseError(coarse, want); d > coarseTol {
		t.Errorf("Align alone is off by %.3f m", d)
	}
	if _, err := g.Align("a", "b", AlignConfig{}); !errors.Is(err, ErrAligned) {
		t.Errorf("second Align: %v", err)
	}

	res, err = g.Optimize()
	if err != nil {
		t.Fatal(err)
	}
	got, cov, err := g.FrameTransform("a", "b")
	if err != nil {
		t.Fatal(err)
	}
	d, yaw := poseError(got, want)
	m2 := must(geom.MahalanobisSq3([3]float64{got.X - want.X, got.Z - want.Z, geom.AngleDiff(got.Yaw, want.Yaw)}, cov))(t)
	sigma := geom.PositionCov(cov).MaxSigma()
	rmse := positionRMSE(t, g, b, truthA[0], truthB)
	t.Logf("%d matches: align rmse %.3f m; frame error %.3f m %.4f rad (sigma %.3f m, mahalanobis² %.2f); robot b rmse %.3f m; %d iterations",
		len(matches), al.RMSE, d, yaw, sigma, m2, rmse, res.Iterations)
	if d > frameTol || yaw > frameYawTol {
		t.Errorf("frame transform off by %.3f m, %.4f rad", d, yaw)
	}
	if m2 > must(geom.ChiSquareQuantile(3, covGate))(t) || sigma < minSigma || sigma > maxSigma {
		t.Errorf("covariance does not describe the error: mahalanobis² %.2f, sigma %.4f m", m2, sigma)
	}
	if rmse > rmseTol {
		t.Errorf("robot b rmse %.3f m in robot a's frame", rmse)
	}
	gate := must(geom.ChiSquareQuantile(3, covGate))(t)
	for _, id := range falses {
		if res.Edges[id].Chi2 <= gate {
			t.Errorf("false match %d has chi2 %.1f, below the gate", id, res.Edges[id].Chi2)
		}
	}
	// The inverse query is the inverse transform.
	back, _, err := g.FrameTransform("b", "a")
	if err != nil {
		t.Fatal(err)
	}
	if d, yaw := poseError(back.Compose(got), geom.Pose2{}); d > 1e-9 || yaw > 1e-9 {
		t.Errorf("FrameTransform(b, a) is not the inverse: %.2g m, %.2g rad", d, yaw)
	}
}

func TestAlignNoConsensus(t *testing.T) {
	g := New(Config{})
	a0 := must(g.AddNode("a", geom.Pose2{}))(t)
	a1 := must(g.AddNode("a", geom.Pose2{X: 1}))(t)
	b0 := must(g.AddNode("b", geom.Pose2{}))(t)
	b1 := must(g.AddNode("b", geom.Pose2{X: 1}))(t)
	info := noise{0.02, 0.01}.info()
	must(g.AddEdge(Relative(a0, a1, geom.Pose2{X: 1}, info)))(t)
	must(g.AddEdge(Relative(b0, b1, geom.Pose2{X: 1}, info)))(t)
	// A landmark seen only by robot b lives in b's frame; one that a also sees does not.
	obs := geom.Cov2{100, 0, 0, 100}
	onlyB := must(g.AddLandmark(geom.Vec2{X: 1, Z: 1}))(t)
	shared := must(g.AddLandmark(geom.Vec2{X: 0, Z: 2}))(t)
	must(g.AddEdge(Observation(b0, onlyB, geom.Vec2{X: 1, Z: 1}, obs)))(t)
	must(g.AddEdge(Observation(b1, shared, geom.Vec2{X: 1, Z: 2}, obs)))(t)
	must(g.AddEdge(Observation(a0, shared, geom.Vec2{X: 0, Z: 2}, obs)))(t)
	// Two matches that put robot b five metres apart.
	must(g.AddEdge(Relative(a0, b0, geom.Pose2{X: 2}, info)))(t)
	must(g.AddEdge(Relative(a1, b1, geom.Pose2{X: 7, Yaw: 1}, info)))(t)
	before := must(g.Pose(b1))(t)
	if _, err := g.Align("a", "b", AlignConfig{}); !errors.Is(err, geom.ErrNoConsensus) {
		t.Fatalf("contradicting matches: %v", err)
	}
	if must(g.Pose(b1))(t) != before || g.Aligned("a", "b") {
		t.Error("a refused alignment changed the graph")
	}
	if _, err := g.Align("a", "nobody", AlignConfig{}); !errors.Is(err, ErrUnknownID) {
		t.Errorf("unknown trajectory: %v", err)
	}
	// A caller that accepts a single match gets the first one's proposal.
	al, err := g.Align("a", "b", AlignConfig{MinInliers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(al.Inliers) != 1 || len(al.Outliers) != 1 || !g.Aligned("a", "b") {
		t.Errorf("single-match alignment: %+v", al)
	}
	if d, yaw := poseError(must(g.Pose(b0))(t), geom.Pose2{X: 2}); d > 1e-12 || yaw > 1e-12 {
		t.Errorf("b0 at %+v, want x=2", must(g.Pose(b0))(t))
	}
	if got := must(g.Landmark(onlyB))(t); got.Dist(geom.Vec2{X: 3, Z: 1}) > 1e-12 {
		t.Errorf("landmark seen only from b at %+v, want it moved with b", got)
	}
	if got := must(g.Landmark(shared))(t); got != (geom.Vec2{X: 0, Z: 2}) {
		t.Errorf("landmark also seen from a moved to %+v", got)
	}
	if tf, cov, err := g.FrameTransform("a", "a"); err != nil || tf != (geom.Pose2{}) || cov != (geom.Mat3{}) {
		t.Errorf("FrameTransform(a, a) = %+v, %v, %v", tf, cov, err)
	}
	if _, _, err := g.FrameTransform("a", "nobody"); !errors.Is(err, ErrUnknownID) {
		t.Errorf("FrameTransform with an unknown trajectory: %v", err)
	}
}

// rangeScenario is a robot circling a square among three fixed UWB beacons,
// with odometry and a range to each beacon at every pose. The robot's first
// estimate is a metre and 0.3 rad from where it is, and no robot node is fixed.
func rangeScenario(t testing.TB, seed uint64, rangeSigma float64) (g *Graph, ids []NodeID, truth []geom.Pose2) {
	odo := noise{0.01, 0.005}
	r := newRand(seed)
	truth = squarePath(6, 0.5, 1)
	for i := range truth {
		truth[i] = geom.Pose2{X: 2, Z: 1, Yaw: 0.4}.Compose(truth[i])
	}
	g = New(Config{})
	var beacons []NodeID
	for _, p := range []geom.Vec2{{X: -1, Z: -2}, {X: 12, Z: 0}, {X: 3, Z: 11}} {
		id := must(g.AddNode("beacons", geom.Pose2{X: p.X, Z: p.Z}))(t)
		must(0, g.SetFixed(id, true))(t)
		beacons = append(beacons, id)
	}
	ids = drive(t, g, "robot", r, truth, truth[0].Compose(geom.Pose2{X: 0.8, Z: -0.6, Yaw: 0.3}), odo)
	for k, id := range ids {
		for _, b := range beacons {
			d := truth[k].Pos().Dist(must(g.Pose(b))(t).Pos()) + r.NormFloat64()*rangeSigma
			must(g.AddEdge(Range(id, b, d, rangeSigma)))(t)
		}
	}
	return g, ids, truth
}

func TestRangeEdges(t *testing.T) {
	const (
		rangeSigma = 0.05 // metres, UWB
		rmseTol    = 0.05 // metres: ranges to three beacons at every pose
		yawTol     = 0.05 // radians, observable only through motion
	)
	g, ids, truth := rangeScenario(t, 5, rangeSigma)
	before := positionRMSE(t, g, ids, geom.Pose2{}, truth)
	res, err := g.Optimize()
	if err != nil {
		t.Fatal(err)
	}
	after := positionRMSE(t, g, ids, geom.Pose2{}, truth)
	_, yaw := poseError(must(g.Pose(ids[0]))(t), truth[0])
	t.Logf("rmse %.3f m -> %.3f m, yaw error %.4f rad, %d iterations, chi2/dof %.2f", before, after, yaw, res.Iterations, res.Chi2/float64(res.DOF))
	if after > rmseTol || yaw > yawTol || len(res.Gauge) != 0 {
		t.Errorf("rmse %.3f m, yaw error %.4f rad, gauge %v", after, yaw, res.Gauge)
	}
	if res.Edges[len(res.Edges)-1].Dim != 1 {
		t.Errorf("range edge dim = %d", res.Edges[len(res.Edges)-1].Dim)
	}
}

// Over many noise draws the squared error normalised by the reported
// covariance must average the state dimension: the marginal is neither
// optimistic nor vacuous.
func TestMarginalCalibration(t *testing.T) {
	const (
		runs = 200
		// Mean of 200 chi-square(3) draws: 3 ± 0.17 (1σ).
		neesLo, neesHi = 2.5, 3.5
	)
	sum := 0.0
	for seed := uint64(0); seed < runs; seed++ {
		g, ids, truth := rangeScenario(t, 100+seed, 0.05)
		must(g.Optimize())(t)
		k := int(seed) % len(ids)
		p := must(g.Pose(ids[k]))(t)
		d := [3]float64{p.X - truth[k].X, p.Z - truth[k].Z, geom.AngleDiff(p.Yaw, truth[k].Yaw)}
		sum += must(geom.MahalanobisSq3(d, must(g.Marginal(ids[k]))(t)))(t)
	}
	if nees := sum / runs; nees < neesLo || nees > neesHi {
		t.Errorf("mean normalised squared error %.2f, want within [%v, %v]", nees, neesLo, neesHi)
	} else {
		t.Logf("mean normalised squared error %.2f over %d runs (ideal 3)", nees, runs)
	}
}

func TestPositionPriors(t *testing.T) {
	const (
		fixSigma = 0.5  // metres per axis, GNSS-like
		rmseTol  = 0.25 // metres: 81 fixes tied together by odometry
		// Heading comes only from the fixes along a 10 m square: about
		// 0.025 rad (1σ) at this noise, so allow three.
		yawTol = 0.08
	)
	odo := noise{0.01, 0.005}
	r := newRand(6)
	truth := squarePath(10, 0.5, 1)
	for i := range truth {
		truth[i] = geom.Pose2{X: 20, Z: 30, Yaw: 1}.Compose(truth[i])
	}
	g := New(Config{})
	// The robot's own frame starts at its first pose; the fixes are in the site frame.
	ids := drive(t, g, "robot", r, truth, geom.Pose2{}, odo)
	info := geom.Cov2{1 / (fixSigma * fixSigma), 0, 0, 1 / (fixSigma * fixSigma)}
	for k, id := range ids {
		fix := truth[k].Pos().Add(geom.Vec2{X: r.NormFloat64() * fixSigma, Z: r.NormFloat64() * fixSigma})
		must(g.AddEdge(PositionPrior(id, fix, info)))(t)
	}
	res, err := g.Optimize()
	if err != nil {
		t.Fatal(err)
	}
	rmse := positionRMSE(t, g, ids, geom.Pose2{}, truth)
	_, yaw := poseError(must(g.Pose(ids[0]))(t), truth[0])
	sigma := geom.PositionCov(must(g.Marginal(ids[40]))(t)).MaxSigma()
	t.Logf("rmse %.3f m, yaw error %.4f rad, marginal sigma %.3f m, %d iterations", rmse, yaw, sigma, res.Iterations)
	if rmse > rmseTol || yaw > yawTol {
		t.Errorf("rmse %.3f m, yaw error %.4f rad", rmse, yaw)
	}
	if len(res.Gauge) != 0 {
		t.Errorf("gauge %v held although the priors anchor the trajectory", res.Gauge)
	}
	if sigma >= fixSigma || sigma <= fixSigma/math.Sqrt(float64(len(ids)))/2 {
		t.Errorf("marginal sigma %.3f m is not between one fix (%.2f m) and all of them averaged", sigma, fixSigma)
	}
}

func TestLandmarks(t *testing.T) {
	const tol = 1e-6 // metres: the measurements are exact
	truth := []geom.Pose2{{}, {X: 2, Z: 0.5, Yaw: 0.7}, {X: 3, Z: 2, Yaw: 2.5}}
	points := []geom.Vec2{{X: 4, Z: -1}, {X: 1, Z: 3}, {X: -2, Z: 1}}
	g := New(Config{})
	var ids []NodeID
	for _, p := range truth {
		// Odometry is not used: the poses are tied together by the landmarks alone.
		ids = append(ids, must(g.AddNode("robot", p.Compose(geom.Pose2{X: 0.3, Z: -0.2, Yaw: 0.2})))(t))
	}
	must(0, g.SetPose(ids[0], truth[0]))(t)
	must(0, g.SetFixed(ids[0], true))(t)
	info := geom.Cov2{400, 0, 0, 400}
	var lms []LandmarkID
	for _, p := range points {
		lm := must(g.AddLandmark(p.Add(geom.Vec2{X: 0.4, Z: 0.3})))(t)
		lms = append(lms, lm)
		for k, id := range ids {
			must(g.AddEdge(Observation(id, lm, truth[k].InverseApply(p), info)))(t)
		}
	}
	res, err := g.Optimize()
	if err != nil {
		t.Fatal(err)
	}
	if res.DOF != 3*3*2-2*3-3*2 || res.Chi2 > tol {
		t.Errorf("dof = %d, chi2 = %g", res.DOF, res.Chi2)
	}
	for k, id := range ids {
		if d, yaw := poseError(must(g.Pose(id))(t), truth[k]); d > tol || yaw > tol {
			t.Errorf("node %d off by %g m, %g rad", k, d, yaw)
		}
	}
	for i, lm := range lms {
		if d := must(g.Landmark(lm))(t).Dist(points[i]); d > tol {
			t.Errorf("landmark %d off by %g m", i, d)
		}
	}
	// A landmark nobody observes is its own floating component and stays put.
	lone := must(g.AddLandmark(geom.Vec2{X: 9, Z: 9}))(t)
	if _, err := g.Optimize(); err != nil {
		t.Fatal(err)
	}
	if got := must(g.Landmark(lone))(t); got != (geom.Vec2{X: 9, Z: 9}) {
		t.Errorf("unobserved landmark moved to %+v", got)
	}
}

// The analytic Jacobians must match central differences for every edge kind,
// including across the ±π yaw seam.
func TestJacobians(t *testing.T) {
	const (
		h   = 1e-6
		tol = 1e-6
	)
	r := newRand(7)
	g := New(Config{})
	a := must(g.AddNode("a", geom.Pose2{X: 1, Z: -2, Yaw: 3.1}))(t)
	b := must(g.AddNode("a", geom.Pose2{X: -0.5, Z: 1.5, Yaw: -3.0}))(t)
	lm := must(g.AddLandmark(geom.Vec2{X: 2, Z: 2}))(t)
	info := geom.Mat3{0: 1, 4: 1, 8: 1}
	edges := []Edge{
		Relative(a, b, geom.Pose2{X: 0.3, Z: 0.1, Yaw: 0.2}, info),
		PosePrior(a, geom.Pose2{X: 1.1, Z: -2.2, Yaw: -3.1}, info),
		PositionPrior(b, geom.Vec2{X: 0, Z: 1}, geom.Cov2{1, 0, 0, 1}),
		Range(a, b, 3, 0.1),
		Observation(a, lm, geom.Vec2{X: 1, Z: 1}, geom.Cov2{1, 0, 0, 1}),
	}
	// perturb moves state component c of the edge's first or second variable.
	perturb := func(e *Edge, second bool, c int, d float64) {
		switch {
		case second && e.Kind == EdgeLandmark:
			p := &g.lms[e.Landmark]
			*p = p.Add(geom.Vec2{X: d * float64(1-c), Z: d * float64(c)})
		default:
			id := e.From
			if second {
				id = e.To
			}
			p := &g.est[id]
			switch c {
			case 0:
				p.X += d
			case 1:
				p.Z += d
			default:
				p.Yaw = geom.WrapAngle(p.Yaw + d)
			}
		}
	}
	for trial := 0; trial < 20; trial++ {
		g.est[a] = geom.Pose2{X: r.Float64()*4 - 2, Z: r.Float64()*4 - 2, Yaw: math.Pi - r.Float64()*0.2}
		g.est[b] = geom.Pose2{X: r.Float64()*4 - 2, Z: r.Float64()*4 - 2, Yaw: -math.Pi + r.Float64()*0.2}
		for _, e := range edges {
			_, ja, jb := g.jacobian(&e)
			for vi, j := range []geom.Mat3{ja, jb} {
				second := vi == 1
				if second && e.Kind != EdgeRelative && e.Kind != EdgeRange && e.Kind != EdgeLandmark {
					continue
				}
				cols := 3
				if second && e.Kind == EdgeLandmark {
					cols = 2
				}
				for c := 0; c < cols; c++ {
					perturb(&e, second, c, h)
					hi, _, _ := g.jacobian(&e)
					perturb(&e, second, c, -2*h)
					lo, _, _ := g.jacobian(&e)
					perturb(&e, second, c, h)
					// The yaw residual is wrapped, so difference it as an angle.
					num := [3]float64{(hi.X - lo.X) / (2 * h), (hi.Y - lo.Y) / (2 * h), geom.AngleDiff(hi.Z, lo.Z) / (2 * h)}
					for row := 0; row < 3; row++ {
						if math.Abs(num[row]-j[row*3+c]) > tol {
							t.Fatalf("kind %d variable %d: d r[%d] / d x[%d] = %g analytic, %g numeric", e.Kind, vi, row, c, j[row*3+c], num[row])
						}
					}
				}
			}
		}
	}
}

// The kernels must be continuous at their threshold and their weight must be
// the derivative of the loss, or the optimiser's accept test is inconsistent.
func TestKernels(t *testing.T) {
	const tol = 1e-5
	for _, k := range []Kernel{KernelNone, KernelHuber, KernelDCS} {
		cfg := Config{Kernel: k}.withDefaults()
		for _, chi2 := range []float64{0.5, 3.999, 4.001, 4.999, 5.001, 40, 4000} {
			rho, w := cfg.kernel(true, chi2)
			lo, _ := cfg.kernel(true, chi2*(1-1e-6))
			hi, _ := cfg.kernel(true, chi2*(1+1e-6))
			if slope := (hi - lo) / (2e-6 * chi2); math.Abs(slope-w) > tol {
				t.Errorf("kernel %d at chi2 %g: weight %g, slope of the loss %g", k, chi2, w, slope)
			}
			if rho > chi2 || w > 1 || w <= 0 {
				t.Errorf("kernel %d at chi2 %g: rho %g, weight %g", k, chi2, rho, w)
			}
			if plain, pw := cfg.kernel(false, chi2); plain != chi2 || pw != 1 {
				t.Errorf("kernel %d applied to a non-robust edge", k)
			}
		}
	}
	if rho, _ := (&Config{Kernel: KernelDCS, DCSPhi: 5}).kernel(true, 1e12); rho > 15 {
		t.Errorf("DCS loss %g exceeds its bound 3Φ", rho)
	}
}

// denseMarginals inverts the normal matrix assembled without the sparse
// machinery: every free node and landmark in id order.
func denseMarginals(t *testing.T, g *Graph, held map[NodeID]bool) (cov geom.Mat, offset map[NodeID]int) {
	offset = map[NodeID]int{}
	n := 0
	for i := range g.est {
		if !g.fixed[i] && !held[NodeID(i)] {
			offset[NodeID(i)] = n
			n += 3
		}
	}
	lmOffset := n
	n += 2 * len(g.lms)
	h := geom.NewMat(n, n)
	for k, e := range g.edges {
		if g.gone[k] {
			continue
		}
		_, ja, jb := g.jacobian(&e)
		type part struct {
			j        geom.Mat3
			off, dim int
		}
		var parts []part
		if off, ok := offset[e.From]; ok {
			parts = append(parts, part{ja, off, 3})
		}
		switch e.Kind {
		case EdgeRelative, EdgeRange:
			if off, ok := offset[e.To]; ok {
				parts = append(parts, part{jb, off, 3})
			}
		case EdgeLandmark:
			parts = append(parts, part{jb, lmOffset + 2*int(e.Landmark), 2})
		}
		for _, p := range parts {
			for _, q := range parts {
				blk := p.j.T().Mul(e.Info.Mul(q.j))
				for r := 0; r < p.dim; r++ {
					for c := 0; c < q.dim; c++ {
						h.Set(p.off+r, q.off+c, h.At(p.off+r, q.off+c)+blk[r*3+c])
					}
				}
			}
		}
	}
	cov, err := geom.Inverse(h)
	if err != nil {
		t.Fatal(err)
	}
	return cov, offset
}

func TestMarginalMatchesDenseInverse(t *testing.T) {
	const relTol = 1e-6
	odo := noise{0.02, 0.01}
	r := newRand(8)
	g, ids, truth, _ := buildManhattan(t, Config{}, r, 80, 5, odo, odo)
	for i := 0; i < 4; i++ {
		p := geom.Vec2{X: r.Float64() * 5, Z: r.Float64() * 5}
		lm := must(g.AddLandmark(p))(t)
		for k := i; k < len(ids); k += 9 {
			must(g.AddEdge(Observation(ids[k], lm, truth[k].InverseApply(p), geom.Cov2{900, 100, 100, 400})))(t)
		}
	}
	must(g.AddEdge(PositionPrior(ids[33], truth[33].Pos(), geom.Cov2{4, 0, 0, 4})))(t)
	must(g.AddEdge(Range(ids[10], ids[60], truth[10].Pos().Dist(truth[60].Pos()), 0.1)))(t)
	if _, err := g.Optimize(); err != nil {
		t.Fatal(err)
	}
	dense, offset := denseMarginals(t, g, nil)
	for _, k := range []int{1, 17, 40, 79} {
		got := must(g.Marginal(ids[k]))(t)
		off := offset[ids[k]]
		for i := 0; i < 9; i++ {
			want := dense.At(off+i/3, off+i%3)
			if scale := math.Sqrt(dense.At(off+i/3, off+i/3) * dense.At(off+i%3, off+i%3)); math.Abs(got[i]-want) > relTol*scale {
				t.Fatalf("node %d covariance[%d] = %g, dense inverse gives %g", k, i, got[i], want)
			}
		}
	}
	if got := must(g.Marginal(ids[0]))(t); got != (geom.Mat3{}) {
		t.Errorf("fixed node covariance = %v", got)
	}
}

// One measured edge from a fixed node: the marginal is the measurement
// covariance moved into the graph frame.
func TestMarginalOfSingleEdge(t *testing.T) {
	g := New(Config{})
	base := geom.Pose2{X: 1, Z: 2, Yaw: 0.8}
	z := geom.Pose2{X: 2, Z: -1, Yaw: 0.5}
	a := must(g.AddNode("r", base))(t)
	b := must(g.AddNode("r", base.Compose(z)))(t)
	must(0, g.SetFixed(a, true))(t)
	covZ := geom.Mat3{0.04, 0.01, 0.002, 0.01, 0.09, -0.003, 0.002, -0.003, 0.01}
	must(g.AddEdge(Relative(a, b, z, must(covZ.Inverse())(t))))(t)
	if _, err := g.Optimize(); err != nil {
		t.Fatal(err)
	}
	got, want := must(g.Marginal(b))(t), geom.ComposeCov(base, geom.Mat3{}, z, covZ)
	for i := range got {
		if math.Abs(got[i]-want[i]) > 1e-9 {
			t.Fatalf("marginal = %v, want %v", got, want)
		}
	}
}

func TestIncremental(t *testing.T) {
	const (
		firstBatch = 300
		total      = 340
		maxIters   = 6    // a warm re-optimisation after 40 new nodes and their closures
		agreeTol   = 1e-3 // metres between incremental and batch solutions
	)
	odo := noise{0.02, 0.01}
	// build optimises after `stops` prefixes of the same measurement stream.
	build := func(stops ...int) (*Graph, []NodeID, []Result) {
		r := newRand(9)
		truth := manhattan(r, total, 10)
		pairs := revisits(truth, 20)
		g := New(Config{})
		ids := []NodeID{must(g.AddNode("robot", truth[0]))(t)}
		must(0, g.SetFixed(ids[0], true))(t)
		var results []Result
		local, nextPair := truth[0], 0
		for k := 1; k < total; k++ {
			z := odo.measure(r, truth[k-1], truth[k])
			local = local.Compose(z)
			ids = append(ids, must(g.AddNode("robot", local))(t))
			must(g.AddEdge(Relative(ids[k-1], ids[k], z, odo.info())))(t)
			for ; nextPair < len(pairs) && pairs[nextPair][1] == k; nextPair++ {
				p := pairs[nextPair]
				must(g.AddEdge(Relative(ids[p[0]], ids[p[1]], odo.measure(r, truth[p[0]], truth[p[1]]), odo.info())))(t)
			}
			for _, stop := range stops {
				if k+1 == stop {
					results = append(results, must(g.Optimize())(t))
				}
			}
		}
		return g, ids, results
	}
	inc, ids, incRes := build(firstBatch, total)
	batch, _, batchRes := build(total)
	// The same calls give bit-identical estimates.
	twin, _, _ := build(firstBatch, total)
	for _, id := range ids {
		if must(inc.Pose(id))(t) != must(twin.Pose(id))(t) {
			t.Fatalf("node %d differs between identical runs", id)
		}
	}
	t.Logf("iterations: first %d, incremental %d, batch %d", incRes[0].Iterations, incRes[1].Iterations, batchRes[0].Iterations)
	if incRes[1].Iterations > maxIters || incRes[1].Iterations > batchRes[0].Iterations {
		t.Errorf("warm re-optimisation took %d iterations (batch %d)", incRes[1].Iterations, batchRes[0].Iterations)
	}
	for _, id := range ids {
		if d := must(inc.Pose(id))(t).Pos().Dist(must(batch.Pose(id))(t).Pos()); d > agreeTol {
			t.Fatalf("node %d: incremental and batch differ by %g m", id, d)
		}
	}
	// Optimising a converged graph again is a no-op in one step, and so is
	// extending it by odometry alone: the new node starts where odometry puts it.
	again := must(inc.Optimize())(t)
	if again.Iterations > 1 || math.Abs(again.Chi2-incRes[1].Chi2) > 1e-6*incRes[1].Chi2 {
		t.Errorf("re-optimising a converged graph: %d iterations, chi2 %g -> %g", again.Iterations, incRes[1].Chi2, again.Chi2)
	}
	step := geom.Pose2{X: 1, Yaw: 0.1}
	tip := must(inc.AddNode("robot", must(inc.Local(ids[total-1]))(t).Compose(step)))(t)
	must(inc.AddEdge(Relative(ids[total-1], tip, step, odo.info())))(t)
	if ext := must(inc.Optimize())(t); ext.Iterations > 1 || math.Abs(ext.Chi2-again.Chi2) > 1e-6*again.Chi2 {
		t.Errorf("extending by one odometry step: %d iterations, chi2 %g -> %g", ext.Iterations, again.Chi2, ext.Chi2)
	}
}

func TestGauge(t *testing.T) {
	odo := noise{0.02, 0.01}
	r := newRand(10)
	truth := squarePath(4, 1, 1)
	g := New(Config{})
	a := drive(t, g, "a", r, truth, geom.Pose2{X: 5, Z: 5, Yaw: 1}, odo)
	b := drive(t, g, "b", r, truth, geom.Pose2{}, odo)
	must(g.AddEdge(Relative(a[len(a)-1], a[0], odo.measure(r, truth[len(truth)-1], truth[0]), odo.info())))(t)
	start := must(g.Pose(a[0]))(t)
	res, err := g.Optimize()
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Gauge) != 2 || res.Gauge[0] != a[0] || res.Gauge[1] != b[0] {
		t.Errorf("gauge = %v, want the first node of each trajectory", res.Gauge)
	}
	if must(g.Pose(a[0]))(t) != start {
		t.Error("the gauge node moved")
	}
	if got := must(g.Marginal(a[0]))(t); got != (geom.Mat3{}) {
		t.Errorf("gauge node covariance = %v", got)
	}
	if g.Aligned("a", "b") {
		t.Error("unconnected trajectories reported as sharing a frame")
	}
	// Fixing a node in each puts both in the graph frame.
	must(0, g.SetFixed(a[0], true))(t)
	must(0, g.SetFixed(b[0], true))(t)
	if res, err = g.Optimize(); err != nil || len(res.Gauge) != 0 || !g.Aligned("a", "b") {
		t.Errorf("fixed trajectories: gauge %v, aligned %v, err %v", res.Gauge, g.Aligned("a", "b"), err)
	}
}

func TestDegenerate(t *testing.T) {
	info := noise{0.02, 0.01}.info()
	if _, err := New(Config{}).Optimize(); !errors.Is(err, geom.ErrInsufficient) {
		t.Errorf("empty graph: %v", err)
	}

	// One position fix leaves the rotation about it free.
	g := New(Config{})
	a := must(g.AddNode("r", geom.Pose2{}))(t)
	b := must(g.AddNode("r", geom.Pose2{X: 1}))(t)
	must(g.AddEdge(Relative(a, b, geom.Pose2{X: 1.1}, info)))(t)
	fix := must(g.AddEdge(PositionPrior(a, geom.Vec2{X: 3, Z: 3}, geom.Cov2{1, 0, 0, 1})))(t)
	if _, err := g.Optimize(); !errors.Is(err, geom.ErrSingular) {
		t.Errorf("single position prior: %v", err)
	}
	if must(g.Pose(a))(t) != (geom.Pose2{}) || must(g.Pose(b))(t) != (geom.Pose2{X: 1}) {
		t.Error("estimates changed although the optimisation was refused")
	}
	if _, err := g.Marginal(b); !errors.Is(err, geom.ErrSingular) {
		t.Errorf("marginal of an unobservable graph: %v", err)
	}
	// A second fix elsewhere pins it.
	must(g.AddEdge(PositionPrior(b, geom.Vec2{X: 3, Z: 4.1}, geom.Cov2{1, 0, 0, 1})))(t)
	if _, err := g.Optimize(); err != nil {
		t.Errorf("two position priors: %v", err)
	}
	if _, yaw := poseError(must(g.Pose(a))(t), geom.Pose2{X: 3, Z: 3, Yaw: math.Pi / 2}); yaw > 1e-3 {
		t.Errorf("heading from two fixes: %+v", must(g.Pose(a))(t))
	}
	must(0, g.RemoveEdge(fix))(t)
	if _, err := g.Optimize(); !errors.Is(err, geom.ErrSingular) {
		t.Errorf("after removing a fix: %v", err)
	}
	if err := g.RemoveEdge(fix); !errors.Is(err, ErrUnknownID) {
		t.Errorf("removing an edge twice: %v", err)
	}

	// Ranges alone do not observe yaw, and one range does not fix a position.
	g = New(Config{})
	a = must(g.AddNode("anchor", geom.Pose2{}))(t)
	b = must(g.AddNode("tag", geom.Pose2{X: 1, Z: 1}))(t)
	must(0, g.SetFixed(a, true))(t)
	must(g.AddEdge(Range(a, b, 2, 0.05)))(t)
	if _, err := g.Optimize(); !errors.Is(err, geom.ErrSingular) {
		t.Errorf("range only: %v", err)
	}

	// An iteration limit is reported with the best iterate kept.
	odo := noise{0.02, 0.01}
	g, _, _, _ = buildManhattan(t, Config{MaxIterations: 1}, newRand(11), 200, 8, odo, odo)
	res, err := g.Optimize()
	if !errors.Is(err, geom.ErrNoConvergence) || res.Converged || res.Iterations != 1 || res.Chi2 >= res.InitialChi2 {
		t.Errorf("one iteration: err %v, result %+v", err, res)
	}
}

func TestInvalidInput(t *testing.T) {
	g := New(Config{})
	a := must(g.AddNode("r", geom.Pose2{}))(t)
	b := must(g.AddNode("r", geom.Pose2{X: 1}))(t)
	lm := must(g.AddLandmark(geom.Vec2{}))(t)
	good := geom.Mat3{0: 1, 4: 1, 8: 1}
	nan := math.NaN()
	for name, tc := range map[string]struct {
		edge Edge
		want error
	}{
		"unknown kind":       {Edge{From: a, Info: good}, ErrInvalid},
		"unknown from":       {Relative(7, b, geom.Pose2{}, good), ErrUnknownID},
		"unknown to":         {Relative(a, -1, geom.Pose2{}, good), ErrUnknownID},
		"self loop":          {Relative(a, a, geom.Pose2{}, good), ErrInvalid},
		"unknown landmark":   {Observation(a, lm+1, geom.Vec2{}, geom.Cov2{1, 0, 0, 1}), ErrUnknownID},
		"nan measurement":    {Relative(a, b, geom.Pose2{X: nan}, good), geom.ErrNonFinite},
		"nan information":    {PosePrior(a, geom.Pose2{}, geom.Mat3{0: nan, 4: 1, 8: 1}), geom.ErrNonFinite},
		"zero information":   {PosePrior(a, geom.Pose2{}, geom.Mat3{}), ErrInvalid},
		"asymmetric":         {PosePrior(a, geom.Pose2{}, geom.Mat3{1, 0.5, 0, 0, 1, 0, 0, 0, 1}), ErrInvalid},
		"indefinite":         {PosePrior(a, geom.Pose2{}, geom.Mat3{1, 2, 0, 2, 1, 0, 0, 0, 1}), ErrInvalid},
		"negative diagonal":  {PositionPrior(a, geom.Vec2{}, geom.Cov2{1, 0, 0, -1}), ErrInvalid},
		"negative range":     {Range(a, b, -1, 0.1), ErrInvalid},
		"non-positive sigma": {Range(a, b, 1, 0), ErrInvalid},
	} {
		if _, err := g.AddEdge(tc.edge); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	if g.NumEdges() != 0 {
		t.Errorf("%d rejected edges were stored", g.NumEdges())
	}
	// A rank-deficient but semi-definite information matrix is a valid partial measurement.
	if _, err := g.AddEdge(Relative(a, b, geom.Pose2{X: 1}, geom.Mat3{0: 1, 4: 1})); err != nil {
		t.Errorf("semi-definite information: %v", err)
	}
	if _, err := g.AddNode("r", geom.Pose2{Yaw: math.Inf(1)}); !errors.Is(err, geom.ErrNonFinite) {
		t.Errorf("non-finite node: %v", err)
	}
	if _, err := g.AddLandmark(geom.Vec2{X: nan}); !errors.Is(err, geom.ErrNonFinite) {
		t.Errorf("non-finite landmark: %v", err)
	}
	for name, err := range map[string]error{
		"SetFixed": g.SetFixed(9, true),
		"SetPose":  g.SetPose(9, geom.Pose2{}),
		"Pose":     func() error { _, err := g.Pose(9); return err }(),
		"Local":    func() error { _, err := g.Local(-1); return err }(),
		"Landmark": func() error { _, err := g.Landmark(5); return err }(),
		"Marginal": func() error { _, err := g.Marginal(9); return err }(),
	} {
		if !errors.Is(err, ErrUnknownID) {
			t.Errorf("%s with an unknown id: %v", name, err)
		}
	}
	if g.Nodes("nobody") != nil || len(g.Nodes("r")) != 2 || g.NumNodes() != 2 {
		t.Errorf("Nodes: %v %v", g.Nodes("nobody"), g.Nodes("r"))
	}
}

// A node added after an optimisation starts from the corrected trajectory,
// not from its raw local pose.
func TestAddNodeFollowsCorrection(t *testing.T) {
	g := New(Config{})
	a := must(g.AddNode("r", geom.Pose2{}))(t)
	b := must(g.AddNode("r", geom.Pose2{X: 1}))(t)
	must(0, g.SetPose(b, geom.Pose2{X: 1, Z: 1, Yaw: math.Pi / 2}))(t) // as an optimisation would
	c := must(g.AddNode("r", geom.Pose2{X: 2}))(t)
	if d, yaw := poseError(must(g.Pose(c))(t), geom.Pose2{X: 1, Z: 2, Yaw: math.Pi / 2}); d > 1e-12 || yaw > 1e-12 {
		t.Errorf("new node at %+v", must(g.Pose(c))(t))
	}
	if must(g.Local(c))(t) != (geom.Pose2{X: 2}) || must(g.Pose(a))(t) != (geom.Pose2{}) {
		t.Error("local pose not kept")
	}
}
