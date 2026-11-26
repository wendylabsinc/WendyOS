package query

import (
	"errors"
	"math"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
)

// epoch is the instant the hand-built rows were observed around.
var epoch = spatial.Stamp(1_767_258_000_000_000_000) // 2026-01-01T09:00:00Z

func row(kind spatial.EntityKind, id string, floor spatial.FloorID, x, z float64) spatial.Entity {
	return spatial.Entity{Kind: kind, ID: id, Floor: floor, Position: geom.Vec3{X: x, Z: z}, Confidence: 1, ObservedAt: epoch}
}

func box(x0, z0, x1, z1 float64) []geom.Vec2 {
	return []geom.Vec2{{X: x0, Z: z0}, {X: x1, Z: z0}, {X: x1, Z: z1}, {X: x0, Z: z1}}
}

// fill sets every cell whose centre lies in the box to the given log-odds.
func fill(t testing.TB, g *grid.Grid, x0, z0, x1, z1, logOdds float64) {
	t.Helper()
	lo, hi := g.CellAt(geom.Vec2{X: x0, Z: z0}), g.CellAt(geom.Vec2{X: x1, Z: z1})
	for iz := lo.IZ; iz <= hi.IZ; iz++ {
		for ix := lo.IX; ix <= hi.IX; ix++ {
			c := grid.Cell{IX: ix, IZ: iz}
			if p := g.Center(c); p.X < x0 || p.X > x1 || p.Z < z0 || p.Z > z1 {
				continue
			}
			if err := g.Set(c, logOdds); err != nil {
				t.Fatal(err)
			}
		}
	}
}

const (
	free     = -1.0
	occupied = 2.0
)

// handWorld is two rooms on floor "f1" split by a wall along x = 2 with a
// 1 m doorway at z in [1.5, 2.5), a sealed closet, and one row upstairs.
//
//	z=8 +--------------+---------------------+
//	    |              |              desk   |
//	    |   west       |   east     (5,5)    |
//	    |              .  <- doorway         |
//	    |  rover(0,0)  |  chair-b (3,0)      |
//	    |              |           [closet]  |
//	z=-4+--------------+---------------------+
//	   x=-4           x=2                   x=8
func handWorld(t testing.TB) World {
	t.Helper()
	g, err := grid.New(grid.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	fill(t, g, -4, -4, 8, 8, free)
	fill(t, g, 2, -4, 2.05, 1.5, occupied) // wall below the doorway
	fill(t, g, 2, 2.5, 2.05, 8, occupied)  // wall above it
	// A closet with walls on all four sides around (6.5, -2.5).
	fill(t, g, 6, -3, 7, -2.95, occupied)
	fill(t, g, 6, -2.05, 7, -2, occupied)
	fill(t, g, 6, -3, 6.05, -2, occupied)
	fill(t, g, 6.95, -3, 7, -2, occupied)

	rover := row(spatial.EntityDevice, "rover", "f1", 0, 0)
	rover.Extent.RadiusM, rover.UncertaintyM = 0.25, 0.03
	desk := row(spatial.EntityObject, "desk", "f1", 5, 5)
	desk.Class, desk.Extent.Polygon = "desk", box(4.5, 4.7, 5.5, 5.3)
	chair := func(id string, x, z float64) spatial.Entity {
		e := row(spatial.EntityObject, id, "f1", x, z)
		e.Class, e.UncertaintyM = "chair", 0.04
		return e
	}
	up := chair("chair-up", 1, 0)
	up.Floor = "f2"
	old := chair("chair-old", -3, -3)
	old.ObservedAt, old.ValidUntil = epoch.Add(-time.Hour), epoch.Add(-30*time.Minute)
	region := func(id, label string, floor spatial.FloorID, poly []geom.Vec2) spatial.Entity {
		c, _ := geom.PolygonCentroid(poly)
		e := row(spatial.EntityRegion, id, floor, c.X, c.Z)
		e.Class, e.Label, e.Extent.Polygon = "region", label, poly
		return e
	}
	return World{
		Entities: []spatial.Entity{
			rover,
			chair("chair-a", 1, 0),
			chair("chair-d", -1, 0), // exactly as far from the rover as chair-a
			chair("chair-c", 0, -2),
			chair("chair-b", 3, 0), // behind the wall
			chair("chair-e", 3, 3), // seen through the doorway
			chair("chair-x", 6.5, -2.5),
			old,
			desk,
			row(spatial.EntityTrack, "person", "f1", 4, 2),
			region("west", "office", "f1", box(-4, -4, 2, 8)),
			region("east", "kitchen", "f1", box(2, -4, 8, 8)),
			region("up", "office", "f2", box(-4, -4, 8, 8)),
			up,
			row(spatial.EntityAnchor, "rover", "f2", 0, 0), // shares an id with the device
		},
		Grids: map[spatial.FloorID]*grid.Grid{"f1": g},
	}
}

func handIndex(t testing.TB) (*Index, World) {
	t.Helper()
	w := handWorld(t)
	for i := range w.Entities {
		w.Entities[i].Attributes = map[string]string{rowAttr: strconv.Itoa(i)}
	}
	ix, err := New(w, Config{})
	if err != nil {
		t.Fatal(err)
	}
	return ix, w
}

// ids runs a text query, checks it against the brute-force reference and
// returns the matched ids in order.
func ids(t *testing.T, ix *Index, w World, text string) []string {
	t.Helper()
	q, err := Parse(text, epoch)
	if err != nil {
		t.Fatalf("%q: %v", text, err)
	}
	res, err := check(t, ix, w, Config{}, q)
	if err != nil {
		t.Fatalf("%q: %v", text, err)
	}
	out := []string{}
	for _, m := range res.Matches {
		out = append(out, m.Entity.ID)
	}
	return out
}

func TestQueryKinds(t *testing.T) {
	ix, w := handIndex(t)
	for _, tc := range []struct {
		text string
		want []string
	}{
		// Nearest: chair-a and chair-d are both exactly 1 m away; the tie
		// goes to the lower id.
		{"nearest 1 class=chair to device/rover", []string{"chair-a"}},
		{"nearest 2 class=chair to device/rover", []string{"chair-a", "chair-d"}},
		{"nearest 3 class=chair to device/rover", []string{"chair-a", "chair-d", "chair-c"}},
		{"nearest 3 to device/rover", []string{"chair-a", "chair-d", "chair-c"}}, // the rover is not its own neighbour
		{"nearest 100 kind=track,region to device/rover", []string{"west", "person", "east"}},
		{"nearest 2 class=chair to @2.9,0.1:f1", []string{"chair-b", "chair-a"}},
		{"nearest 9223372036854775807 kind=track,device to chair-a", []string{"rover", "person"}}, // any K is a valid K
		// Within is inclusive: chair-c is exactly 2 m away.
		{"find class=chair to device/rover within 2m", []string{"chair-a", "chair-d", "chair-c"}},
		{"find class=chair to device/rover within 1.99", []string{"chair-a", "chair-d"}},
		{"nearest 1 class=chair to device/rover within 0.5m", []string{}},
		// Box, bounds included.
		{"find kind=object in box 1,0,3,3", []string{"chair-a", "chair-b", "chair-e", "chair-up"}}, // a box spans every floor
		{"find kind=object in box 1,0,3,3 on f1", []string{"chair-a", "chair-b", "chair-e"}},
		{"find kind=object in box 1,0,3,3 on f2", []string{"chair-up"}},
		// Region by id, by label (two floors carry an "office"), polygon.
		{"find kind=object in region east", []string{"chair-b", "chair-e", "chair-x", "desk"}},
		{"find class=chair in region office", []string{"chair-a", "chair-c", "chair-d", "chair-old", "chair-up"}},
		{"find class=chair in region office on f1", []string{"chair-a", "chair-c", "chair-d", "chair-old"}},
		{"find in polygon 0.5,-1 4,-1 4,1", []string{"chair-b"}},               // below the line from (0.5,-1) to (4,1)
		{"find in polygon 0.5,-1 4,-1 0.5,2", []string{"chair-a", "chair-up"}}, // below the line from (4,-1) to (0.5,2), on both floors
		// Floor, kind, class.
		{"find on f2", []string{"rover", "chair-up", "up"}},
		{"find kind=device,track", []string{"rover", "person"}},
		{"find class=desk,none", []string{"desk"}},
		// Visibility from the rover: the wall hides chair-b, the doorway
		// shows chair-e, the closet walls hide chair-x.
		// chair-e and chair-old are both √18 m away: the id decides.
		{"find class=chair to device/rover visible", []string{"chair-a", "chair-d", "chair-c", "chair-e", "chair-old"}},
		{"find class=chair,desk visible-from person", []string{"chair-b", "chair-e", "desk"}},
		// Path: around the wall through the doorway; the closet is sealed.
		{"find class=chair to device/rover path 4m", []string{"chair-a", "chair-d", "chair-c"}},
		// (The default order with an origin is straight-line distance.)
		{"find class=chair to device/rover path 5m", []string{"chair-a", "chair-d", "chair-c", "chair-b", "chair-e", "chair-old"}},
		{"find class=chair to device/rover order path", []string{"chair-a", "chair-d", "chair-c", "chair-e", "chair-old", "chair-b", "chair-x"}},
		// The two chairs nearest chair-e are chair-b (3 m) and chair-a (3.6 m);
		// chair-a is the longer walk, round the door jamb.
		{"nearest 2 class=chair to chair-e order path desc", []string{"chair-a", "chair-b"}},
		// Relations.
		{"find kind=object near person 2m", []string{"chair-e"}}, // chair-b is √5 m from the person
		{"find kind=object near person 2.3m", []string{"chair-b", "chair-e"}},
		{"find kind=object same-region person", []string{"chair-b", "chair-e", "chair-x", "desk"}},
		{"find kind=object same-region person near desk 3m", []string{"chair-e"}},
		{"find kind=object inside west", []string{"chair-a", "chair-c", "chair-d", "chair-old"}},
		{"find inside device/rover", []string{}},
		{"find kind=object to device/rover near chair-b 1m", []string{}},
		// Time: chair-old was seen an hour before the others and expired.
		{"find class=chair on f1 since now-1m", []string{"chair-a", "chair-b", "chair-c", "chair-d", "chair-e", "chair-x"}},
		{"find class=chair before now-1m", []string{"chair-old"}},
		{"find class=chair on f1 at now-45m", []string{"chair-old"}},
		{"find class=chair on f1 at now-10m", []string{}},
		{"find class=chair at 2026-01-01T09:00:00Z on f1 limit 2", []string{"chair-a", "chair-b"}},
		// Order and limit.
		{"find class=chair on f1 order time limit 2", []string{"chair-old", "chair-a"}},
		// Descending reverses the distance, not the tie-break between
		// chair-e and chair-old.
		{"find class=chair to device/rover order distance desc limit 2", []string{"chair-x", "chair-e"}},
		{"find class=chair to device/rover order key desc limit 3", []string{"chair-a", "chair-b", "chair-c"}}, // key order has no key to reverse
		{"nearest 3 class=chair to device/rover order key", []string{"chair-a", "chair-c", "chair-d"}},
	} {
		if got := ids(t, ix, w, tc.text); !slices.Equal(got, tc.want) {
			t.Errorf("%q\n got %v\nwant %v", tc.text, got, tc.want)
		}
	}
}

func TestQueryDistances(t *testing.T) {
	ix, w := handIndex(t)
	q, err := Parse("find class=chair to device/rover order path", epoch)
	if err != nil {
		t.Fatal(err)
	}
	res, err := check(t, ix, w, Config{}, q)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]Match{}
	for _, m := range res.Matches {
		by[m.Entity.ID] = m
	}
	if res.Total != 7 || res.Examined == 0 {
		t.Fatalf("total %d examined %d", res.Total, res.Examined)
	}
	// Straight line in free space: path equals distance to within a cell.
	if a := by["chair-a"]; a.DistanceM != 1 || !a.Reachable || math.Abs(a.PathM-1) > 0.05 {
		t.Errorf("chair-a: %+v", a)
	}
	// The uncertainty of a distance is that of both ends: 3 cm and 4 cm.
	if a := by["chair-a"]; math.Abs(a.DistanceSigmaM-0.05) > 1e-12 {
		t.Errorf("chair-a sigma %v, want 0.05", a.DistanceSigmaM)
	}
	// chair-b is 3 m away through the wall. The shortest way round turns at
	// the lower door jamb, keeping the default 0.2 m clear of its last cell
	// (centre (2.025, 1.475)): a tangent of 2.50 m, an arc of 104° (0.36 m)
	// and a tangent of 1.76 m, 4.62 m in all. An 8-connected path may be up
	// to 8.3 % longer.
	const around = 4.62
	if b := by["chair-b"]; b.DistanceM != 3 || !b.Reachable || b.PathM < around-0.05 || b.PathM > around*1.083+0.05 {
		t.Errorf("chair-b: distance %v path %v, want a path of %v to %v m", b.DistanceM, b.PathM, around, around*1.083)
	}
	if x := by["chair-x"]; x.Reachable || x.PathM != 0 {
		t.Errorf("chair-x is in a sealed closet: %+v", x)
	}
	if last := res.Matches[len(res.Matches)-1]; last.Entity.ID != "chair-x" {
		t.Errorf("unreachable rows sort last, got %s", last.Entity.ID)
	}
}

func TestQueryRefusals(t *testing.T) {
	ix, w := handIndex(t)
	for _, tc := range []struct {
		text string
		want error
	}{
		{"nearest 1 to ghost", ErrUnknownEntity},
		{"nearest 1 to region/ghost", ErrUnknownEntity},
		{"nearest 1 to rover", ErrAmbiguous}, // a device and an anchor share the id
		{"find near rover 1m", ErrAmbiguous},
		{"nearest 1 to @0,0", ErrNoFloor}, // two floors, none named
		{"find in region pantry", ErrUnknownRegion},
		{"find inside chair-a", ErrNoExtent},
		{"find to anchor/rover visible", ErrNoGrid}, // no map upstairs
		{"find to anchor/rover order path", ErrNoGrid},
		{"find visible-from chair-up", ErrNoGrid},
	} {
		q, err := Parse(tc.text, epoch)
		if err != nil {
			t.Fatalf("%q: %v", tc.text, err)
		}
		if _, err := check(t, ix, w, Config{}, q); !errors.Is(err, tc.want) {
			t.Errorf("%q: error %v, want %v", tc.text, err, tc.want)
		}
	}
	// A point with the query's floor, or in a world with one floor, is fine.
	if got := ids(t, ix, w, "nearest 1 to @0,0 on f2"); !slices.Equal(got, []string{"rover"}) {
		t.Errorf("point on the query floor: %v", got)
	}
	// Floors that contradict each other give an empty answer, not an error.
	if got := ids(t, ix, w, "find to anchor/rover visible on f1"); len(got) != 0 {
		t.Errorf("contradicting floors: %v", got)
	}

	for name, q := range map[string]Query{
		"nearest without origin": {Nearest: 3},
		"within without origin":  {WithinM: 2},
		"visible without origin": {Visible: true},
		"path order no origin":   {Order: OrderPath},
		"negative limit":         {Limit: -1},
		"bad kind":               {Kinds: []spatial.EntityKind{"robot"}},
		"bad order":              {Order: "size"},
		"descending default":     {Descending: true},
		"inverted box":           {Box: &Box{Min: geom.Vec2{X: 1}, Max: geom.Vec2{X: 0}}},
		"two-vertex polygon":     {Polygon: []geom.Vec2{{}, {X: 1}}},
		"nan radius":             {Origin: EntityRef("rover"), WithinM: math.NaN()},
		"empty ref":              {Origin: &Ref{}},
		"entity and point":       {Origin: &Ref{Entity: "rover", Point: &geom.Vec2{}}},
		"near without distance":  {Relations: []Relation{{Kind: RelNear, To: *EntityRef("rover")}}},
		"inside with distance":   {Relations: []Relation{{Kind: RelInside, To: *EntityRef("west"), DistanceM: 1}}},
		"unknown relation":       {Relations: []Relation{{Kind: "above", To: *EntityRef("west")}}},
	} {
		if _, err := ix.Run(q); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: error %v, want ErrInvalid", name, err)
		}
	}
}

// TestRegionNames: a region name is an id first and a label only when no
// region has that id.
func TestRegionNames(t *testing.T) {
	region := func(id, label string, x float64) spatial.Entity {
		e := row(spatial.EntityRegion, id, "f", x+1, 1)
		e.Label, e.Extent.Polygon = label, box(x, 0, x+2, 2)
		return e
	}
	w := World{Entities: []spatial.Entity{
		region("kitchen", "room", 0), region("r2", "kitchen", 10), region("r3", "room", 20),
		row(spatial.EntityObject, "a", "f", 1, 1), row(spatial.EntityObject, "b", "f", 11, 1), row(spatial.EntityObject, "c", "f", 21, 1),
	}}
	for i := range w.Entities {
		w.Entities[i].Attributes = map[string]string{rowAttr: strconv.Itoa(i)}
	}
	ix, err := New(w, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(t, ix, w, "find kind=object in region kitchen"); !slices.Equal(got, []string{"a"}) {
		t.Errorf("by id: %v", got)
	}
	if got := ids(t, ix, w, "find kind=object in region room"); !slices.Equal(got, []string{"a", "c"}) {
		t.Errorf("by label: %v", got)
	}
}

func TestNewRejects(t *testing.T) {
	bad := row(spatial.EntityObject, "x", "f1", math.NaN(), 0)
	if _, err := New(World{Entities: []spatial.Entity{bad}}, Config{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("NaN position: %v", err)
	}
	if _, err := New(World{Grids: map[spatial.FloorID]*grid.Grid{"f1": nil}}, Config{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("nil grid: %v", err)
	}
	for _, cfg := range []Config{{CellM: -1}, {CellM: 1e-6}, {TargetLoad: -2}, {SurfaceCells: -1}, {SightCells: -1}, {SightCells: 100}, {ClearanceM: math.Inf(1)}} {
		if _, err := New(World{}, cfg); !errors.Is(err, ErrConfig) {
			t.Errorf("config %+v: %v", cfg, err)
		}
	}
	// An empty world answers with nothing.
	ix, err := New(World{}, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if res, err := ix.Run(Query{}); err != nil || len(res.Matches) != 0 || ix.Len() != 0 {
		t.Errorf("empty world: %+v %v", res, err)
	}
}

// TestPathRules checks the path conventions on maps small enough to count
// cells by hand: 1 m cells, a point exactly at a cell centre.
func TestPathRules(t *testing.T) {
	build := func(occupiedCells ...grid.Cell) (*grid.Grid, World) {
		g, err := grid.New(grid.Config{CellSize: 1, FieldDistance: 3})
		if err != nil {
			t.Fatal(err)
		}
		fill(t, g, 0, 0, 5, 5, free) // cells (0..4, 0..4)
		for _, c := range occupiedCells {
			if err := g.Set(c, occupied); err != nil {
				t.Fatal(err)
			}
		}
		w := World{
			Entities: []spatial.Entity{row(spatial.EntityObject, "goal", "f", 3.5, 2.5), row(spatial.EntityObject, "corner", "f", 1.5, 1.5)},
			Grids:    map[spatial.FloorID]*grid.Grid{"f": g},
		}
		return g, w
	}
	path := func(w World, cfg Config, id string) (float64, bool) {
		t.Helper()
		ix, err := New(w, cfg)
		if err != nil {
			t.Fatal(err)
		}
		res, err := check(t, ix, w, cfg, Query{Origin: PointRef(geom.Vec2{X: 0.5, Z: 0.5}, "f"), Order: OrderPath})
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range res.Matches {
			if m.Entity.ID == id {
				return m.PathM, m.Reachable
			}
		}
		t.Fatalf("%s not in the answer", id)
		return 0, false
	}
	// A surface thinner than the snap reach keeps the ends at their own cell.
	cfg := Config{SurfaceCells: 0.01}

	// Open floor: (0,0) to (3,2) is two diagonal steps and one straight one.
	_, w := build()
	if got, ok := path(w, cfg, "goal"); !ok || math.Abs(got-(2*1.414214+1)) > 1e-9 {
		t.Errorf("open floor: %v %v, want %v", got, ok, 2*1.414214+1)
	}
	// Occupied (1,0) and (0,1): the diagonal to (1,1) would cut both corners,
	// so the origin cell is sealed in.
	_, w = build(grid.Cell{IX: 1, IZ: 0}, grid.Cell{IX: 0, IZ: 1})
	if _, ok := path(w, cfg, "corner"); ok {
		t.Error("a diagonal step cut a corner")
	}
	// Occupied (1,0) only: the diagonal is still barred, the way is (0,1) then
	// (1,1): two straight steps instead of 1.41.
	_, w = build(grid.Cell{IX: 1, IZ: 0})
	if got, ok := path(w, cfg, "corner"); !ok || math.Abs(got-2) > 1e-9 {
		t.Errorf("one blocked corner: %v %v, want 2", got, ok)
	}
	// The bound of "path D" is inclusive, to the unit path lengths are
	// counted in.
	_, w = build()
	ix, err := New(w, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for bound, want := range map[float64]int{2*1.414214 + 1: 2, 2*1.414214 + 1 - 2e-6: 1, 1.414214: 1, 1.414213: 0} {
		res, err := check(t, ix, w, cfg, Query{Origin: PointRef(geom.Vec2{X: 0.5, Z: 0.5}, "f"), PathWithinM: bound})
		if err != nil || res.Total != want {
			t.Errorf("path within %v m: %d rows (%v), want %d", bound, res.Total, err, want)
		}
	}
	// Unknown cells bar the way unless TraverseUnknown is set.
	g, w := build()
	for iz := int32(0); iz < 5; iz++ {
		if err := g.Set(grid.Cell{IX: 2, IZ: iz}, 0); err != nil { // an unobserved strip across the floor
			t.Fatal(err)
		}
	}
	if _, ok := path(w, cfg, "goal"); ok {
		t.Error("a path crossed unknown cells")
	}
	cfg.TraverseUnknown = true
	if got, ok := path(w, cfg, "goal"); !ok || math.Abs(got-(2*1.414214+1)) > 1e-9 {
		t.Errorf("TraverseUnknown: %v %v", got, ok)
	}
	// Clearance: a wall with a one-cell gap is passable for a point. The
	// centre of the gap cell is 1 m from the centre of each jamb cell, so the
	// gap is closed to anything that needs 1.2 m.
	_, w = build(grid.Cell{IX: 2, IZ: 0}, grid.Cell{IX: 2, IZ: 1}, grid.Cell{IX: 2, IZ: 3}, grid.Cell{IX: 2, IZ: 4})
	cfg = Config{SurfaceCells: 0.01}
	if _, ok := path(w, cfg, "goal"); !ok {
		t.Error("the gap is open to a point")
	}
	cfg.ClearanceM = 1
	if _, ok := path(w, cfg, "goal"); !ok {
		t.Error("the gap is open at exactly 1 m clearance")
	}
	cfg.ClearanceM = 1.2
	if _, ok := path(w, cfg, "goal"); ok {
		t.Error("the gap is closed at 1.2 m clearance")
	}
	cfg.ClearanceM = 5 // beyond the grid's field distance
	ix, err = New(w, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ix.Run(Query{Origin: PointRef(geom.Vec2{X: 0.5, Z: 0.5}, "f"), Order: OrderPath}); !errors.Is(err, ErrConfig) {
		t.Errorf("clearance beyond the field distance: %v", err)
	}
}

// TestVisibilityFootprint: an entity that is itself in the map must not hide
// behind its own cells, and the sight line is measured to the footprint.
func TestVisibilityFootprint(t *testing.T) {
	g, err := grid.New(grid.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	fill(t, g, -1, -3, 9, 3, free)
	fill(t, g, 3.7, -0.3, 4.3, 0.3, occupied) // the round table, as mapped
	fill(t, g, 5.95, -1, 6.6, 1.05, occupied) // the shelf, as mapped: one cell proud of its box
	fill(t, g, 7.5, -3, 7.55, 3, occupied)    // a wall behind both
	table := row(spatial.EntityObject, "table", "f", 4, 0)
	table.Extent.RadiusM = 0.3
	shelf := row(spatial.EntityObject, "shelf", "f", 6.3, 0)
	shelf.Extent.Polygon = box(6, -1, 6.6, 1)
	w := World{
		Entities: []spatial.Entity{table, shelf, row(spatial.EntityObject, "hidden", "f", 8.5, 2.5), row(spatial.EntityObject, "bare", "f", 4, 0.2)},
		Grids:    map[spatial.FloorID]*grid.Grid{"f": g},
	}
	ix, err := New(w, Config{})
	if err != nil {
		t.Fatal(err)
	}
	seen := func(text string) []string {
		q, err := Parse(text, epoch)
		if err != nil {
			t.Fatal(err)
		}
		res, err := check(t, ix, w, Config{}, q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range res.Matches {
			out = append(out, m.Entity.ID)
		}
		return out
	}
	// From (0, 2): the table and the shelf are seen although their own cells
	// are occupied; "bare" is a point inside the table's cells and is not;
	// the wall hides what is behind it.
	if got := seen("find to @0,2 visible order key"); !slices.Equal(got, []string{"shelf", "table"}) {
		t.Errorf("from (0,2): %v", got)
	}
	// From (0, 0) the table stands in front of the shelf.
	if got := seen("find to @0,0 visible order key"); !slices.Equal(got, []string{"table"}) {
		t.Errorf("from (0,0): %v", got)
	}
	// The other way round: the shelf sees the table, and nothing else.
	if got := seen("find visible-from shelf order key"); !slices.Equal(got, []string{"table"}) {
		t.Errorf("from the shelf: %v", got)
	}
}

// TestWith: the next snapshot answers over its own rows and shares the maps.
func TestWith(t *testing.T) {
	ix, w := handIndex(t)
	if got := ids(t, ix, w, "find class=chair to device/rover visible"); len(got) != 5 {
		t.Fatalf("before: %v", got)
	}
	next := World{Grids: w.Grids}
	for _, e := range w.Entities {
		if e.ID == "chair-a" {
			continue // gone
		}
		if e.ID == "chair-b" {
			e.Position.Z = 3 // moved next to chair-e, in the doorway's line of sight
		}
		next.Entities = append(next.Entities, e)
	}
	ix2, err := ix.With(next.Entities)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(t, ix2, next, "find class=chair to device/rover visible"); !slices.Equal(got, []string{"chair-d", "chair-c", "chair-b", "chair-e", "chair-old"}) {
		t.Errorf("after: %v", got)
	}
	if ix2.maps["f1"] != ix.maps["f1"] || ix2.Len() != ix.Len()-1 {
		t.Error("With did not keep the maps or the rows")
	}
	// The first snapshot is untouched.
	if got := ids(t, ix, w, "nearest 1 class=chair to device/rover"); !slices.Equal(got, []string{"chair-a"}) {
		t.Errorf("first snapshot: %v", got)
	}
	bad := row(spatial.EntityObject, "", "f1", 0, 0)
	if _, err := ix.With([]spatial.Entity{bad}); !errors.Is(err, ErrInvalid) {
		t.Errorf("invalid row: %v", err)
	}
}

// TestConcurrentRun: an Index answers from many goroutines at once, the
// first of which build the maps sight and path read (run with -race).
func TestConcurrentRun(t *testing.T) {
	ix, _ := handIndex(t)
	texts := []string{
		"find class=chair to device/rover visible", "find class=chair to device/rover order path",
		"nearest 3 to device/rover", "find kind=object visible-from person", "find to chair-e path 6m",
	}
	answer := func(text string) string {
		q, err := Parse(text, epoch)
		if err != nil {
			return err.Error()
		}
		res, err := ix.Run(q)
		if err != nil {
			return err.Error()
		}
		out := ""
		for _, m := range res.Matches {
			out += m.Entity.ID + " " + strconv.FormatFloat(m.PathM, 'g', -1, 64) + ";"
		}
		return out
	}
	const workers = 8
	got := make([][]string, workers)
	var wg sync.WaitGroup
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := range texts {
				got[i] = append(got[i], answer(texts[(k+i)%len(texts)]))
			}
		}()
	}
	wg.Wait()
	for i := range got {
		for k := range texts {
			if want := answer(texts[(k+i)%len(texts)]); got[i][k] != want || want == "" {
				t.Errorf("worker %d %q: %q, alone %q", i, texts[(k+i)%len(texts)], got[i][k], want)
			}
		}
	}
}

func TestIndexLayout(t *testing.T) {
	// Rows on a line, rows on one point, and one far outlier must all index
	// without a huge table, and still answer exactly.
	var ents []spatial.Entity
	for i := range 200 {
		ents = append(ents, row(spatial.EntityObject, "line-"+strconv.Itoa(i), "line", float64(i)*0.1, 3))
		ents = append(ents, row(spatial.EntityObject, "dot-"+strconv.Itoa(i), "dot", 7, 7))
		ents = append(ents, row(spatial.EntityObject, "wide-"+strconv.Itoa(i), "wide", float64(i%20), float64(i/20)))
		ents = append(ents, row(spatial.EntityObject, "even-"+strconv.Itoa(i), "even", float64(i%20), float64(i/20)))
	}
	ents = append(ents, row(spatial.EntityObject, "outlier", "wide", 9e5, -9e5))
	for i := range ents {
		ents[i].Attributes = map[string]string{rowAttr: strconv.Itoa(i)}
	}
	w := World{Entities: ents}
	// With CellM 1 the rows of "even" and "wide" sit exactly on cell borders,
	// and the circles and boxes below pass exactly through rows.
	for _, cfg := range []Config{{}, {CellM: 0.001}, {CellM: 1}, {CellM: 50}, {TargetLoad: 0.01}} {
		ix, err := New(w, cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range ix.floors {
			if cells := f.cols * f.rows; cells > 4*len(f.items)+64 {
				t.Errorf("config %+v floor %s: %d cells for %d rows", cfg, f.id, cells, len(f.items))
			}
		}
		for _, text := range []string{
			"nearest 5 to @3.33,3:line", "nearest 5 to @-50,40:line", "nearest 3 to @7,7:dot", "nearest 3 to @0,0:dot",
			"nearest 4 to @9e5,-9e5:wide", "nearest 4 to @5.5,5.5:wide", "find to @5,5:wide within 1.5", "find in box 2,2,4,3", "find on dot limit 3",
			"find to @5,5:even within 2", "find near even-105 2m", "find near even-105 5m on even", "nearest 5 to even-105", "nearest 9 to @5,5:even within 1",
			"find in polygon 2,2 6,2 6,5 2,5 on even", "find to @3,3:even within 2 in box 3,1,5,3",
		} {
			q, err := Parse(text, epoch)
			if err != nil {
				t.Fatal(err)
			}
			res, err := check(t, ix, w, cfg, q)
			if err != nil {
				t.Errorf("config %+v %q: %v", cfg, text, err)
			}
			// The circle of 2 m round (5, 5) passes through four rows and
			// holds thirteen.
			if text == "find to @5,5:even within 2" && res.Total != 13 {
				t.Errorf("config %+v %q: %d rows, want 13", cfg, text, res.Total)
			}
		}
	}
	// The index narrows the search on an evenly filled floor: a nearest
	// query reads the cells around its origin, a box query the cells it
	// covers. (The outlier on "wide" stretches that floor's cells until one
	// holds every other row: exact, but a scan.)
	ix, err := New(w, Config{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := ix.Run(Query{Origin: PointRef(geom.Vec2{X: 5.5, Z: 5.5}, "even"), Nearest: 4})
	if err != nil || res.Examined > 50 {
		t.Errorf("nearest 4 of 200 rows examined %d (%v)", res.Examined, err)
	}
	res, err = ix.Run(Query{Floor: "even", Box: &Box{Min: geom.Vec2{X: 2, Z: 2}, Max: geom.Vec2{X: 4, Z: 3}}})
	if err != nil || len(res.Matches) != 6 || res.Examined > 30 {
		t.Errorf("box of 6 rows examined %d and matched %d (%v)", res.Examined, len(res.Matches), err)
	}
}
