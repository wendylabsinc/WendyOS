package grid

import (
	"errors"
	"math"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

const (
	// A wall point must lie in an Occupied cell or one of its 8 neighbours.
	wallCellTolerance = 1
	// Share of interior cells a 720-beam scan must free in a 6x4 m room.
	minInteriorFreeFraction = 0.98
	// Raycast reports the boundary of the occupied cell; with the cell's own
	// quantisation the range may differ from the wall by up to two cells at
	// normal incidence.
	raycastToleranceCells = 2.0
	// The distance transform is exact; float32 storage costs this much.
	fieldTolerance = 1e-5
)

func mustGrid(t testing.TB, cfg Config) *Grid {
	t.Helper()
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func box(x0, z0, x1, z1 float64) []geom.Segment {
	a, b, c, d := geom.Vec2{X: x0, Z: z0}, geom.Vec2{X: x1, Z: z0}, geom.Vec2{X: x1, Z: z1}, geom.Vec2{X: x0, Z: z1}
	return []geom.Segment{{A: a, B: b}, {A: b, B: c}, {A: c, B: d}, {A: d, B: a}}
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

// simScan is a 360° scan of the walls from pose with Gaussian range noise.
func simScan(walls []geom.Segment, pose geom.Pose2, beams int, rangeMax, sigma float64, rng *rand.Rand) spatial.Scan {
	s := spatial.Scan{
		Header:   spatial.Header{Device: "sim", Sensor: "lidar", Time: time.Unix(1, 0).UTC()},
		AngleMin: -math.Pi, AngleIncrement: 2 * math.Pi / float64(beams),
		RangeMin: 0.05, RangeMax: rangeMax, RangeSigma: sigma, Ranges: make([]float64, beams),
	}
	for i := range s.Ranges {
		r, ok := rayRange(walls, pose.Pos(), pose.Yaw+s.Angle(i))
		if ok && sigma > 0 {
			r += rng.NormFloat64() * sigma
		}
		if !ok || r > rangeMax {
			r = spatial.RangeNoReturn
		}
		s.Ranges[i] = r
	}
	return s
}

func roomGrid(t testing.TB) (*Grid, []geom.Segment) {
	t.Helper()
	walls := box(0.02, 0.01, 6.02, 4.01)
	g := mustGrid(t, Config{})
	rng := rand.New(rand.NewPCG(7, 11))
	for _, pose := range []geom.Pose2{{X: 2.1, Z: 1.7, Yaw: 0.3}, {X: 4.4, Z: 2.6, Yaw: -1.2}, {X: 1.2, Z: 3.1, Yaw: 2.0}} {
		if err := g.InsertScan(pose, simScan(walls, pose, 720, 12, 0.01, rng)); err != nil {
			t.Fatal(err)
		}
	}
	return g, walls
}

func TestConfigDefaultsAndValidation(t *testing.T) {
	g := mustGrid(t, Config{})
	if g.Config() != DefaultConfig() {
		t.Fatalf("defaults = %+v", g.Config())
	}
	for name, cfg := range map[string]Config{
		"negative cell": {CellSize: -1},
		"nan":           {Hit: math.NaN()},
		"clamp range":   {Max: 1000},
		"thresholds":    {Occupied: 5},
		"free positive": {Free: 0.2},
		"field":         {FieldDistance: 0.01},
	} {
		if _, err := New(cfg); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: err = %v, want ErrConfig", name, err)
		}
	}
}

func TestCellGeometry(t *testing.T) {
	g := mustGrid(t, Config{CellSize: 0.1})
	for _, tc := range []struct {
		p    geom.Vec2
		want Cell
	}{
		{geom.Vec2{X: 0.05, Z: 0.05}, Cell{0, 0}},
		{geom.Vec2{X: -0.01, Z: 0.19}, Cell{-1, 1}},
		{geom.Vec2{X: 3.21, Z: -3.21}, Cell{32, -33}},
	} {
		if got := g.CellAt(tc.p); got != tc.want {
			t.Errorf("CellAt(%v) = %v, want %v", tc.p, got, tc.want)
		}
		if c := g.Center(tc.want); g.CellAt(c) != tc.want {
			t.Errorf("Center(%v) = %v is outside the cell", tc.want, c)
		}
	}
}

func TestInsertScanClassifiesRoom(t *testing.T) {
	g, walls := roomGrid(t)
	s := g.CellSize()
	for _, w := range walls {
		for d := 0.1; d < w.Length()-0.1; d += 0.2 {
			p := w.A.Add(w.Dir().Scale(d))
			c := g.CellAt(p)
			found := false
			for dz := int32(-wallCellTolerance); dz <= wallCellTolerance; dz++ {
				for dx := int32(-wallCellTolerance); dx <= wallCellTolerance; dx++ {
					found = found || g.State(Cell{c.IX + dx, c.IZ + dz}) == Occupied
				}
			}
			if !found {
				t.Errorf("no occupied cell near wall point %v", p)
			}
		}
	}
	interior, free := 0, 0
	for x := 0.02 + 0.2; x < 6.02-0.2; x += s {
		for z := 0.01 + 0.2; z < 4.01-0.2; z += s {
			p := geom.Vec2{X: x, Z: z}
			interior++
			switch g.StateAt(p) {
			case Free:
				free++
			case Occupied:
				t.Fatalf("interior point %v is occupied", p)
			}
		}
	}
	if frac := float64(free) / float64(interior); frac < minInteriorFreeFraction {
		t.Errorf("interior free fraction %.3f, want ≥ %.2f", frac, minInteriorFreeFraction)
	}
	for _, p := range []geom.Vec2{{X: -0.3, Z: 2}, {X: 6.4, Z: 2}, {X: 3, Z: -0.3}, {X: 3, Z: 4.4}, {X: 20, Z: 20}} {
		if g.Known(g.CellAt(p)) {
			t.Errorf("point %v outside the room is known (%s)", p, g.StateAt(p))
		}
		if got := g.ProbabilityAt(p); got != 0.5 {
			t.Errorf("unknown probability = %v, want 0.5", got)
		}
	}
	n := g.Counts()
	if n.Occupied == 0 || n.Free == 0 || n.Known < n.Occupied+n.Free {
		t.Errorf("counts = %+v", n)
	}
	lo, hi, ok := g.BoundsM()
	if !ok || lo.X > 0.02 || lo.Z > 0.01 || hi.X < 6.02 || hi.Z < 4.01 || lo.X < -0.2 || hi.X > 6.3 {
		t.Errorf("bounds = %v..%v ok=%v", lo, hi, ok)
	}
}

func TestInsertScanUpdatesEachCellOnce(t *testing.T) {
	g := mustGrid(t, Config{})
	pose := geom.Pose2{X: 1.01, Z: 1.02}
	rng := rand.New(rand.NewPCG(1, 2))
	if err := g.InsertScan(pose, simScan(box(0, 0, 2, 2), pose, 720, 10, 0, rng)); err != nil {
		t.Fatal(err)
	}
	// All 720 beams leave through the sensor's own cell.
	if got := g.LogOddsAt(pose.Pos()); math.Abs(got+DefaultConfig().Miss) > 1e-9 {
		t.Errorf("sensor cell log-odds = %v, want %v", got, -DefaultConfig().Miss)
	}
	hits := 0
	g.Each(func(_ Cell, l float64) bool {
		if l > 0 {
			hits++
			if math.Abs(l-DefaultConfig().Hit) > 1e-9 {
				t.Errorf("hit cell log-odds = %v, want %v", l, DefaultConfig().Hit)
			}
		}
		return true
	})
	if hits == 0 {
		t.Fatal("no hit cells")
	}
}

func TestNoReturnAndMaxRangeOnlyClear(t *testing.T) {
	scan := spatial.Scan{AngleMin: 0, AngleIncrement: math.Pi / 2, RangeMin: 0.05, RangeMax: 3, Ranges: []float64{spatial.RangeNoReturn, 2.5, spatial.RangeInvalid, math.NaN()}}
	g := mustGrid(t, Config{MaxRange: 2})
	if err := g.InsertScan(geom.Pose2{X: 0.025, Z: 0.025}, scan); err != nil {
		t.Fatal(err)
	}
	if n := g.Counts(); n.Occupied != 0 || n.Free == 0 {
		t.Fatalf("counts = %+v, want only free cells", n)
	}
	for _, tc := range []struct {
		p    geom.Vec2
		want State
	}{
		{geom.Vec2{X: 1.9, Z: 0.025}, Free},     // no return, cleared to MaxRange
		{geom.Vec2{X: 2.2, Z: 0.025}, Unknown},  // beyond MaxRange
		{geom.Vec2{X: 0.025, Z: 1.9}, Free},     // return beyond MaxRange only clears
		{geom.Vec2{X: 0.025, Z: 2.5}, Unknown},  // its endpoint is not marked
		{geom.Vec2{X: -1.0, Z: 0.025}, Unknown}, // invalid beam
		{geom.Vec2{X: 0.025, Z: -1.0}, Unknown}, // NaN beam
	} {
		if got := g.StateAt(tc.p); got != tc.want {
			t.Errorf("state at %v = %s, want %s", tc.p, got, tc.want)
		}
	}
	if err := g.InsertScan(geom.Pose2{X: math.NaN()}, scan); !errors.Is(err, ErrInvalid) {
		t.Errorf("NaN pose: err = %v", err)
	}
	huge := mustGrid(t, Config{CellSize: 0.001})
	scan.RangeMax = 60
	if err := huge.InsertScan(geom.Pose2{}, scan); !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversized footprint: err = %v", err)
	}
}

func TestClampAndThresholds(t *testing.T) {
	g := mustGrid(t, Config{})
	c := Cell{3, -4}
	for i := 0; i < 20; i++ {
		if err := g.Add(c, 0.85); err != nil {
			t.Fatal(err)
		}
	}
	if got := g.LogOdds(c); got != 3.5 {
		t.Errorf("clamped high = %v, want 3.5", got)
	}
	if p := g.Probability(c); p < 0.97 || p >= 1 {
		t.Errorf("probability = %v", p)
	}
	for i := 0; i < 40; i++ {
		_ = g.Add(c, -0.4)
	}
	if got := g.LogOdds(c); got != -2 {
		t.Errorf("clamped low = %v, want -2", got)
	}
	_ = g.Set(c, 0.45)
	if g.State(c) != Unknown || !g.Known(c) {
		t.Errorf("0.45 should be known but unclassified, got %s", g.State(c))
	}
	_ = g.Set(c, 0.5)
	if g.State(c) != Occupied {
		t.Errorf("0.5 should be occupied")
	}
	_ = g.Set(c, -0.3)
	if g.State(c) != Free {
		t.Errorf("-0.3 should be free")
	}
	_ = g.Set(c, 0)
	if g.Known(c) || g.Counts().Known != 0 {
		t.Errorf("zero should be unknown")
	}
	if err := g.Set(c, math.Inf(1)); !errors.Is(err, ErrInvalid) {
		t.Errorf("Set(+Inf) err = %v", err)
	}
}

func TestSquaredDistanceMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 9))
	for trial := 0; trial < 20; trial++ {
		w, h := 1+rng.IntN(40), 1+rng.IntN(40)
		site := make([]bool, w*h)
		var sites [][2]int
		for i := range site {
			if rng.Float64() < 0.03*float64(trial%4) {
				site[i] = true
				sites = append(sites, [2]int{i % w, i / w})
			}
		}
		got := SquaredDistance(site, w, h)
		for i, d := range got {
			want := math.Inf(1)
			for _, s := range sites {
				dx, dz := float64(i%w-s[0]), float64(i/w-s[1])
				want = math.Min(want, dx*dx+dz*dz)
			}
			if d != want {
				t.Fatalf("trial %d %dx%d cell %d: got %v, want %v", trial, w, h, i, d, want)
			}
		}
	}
}

func TestFieldLazyInvalidatedAndInterpolated(t *testing.T) {
	g := mustGrid(t, Config{})
	empty, err := g.Field()
	if err != nil || empty.W != 0 || empty.Distance(geom.Vec2{X: 1, Z: 1}) != g.Config().FieldDistance {
		t.Fatalf("empty field = %+v, %v", empty, err)
	}
	// A wall along z at ix = 10.
	for iz := int32(-40); iz <= 40; iz++ {
		_ = g.Set(Cell{10, iz}, 2)
	}
	f1, err := g.Field()
	if err != nil || f1 == empty {
		t.Fatalf("field not rebuilt after an update: %v", err)
	}
	if f2, _ := g.Field(); f2 != f1 {
		t.Fatal("field rebuilt without an update")
	}
	s := g.CellSize()
	for dx := int32(-25); dx <= 25; dx++ {
		want := math.Min(math.Abs(float64(dx))*s, f1.MaxDistance)
		if got := f1.At(Cell{10 + dx, 0}); math.Abs(got-want) > fieldTolerance {
			t.Errorf("At(dx=%d) = %v, want %v", dx, got, want)
		}
	}
	// Between cell centres the field is linear and its gradient points away
	// from the wall.
	wallX := g.Center(Cell{10, 0}).X
	for _, off := range []float64{-0.31, -0.07, 0.012, 0.2, 0.633} {
		d, grad := f1.Sample(geom.Vec2{X: wallX + off, Z: 0.013})
		if math.Abs(d-math.Abs(off)) > fieldTolerance {
			t.Errorf("Sample(off=%v) = %v", off, d)
		}
		if math.Abs(grad.X-math.Copysign(1, off)) > 1e-4 || math.Abs(grad.Z) > 1e-4 {
			t.Errorf("gradient(off=%v) = %v", off, grad)
		}
	}
	if d, grad := f1.Sample(geom.Vec2{X: wallX + 5, Z: 0}); d != f1.MaxDistance || grad != (geom.Vec2{}) {
		t.Errorf("far sample = %v %v", d, grad)
	}
	// Clearing one wall cell must change the field.
	_ = g.Set(Cell{10, 0}, -1)
	f3, _ := g.Field()
	if f3 == f1 || math.Abs(f3.At(Cell{10, 0})-s) > fieldTolerance {
		t.Errorf("stale field after clearing a cell: %v", f3.At(Cell{10, 0}))
	}
	if f1.At(Cell{10, 0}) != 0 {
		t.Error("an earlier snapshot was modified")
	}
}

func TestFieldMatchesBruteForceOnRoom(t *testing.T) {
	g, _ := roomGrid(t)
	f, err := g.Field()
	if err != nil {
		t.Fatal(err)
	}
	var occ []geom.Vec2
	g.Each(func(c Cell, _ float64) bool {
		if g.State(c) == Occupied {
			occ = append(occ, g.Center(c))
		}
		return true
	})
	rng := rand.New(rand.NewPCG(5, 5))
	for i := 0; i < 300; i++ {
		c := Cell{f.Origin.IX + int32(rng.IntN(f.W)), f.Origin.IZ + int32(rng.IntN(f.H))}
		want := f.MaxDistance
		for _, o := range occ {
			want = math.Min(want, o.Dist(g.Center(c)))
		}
		if got := f.At(c); math.Abs(got-want) > fieldTolerance {
			t.Fatalf("At(%v) = %v, want %v", c, got, want)
		}
	}
}

func TestRaycastMatchesGeometry(t *testing.T) {
	// Rasterise the walls directly so the map has no gaps and the test is
	// about the traversal alone.
	walls := box(0.02, 0.01, 6.02, 4.01)
	g := mustGrid(t, Config{})
	for _, w := range walls {
		for d := 0.0; d <= w.Length(); d += 0.01 {
			_ = g.Set(g.CellAt(w.A.Add(w.Dir().Scale(d))), 2)
		}
	}
	origin := geom.Vec2{X: 3.3, Z: 1.1}
	for i := 0; i < 360; i++ {
		yaw := float64(i) * 2 * math.Pi / 360
		want, cosIncidence := math.Inf(1), 1.0
		for _, w := range walls {
			if r, ok := geom.RaySegment(origin, geom.Heading(yaw), w); ok && r < want {
				want, cosIncidence = r, math.Abs(geom.Heading(yaw).Dot(w.Dir().Perp()))
			}
		}
		// A one-cell-thick wall is entered earlier the more obliquely the
		// ray meets it.
		tol := raycastToleranceCells * g.CellSize() / cosIncidence
		got, c, hit := g.Raycast(origin, yaw, 20)
		if !hit || math.Abs(got-want) > tol {
			t.Errorf("yaw %.2f: range %v hit=%v, want %v ± %v", yaw, got, hit, want, tol)
		}
		if hit && (g.State(c) != Occupied || math.Abs(g.Center(c).Dist(origin)-got) > g.CellSize()) {
			t.Errorf("yaw %.2f: reported cell %v (%s) is not at range %v", yaw, c, g.State(c), got)
		}
	}
	if r, _, hit := g.Raycast(origin, 0, 1.0); hit || r != 1.0 {
		t.Errorf("short ray = %v hit=%v", r, hit)
	}
	if r, c, hit := g.Raycast(g.Center(Cell{0, 0}), 1, 5); !hit || r != 0 || c != (Cell{0, 0}) {
		t.Errorf("ray starting in an occupied cell = %v %v %v", r, c, hit)
	}
	if !g.LineOfSight(origin, geom.Vec2{X: 5.5, Z: 3.5}) {
		t.Error("no line of sight inside the room")
	}
	if g.LineOfSight(origin, geom.Vec2{X: 8, Z: 1.1}) {
		t.Error("line of sight through a wall")
	}
}

func TestWalkVisitsTheCrossedCells(t *testing.T) {
	rng := rand.New(rand.NewPCG(17, 4))
	for trial := 0; trial < 500; trial++ {
		ax, az := rng.Float64()*40-20, rng.Float64()*40-20
		bx, bz := rng.Float64()*40-20, rng.Float64()*40-20
		if trial%10 == 0 {
			bz = az // axis-aligned
		}
		seg := geom.Segment{A: geom.Vec2{X: ax, Z: az}, B: geom.Vec2{X: bx, Z: bz}}
		ex, ez := cellIndex(bx), cellIndex(bz)
		px, pz := cellIndex(ax), cellIndex(az)
		want := abs32(ex-px) + abs32(ez-pz)
		n := int32(0)
		walk(ax, az, bx, bz, func(ix, iz int32) {
			if n == 0 && (ix != px || iz != pz) {
				t.Fatalf("trial %d: first cell (%d,%d), want the start cell", trial, ix, iz)
			}
			if n > 0 && abs32(ix-px)+abs32(iz-pz) != 1 {
				t.Fatalf("trial %d: jump from (%d,%d) to (%d,%d)", trial, px, pz, ix, iz)
			}
			// The segment passes within half a cell diagonal of the centre.
			if d := seg.DistanceTo(geom.Vec2{X: float64(ix) + 0.5, Z: float64(iz) + 0.5}); d > math.Sqrt2/2+1e-9 {
				t.Fatalf("trial %d: cell (%d,%d) is %v cells from the segment", trial, ix, iz, d)
			}
			px, pz = ix, iz
			n++
		})
		if n != want {
			t.Fatalf("trial %d: visited %d cells, want %d", trial, n, want)
		}
		if n > 0 && abs32(ex-px)+abs32(ez-pz) != 1 {
			t.Fatalf("trial %d: walk stopped at (%d,%d), not next to the end cell (%d,%d)", trial, px, pz, ex, ez)
		}
	}
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

// randomGrid fills cells in a small area with log-odds up to ±limit.
func randomGrid(t testing.TB, rng *rand.Rand, limit float64) *Grid {
	g := mustGrid(t, Config{})
	for i := 0; i < 400; i++ {
		c := Cell{int32(rng.IntN(120) - 60), int32(rng.IntN(120) - 60)}
		_ = g.Set(c, (rng.Float64()*2-1)*limit)
	}
	return g
}

func merged(t testing.TB, grids ...*Grid) *Grid {
	t.Helper()
	out := grids[0].Clone()
	for _, g := range grids[1:] {
		if err := out.Merge(g); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestMergeIsCommutativeAndAssociativeUpToClamping(t *testing.T) {
	rng := rand.New(rand.NewPCG(21, 22))
	for trial := 0; trial < 30; trial++ {
		// ±0.6 keeps every three-way sum inside the clamp [−2, 3.5].
		a, b, c := randomGrid(t, rng, 0.6), randomGrid(t, rng, 0.6), randomGrid(t, rng, 0.6)
		ab, ba := merged(t, a, b), merged(t, b, a)
		if !ab.Equal(ba) || ab.Digest() != ba.Digest() {
			t.Fatalf("trial %d: merge is not commutative", trial)
		}
		left, right := merged(t, ab, c), merged(t, a, merged(t, b, c))
		if !left.Equal(right) || left.Digest() != right.Digest() {
			t.Fatalf("trial %d: merge is not associative", trial)
		}
		if string(left.Encode()) != string(right.Encode()) {
			t.Fatalf("trial %d: equal grids encode differently", trial)
		}
		// Once sums reach the clamp only commutativity is promised.
		x, y := randomGrid(t, rng, 3), randomGrid(t, rng, 3)
		xy, yx := merged(t, x, y), merged(t, y, x)
		if !xy.Equal(yx) {
			t.Fatalf("trial %d: clamped merge is not commutative", trial)
		}
		xy.Each(func(cell Cell, l float64) bool {
			if l < -2 || l > 3.5 {
				t.Fatalf("trial %d: merged cell %v = %v escapes the clamp", trial, cell, l)
			}
			want := math.Max(-2, math.Min(3.5, x.LogOdds(cell)+y.LogOdds(cell)))
			if math.Abs(l-want) > 1e-9 {
				t.Fatalf("trial %d: merged cell %v = %v, want %v", trial, cell, l, want)
			}
			return true
		})
	}
	other := mustGrid(t, Config{CellSize: 0.1})
	if err := mustGrid(t, Config{}).Merge(other); !errors.Is(err, ErrIncompatible) {
		t.Errorf("merge across cell sizes: err = %v", err)
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	g, _ := roomGrid(t)
	// A chunk that became entirely unknown must not affect the encoding.
	_ = g.Set(Cell{5000, 5000}, 1)
	before := g.Digest()
	_ = g.Set(Cell{5000, 5000}, 0)
	data := g.Encode()
	back, err := Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if !back.Equal(g) || back.Digest() != g.Digest() || back.Config() != g.Config() || back.Counts() != g.Counts() {
		t.Fatal("decoded grid differs")
	}
	if string(back.Encode()) != string(data) {
		t.Fatal("re-encoding is not stable")
	}
	if before == g.Digest() {
		t.Error("digest ignored a cell change")
	}
	raw := g.Counts().Known * 2
	if len(data) > raw/4 {
		t.Errorf("encoding is %d bytes for %d known cells; run-length coding should beat %d", len(data), g.Counts().Known, raw/4)
	}
	// A different update model with the same cells shares the digest.
	other := mustGrid(t, Config{Hit: 1.2})
	if err := other.Merge(g); err != nil {
		t.Fatal(err)
	}
	if other.Digest() != g.Digest() {
		t.Error("digest depends on update parameters")
	}
	emptyBack, err := Decode(mustGrid(t, Config{CellSize: 0.1}).Encode())
	if err != nil || emptyBack.CellSize() != 0.1 || emptyBack.Counts().Known != 0 {
		t.Errorf("empty round trip: %v", err)
	}
}

func TestDecodeRejectsMalformedInput(t *testing.T) {
	g := mustGrid(t, Config{})
	_ = g.Set(Cell{1, 1}, 1)
	_ = g.Set(Cell{40, 1}, -1)
	data := g.Encode()
	for n := 0; n < len(data); n++ {
		if _, err := Decode(data[:n]); !errors.Is(err, ErrFormat) {
			t.Fatalf("truncated to %d bytes: err = %v", n, err)
		}
	}
	if _, err := Decode(append(append([]byte{}, data...), 0)); !errors.Is(err, ErrFormat) {
		t.Errorf("trailing byte: err = %v", err)
	}
	bad := append([]byte{}, data...)
	bad[0] = 'X'
	if _, err := Decode(bad); !errors.Is(err, ErrFormat) {
		t.Errorf("bad magic: err = %v", err)
	}
	// No single-byte corruption may panic, and whatever decodes must
	// re-encode to exactly the bytes it came from.
	for i := range data {
		for _, flip := range []byte{0x01, 0x80, 0xff} {
			mut := append([]byte{}, data...)
			mut[i] ^= flip
			if back, err := Decode(mut); err == nil && string(back.Encode()) != string(mut) {
				t.Fatalf("byte %d ^ %#x decoded to a grid that encodes differently", i, flip)
			}
		}
	}
}

func TestPyramidBoundsShiftedSums(t *testing.T) {
	g, _ := roomGrid(t)
	f, err := g.Field()
	if err != nil {
		t.Fatal(err)
	}
	const levels = 5
	p, err := f.Pyramid(0.1, levels)
	if err != nil {
		t.Fatal(err)
	}
	// Level 0 is the kernel of the field distance.
	rng := rand.New(rand.NewPCG(8, 8))
	for i := 0; i < 500; i++ {
		c := Cell{p.Origin.IX + int32(rng.IntN(p.W+20)) - 10, p.Origin.IZ + int32(rng.IntN(p.H+20)) - 10}
		want := uint8(0)
		if d := f.At(c); d < f.MaxDistance {
			want = uint8(math.Round(255 * math.Exp(-d*d/(2*0.1*0.1))))
		}
		if got := p.At(0, c); got != want {
			t.Fatalf("level 0 at %v = %d, want %d", c, got, want)
		}
		// Every level is the block maximum of level 0, including blocks that
		// start before the raster or run off its end.
		for h := 1; h < levels; h++ {
			block := uint8(0)
			for dz := int32(0); dz < 1<<h; dz++ {
				for dx := int32(0); dx < 1<<h; dx++ {
					block = max(block, p.At(0, Cell{c.IX + dx, c.IZ + dz}))
				}
			}
			if uint(int(c.IX)-int(p.Origin.IX)) >= uint(p.W) || uint(int(c.IZ)-int(p.Origin.IZ)) >= uint(p.H) {
				if block != 0 {
					t.Fatalf("level %d block at %v outside the raster sees score %d: padding too small", h, c, block)
				}
				continue
			}
			if got := p.At(h, c); got != block {
				t.Fatalf("level %d at %v = %d, want block max %d", h, c, got, block)
			}
		}
	}
	if _, err := f.Pyramid(0, 3); !errors.Is(err, ErrInvalid) {
		t.Errorf("zero sigma: err = %v", err)
	}
	if _, err := f.Pyramid(0.1, MaxPyramidLevels+1); !errors.Is(err, ErrInvalid) {
		t.Errorf("too many levels: err = %v", err)
	}
}

func TestEachOrderIsDeterministic(t *testing.T) {
	rng := rand.New(rand.NewPCG(4, 4))
	g := randomGrid(t, rng, 2)
	collect := func(g *Grid) []Cell {
		var out []Cell
		g.Each(func(c Cell, l float64) bool {
			if l == 0 || l != g.LogOdds(c) {
				t.Fatalf("Each reported %v = %v, LogOdds says %v", c, l, g.LogOdds(c))
			}
			out = append(out, c)
			return true
		})
		return out
	}
	first := collect(g)
	if len(first) != g.Counts().Known || len(first) == 0 {
		t.Fatalf("Each visited %d cells, Counts says %d", len(first), g.Counts().Known)
	}
	// A grid holding the same cells, inserted in another order, iterates
	// identically.
	other := mustGrid(t, Config{})
	for i := len(first) - 1; i >= 0; i-- {
		_ = other.Set(first[i], g.LogOdds(first[i]))
	}
	second := collect(other)
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("iteration order differs at %d: %v vs %v", i, first[i], second[i])
		}
	}
	lo, hi, ok := g.Bounds()
	for _, c := range first {
		if !ok || c.IX < lo.IX || c.IX > hi.IX || c.IZ < lo.IZ || c.IZ > hi.IZ {
			t.Fatalf("cell %v outside bounds %v..%v", c, lo, hi)
		}
	}
	stopped := 0
	g.Each(func(Cell, float64) bool { stopped++; return stopped < 3 })
	if stopped != 3 {
		t.Errorf("Each ignored the stop signal: %d calls", stopped)
	}
	if _, _, ok := mustGrid(t, Config{}).Bounds(); ok {
		t.Error("empty grid reports bounds")
	}
}

func TestDiffAndClone(t *testing.T) {
	rng := rand.New(rand.NewPCG(6, 6))
	a := randomGrid(t, rng, 2)
	b := a.Clone()
	if d, err := a.Diff(b); err != nil || len(d) != 0 {
		t.Fatalf("clone differs: %v %v", d, err)
	}
	_ = b.Set(Cell{-7, 3}, 1.5)   // changed or new in b
	_ = b.Set(Cell{900, 900}, -1) // chunk only in b
	_ = a.Set(Cell{-900, 2}, 2)   // chunk only in a
	d, err := a.Diff(b)
	if err != nil || len(d) != 3 {
		t.Fatalf("diff = %v, %v", d, err)
	}
	for i, x := range d {
		if x.From != a.LogOdds(x.Cell) || x.To != b.LogOdds(x.Cell) || x.From == x.To {
			t.Errorf("delta %+v does not match the grids", x)
		}
		if i > 0 && (d[i-1].Cell.IZ > x.Cell.IZ || (d[i-1].Cell.IZ == x.Cell.IZ && d[i-1].Cell.IX >= x.Cell.IX)) {
			t.Errorf("diff is not ordered: %v before %v", d[i-1].Cell, x.Cell)
		}
	}
	if a.Known(Cell{900, 900}) {
		t.Error("clone shares storage with its source")
	}
	if _, err := a.Diff(mustGrid(t, Config{CellSize: 1})); !errors.Is(err, ErrIncompatible) {
		t.Errorf("diff across cell sizes: err = %v", err)
	}
}

func TestConcurrentReaders(t *testing.T) {
	g, _ := roomGrid(t)
	want := g.Counts()
	fields := make([]*Field, 8)
	var wg sync.WaitGroup
	for i := range fields {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, err := g.Field() // all race to build the one cached field
			if err != nil {
				t.Error(err)
			}
			fields[i] = f
			if n := g.Counts(); n != want {
				t.Errorf("counts = %+v, want %+v", n, want)
			}
			if _, _, hit := g.Raycast(geom.Vec2{X: 3, Z: 2}, float64(i), 20); !hit {
				t.Error("ray escaped the room")
			}
			g.Each(func(Cell, float64) bool { return true })
			_ = g.Clone().Digest()
		}()
	}
	wg.Wait()
	for _, f := range fields {
		if f != fields[0] {
			t.Fatal("concurrent callers built different fields")
		}
	}
}

func BenchmarkInsertScan(b *testing.B) {
	walls := box(0.02, 0.01, 12.02, 9.01)
	g := mustGrid(b, Config{})
	pose := geom.Pose2{X: 5.1, Z: 4.2, Yaw: 0.4}
	scan := simScan(walls, pose, 360, 12, 0.015, rand.New(rand.NewPCG(1, 1)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := g.InsertScan(pose, scan); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkField is a full build of the field.
func BenchmarkField(b *testing.B) {
	g, _ := roomGrid(b)
	for i := 0; i < b.N; i++ {
		if _, err := g.build(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkFieldRefresh is the field update after one cell flips: what a
// match-insert loop pays per changed cell.
func BenchmarkFieldRefresh(b *testing.B) {
	g, _ := roomGrid(b)
	c := g.CellAt(geom.Vec2{X: 3, Z: 2})
	for i := 0; i < b.N; i++ {
		_ = g.Set(c, float64(2-3*(i%2))) // occupied, free, occupied, ...
		if _, err := g.Field(); err != nil {
			b.Fatal(err)
		}
	}
}
