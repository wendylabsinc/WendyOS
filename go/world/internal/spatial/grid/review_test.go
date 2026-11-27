package grid

import (
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// rebuilt is the field of g computed from scratch, bypassing every cache.
func rebuilt(t testing.TB, g *Grid) *Field {
	t.Helper()
	f, err := g.build()
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func sameField(a, b *Field) bool {
	return a.Origin == b.Origin && a.W == b.W && a.H == b.H && a.Pad == b.Pad && a.CellSize == b.CellSize &&
		a.MaxDistance == b.MaxDistance && slices.Equal(a.Dist, b.Dist)
}

// The field a mapper gets after any sequence of updates must be, to the last
// bit, the field of the cells: the incremental path may not drift from a
// rebuild, and no mutation may leave a stale field behind.
func TestFieldFollowsEveryMutation(t *testing.T) {
	const (
		rounds = 300
		// Share of rounds the incremental path must serve: the edits below
		// mostly stay inside the room, where it applies.
		minIncrementalShare = 0.6
	)
	rng := rand.New(rand.NewPCG(21, 4))
	g, walls := roomGrid(t)
	if _, err := g.Field(); err != nil {
		t.Fatal(err)
	}
	inside := func() Cell { return g.CellAt(geom.Vec2{X: 0.3 + 5.4*rng.Float64(), Z: 0.3 + 3.4*rng.Float64()}) }
	incremental := 0
	for round := 0; round < rounds; round++ {
		before, _ := g.Field()
		snapshot := slices.Clone(before.Dist)
		switch kind := rng.IntN(10); {
		case kind < 4: // furniture appears and disappears
			for n := rng.IntN(6) + 1; n > 0; n-- {
				_ = g.Set(inside(), []float64{2, -1, 0, 0.6}[rng.IntN(4)])
			}
		case kind < 6: // evidence trickles in and crosses the threshold
			for n := rng.IntN(8) + 1; n > 0; n-- {
				_ = g.Add(inside(), []float64{0.85, -0.4}[rng.IntN(2)])
			}
		case kind < 8: // another scan of the room
			pose := geom.Pose2{X: 1 + 4*rng.Float64(), Z: 1 + 2*rng.Float64(), Yaw: rng.Float64() * 6}
			if err := g.InsertScan(pose, simScan(walls, pose, 180, 12, 0.02, rng)); err != nil {
				t.Fatal(err)
			}
		case kind < 9: // a peer's evidence
			other := mustGrid(t, Config{})
			for n := 5; n > 0; n-- {
				_ = other.Set(inside(), 3*(rng.Float64()*2-1))
			}
			if err := g.Merge(other); err != nil {
				t.Fatal(err)
			}
		default: // the map grows or shrinks at its edge
			far := g.CellAt(geom.Vec2{X: 6.5 + rng.Float64(), Z: 4.5 + rng.Float64()})
			_ = g.Set(far, []float64{2, 0}[rng.IntN(2)])
		}
		want := rebuilt(t, g)
		if len(g.flips) > 0 {
			if f := g.refresh(before, g.flips); f != nil {
				incremental++
				if !sameField(f, want) {
					t.Fatalf("round %d: incremental field differs from a rebuild after %d changed cells", round, len(g.flips))
				}
			}
		}
		got, err := g.Field()
		if err != nil || !sameField(got, want) {
			t.Fatalf("round %d: Field is not the field of the cells (%v)", round, err)
		}
		if !slices.Equal(before.Dist, snapshot) {
			t.Fatalf("round %d: an earlier field was modified", round)
		}
		if again, _ := g.Field(); again != got {
			t.Fatalf("round %d: field rebuilt without a change", round)
		}
	}
	if float64(incremental) < minIncrementalShare*rounds {
		t.Fatalf("incremental path served %d of %d rounds, want at least %.0f%%", incremental, rounds, 100*minIncrementalShare)
	}
}

// An update that leaves every cell on its side of the Occupied threshold
// must not cost a field: the same *Field comes back.
func TestFieldSurvivesUpdatesThatFlipNothing(t *testing.T) {
	g, _ := roomGrid(t)
	f, _ := g.Field()
	free := g.CellAt(geom.Vec2{X: 3, Z: 2})
	if g.State(free) != Free {
		t.Fatal("test room is not mapped as expected")
	}
	_ = g.Add(free, -0.4)
	_ = g.Add(free, 0.2)
	_ = g.Set(g.CellAt(geom.Vec2{X: 2, Z: 2}), -1.5)
	if again, _ := g.Field(); again != f {
		t.Fatal("field replaced although no cell crossed the Occupied threshold")
	}
	// A clone taken now shares the field and follows its own changes only.
	clone := g.Clone()
	_ = clone.Set(free, 2)
	cf, _ := clone.Field()
	if again, _ := g.Field(); again != f || cf == f || cf.At(free) != 0 || f.At(free) == 0 {
		t.Fatal("a clone's change leaked into the original's field, or was lost")
	}
	// More pending changes than the incremental path remembers still give
	// the right field.
	for i := 0; i < maxPendingFlips+10; i++ {
		_ = g.Set(Cell{int32(20 + i%60), int32(10 + i/60)}, 2)
	}
	if got, _ := g.Field(); !sameField(got, rebuilt(t, g)) {
		t.Fatal("field wrong after a burst of changes")
	}
}

// A grid built by integrating scans with the default sensor model must
// survive the trip through published map tiles without losing a bit, so that
// replicas agree on digests whichever way they got their cells.
func TestTilesRoundTripLossless(t *testing.T) {
	g, _ := roomGrid(t)
	versions := map[string]uint64{"rover-1": 3}
	for _, side := range []int{16, 50, 64, spatial.MaxTileSide} {
		tiles, err := g.Tiles("ground", side, 0, versions)
		if err != nil {
			t.Fatal(err)
		}
		if len(tiles) == 0 {
			t.Fatal("no tiles")
		}
		known := 0
		for i, tile := range tiles {
			if err := tile.Validate(); err != nil {
				t.Fatalf("side %d tile %d: %v", side, i, err)
			}
			if tile.LogOddsScale != TileLogOddsScale || tile.CellSizeM != g.CellSize() || tile.Floor != "ground" || tile.OriginXZ != spatial.TileOrigin(tile.IX, tile.IZ, side, g.CellSize()) {
				t.Fatalf("tile header: %+v", tile)
			}
			if i > 0 && !(tiles[i-1].IZ < tile.IZ || (tiles[i-1].IZ == tile.IZ && tiles[i-1].IX < tile.IX)) {
				t.Fatal("tiles are not ordered by (IZ, IX)")
			}
			for j, b := range tile.Cells {
				if b == 0 {
					continue
				}
				known++
				c := Cell{tile.IX*int32(side) + int32(j%side), tile.IZ*int32(side) + int32(j/side)}
				if ix, iz := TileOf(c, side); ix != tile.IX || iz != tile.IZ {
					t.Fatalf("cell %v is in tile (%d, %d), exported in (%d, %d)", c, ix, iz, tile.IX, tile.IZ)
				}
				if want := g.LogOdds(c); math.Abs(float64(int8(b))*TileLogOddsScale-want) > 1e-9 {
					t.Fatalf("cell %v: tile holds %g, grid %g", c, float64(int8(b))*TileLogOddsScale, want)
				}
			}
		}
		if known != g.Counts().Known {
			t.Fatalf("side %d: tiles hold %d known cells, grid %d", side, known, g.Counts().Known)
		}
		back, err := FromTiles(g.Config(), tiles)
		if err != nil {
			t.Fatal(err)
		}
		if !back.Equal(g) || back.Digest() != g.Digest() {
			t.Fatalf("side %d: grid changed in the round trip", side)
		}
		// Same cells, same tiles: a replica that rebuilt from tiles publishes
		// the digests it received.
		again, err := back.Tiles("ground", side, 0, versions)
		if err != nil || len(again) != len(tiles) {
			t.Fatal(err)
		}
		for i := range tiles {
			if again[i].Digest != tiles[i].Digest {
				t.Fatalf("side %d tile %d: digest differs after the round trip", side, i)
			}
		}
	}
	// One tile addressed directly equals the one Tiles produced.
	tiles, _ := g.Tiles("ground", 64, 0, versions)
	one, err := g.Tile("ground", tiles[0].IX, tiles[0].IZ, 64, 0, versions)
	if err != nil || one.Digest != tiles[0].Digest {
		t.Fatalf("Tile and Tiles disagree: %v", err)
	}
	versions["rover-1"] = 9
	if tiles[0].Versions["rover-1"] != 3 {
		t.Fatal("a tile must own its version vector")
	}
}

// Values off the tile lattice round, saturate and never turn a known cell
// into an unknown one, in either direction.
func TestTilesRoundAndSaturate(t *testing.T) {
	wide := mustGrid(t, Config{Min: -300, Max: 300})
	for c, v := range map[Cell]float64{{0, 0}: 0.01, {1, 0}: -0.02, {2, 0}: 0.03, {3, 0}: 0.12, {4, 0}: -0.13, {5, 0}: 250, {6, 0}: -250, {-1, -1}: 0.5} {
		_ = wide.Set(c, v)
	}
	tiles, err := wide.Tiles("ground", 8, 0, map[string]uint64{"a": 1})
	if err != nil || len(tiles) != 2 || tiles[0].IX != -1 || tiles[0].IZ != -1 || tiles[1].IX != 0 || tiles[1].IZ != 0 {
		t.Fatalf("tiles: %+v, %v", tiles, err)
	}
	got := tiles[1].Cells[:7]
	want := []int8{1, -1, 1, 2, -3, 127, -127}
	for i, w := range want {
		if int8(got[i]) != w {
			t.Fatalf("cell %d exported as %d, want %d", i, int8(got[i]), w)
		}
	}
	if int8(tiles[0].Cells[7*8+7]) != 10 {
		t.Fatalf("cell (−1, −1) belongs at the far corner of tile (−1, −1): %v", tiles[0].Cells)
	}
	// Into a default grid: clamped to [−2, 3.5], known cells stay known, and
	// a zero tile cell clears what was there.
	g := mustGrid(t, Config{})
	_ = g.Set(Cell{7, 7}, 2)
	if _, err := g.Field(); err != nil {
		t.Fatal(err)
	}
	if err := g.SetTile(tiles[1]); err != nil {
		t.Fatal(err)
	}
	for c, w := range map[Cell]float64{{0, 0}: 0.05, {1, 0}: -0.05, {3, 0}: 0.1, {5, 0}: 3.5, {6, 0}: -2, {7, 7}: 0} {
		if got := g.LogOdds(c); math.Abs(got-w) > 1e-9 {
			t.Fatalf("cell %v = %g after SetTile, want %g", c, got, w)
		}
	}
	if f, _ := g.Field(); !sameField(f, rebuilt(t, g)) {
		t.Fatal("field not updated by SetTile")
	}
	// A coarser publisher's scale: a known cell below half a grid step stays known.
	fine := tiles[1]
	fine.LogOddsScale, fine.Cells = 0.004, append([]byte(nil), fine.Cells...)
	fine.Cells[0] = 1
	fine.Seal(0)
	if err := g.SetTile(fine); err != nil || g.LogOdds(Cell{0, 0}) != LogOddsStep {
		t.Fatalf("fine-scale tile: %v, cell %g", err, g.LogOdds(Cell{0, 0}))
	}

	other := mustGrid(t, Config{CellSize: 0.1})
	if err := other.SetTile(tiles[0]); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("tile of another cell size: %v", err)
	}
	stale := tiles[0]
	stale.Cells = append([]byte(nil), stale.Cells...)
	stale.Cells[0] = 9
	if err := g.SetTile(stale); err == nil {
		t.Fatal("tile with a stale digest accepted")
	}
	upper := tiles[0]
	upper.Floor = "upper"
	upper.Seal(3)
	if _, err := FromTiles(Config{}, []spatial.MapTile{tiles[1], upper}); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("tiles of two floors: %v", err)
	}
	if _, err := wide.Tiles("ground", spatial.MaxTileSide+1, 0, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized tile side: %v", err)
	}
	if _, err := wide.Tile("ground", math.MaxInt32, 0, 64, 0, map[string]uint64{"a": 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("tile beyond the map: %v", err)
	}
	if _, err := wide.Tile("ground", 0, 0, 8, 0, nil); err == nil {
		t.Fatal("tile without a version vector exported")
	}
	if empty, err := FromTiles(Config{}, nil); err != nil || empty.Counts().Known != 0 {
		t.Fatalf("no tiles: %v", err)
	}
}

func TestTileOfFloorsTowardMinusInfinity(t *testing.T) {
	for _, tc := range []struct {
		c      Cell
		side   int
		ix, iz int32
	}{{Cell{0, 0}, 64, 0, 0}, {Cell{63, 64}, 64, 0, 1}, {Cell{-1, -64}, 64, -1, -1}, {Cell{-65, 127}, 64, -2, 1}, {Cell{-50, 49}, 50, -1, 0}} {
		if ix, iz := TileOf(tc.c, tc.side); ix != tc.ix || iz != tc.iz {
			t.Errorf("TileOf(%v, %d) = (%d, %d), want (%d, %d)", tc.c, tc.side, ix, iz, tc.ix, tc.iz)
		}
	}
}

// Merge clamps at every step, so its result depends on the order once a
// clamp engages; Sum must not.
func TestSumIsOrderIndependent(t *testing.T) {
	c := Cell{3, -2}
	single := func(v float64) *Grid {
		g := mustGrid(t, Config{Min: -300, Max: 300})
		_ = g.Set(c, v)
		return g
	}
	a, b, neg := single(3), single(3), single(-2)
	// The reviewer's counterexample: (a+b)+c = 1.5 but a+(b+c) = 3.5 under Merge.
	left, right := mustGrid(t, Config{}), mustGrid(t, Config{})
	for _, g := range []*Grid{a, b, neg} {
		_ = left.Merge(g)
	}
	for _, g := range []*Grid{neg, b, a} {
		_ = right.Merge(g)
	}
	if left.LogOdds(c) == right.LogOdds(c) {
		t.Fatal("the example no longer shows Merge's order dependence")
	}
	for _, order := range [][]*Grid{{a, b, neg}, {neg, a, b}, {b, neg, a}} {
		sum, err := Sum(Config{}, order...)
		if err != nil || math.Abs(sum.LogOdds(c)-3.5) > 1e-9 {
			t.Fatalf("Sum = %g (%v), want 3 + 3 − 2 clamped once to 3.5", sum.LogOdds(c), err)
		}
	}
	// Property: any permutation of random saturating grids sums to the same cells.
	rng := rand.New(rand.NewPCG(5, 9))
	for trial := 0; trial < 20; trial++ {
		parts := make([]*Grid, 6)
		for i := range parts {
			parts[i] = randomGrid(t, rng, 4)
		}
		want, err := Sum(Config{}, parts...)
		if err != nil {
			t.Fatal(err)
		}
		rng.Shuffle(len(parts), func(i, j int) { parts[i], parts[j] = parts[j], parts[i] })
		got, err := Sum(Config{}, parts...)
		if err != nil || !got.Equal(want) || got.Digest() != want.Digest() {
			t.Fatalf("trial %d: Sum depends on the order of its parts", trial)
		}
	}
	// Opposite evidence cancels to unknown and leaves no empty chunk behind.
	cancel, err := Sum(Config{}, single(1), single(-1))
	if err != nil || cancel.Known(c) || len(cancel.Encode()) != len(mustGrid(t, Config{}).Encode()) {
		t.Fatalf("cancelled evidence: %v", err)
	}
	if _, err := Sum(Config{}, mustGrid(t, Config{CellSize: 0.1})); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("grid of another cell size: %v", err)
	}
	if empty, err := Sum(Config{}); err != nil || empty.Counts().Known != 0 {
		t.Fatalf("empty sum: %v", err)
	}
}
