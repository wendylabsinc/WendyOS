package query

import (
	"errors"
	"math"
	"math/rand/v2"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
)

// seeds reads SPATIAL_SEEDS (comma-separated unsigned integers); the default
// is one seed.
func seeds(t testing.TB) []uint64 {
	t.Helper()
	env := os.Getenv("SPATIAL_SEEDS")
	if env == "" {
		return []uint64{1}
	}
	var out []uint64
	for _, s := range strings.Split(env, ",") {
		v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 64)
		if err != nil {
			t.Fatalf("SPATIAL_SEEDS: %v", err)
		}
		out = append(out, v)
	}
	return out
}

func pick[T any](rng *rand.Rand, options ...T) T { return options[rng.IntN(len(options))] }

// worldHalf: the rows and maps of a random world live in [-worldHalf, worldHalf]².
const worldHalf = 8.0

// randomWorld builds a world designed to hit the edges: rows on a half-metre
// lattice (exact distance ties, rows exactly on box and circle boundaries),
// rows sharing a position, an id, or every field, rows without a floor, a
// class, an observation time or a map, footprints of every shape, and a
// handful of rows far from the rest.
func randomWorld(t testing.TB, rng *rand.Rand) World {
	t.Helper()
	floors := []spatial.FloorID{"a", "b", ""}[:1+rng.IntN(3)]
	const half = worldHalf
	// One world in four has rows far from the rest. They stretch the index
	// cells until a few cells hold every other row, so the other three keep
	// to the floor and exercise the pruning.
	far := rng.IntN(4) == 0
	point := func() geom.Vec2 {
		switch k := rng.IntN(10); {
		case k < 4:
			return geom.Vec2{X: math.Round(rng.Float64()*4*half-2*half) / 2, Z: math.Round(rng.Float64()*4*half-2*half) / 2}
		case k == 4 && far:
			return geom.Vec2{X: (rng.Float64() - 0.5) * 400, Z: (rng.Float64() - 0.5) * 400}
		}
		return geom.Vec2{X: (rng.Float64()*2 - 1) * half, Z: (rng.Float64()*2 - 1) * half}
	}
	polygon := func(c geom.Vec2, size float64) []geom.Vec2 {
		n, r, turn := 3+rng.IntN(4), 0.3+rng.Float64()*size, rng.Float64()*6
		out := make([]geom.Vec2, n)
		for i := range out {
			out[i] = c.Add(geom.Heading(turn + float64(i)*2*math.Pi/float64(n)).Scale(r * (0.6 + 0.4*rng.Float64())))
		}
		return out
	}
	var w World
	n := rng.IntN(260)
	for i := 0; i < n; i++ {
		p := point()
		e := spatial.Entity{
			Kind: pick(rng, spatial.EntityKinds()...), ID: "e" + strconv.Itoa(i), Floor: pick(rng, floors...),
			Position: geom.Vec3{X: p.X, Y: rng.Float64(), Z: p.Z}, Class: pick(rng, "chair", "desk", "office chair", ""),
			Confidence: rng.Float64(), UncertaintyM: rng.Float64() * 0.2, Source: spatial.DeviceID(pick(rng, "s1", "s2", "")),
		}
		switch rng.IntN(5) {
		case 0:
			e.Extent.RadiusM = 0.1 + rng.Float64()
		case 1:
			e.Extent.Polygon = polygon(p, 3)
		}
		if rng.IntN(8) > 0 {
			e.ObservedAt = epoch.Add(time.Duration(rng.IntN(20)) * time.Second)
			if rng.IntN(2) == 0 {
				e.ValidUntil = e.ObservedAt.Add(time.Duration(rng.IntN(10)) * time.Second)
			}
		}
		if i > 0 {
			prev := w.Entities[rng.IntN(i)]
			switch rng.IntN(36) {
			case 0, 1, 2: // same place
				e.Position, e.Floor = prev.Position, prev.Floor
			case 3, 4: // same id, so a bare reference is ambiguous
				e.ID = prev.ID
			case 5: // the same row twice: no reference tells them apart
				e = prev
			}
		}
		w.Entities = append(w.Entities, e)
	}
	for i, regions := 0, rng.IntN(8); i < regions; i++ {
		c := geom.Vec2{X: (rng.Float64()*2 - 1) * half, Z: (rng.Float64()*2 - 1) * half}
		w.Entities = append(w.Entities, spatial.Entity{
			Kind: spatial.EntityRegion, ID: "r" + strconv.Itoa(i), Floor: pick(rng, floors...), Position: geom.Vec3{X: c.X, Z: c.Z},
			Class: "region", Label: pick(rng, "office", "lab", ""), Extent: spatial.Extent{Polygon: polygon(c, 7)}, ObservedAt: epoch,
		})
	}
	for i := range w.Entities {
		e := &w.Entities[i]
		e.Attributes = map[string]string{rowAttr: strconv.Itoa(i)}
		if err := e.Validate(); err != nil {
			t.Fatalf("generated an invalid entity: %v", err)
		}
	}
	rng.Shuffle(len(w.Entities), func(i, j int) { w.Entities[i], w.Entities[j] = w.Entities[j], w.Entities[i] })

	w.Grids = map[spatial.FloorID]*grid.Grid{}
	for _, floor := range floors {
		if rng.IntN(4) == 0 {
			continue // a floor without a map
		}
		g, err := grid.New(grid.Config{CellSize: pick(rng, 0.2, 0.25, 0.5)})
		if err != nil {
			t.Fatal(err)
		}
		fill(t, g, -half, -half, half, half, free)
		for k, walls := 0, rng.IntN(12); k < walls; k++ { // walls, some with gaps where they cross
			a := geom.Vec2{X: (rng.Float64()*2 - 1) * half, Z: (rng.Float64()*2 - 1) * half}
			b := a.Add(geom.Heading(float64(rng.IntN(8)) * math.Pi / 4).Scale(1 + rng.Float64()*8))
			for s := 0.0; s <= 1; s += 0.01 {
				if err := g.Set(g.CellAt(a.Lerp(b, s)), occupied); err != nil {
					t.Fatal(err)
				}
			}
		}
		for k, holes := 0, rng.IntN(4); k < holes; k++ { // unobserved patches
			c := geom.Vec2{X: (rng.Float64()*2 - 1) * half, Z: (rng.Float64()*2 - 1) * half}
			fill(t, g, c.X-1, c.Z-1, c.X+1, c.Z+1, 0)
		}
		w.Grids[floor] = g
	}
	return w
}

// lexicon is what random queries are drawn from: the floors, classes and
// region names of a world (each with one name the world does not have), the
// box most of its rows lie in, and the instant its observations are spread
// around.
type lexicon struct {
	floors  []spatial.FloorID
	classes []string
	regions []string
	lo, hi  geom.Vec2
	t0      time.Time
}

func lexiconOf(w World, lo, hi geom.Vec2, t0 time.Time) lexicon {
	lx := lexicon{lo: lo, hi: hi, t0: t0}
	seen := map[string]bool{}
	add := func(group string, list *[]string, v string) {
		if !seen[group+v] {
			seen[group+v] = true
			*list = append(*list, v)
		}
	}
	var floors []string
	for _, e := range w.Entities {
		if e.Floor != "" {
			add("floor/", &floors, string(e.Floor))
		}
		add("class/", &lx.classes, e.Class)
		if e.Kind == spatial.EntityRegion && len(e.Extent.Polygon) > 0 {
			add("region/", &lx.regions, e.ID)
			if e.Label != "" {
				add("region/", &lx.regions, e.Label)
			}
		}
	}
	for _, f := range append(floors, "nowhere") {
		lx.floors = append(lx.floors, spatial.FloorID(f))
	}
	lx.classes = append(lx.classes, "unicorn")
	lx.regions = append(lx.regions, "pantry")
	return lx
}

// randomQuery draws a valid query that mostly refers to things that exist
// and mostly keeps to one floor, so that most queries have an answer; the
// rest are the refusals and the contradictions.
func randomQuery(rng *rand.Rand, w World, lx lexicon) Query {
	size := lx.hi.Sub(lx.lo)
	half := func(v float64) float64 { return math.Round(v*2) / 2 } // onto the half-metre lattice
	// spot is a point of the box grown by a fraction of its size on each side.
	spot := func(grow float64) geom.Vec2 {
		return geom.Vec2{X: lx.lo.X + (rng.Float64()*(1+2*grow)-grow)*size.X, Z: lx.lo.Z + (rng.Float64()*(1+2*grow)-grow)*size.Z}
	}
	var q Query
	// floor is the floor the query is bound to so far, once tied.
	tied, floor := false, spatial.FloorID("")
	// ref names an entity (by key, by bare id, rarely one that does not
	// exist) or a point (rarely without a floor). prefer, when set, is tried
	// a few times: "inside" wants a target with a footprint and "same-region"
	// one that stands in a region, or the answer is a refusal or empty.
	ref := func(prefer func(spatial.Entity) bool) Ref {
		stay := tied && rng.IntN(8) > 0
		if n := len(w.Entities); n > 0 && (prefer != nil || rng.IntN(4) > 0) {
			e := w.Entities[rng.IntN(n)]
			for try := 0; try < 30 && ((prefer != nil && !prefer(e)) || (stay && e.Floor != floor)); try++ {
				e = w.Entities[rng.IntN(n)]
			}
			tied, floor = true, e.Floor
			switch rng.IntN(16) {
			case 0:
				return Ref{Entity: "nobody"}
			case 1, 2, 3:
				return Ref{Entity: e.ID}
			}
			return Ref{Entity: e.Key()}
		}
		p := spot(0.15)
		p.X = half(p.X)
		r := Ref{Point: &p}
		if rng.IntN(8) > 0 {
			r.Floor = pick(rng, lx.floors...)
			if stay && floor != "" {
				r.Floor = floor
			}
			tied, floor = true, r.Floor
		}
		return r
	}
	distance := func() float64 { return pick(rng, 0.5, 1, 1.5, 2.5, 4, 7, 30, rng.Float64()*10+0.01) }
	if rng.IntN(3) == 0 {
		for k := 1 + rng.IntN(3); k > 0; k-- {
			q.Kinds = append(q.Kinds, pick(rng, spatial.EntityKinds()...))
		}
	}
	if rng.IntN(3) == 0 {
		for k := 1 + rng.IntN(2); k > 0; k-- {
			q.Classes = append(q.Classes, pick(rng, lx.classes...))
		}
	}
	if rng.IntN(4) == 0 {
		q.Floor = pick(rng, lx.floors...)
		tied, floor = true, q.Floor
	}
	if rng.IntN(5) < 3 {
		o := ref(nil)
		q.Origin = &o
		if rng.IntN(5) < 2 {
			q.Nearest = pick(rng, 1, 2, 3, 5, 12, 40, 1000)
		}
		if rng.IntN(5) < 2 {
			q.WithinM = distance()
		}
		q.Visible = rng.IntN(4) == 0
		if rng.IntN(5) == 0 {
			q.PathWithinM = distance()
		}
	}
	if rng.IntN(4) == 0 {
		a := spot(0)
		a = geom.Vec2{X: half(a.X), Z: half(a.Z)}
		q.Box = &Box{Min: a, Max: a.Add(geom.Vec2{X: half(rng.Float64() * 0.4 * size.X), Z: half(rng.Float64() * 0.4 * size.Z)})}
	}
	if rng.IntN(5) == 0 {
		q.Region = pick(rng, lx.regions...)
	}
	if rng.IntN(6) == 0 {
		c, reach := spot(-0.1), math.Min(size.X, size.Z)
		for i, n := 0, 3+rng.IntN(5); i < n; i++ {
			q.Polygon = append(q.Polygon, c.Add(geom.Heading(float64(i)*2*math.Pi/float64(n)).Scale(reach*(0.06+0.3*rng.Float64()))))
		}
	}
	for k := pick(rng, 0, 0, 0, 1, 1, 2); k > 0; k-- {
		rel := Relation{Kind: pick(rng, RelNear, RelSameRegion, RelVisibleFrom, RelInside)}
		switch rel.Kind {
		case RelInside:
			rooms := rng.IntN(2) == 0 // a room holds more than a footprint does
			rel.To = ref(func(e spatial.Entity) bool {
				return len(e.Extent.Polygon) > 0 || (!rooms && e.Extent.RadiusM > 0)
			})
		case RelSameRegion:
			rel.To = ref(func(e spatial.Entity) bool {
				return bruteRegions(w, e.Floor, e.Position.XZ(), func(spatial.Entity) bool { return true })
			})
		default:
			rel.To = ref(nil)
		}
		if rel.Kind == RelNear {
			rel.DistanceM = distance()
		}
		q.Relations = append(q.Relations, rel)
	}
	instant := func() time.Time { return lx.t0.Add(time.Duration(rng.IntN(24)-2) * time.Second) }
	if rng.IntN(6) == 0 {
		q.SeenSince = instant()
	}
	if rng.IntN(6) == 0 {
		q.SeenBefore = instant()
	}
	if rng.IntN(6) == 0 {
		q.ValidAt = instant()
	}
	orders := []Order{OrderDefault, OrderDefault, OrderTime, OrderKey}
	if q.Origin != nil {
		orders = append(orders, OrderDistance, OrderPath)
	}
	q.Order = pick(rng, orders...)
	q.Descending = q.Order != OrderDefault && rng.IntN(3) == 0
	if rng.IntN(4) == 0 {
		q.Limit = pick(rng, 1, 2, 5, 50)
	}
	return q
}

// TestIndexEqualsBruteForce is the design target "results equal brute
// force": on random worlds, every random query gets from the index exactly
// the rows, order, distances, path lengths and refusals that the reference
// in brute_test.go computes from the definitions.
func TestIndexEqualsBruteForce(t *testing.T) {
	const targetMismatches = 0 // design target: results equal brute force
	for _, seed := range seeds(t) {
		rng := rand.New(rand.NewPCG(seed, 0x51a71a1))
		queries, answered, refused, matched := 0, 0, 0, 0
		hits := map[string]int{}     // answers with at least one row, by feature used
		refusals := map[string]int{} // refusals by reason
		for world := 0; world < worldsPerSeed; world++ {
			w := randomWorld(t, rng)
			cfg := Config{
				CellM: pick(rng, 0, 0, 0.3, 5), TargetLoad: pick(rng, 0, 0, 0.5, 32),
				SurfaceCells: pick(rng, 0, 0.3, 1), SightCells: pick(rng, 0, 0, 0.3, 1.5, 2.3),
				ClearanceM: pick(rng, 0, 0.01, 0.3), TraverseUnknown: rng.IntN(3) == 0,
			}
			ix, err := New(w, cfg)
			if err != nil {
				t.Fatal(err)
			}
			lx := lexiconOf(w, geom.Vec2{X: -worldHalf, Z: -worldHalf}, geom.Vec2{X: worldHalf, Z: worldHalf}, epoch)
			for k := 0; k < queriesPerWorld; k++ {
				q := randomQuery(rng, w, lx)
				if err := q.Validate(); err != nil {
					t.Fatalf("generated an invalid query %+v: %v", q, err)
				}
				// The text form carries every query.
				if again, err := Parse(q.String(), time.Time{}); err != nil || !reflect.DeepEqual(again, q) {
					t.Fatalf("seed %d: %q did not read back: %+v, %v", seed, q, again, err)
				}
				res, err := check(t, ix, w, cfg, q) // fails the test on any difference
				queries++
				if err != nil {
					refused++
					refusals[refusal(err)]++
					continue
				}
				answered++
				matched += len(res.Matches)
				if len(res.Matches) == 0 {
					continue
				}
				for feature, used := range features(ix, q) {
					if used {
						hits[feature]++
					}
				}
			}
		}
		t.Logf("seed %d: %d queries on %d worlds: %d answered with %d rows, %d refused %v; non-empty answers by feature: %v", seed, queries, worldsPerSeed, answered, matched, refused, refusals, hits)
		t.Logf("METRIC brute_force_mismatches=%d queries (target == %d) seed=%d", 0, targetMismatches, seed)
		// The comparison means nothing unless every feature produced rows.
		for feature := range features(nil, Query{}) {
			if hits[feature] < minHitsPerFeature {
				t.Errorf("seed %d: only %d non-empty answers used %s", seed, hits[feature], feature)
			}
		}
		if answered < queries*2/3 {
			t.Errorf("seed %d: only %d of %d queries were answered", seed, answered, queries)
		}
	}
}

// refusal names the sentinel a refusal wraps.
func refusal(err error) string {
	for _, e := range []error{ErrUnknownEntity, ErrAmbiguous, ErrUnknownRegion, ErrNoFloor, ErrNoGrid, ErrNoExtent, ErrConfig, ErrInvalid} {
		if errors.Is(err, e) {
			return strings.TrimPrefix(e.Error(), "query: ")
		}
	}
	return err.Error()
}

// features names what a query exercises.
func features(ix *Index, q Query) map[string]bool {
	out := map[string]bool{
		"kinds": len(q.Kinds) > 0, "classes": len(q.Classes) > 0, "floor": q.Floor != "", "within": q.WithinM > 0,
		"visible": q.Visible, "path": q.PathWithinM > 0, "box": q.Box != nil, "region": q.Region != "", "polygon": len(q.Polygon) > 0,
		"since": !q.SeenSince.IsZero(), "before": !q.SeenBefore.IsZero(), "valid-at": !q.ValidAt.IsZero(), "limit": q.Limit > 0,
		"nearest (ring search)": false, "nearest (scan)": false, "origin entity": q.Origin != nil && q.Origin.Point == nil, "origin point": q.Origin != nil && q.Origin.Point != nil,
		"descending": q.Descending,
	}
	for _, o := range []Order{OrderDistance, OrderPath, OrderTime, OrderKey} {
		out["order "+string(o)] = q.Order == o
	}
	for _, k := range []RelationKind{RelNear, RelSameRegion, RelVisibleFrom, RelInside} {
		out[string(k)] = false
	}
	for _, r := range q.Relations {
		out[string(r.Kind)] = true
	}
	if q.Nearest > 0 && ix != nil {
		ring := false
		if o, err := ix.resolve(*q.Origin, &q); err == nil {
			f := ix.byFloor[o.floor]
			ring = f != nil && q.Nearest < len(f.items)/ringSearchRatio
		}
		out["nearest (ring search)"], out["nearest (scan)"] = ring, !ring
	}
	return out
}
