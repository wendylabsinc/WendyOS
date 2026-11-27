package query

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
)

const (
	// perfEntities and targetAnswerMS are the design target: 10,000
	// entities answered in under 10 ms.
	perfEntities   = 10_000
	targetAnswerMS = 10.0
	// The floor of the performance world: a hall of 80 m by 50 m cut into
	// 20 m by 25 m rooms, mapped at 0.1 m (400,000 cells, about the size of
	// the Office ground floor at 0.05 m).
	perfW, perfH, perfRoomW, perfRoomH, perfCellM = 80.0, 50.0, 20.0, 25.0, 0.1
)

// perfWorld is 10,000 rows spread evenly over one floor with eight rooms,
// walls with a doorway in each, and a map.
func perfWorld(t testing.TB) World {
	t.Helper()
	rng := rand.New(rand.NewPCG(7, 0xbe7c4))
	g, err := grid.New(grid.Config{CellSize: perfCellM})
	if err != nil {
		t.Fatal(err)
	}
	fill(t, g, 0, 0, perfW, perfH, free)
	for x := perfRoomW; x < perfW; x += perfRoomW { // walls between rooms, a 2 m doorway in the middle of each
		for z := 0.0; z < perfH; z += perfRoomH {
			fill(t, g, x, z, x+perfCellM, z+perfRoomH/2-1, occupied)
			fill(t, g, x, z+perfRoomH/2+1, x+perfCellM, z+perfRoomH, occupied)
		}
	}
	for x := 0.0; x < perfW; x += perfRoomW {
		fill(t, g, x, perfRoomH, x+perfRoomW/2-1, perfRoomH+perfCellM, occupied)
		fill(t, g, x+perfRoomW/2+1, perfRoomH, x+perfRoomW, perfRoomH+perfCellM, occupied)
	}
	w := World{Grids: map[spatial.FloorID]*grid.Grid{"f": g}}
	for i := 0; i < perfEntities; i++ {
		e := row(pick(rng, spatial.EntityObject, spatial.EntityObject, spatial.EntityTrack, spatial.EntityDevice), "e"+strconv.Itoa(i), "f", rng.Float64()*perfW, rng.Float64()*perfH)
		e.Class = pick(rng, "chair", "desk", "shelf", "person", "robot")
		e.Extent.RadiusM, e.UncertaintyM = 0.2, 0.05
		e.ObservedAt = epoch.Add(time.Duration(rng.IntN(600)) * time.Second)
		w.Entities = append(w.Entities, e)
	}
	for i := 0; i < 8; i++ {
		x, z := float64(i%4)*perfRoomW, float64(i/4)*perfRoomH
		r := row(spatial.EntityRegion, "room-"+strconv.Itoa(i), "f", x+perfRoomW/2, z+perfRoomH/2)
		r.Class, r.Label, r.Extent.Polygon = "region", pick(rng, "office", "lab"), box(x, z, x+perfRoomW, z+perfRoomH)
		w.Entities = append(w.Entities, r)
	}
	return w
}

// perfKind is one kind of query, drawn at a random place. The indexed kinds
// are those the uniform-grid index narrows. The others read every row (scan)
// or cast one ray per candidate (sight) and are held to the same target, or
// flood the occupancy map from the origin (path), which costs by the area of
// the map rather than by the number of rows and is measured for the record.
type perfKind struct {
	name    string
	indexed bool // the index must narrow it
	timed   bool // held to targetAnswerMS
	query   func(rng *rand.Rand) Query
}

func perfKinds() []perfKind {
	at := func(rng *rand.Rand) *Ref {
		return PointRef(geom.Vec2{X: rng.Float64() * perfW, Z: rng.Float64() * perfH}, "f")
	}
	someone := func(rng *rand.Rand) Ref { return *EntityRef("e" + strconv.Itoa(rng.IntN(perfEntities))) }
	return []perfKind{
		{"nearest", true, true, func(rng *rand.Rand) Query { return Query{Origin: at(rng), Nearest: 10} }},
		{"nearest_filtered", true, true, func(rng *rand.Rand) Query {
			return Query{Origin: at(rng), Nearest: 3, WithinM: 5, Kinds: []spatial.EntityKind{spatial.EntityObject}, Classes: []string{"chair"}}
		}},
		{"within", true, true, func(rng *rand.Rand) Query { return Query{Origin: at(rng), WithinM: 5} }},
		{"box", true, true, func(rng *rand.Rand) Query {
			p := at(rng).Point
			return Query{Box: &Box{Min: *p, Max: p.Add(geom.Vec2{X: 8, Z: 6})}}
		}},
		{"polygon", true, true, func(rng *rand.Rand) Query {
			p := *at(rng).Point
			return Query{Polygon: []geom.Vec2{p, p.Add(geom.Vec2{X: 9, Z: 1}), p.Add(geom.Vec2{X: 6, Z: 8}), p.Add(geom.Vec2{X: -2, Z: 5})}}
		}},
		{"region", true, true, func(rng *rand.Rand) Query { return Query{Region: "room-" + strconv.Itoa(rng.IntN(8))} }},
		{"near", true, true, func(rng *rand.Rand) Query {
			return Query{Relations: []Relation{{Kind: RelNear, To: someone(rng), DistanceM: 4}}}
		}},
		{"inside", true, true, func(rng *rand.Rand) Query {
			return Query{Kinds: []spatial.EntityKind{spatial.EntityTrack}, Relations: []Relation{{Kind: RelInside, To: *EntityRef("room-" + strconv.Itoa(rng.IntN(8)))}}}
		}},
		{"same_region", true, true, func(rng *rand.Rand) Query {
			return Query{Classes: []string{"robot"}, Relations: []Relation{{Kind: RelSameRegion, To: someone(rng)}}}
		}},
		{"scan_time_limit", false, true, func(rng *rand.Rand) Query {
			return Query{SeenSince: epoch.Add(time.Duration(rng.IntN(600)) * time.Second), Order: OrderTime, Descending: true, Limit: 20}
		}},
		{"scan_everything", false, true, func(*rand.Rand) Query { return Query{} }},
		{"sight_nearest", false, true, func(rng *rand.Rand) Query { return Query{Origin: at(rng), Nearest: 5, Visible: true} }},
		{"sight_within", false, true, func(rng *rand.Rand) Query { return Query{Origin: at(rng), WithinM: 10, Visible: true} }},
		{"path_within", false, false, func(rng *rand.Rand) Query { return Query{Origin: at(rng), PathWithinM: 10} }},
		{"path_order", false, false, func(rng *rand.Rand) Query { return Query{Origin: at(rng), Nearest: 20, Order: OrderPath} }},
	}
}

// TestTenThousandEntities is the design target "10,000 entities answered in
// under 10 ms" for every kind of query the index serves, and for scans and
// sight as well. The figure for a kind is the slowest of perfQueries
// different queries, each taken as the best of perfRepeats runs (the machine
// is shared). No time is asserted under the race detector.
func TestTenThousandEntities(t *testing.T) {
	w := perfWorld(t)
	start := time.Now()
	ix, err := New(w, Config{})
	if err != nil {
		t.Fatal(err)
	}
	built := time.Since(start)
	start = time.Now()
	if _, err := ix.With(w.Entities); err != nil {
		t.Fatal(err)
	}
	t.Logf("METRIC index_build=%.2f ms (no target) for %d rows with New, %.2f ms with With", ms(built), ix.Len(), ms(time.Since(start)))
	// The maps derived from the grid are built on first use; that is not
	// part of answering.
	if _, err := ix.Run(Query{Origin: PointRef(geom.Vec2{X: 1, Z: 1}, "f"), Visible: true, PathWithinM: 1}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range perfKinds() {
		rng := rand.New(rand.NewPCG(11, 0xbe7c4))
		var times []time.Duration
		rows, examined := 0, 0
		for i := 0; i < perfQueries; i++ {
			q := kind.query(rng)
			best := time.Duration(1 << 62)
			for r := 0; r < perfRepeats; r++ {
				start := time.Now()
				res, err := ix.Run(q)
				best = min(best, time.Since(start))
				if err != nil {
					t.Fatalf("%s: %q: %v", kind.name, q, err)
				}
				if r == 0 {
					rows, examined = rows+len(res.Matches), examined+res.Examined
				}
			}
			times = append(times, best)
		}
		slices.Sort(times)
		worst, median := times[len(times)-1], times[len(times)/2]
		note := fmt.Sprintf("median %.3f ms; %d rows returned and %d examined per query", ms(median), rows/perfQueries, examined/perfQueries)
		if kind.indexed && examined/perfQueries > perfEntities/4 {
			t.Errorf("%s examined %d rows per query: the index did not narrow it", kind.name, examined/perfQueries)
		}
		if !kind.timed {
			t.Logf("METRIC answer_%s=%.3f ms (map flood: no target) %s", kind.name, ms(worst), note)
			continue
		}
		t.Logf("METRIC answer_%s=%.3f ms (target < %g) %s", kind.name, ms(worst), targetAnswerMS, note)
		if !raceEnabled && ms(worst) >= targetAnswerMS {
			t.Errorf("%s: %.3f ms, want under %g ms", kind.name, ms(worst), targetAnswerMS)
		}
	}
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }

// BenchmarkRun measures each kind of query over the 10,000-row world.
func BenchmarkRun(b *testing.B) {
	w := perfWorld(b)
	ix, err := New(w, Config{})
	if err != nil {
		b.Fatal(err)
	}
	for _, kind := range perfKinds() {
		rng := rand.New(rand.NewPCG(11, 0xbe7c4))
		queries := make([]Query, 64)
		for i := range queries {
			queries[i] = kind.query(rng)
		}
		b.Run(kind.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; b.Loop(); i++ {
				if _, err := ix.Run(queries[i%len(queries)]); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkBuild measures indexing 10,000 rows.
func BenchmarkBuild(b *testing.B) {
	w := perfWorld(b)
	for b.Loop() {
		if _, err := New(w, Config{}); err != nil {
			b.Fatal(err)
		}
	}
}
