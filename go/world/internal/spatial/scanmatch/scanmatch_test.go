package scanmatch

import (
	"errors"
	"math"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
)

// Simulated scanner, matching the design's default noise.
const (
	simBeams      = 360
	simRangeMax   = 12.0
	simRangeSigma = 0.015
)

// Acceptance thresholds.
const (
	// Local refinement from a 20 cm / 5° perturbation, for every one of
	// refineTrials random poses. Achieved: mean 0.9 cm, worst 1.8 cm, worst
	// yaw 0.10°. The translation margin is thin by nature: the map knows a
	// wall only to the centre of its 5 cm cells, and this building is
	// aligned with the grid, so a whole wall shares one offset and the bias
	// does not average out along it. The mean is asserted too, so a
	// regression cannot hide inside the worst case.
	perturbTranslation   = 0.20
	perturbYaw           = 5 * math.Pi / 180
	maxRefineTranslation = 0.02
	maxRefineMean        = 0.012
	maxRefineYaw         = 0.3 * math.Pi / 180
	refineTrials         = 200
	// Global search from an unknown pose: the right basin, then refined to
	// the local accuracy above.
	maxGlobalTranslation = 0.03
	maxGlobalYaw         = 0.3 * math.Pi / 180
	// A full-map, full-rotation search must finish well under a second
	// (measured: about 60 ms). The race detector slows it roughly tenfold.
	maxGlobalSearch = time.Second
	raceSlowdown    = 20
	// Covariance sanity: 1σ between a millimetre and 5 cm / 1°, and the
	// true error inside the 99.9 % gate of the reported covariance.
	minSigmaTranslation = 0.001
	maxSigmaTranslation = 0.05
	maxSigmaYaw         = 1 * math.Pi / 180
	covarianceGate      = 0.999
	// A well-matched scan in a mapped room.
	minGoodScore  = 0.8
	minGoodInlier = 0.9
	// Scan-to-scan ICP has no grid quantisation.
	maxIcpTranslation = 0.015
	maxIcpYaw         = 0.2 * math.Pi / 180
	// Along an unobserved direction the reported 1σ must be at least this.
	minUnobservedSigma = 10.0
	// In a corridor the weak direction must be the corridor axis to within
	// this cosine, and the observed directions must still be accurate.
	minWeakAlignment = 0.99
	// Scan-to-scan must not move along an unobserved direction.
	maxUnobservedDrift = 0.001
	// Information of a corridor match: next to none along its weakest
	// direction (UnobservedSigma 100 m gives 1e-4 m⁻²), and better than 5 cm
	// (400 m⁻²) across the corridor.
	maxUnobservedInformation = 1e-3
	minObservedInformation   = 400
	// Mean squared Mahalanobis error over (x, z, yaw); 3 is calibrated. Below
	// 1 an edge enters a pose graph with under a third of its information,
	// above 6 with twice too much.
	minMeanMahalanobis = 1.0
	maxMeanMahalanobis = 6.0
	calibrationTrials  = 200
)

func box(x0, z0, x1, z1 float64) []geom.Segment {
	a, b, c, d := geom.Vec2{X: x0, Z: z0}, geom.Vec2{X: x1, Z: z0}, geom.Vec2{X: x1, Z: z1}, geom.Vec2{X: x0, Z: z1}
	return []geom.Segment{{A: a, B: b}, {A: b, B: c}, {A: c, B: d}, {A: d, B: a}}
}

func seg(x0, z0, x1, z1 float64) geom.Segment {
	return geom.Segment{A: geom.Vec2{X: x0, Z: z0}, B: geom.Vec2{X: x1, Z: z1}}
}

// building is a 40 x 30 m floor with five rooms of different sizes, doors,
// stub walls and furniture, none of it aligned to the 5 cm grid. No room is
// symmetric and no two rooms share a dimension, so every pose is globally
// identifiable.
func building() []geom.Segment {
	w := box(0.013, 0.027, 40.013, 30.027)
	w = append(w,
		seg(15.32, 0.027, 15.32, 8.1), seg(15.32, 9.3, 15.32, 18.04), // west room | hall, with a door
		seg(0.013, 18.04, 6.1, 18.04), seg(7.3, 18.04, 20.2, 18.04), seg(21.4, 18.04, 27.13, 18.04), // south | north rooms
		seg(27.13, 18.04, 27.13, 23.5), seg(27.13, 24.7, 27.13, 30.027),
		seg(29.61, 0.027, 29.61, 5.2), seg(29.61, 6.4, 29.61, 11.23), seg(29.61, 11.23, 40.013, 11.23),
		seg(9.37, 18.04, 9.37, 25.1),  // partition in the north-west room
		seg(31.2, 14.1, 36.9, 16.4),   // a slanted screen
		seg(0.013, 10.37, 4.6, 10.37), // stub walls
		seg(10.4, 0.027, 10.4, 2.9),
		seg(19.7, 9.1, 24.6, 9.1),
		seg(34.3, 30.027, 34.3, 26.2),
		seg(21.8, 30.027, 23.9, 25.4),
		seg(36.2, 11.23, 36.2, 7.9),
	)
	for _, f := range [][4]float64{
		{5.11, 5.23, 6.07, 7.31}, {20.33, 4.12, 21.41, 5.08}, {32.2, 20.4, 34.7, 21.3}, {2.3, 12.2, 3.1, 15.6},
		{17.4, 22.2, 18.6, 27.3}, {33.1, 3.3, 37.2, 4.2}, {22.2, 12.6, 23.1, 13.4}, {11.2, 26.1, 12.9, 28.2},
		{11.3, 13.2, 13.4, 14.1}, {2.2, 21.3, 3.4, 22.1}, {37.3, 23.1, 38.4, 27.2}, {25.3, 2.2, 26.4, 3.1},
	} {
		w = append(w, box(f[0], f[1], f[2], f[3])...)
	}
	return w
}

func rayRange(walls []geom.Segment, origin geom.Vec2, yaw float64) (float64, bool) {
	best, ok := math.Inf(1), false
	for _, w := range walls {
		if t, hit := geom.RaySegment(origin, geom.Heading(yaw), w); hit && t < best {
			best, ok = t, true
		}
	}
	return best, ok
}

// simScan is a noisy 360° scan of the walls from pose. pose is the
// simulator's ground truth; the returned scan does not carry it.
func simScan(walls []geom.Segment, pose geom.Pose2, rangeMax float64, rng *rand.Rand) spatial.Scan {
	s := spatial.Scan{
		Header:   spatial.Header{Device: "sim", Sensor: "lidar", Time: time.Unix(1, 0).UTC()},
		AngleMin: -math.Pi, AngleIncrement: 2 * math.Pi / simBeams,
		RangeMin: 0.05, RangeMax: rangeMax, RangeSigma: simRangeSigma, Ranges: make([]float64, simBeams),
	}
	for i := range s.Ranges {
		r, ok := rayRange(walls, pose.Pos(), pose.Yaw+s.Angle(i))
		if ok {
			r += rng.NormFloat64() * simRangeSigma
		}
		if !ok || r > rangeMax {
			r = spatial.RangeNoReturn
		}
		s.Ranges[i] = r
	}
	return s
}

// inside reports whether p is in the free space of the walls: an odd number
// of crossings toward +x, and not within clearance of any wall.
func clearOf(walls []geom.Segment, p geom.Vec2, clearance float64) bool {
	for _, w := range walls {
		if w.DistanceTo(p) < clearance {
			return false
		}
	}
	return true
}

// surveyMap builds the reference map the way a mapping run would have left
// it: scans inserted at surveyed poses on a lattice. The poses given to the
// grid here are the survey's, a fixture; the scans matched in the tests are
// taken elsewhere and their poses are never given to a matcher.
func surveyMap(t testing.TB, walls []geom.Segment, lo, hi geom.Vec2, spacing float64, seed uint64) *grid.Grid {
	t.Helper()
	g, err := grid.New(grid.Config{})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(seed, 99))
	for z := lo.Z + spacing/2; z < hi.Z; z += spacing {
		for x := lo.X + spacing/2; x < hi.X; x += spacing {
			pose := geom.Pose2{X: x, Z: z, Yaw: rng.Float64() * 2 * math.Pi}
			if !clearOf(walls, pose.Pos(), 0.3) || insideFurniture(walls, pose.Pos()) {
				continue
			}
			if err := g.InsertScan(pose, simScan(walls, pose, simRangeMax, rng)); err != nil {
				t.Fatal(err)
			}
		}
	}
	return g
}

// insideFurniture reports whether p is enclosed at close range on all four
// sides, which only happens inside a furniture box.
func insideFurniture(walls []geom.Segment, p geom.Vec2) bool {
	for _, yaw := range []float64{0, math.Pi / 2, math.Pi, -math.Pi / 2} {
		if r, ok := rayRange(walls, p, yaw); !ok || r > 2.6 {
			return false
		}
	}
	return true
}

var (
	buildingOnce sync.Once
	buildingGrid *grid.Grid
)

func buildingMap(t testing.TB) (*grid.Grid, []geom.Segment) {
	walls := building()
	buildingOnce.Do(func() {
		buildingGrid = surveyMap(t, walls, geom.Vec2{}, geom.Vec2{X: 40, Z: 30}, 2.5, 1)
	})
	return buildingGrid, walls
}

func mustMatcher(t testing.TB, g *grid.Grid, cfg Config) *Matcher {
	t.Helper()
	m, err := NewMatcher(g, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func poseError(got, want geom.Pose2) (translation, yaw float64) {
	return got.Pos().Dist(want.Pos()), math.Abs(geom.AngleDiff(got.Yaw, want.Yaw))
}

// queryPoses are scan poses spread over the building's rooms, none of them
// a survey pose.
var queryPoses = []geom.Pose2{
	{X: 6.83, Z: 9.41, Yaw: 0.7}, {X: 21.37, Z: 9.12, Yaw: -2.1}, {X: 34.41, Z: 7.77, Yaw: 1.9},
	{X: 4.52, Z: 23.63, Yaw: 3.0}, {X: 21.94, Z: 24.18, Yaw: -0.4}, {X: 33.08, Z: 25.46, Yaw: 2.4},
	{X: 11.21, Z: 3.17, Yaw: -1.3}, {X: 36.66, Z: 17.52, Yaw: 0.1},
}

func checkCovariance(t *testing.T, label string, r Result, truth geom.Pose2) {
	t.Helper()
	sx, sz, syaw := math.Sqrt(r.Covariance[0]), math.Sqrt(r.Covariance[4]), math.Sqrt(r.Covariance[8])
	if sx < minSigmaTranslation || sx > maxSigmaTranslation || sz < minSigmaTranslation || sz > maxSigmaTranslation || !(syaw > 0) || syaw > maxSigmaYaw {
		t.Errorf("%s: 1σ = (%.4f m, %.4f m, %.4f rad), outside the sane range", label, sx, sz, syaw)
	}
	for i := 0; i < 3; i++ {
		for j := 0; j < i; j++ {
			if math.Abs(r.Covariance[i*3+j]-r.Covariance[j*3+i]) > 1e-12 {
				t.Errorf("%s: covariance is not symmetric", label)
			}
		}
	}
	d := [3]float64{r.Pose.X - truth.X, r.Pose.Z - truth.Z, geom.AngleDiff(r.Pose.Yaw, truth.Yaw)}
	m2, err := geom.MahalanobisSq3(d, r.Covariance)
	gate, _ := geom.ChiSquareQuantile(3, covarianceGate)
	if err != nil || m2 > gate {
		t.Errorf("%s: error %v has squared Mahalanobis distance %.1f (gate %.1f, err %v): covariance too small", label, d, m2, gate, err)
	}
}

// randomPose draws a pose in the free space of the building.
func randomPose(walls []geom.Segment, rng *rand.Rand) geom.Pose2 {
	for {
		p := geom.Pose2{X: 1 + rng.Float64()*38, Z: 1 + rng.Float64()*28, Yaw: (rng.Float64()*2 - 1) * math.Pi}
		if clearOf(walls, p.Pos(), 0.4) && !insideFurniture(walls, p.Pos()) {
			return p
		}
	}
}

func perturb(p geom.Pose2, rng *rand.Rand) geom.Pose2 {
	dir := rng.Float64() * 2 * math.Pi
	yaw := perturbYaw
	if rng.IntN(2) == 0 {
		yaw = -yaw
	}
	return geom.Pose2{X: p.X + perturbTranslation*math.Cos(dir), Z: p.Z + perturbTranslation*math.Sin(dir), Yaw: p.Yaw + yaw}
}

func TestRefineRecoversPerturbedPose(t *testing.T) {
	g, walls := buildingMap(t)
	m := mustMatcher(t, g, Config{})
	rng := rand.New(rand.NewPCG(42, 1))
	var sum, worst, worstYaw float64
	for trial := 0; trial < refineTrials; trial++ {
		truth := randomPose(walls, rng)
		scan := simScan(walls, truth, simRangeMax, rng)
		r, err := m.Refine(scan, perturb(truth, rng))
		if err != nil {
			t.Fatalf("truth %+v: %v", truth, err)
		}
		et, ey := poseError(r.Pose, truth)
		sum, worst, worstYaw = sum+et, math.Max(worst, et), math.Max(worstYaw, ey)
		if et > maxRefineTranslation || ey > maxRefineYaw {
			t.Errorf("truth %+v: error %.4f m, %.3f°; want < %.2f m, %.1f°", truth, et, ey*180/math.Pi, maxRefineTranslation, maxRefineYaw*180/math.Pi)
		}
		if !r.Converged || r.Iterations < 1 || r.Points < DefaultConfig().MinPoints {
			t.Errorf("truth %+v: converged=%v iterations=%d points=%d", truth, r.Converged, r.Iterations, r.Points)
		}
		if r.Score < minGoodScore || r.InlierFraction < minGoodInlier || r.Score > 1 || r.InlierFraction > 1 {
			t.Errorf("truth %+v: score %.3f, inliers %.3f", truth, r.Score, r.InlierFraction)
		}
		if r.Degenerate {
			t.Errorf("truth %+v: a furnished room reported degenerate (min eigen %.3f)", truth, r.MinEigen)
		}
		checkCovariance(t, "refine", r, truth)
	}
	mean := sum / refineTrials
	t.Logf("refinement over %d poses: mean %.4f m, worst %.4f m, worst yaw %.3f°", refineTrials, mean, worst, worstYaw*180/math.Pi)
	if mean > maxRefineMean {
		t.Errorf("mean error %.4f m, want < %.3f m", mean, maxRefineMean)
	}
}

func TestRefineAndScoreAreDeterministic(t *testing.T) {
	g, walls := buildingMap(t)
	m := mustMatcher(t, g, Config{})
	scan := simScan(walls, queryPoses[0], simRangeMax, rand.New(rand.NewPCG(3, 3)))
	guess := geom.Pose2{X: queryPoses[0].X + 0.1, Z: queryPoses[0].Z - 0.1, Yaw: queryPoses[0].Yaw + 0.05}
	a, errA := m.Refine(scan, guess)
	b, errB := m.Refine(scan, guess)
	if errA != nil || errB != nil || a != b {
		t.Fatalf("repeat refinement differs: %+v vs %+v (%v, %v)", a, b, errA, errB)
	}
	// Score at the refined pose reports the same quality without iterating.
	s, err := m.Score(scan, a.Pose)
	if err != nil || s.Score != a.Score || s.InlierFraction != a.InlierFraction || s.Covariance != a.Covariance || s.Iterations != 0 {
		t.Errorf("Score = %+v, %v; Refine gave %+v", s, err, a)
	}
	// Half a metre off, the same scan scores far worse.
	off, err := m.Score(scan, geom.Pose2{X: a.Pose.X + 0.5, Z: a.Pose.Z + 0.5, Yaw: a.Pose.Yaw})
	if err != nil || off.Score > a.Score/2 {
		t.Errorf("displaced score %.3f vs %.3f (%v)", off.Score, a.Score, err)
	}
}

func TestGlobalLocalizesInAsymmetricBuilding(t *testing.T) {
	g, walls := buildingMap(t)
	m := mustMatcher(t, g, Config{})
	rng := rand.New(rand.NewPCG(7, 7))
	limit := maxGlobalSearch
	if raceEnabled {
		limit *= raceSlowdown
	}
	// Build the field and pyramid once, as a running localizer would.
	if _, _, err := m.prepare(true); err != nil {
		t.Fatal(err)
	}
	poses := queryPoses
	if raceEnabled {
		poses = poses[:4] // each search takes about a second under the detector
	}
	var slowest time.Duration
	for i, truth := range poses {
		scan := simScan(walls, truth, simRangeMax, rng)
		start := time.Now()
		r, err := m.Global(scan)
		elapsed := time.Since(start)
		slowest = max(slowest, elapsed)
		if err != nil {
			t.Errorf("pose %d: %v (rivals %+v)", i, err, r.Rivals)
			continue
		}
		et, ey := poseError(r.Best.Pose, truth)
		if et > maxGlobalTranslation || ey > maxGlobalYaw {
			t.Errorf("pose %d: error %.3f m, %.2f°: wrong basin or poor refinement (got %+v, truth %+v)", i, et, ey*180/math.Pi, r.Best.Pose, truth)
		}
		if len(r.Rivals) != 0 || r.ScoreRatio != 0 || r.Evaluated == 0 || r.Best.Score < minGoodScore {
			t.Errorf("pose %d: rivals=%d ratio=%.3f evaluated=%d score=%.3f", i, len(r.Rivals), r.ScoreRatio, r.Evaluated, r.Best.Score)
		}
		checkCovariance(t, "global", r.Best, truth)
		if elapsed > limit {
			t.Errorf("pose %d: full-map search took %v, want < %v", i, elapsed, limit)
		}
		if i == 0 {
			again, err := m.Global(scan)
			if err != nil || again.Best != r.Best || again.Evaluated != r.Evaluated {
				t.Errorf("repeat search differs: %+v vs %+v (%v)", again.Best, r.Best, err)
			}
		}
	}
	lo, hi, _ := g.BoundsM()
	t.Logf("slowest full search of the %.0f x %.0f m map: %v", hi.X-lo.X, hi.Z-lo.Z, slowest)
}

func TestSearchWindow(t *testing.T) {
	g, walls := buildingMap(t)
	m := mustMatcher(t, g, Config{})
	truth := queryPoses[2]
	scan := simScan(walls, truth, simRangeMax, rand.New(rand.NewPCG(5, 1)))
	// A prior 2.7 m and 57° off, far outside the basin of Refine.
	prior := geom.Pose2{X: truth.X + 2.0, Z: truth.Z - 1.8, Yaw: truth.Yaw + 1.0}
	if r, err := m.Refine(scan, prior); err == nil {
		if et, _ := poseError(r.Pose, truth); et < maxGlobalTranslation {
			t.Fatalf("the prior is inside the refinement basin; the test would not exercise the search")
		}
	}
	r, err := m.Search(scan, Window{Center: prior, Linear: 2.5, Angular: 1.2})
	if err != nil {
		t.Fatal(err)
	}
	if et, ey := poseError(r.Best.Pose, truth); et > maxGlobalTranslation || ey > maxGlobalYaw {
		t.Errorf("window search error %.3f m, %.2f°", et, ey*180/math.Pi)
	}
	// A window that excludes the true pose must not report a confident match
	// at the true pose's quality.
	away := geom.Pose2{X: truth.X + 4, Z: truth.Z, Yaw: truth.Yaw + 1.5}
	if miss, err := m.Search(scan, Window{Center: away, Linear: 0.5, Angular: 0.2}); err == nil && miss.Best.Score >= r.Best.Score {
		t.Errorf("a window without the true pose scored %.3f ≥ %.3f", miss.Best.Score, r.Best.Score)
	}
	for _, w := range []Window{{Center: prior, Linear: -1}, {Center: prior, Linear: 1, Angular: math.NaN()}, {Center: geom.Pose2{X: math.Inf(1)}}} {
		if _, err := m.Search(scan, w); !errors.Is(err, ErrConfig) {
			t.Errorf("window %+v: err = %v, want ErrConfig", w, err)
		}
	}
}

func TestGlobalReportsAmbiguityInSymmetricRoom(t *testing.T) {
	// A bare 8 x 6 m rectangle looks the same after a half turn about its
	// centre.
	walls := box(0.02, 0.03, 8.02, 6.03)
	centre := geom.Vec2{X: 4.02, Z: 3.03}
	g := surveyMap(t, walls, geom.Vec2{}, geom.Vec2{X: 8, Z: 6}, 2, 5)
	m := mustMatcher(t, g, Config{})
	truth := geom.Pose2{X: 2.3, Z: 1.9, Yaw: 0.6}
	mirror := geom.Pose2{X: 2*centre.X - truth.X, Z: 2*centre.Z - truth.Z, Yaw: geom.WrapAngle(truth.Yaw + math.Pi)}
	scan := simScan(walls, truth, simRangeMax, rand.New(rand.NewPCG(9, 9)))
	r, err := m.Global(scan)
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("err = %v, want ErrAmbiguous (best %+v, rivals %d)", err, r.Best.Pose, len(r.Rivals))
	}
	if len(r.Rivals) != 1 {
		t.Fatalf("rivals = %d, want exactly the mirrored pose", len(r.Rivals))
	}
	if r.ScoreRatio < m.Config().AmbiguityRatio || r.ScoreRatio > 1 || r.ScoreRatio != r.Rivals[0].Score/r.Best.Score {
		t.Errorf("score ratio = %.3f", r.ScoreRatio)
	}
	// The two candidates are the true pose and its mirror image, in either
	// order, and they are reported as distinct.
	a, b := r.Best.Pose, r.Rivals[0].Pose
	if et, _ := poseError(a, truth); et > maxGlobalTranslation {
		a, b = b, a
	}
	for _, c := range []struct{ got, want geom.Pose2 }{{a, truth}, {b, mirror}} {
		if et, ey := poseError(c.got, c.want); et > maxGlobalTranslation || ey > maxGlobalYaw {
			t.Errorf("candidate %+v is %.3f m, %.2f° from %+v", c.got, et, ey*180/math.Pi, c.want)
		}
	}
	if sep := r.Best.Pose.Pos().Dist(r.Rivals[0].Pose.Pos()); sep < m.Config().DistinctTranslation {
		t.Errorf("candidates only %.2f m apart", sep)
	}
	// With a prior that covers one of them the same scan is unambiguous.
	local, err := m.Search(scan, Window{Center: geom.Pose2{X: 2.6, Z: 1.6, Yaw: 0.4}, Linear: 1, Angular: 0.5})
	if err != nil {
		t.Fatalf("windowed search: %v", err)
	}
	if et, ey := poseError(local.Best.Pose, truth); et > maxGlobalTranslation || ey > maxGlobalYaw {
		t.Errorf("windowed search error %.3f m, %.2f°", et, ey*180/math.Pi)
	}
}

func TestCorridorIsReportedDegenerate(t *testing.T) {
	// Two parallel walls 2.4 m apart whose ends are far beyond sensor range:
	// nothing in a scan fixes the position along the corridor.
	walls := []geom.Segment{seg(0, 0.02, 60, 0.02), seg(0, 2.42, 60, 2.42)}
	g, err := grid.New(grid.Config{})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(11, 3))
	for x := 12.0; x <= 48; x += 1.5 {
		pose := geom.Pose2{X: x, Z: 1.2 + 0.3*math.Sin(x), Yaw: rng.Float64() * 6}
		if err := g.InsertScan(pose, simScan(walls, pose, simRangeMax, rng)); err != nil {
			t.Fatal(err)
		}
	}
	m := mustMatcher(t, g, Config{})
	truth := geom.Pose2{X: 30.31, Z: 1.07, Yaw: 0.35}
	scan := simScan(walls, truth, simRangeMax, rng)
	r, err := m.Refine(scan, geom.Pose2{X: truth.X + 0.15, Z: truth.Z + 0.12, Yaw: truth.Yaw - perturbYaw})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Degenerate || r.MinEigen >= m.Config().DegenerateEigen {
		t.Fatalf("corridor not reported degenerate: min eigen %.4f", r.MinEigen)
	}
	if math.Abs(r.Weak[0]) < minWeakAlignment {
		t.Errorf("weak direction %v is not the corridor axis", r.Weak)
	}
	if sx := math.Sqrt(r.Covariance[0]); sx < minUnobservedSigma {
		t.Errorf("1σ along the corridor = %.3f m, want ≥ %.0f m", sx, minUnobservedSigma)
	}
	// Across the corridor and in heading the scan is as good as anywhere.
	if ez, ey := math.Abs(r.Pose.Z-truth.Z), math.Abs(geom.AngleDiff(r.Pose.Yaw, truth.Yaw)); ez > maxRefineTranslation || ey > maxRefineYaw {
		t.Errorf("observed directions: %.4f m across, %.3f°", ez, ey*180/math.Pi)
	}
	if sz, syaw := math.Sqrt(r.Covariance[4]), math.Sqrt(r.Covariance[8]); sz > maxSigmaTranslation || syaw > maxSigmaYaw {
		t.Errorf("1σ across = %.4f m, yaw = %.4f rad", sz, syaw)
	}
	if r.Score < minGoodScore {
		t.Errorf("score %.3f: degeneracy must not be confused with a poor match", r.Score)
	}
	// A search cannot choose between positions along the corridor either.
	found, err := m.Search(scan, Window{Center: geom.Pose2{X: truth.X + 0.7, Z: truth.Z + 0.2, Yaw: truth.Yaw - 0.1}, Linear: 3, Angular: 0.3})
	if !errors.Is(err, ErrAmbiguous) || len(found.Rivals) == 0 {
		t.Errorf("corridor search: err = %v, rivals = %d; want ErrAmbiguous", err, len(found.Rivals))
	}
	if !found.Best.Degenerate {
		t.Error("corridor search result not flagged degenerate")
	}
	// Scan-to-scan sees the same thing: the motion along the corridor is
	// unobserved, the rest is measured. The axis is expressed in the
	// reference scan's frame.
	motion := geom.Pose2{X: 0.2, Z: 0.05, Yaw: 0.04}
	icp, err := ScanToScan(scan, simScan(walls, truth.Compose(motion), simRangeMax, rng), geom.Pose2{}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	axis := geom.Heading(-truth.Yaw)
	across := axis.Perp()
	if !icp.Degenerate || math.Abs(icp.Weak[0]*axis.X+icp.Weak[1]*axis.Z) < minWeakAlignment {
		t.Errorf("scan-to-scan in a corridor: degenerate=%v weak=%v, axis %v", icp.Degenerate, icp.Weak, axis)
	}
	d := icp.Pose.Pos().Sub(motion.Pos())
	if ea, ey := math.Abs(d.Dot(across)), math.Abs(geom.AngleDiff(icp.Pose.Yaw, motion.Yaw)); ea > maxIcpTranslation || ey > maxIcpYaw {
		t.Errorf("scan-to-scan observed directions: %.4f m across, %.3f°", ea, ey*180/math.Pi)
	}
	along := geom.TransformCov2(geom.Pose2{Yaw: truth.Yaw}, geom.PositionCov(icp.Covariance))
	if s := math.Sqrt(along[0]); s < minUnobservedSigma {
		t.Errorf("scan-to-scan 1σ along the corridor = %.3f m, want ≥ %.0f m", s, minUnobservedSigma)
	}
	// With nothing to hold it, the estimate along the corridor must stay at
	// the guess rather than wander with the noise, and the fit must end.
	guess := geom.Pose2{X: 0.1, Z: -0.03, Yaw: 0.01}
	held, err := ScanToScan(scan, simScan(walls, truth.Compose(motion), simRangeMax, rng), guess, Config{})
	if err != nil || !held.Converged || !held.Degenerate {
		t.Fatalf("scan-to-scan in a corridor: err=%v converged=%v degenerate=%v after %d iterations", err, held.Converged, held.Degenerate, held.Iterations)
	}
	if moved := math.Abs(held.Pose.Pos().Sub(guess.Pos()).Dot(axis)); moved > maxUnobservedDrift {
		t.Errorf("scan-to-scan moved %.4f m along the corridor from its guess, want < %.3f m", moved, maxUnobservedDrift)
	}
	if ea := math.Abs(held.Pose.Pos().Sub(motion.Pos()).Dot(across)); ea > maxIcpTranslation {
		t.Errorf("scan-to-scan from a guess: %.4f m across", ea)
	}
	// Each covariance must turn into pose-graph information: next to nothing
	// along the direction the scan cannot observe, full strength across the
	// corridor. (Along the nominal axis the information is not small: the
	// unobserved direction is never exactly an axis.)
	for name, res := range map[string]Result{"refine": r, "scan-to-scan": icp, "scan-to-scan from a guess": held} {
		info, err := res.Covariance.Inverse()
		if err != nil {
			t.Errorf("%s: corridor covariance has no inverse: %v", name, err)
			continue
		}
		vals, _, err := geom.SymEigen(info.Mat())
		if err != nil {
			t.Fatal(err)
		}
		wall := geom.Vec2{Z: 1} // across the corridor in the map frame
		if name != "refine" {
			wall = across // and in the reference scan's frame
		}
		acrossInfo := wall.X*(info[0]*wall.X+info[1]*wall.Z) + wall.Z*(info[3]*wall.X+info[4]*wall.Z)
		if vals[2] > maxUnobservedInformation || vals[2] < 0 || acrossInfo < minObservedInformation {
			t.Errorf("%s: weakest information %.3g (want < %g), across the corridor %.3g (want > %g)", name, vals[2], float64(maxUnobservedInformation), acrossInfo, float64(minObservedInformation))
		}
	}
}

// The covariance must describe the error, not merely bound it: a pose graph
// weighs an edge by it and gates outliers with it. The squared Mahalanobis
// error over (x, z, yaw) averages 3 when calibrated. Achieved: 1.4 for both
// paths, that is 1σ about 1.5 times the real error.
func TestCovarianceIsCalibrated(t *testing.T) {
	g, walls := buildingMap(t)
	m := mustMatcher(t, g, Config{})
	rng := rand.New(rand.NewPCG(99, 7))
	var refine, icp float64
	trials := calibrationTrials
	if raceEnabled {
		trials /= 4 // the race detector slows a match tenfold; the mean is stable from 50 poses
	}
	for trial := 0; trial < trials; trial++ {
		a := randomPose(walls, rng)
		motion := geom.Pose2{X: 0.3 * (rng.Float64()*2 - 1), Z: 0.15 * (rng.Float64()*2 - 1), Yaw: 0.1 * (rng.Float64()*2 - 1)}
		ref, cur := simScan(walls, a, simRangeMax, rng), simScan(walls, a.Compose(motion), simRangeMax, rng)
		for _, c := range []struct {
			sum   *float64
			truth geom.Pose2
			match func() (Result, error)
		}{
			{&refine, a, func() (Result, error) { return m.Refine(ref, perturb(a, rng)) }},
			{&icp, motion, func() (Result, error) { return ScanToScan(ref, cur, geom.Pose2{}, Config{}) }},
		} {
			r, err := c.match()
			if err != nil {
				t.Fatalf("trial %d at %+v: %v", trial, a, err)
			}
			d := [3]float64{r.Pose.X - c.truth.X, r.Pose.Z - c.truth.Z, geom.AngleDiff(r.Pose.Yaw, c.truth.Yaw)}
			m2, err := geom.MahalanobisSq3(d, r.Covariance)
			if err != nil {
				t.Fatal(err)
			}
			*c.sum += m2
		}
	}
	for name, sum := range map[string]float64{"refine": refine, "scan-to-scan": icp} {
		mean := sum / float64(trials)
		t.Logf("%s: mean squared Mahalanobis error %.2f over %d poses (3 is calibrated)", name, mean, trials)
		if mean < minMeanMahalanobis || mean > maxMeanMahalanobis {
			t.Errorf("%s: mean squared Mahalanobis error %.2f, want within [%g, %g]", name, mean, float64(minMeanMahalanobis), float64(maxMeanMahalanobis))
		}
	}
}

func TestScanToScanRecoversRelativeMotion(t *testing.T) {
	walls := building()
	rng := rand.New(rand.NewPCG(13, 2))
	// Motion between consecutive scans of a moving robot, no odometry.
	motions := []geom.Pose2{{X: 0.20, Z: -0.08, Yaw: 0.07}, {X: -0.12, Z: 0.15, Yaw: -0.09}, {X: 0.3, Z: 0.02, Yaw: 0.03}}
	var worstT, worstYaw float64
	for i, a := range queryPoses {
		motion := motions[i%len(motions)]
		b := a.Compose(motion)
		ref, cur := simScan(walls, a, simRangeMax, rng), simScan(walls, b, simRangeMax, rng)
		r, err := ScanToScan(ref, cur, geom.Pose2{}, Config{})
		if err != nil {
			t.Errorf("pose %d: %v", i, err)
			continue
		}
		et, ey := poseError(r.Pose, motion)
		worstT, worstYaw = math.Max(worstT, et), math.Max(worstYaw, ey)
		if et > maxIcpTranslation || ey > maxIcpYaw {
			t.Errorf("pose %d: error %.4f m, %.3f°; want < %.3f m, %.1f°", i, et, ey*180/math.Pi, maxIcpTranslation, maxIcpYaw*180/math.Pi)
		}
		if !r.Converged || r.InlierFraction < minGoodInlier || r.Score < minGoodScore || r.Degenerate {
			t.Errorf("pose %d: converged=%v inliers=%.3f score=%.3f degenerate=%v", i, r.Converged, r.InlierFraction, r.Score, r.Degenerate)
		}
		checkCovariance(t, "icp", r, motion)
	}
	t.Logf("worst scan-to-scan error: %.4f m, %.3f°", worstT, worstYaw*180/math.Pi)
}

func TestScanToScanRefusesInsufficientOverlap(t *testing.T) {
	walls := building()
	rng := rand.New(rand.NewPCG(17, 2))
	// Scans from two different rooms share no surfaces.
	ref, cur := simScan(walls, queryPoses[0], simRangeMax, rng), simScan(walls, queryPoses[5], simRangeMax, rng)
	r, err := ScanToScan(ref, cur, geom.Pose2{}, Config{})
	if !errors.Is(err, ErrNoOverlap) {
		t.Fatalf("err = %v, want ErrNoOverlap (inliers %.2f)", err, r.InlierFraction)
	}
	if r.InlierFraction >= DefaultConfig().MinOverlap || r.Points == 0 {
		t.Errorf("rejected result should describe the failed alignment: %+v", r)
	}
	if _, err := ScanToScanSearch(ref, cur, Window{Linear: 3, Angular: math.Pi}, Config{}); !errors.Is(err, ErrNoMatch) && !errors.Is(err, ErrNoOverlap) && !errors.Is(err, ErrAmbiguous) {
		t.Errorf("search across rooms: err = %v, want a refusal", err)
	}
	// A scan with a handful of returns cannot be matched at all.
	sparse := cur
	sparse.Ranges = append([]float64{}, cur.Ranges...)
	for i := range sparse.Ranges {
		if i%40 != 0 {
			sparse.Ranges[i] = spatial.RangeInvalid
		}
	}
	if _, err := ScanToScan(ref, sparse, geom.Pose2{}, Config{}); !errors.Is(err, ErrInsufficient) {
		t.Errorf("sparse scan: err = %v, want ErrInsufficient", err)
	}
}

func TestScanToScanSearchClosesLoop(t *testing.T) {
	walls := building()
	rng := rand.New(rand.NewPCG(19, 4))
	// The same place revisited from another direction: no usable guess.
	for i, offset := range []geom.Pose2{{X: 0.9, Z: -0.6, Yaw: 2.6}, {X: -0.7, Z: 1.1, Yaw: -1.9}} {
		a := queryPoses[i*3]
		b := a.Compose(offset)
		ref, cur := simScan(walls, a, simRangeMax, rng), simScan(walls, b, simRangeMax, rng)
		if _, err := ScanToScan(ref, cur, geom.Pose2{}, Config{}); err == nil {
			t.Fatalf("case %d: plain ICP from the identity succeeded; the offset is too easy", i)
		}
		r, err := ScanToScanSearch(ref, cur, Window{Linear: 2, Angular: math.Pi}, Config{})
		if err != nil {
			t.Errorf("case %d: %v", i, err)
			continue
		}
		if et, ey := poseError(r.Best.Pose, offset); et > maxIcpTranslation || ey > maxIcpYaw {
			t.Errorf("case %d: error %.4f m, %.3f°", i, et, ey*180/math.Pi)
		}
		checkCovariance(t, "loop", r.Best, offset)
	}
}

func TestMatcherFollowsGridUpdates(t *testing.T) {
	walls := box(0.02, 0.03, 7.02, 5.03)
	walls = append(walls, box(1.1, 1.2, 2.3, 1.9)...)
	g, err := grid.New(grid.Config{})
	if err != nil {
		t.Fatal(err)
	}
	m := mustMatcher(t, g, Config{})
	rng := rand.New(rand.NewPCG(23, 1))
	truth := geom.Pose2{X: 4.4, Z: 2.9, Yaw: -0.8}
	scan := simScan(walls, truth, simRangeMax, rng)
	if _, err := m.Refine(scan, truth); !errors.Is(err, ErrEmptyMap) {
		t.Fatalf("empty map: err = %v", err)
	}
	if _, err := m.Global(scan); !errors.Is(err, ErrEmptyMap) {
		t.Fatalf("empty map search: err = %v", err)
	}
	for _, pose := range []geom.Pose2{{X: 3.5, Z: 2.5}, {X: 5.5, Z: 3.9, Yaw: 2}, {X: 3.1, Z: 4.1, Yaw: 4}} {
		if err := g.InsertScan(pose, simScan(walls, pose, simRangeMax, rng)); err != nil {
			t.Fatal(err)
		}
	}
	r, err := m.Global(scan)
	if err != nil {
		t.Fatalf("after mapping: %v", err)
	}
	if et, ey := poseError(r.Best.Pose, truth); et > maxGlobalTranslation || ey > maxGlobalYaw {
		t.Errorf("after mapping: error %.3f m, %.2f°", et, ey*180/math.Pi)
	}
}

func TestRejectsBadInput(t *testing.T) {
	g, walls := buildingMap(t)
	for name, cfg := range map[string]Config{
		"negative sigma": {Sigma: -1},
		"nan":            {MinScore: math.NaN()},
		"points":         {MinPoints: 50, MaxPoints: 10},
		"depth":          {Depth: 40},
		"ratio":          {AmbiguityRatio: 1.5},
	} {
		if _, err := NewMatcher(g, cfg); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: err = %v, want ErrConfig", name, err)
		}
	}
	if _, err := NewMatcher(nil, Config{}); !errors.Is(err, ErrConfig) {
		t.Errorf("nil grid: err = %v", err)
	}
	m := mustMatcher(t, g, Config{})
	if m.Config() != DefaultConfig() {
		t.Errorf("defaults = %+v", m.Config())
	}
	truth := queryPoses[1]
	scan := simScan(walls, truth, simRangeMax, rand.New(rand.NewPCG(29, 1)))
	if _, err := m.Refine(scan, geom.Pose2{X: math.NaN()}); !errors.Is(err, ErrConfig) {
		t.Errorf("NaN guess: err = %v", err)
	}
	// Outside the map nothing matches, and the result says how badly.
	r, err := m.Refine(scan, geom.Pose2{X: 500, Z: 500})
	if !errors.Is(err, ErrNoMatch) || r.Score >= m.Config().MinScore {
		t.Errorf("off-map guess: err = %v, score %.3f", err, r.Score)
	}
	if sx := math.Sqrt(r.Covariance[0]); sx < minUnobservedSigma {
		t.Errorf("off-map guess reports 1σ = %.3f m", sx)
	}
	empty := scan
	empty.Ranges = make([]float64, len(scan.Ranges))
	for i := range empty.Ranges {
		empty.Ranges[i] = spatial.RangeNoReturn
	}
	if _, err := m.Refine(empty, truth); !errors.Is(err, ErrInsufficient) {
		t.Errorf("scan without returns: err = %v", err)
	}
	if _, err := m.Global(empty); !errors.Is(err, ErrInsufficient) {
		t.Errorf("search without returns: err = %v", err)
	}
	// A scan of a room that is not in this map finds no confident pose.
	elsewhere := simScan(box(0, 0, 3.3, 2.1), geom.Pose2{X: 1.2, Z: 1.1}, simRangeMax, rand.New(rand.NewPCG(31, 1)))
	if found, err := m.Search(elsewhere, Window{Center: truth, Linear: 4, Angular: math.Pi}); err == nil {
		t.Errorf("foreign scan matched at %+v with score %.3f", found.Best.Pose, found.Best.Score)
	}
}

func TestConcurrentMatchesAgree(t *testing.T) {
	g, walls := buildingMap(t)
	m := mustMatcher(t, g, Config{}) // a fresh matcher, so the goroutines race to build its rasters
	truth := queryPoses[3]
	scan := simScan(walls, truth, simRangeMax, rand.New(rand.NewPCG(37, 1)))
	window := Window{Center: geom.Pose2{X: truth.X + 0.4, Z: truth.Z - 0.3, Yaw: truth.Yaw + 0.1}, Linear: 1, Angular: 0.3}
	const workers = 6
	results := make([]SearchResult, workers)
	errs := make([]error, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = m.Search(scan, window)
		}()
	}
	wg.Wait()
	for i := range results {
		if errs[i] != nil || results[i].Best != results[0].Best || results[i].Evaluated != results[0].Evaluated {
			t.Fatalf("worker %d: %+v, %v; worker 0: %+v", i, results[i].Best, errs[i], results[0].Best)
		}
	}
	if et, ey := poseError(results[0].Best.Pose, truth); et > maxGlobalTranslation || ey > maxGlobalYaw {
		t.Errorf("error %.3f m, %.2f°", et, ey*180/math.Pi)
	}
}

func BenchmarkGlobal(b *testing.B) {
	g, walls := buildingMap(b)
	m := mustMatcher(b, g, Config{})
	scan := simScan(walls, queryPoses[0], simRangeMax, rand.New(rand.NewPCG(1, 1)))
	if _, err := m.Global(scan); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := m.Global(scan); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRefine(b *testing.B) {
	g, walls := buildingMap(b)
	m := mustMatcher(b, g, Config{})
	truth := queryPoses[0]
	scan := simScan(walls, truth, simRangeMax, rand.New(rand.NewPCG(1, 1)))
	guess := geom.Pose2{X: truth.X + 0.15, Z: truth.Z - 0.1, Yaw: truth.Yaw + 0.06}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := m.Refine(scan, guess); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkScanToScan(b *testing.B) {
	walls := building()
	rng := rand.New(rand.NewPCG(1, 1))
	a := queryPoses[0]
	ref, cur := simScan(walls, a, simRangeMax, rng), simScan(walls, a.Compose(geom.Pose2{X: 0.2, Z: -0.1, Yaw: 0.06}), simRangeMax, rng)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ScanToScan(ref, cur, geom.Pose2{}, Config{}); err != nil {
			b.Fatal(err)
		}
	}
}

// TestLatticeSearchIsExhaustive checks branch and bound against scoring
// every lattice pose: the pyramid bounds must never prune the best pose, nor
// the best pose outside the neighbourhood of the first.
func TestLatticeSearchIsExhaustive(t *testing.T) {
	g, walls := buildingMap(t)
	cfg := Config{MaxPoints: 90, MinScore: 0.2}
	m := mustMatcher(t, g, cfg)
	cfg = m.Config()
	f, pyr, err := m.prepare(true)
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(41, 1))
	rivals := 0
	for trial, truth := range queryPoses[:3] {
		scan := simScan(walls, truth, simRangeMax, rng)
		_, pts, far, err := m.fieldProblem(f, scan)
		if err != nil {
			t.Fatal(err)
		}
		c := g.CellAt(geom.Vec2{X: truth.X + 0.3, Z: truth.Z - 0.4})
		const half = 24
		lo, hi := grid.Cell{IX: c.IX - half, IZ: c.IZ - half}, grid.Cell{IX: c.IX + half, IZ: c.IZ + half}
		const ratio = 0.5
		l, err := cfg.newLattice(pyr, pts, far, lo, hi, truth.Yaw+0.05, 0.25)
		if err != nil {
			t.Fatal(err)
		}
		found := l.candidates(cfg.MinScore, ratio, 1)
		if len(found) == 0 {
			t.Fatalf("trial %d: nothing found", trial)
		}
		if l.level < 3 {
			t.Fatalf("trial %d: only %d levels searched; the window is too small to test pruning", trial, l.level)
		}
		brute, _ := cfg.newLattice(pyr, pts, far, lo, hi, truth.Yaw+0.05, 0.25)
		var best, rival node
		haveRival := false
		brute.found = found[:1]
		for k := range brute.yaws {
			for j := brute.j0; j <= brute.j1; j++ {
				for i := brute.i0; i <= brute.i1; i++ {
					n := node{brute.score(0, int32(k), i, j), int32(k), i, j}
					if n.score > best.score {
						best = n
					}
					if !brute.excluded(n.k, i, j, 1) && n.score > rival.score {
						rival, haveRival = n, true
					}
				}
			}
		}
		// Ties may resolve to different poses; the score is what is exact.
		if found[0].score != best.score {
			t.Errorf("trial %d: branch and bound found score %d, brute force %d", trial, found[0].score, best.score)
		}
		wantRival := haveRival && float64(rival.score) >= ratio*float64(best.score)
		if wantRival {
			rivals++
		}
		if wantRival != (len(found) == 2) || (wantRival && found[1].score != rival.score) {
			t.Errorf("trial %d: rival %+v, brute force %+v (exists=%v)", trial, found[1:], rival, wantRival)
		}
		if l.evaluated >= brute.evaluated/4 {
			t.Errorf("trial %d: branch and bound scored %d poses, brute force %d: no pruning", trial, l.evaluated, brute.evaluated)
		}
	}
	if rivals == 0 {
		t.Error("no trial had a rival; the second pass was not exercised")
	}
}
