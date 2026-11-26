package query

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
)

// Config holds the tunables of an Index. A zero field takes its default;
// DefaultConfig lists them.
type Config struct {
	// CellM is the smallest side of an index cell in metres. Zero sizes the
	// cells of each floor from TargetLoad. A floor never gets more than four
	// cells per row: the side doubles until that holds.
	CellM float64
	// TargetLoad is the mean number of rows per index cell when CellM is
	// zero. Default 4: a nearest query for a few rows then reads the 3x3
	// cells around its origin, about 36 rows, and the cell table is a
	// quarter of the row count.
	TargetLoad float64
	// SurfaceCells is the thickness, in cells of the floor's occupancy grid,
	// that a mapped surface is assumed to have. A sight line stops this far
	// short of each footprint, and a path may start or end this far outside
	// one. Default 2: the cell holding the true surface plus one for range
	// noise at the cell size. Raise it for a noisier map; every cell added
	// lets sight and path leak through a wall that much closer to an entity
	// standing against its far side.
	SurfaceCells float64
	// SightCells is how far, in cells of the floor's occupancy grid, a sight
	// line must stay from every Occupied cell (centre to centre). Default 1:
	// a map cannot resolve an opening one or two cells wide, and a wall
	// mapped by beams that graze it is full of such holes, so they are not
	// windows. Raise it for a map with wider holes; every cell added narrows
	// each doorway by two and hides what stands that close to a wall along
	// the line. Below 1 the line is tested against the Occupied cells alone.
	SightCells float64
	// ClearanceM is the distance a path keeps from Occupied cells: the
	// radius of whatever travels it. Default 0.2, about the half-width of a
	// person or a small robot: nothing that travels is a point, and a point
	// slips through every one-cell hole in a mapped wall. Set it to the body
	// radius of the device the answer is for. It must not exceed the grid's
	// Config.FieldDistance.
	ClearanceM float64
	// TraverseUnknown lets a path cross cells that are not known to be
	// Free, inside the bounding box of the known cells. Default false: a
	// path crosses observed free space only.
	TraverseUnknown bool
}

// DefaultConfig returns the defaults documented on Config.
func DefaultConfig() Config {
	return Config{TargetLoad: 4, SurfaceCells: 2, SightCells: 1, ClearanceM: 0.2}
}

// minCellM keeps cell indices far inside the integer range for any site
// coordinate (spatial.MaxCoordinateM).
const minCellM = 1e-3

// maxSightCells bounds the work of widening a map: each Occupied cell marks
// a disc of this radius at most.
const maxSightCells = 16

func (c Config) withDefaults() (Config, error) {
	d := DefaultConfig()
	if c.TargetLoad == 0 {
		c.TargetLoad = d.TargetLoad
	}
	if c.SurfaceCells == 0 {
		c.SurfaceCells = d.SurfaceCells
	}
	if c.SightCells == 0 {
		c.SightCells = d.SightCells
	}
	if c.ClearanceM == 0 {
		c.ClearanceM = d.ClearanceM
	}
	if !finite(c.CellM, c.TargetLoad, c.SurfaceCells, c.SightCells, c.ClearanceM) || c.SightCells > maxSightCells ||
		(c.CellM != 0 && c.CellM < minCellM) || c.TargetLoad < 0 || c.SurfaceCells < 0 || c.SightCells < 0 || c.ClearanceM < 0 {
		return c, ErrConfig
	}
	return c, nil
}

// World is what a query runs over. The Index keeps the grids by reference:
// they must not be mutated while the Index is in use.
type World struct {
	// Entities are the rows. Region entities with a polygon extent are the
	// named regions.
	Entities []spatial.Entity
	// Grids holds the occupancy grid of each floor that has one. Visibility
	// and path distance need it.
	Grids map[spatial.FloorID]*grid.Grid
}

// Index is an immutable snapshot of a World with a uniform-grid index of the
// rows of each floor. It is safe for concurrent use.
//
// ponytail: a change to any row means a new Index (With: linear, about 1 ms
// per 10,000 rows), and a change to a grid means New. Add in-place updates if
// rebuilding ever shows up in a profile.
type Index struct {
	cfg     Config
	ents    []spatial.Entity
	floors  []*floorIndex // ascending floor id
	byFloor map[spatial.FloorID]*floorIndex
	byID    map[string][]int32
	regions []int32 // rows of the region entities that have a polygon
	maps    map[spatial.FloorID]*floorMap
}

// floorMap is the occupancy grid of a floor with what sight and path derive
// from it, each built on first use.
type floorMap struct {
	grid   *grid.Grid
	sight  func() (*grid.Grid, error) // the grid a sight line is cast through
	raster func() (*raster, error)    // the cells a path may cross
}

// floorIndex buckets the rows of one floor by position: cell (ix, iz) covers
// [x0 + ix·cell, x0 + (ix+1)·cell) × [z0 + iz·cell, z0 + (iz+1)·cell). Rows
// of cell c are items[start[c]:start[c+1]], ascending.
//
// ponytail: uniform cells over the bounding box. A row far from the rest, or
// rows packed into a small part of the floor, leave most rows in a few cells
// and a query degrades toward a scan of the floor (still exact, and a scan of
// 10,000 rows is well under a millisecond). Size the cells from quantiles, or
// use a k-d tree, if such worlds turn up at a scale where that matters.
type floorIndex struct {
	id         spatial.FloorID
	x0, z0     float64
	cell       float64
	cols, rows int
	start      []int32
	items      []int32
}

// New validates the world and builds its index.
func New(w World, cfg Config) (*Index, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	maps := map[spatial.FloorID]*floorMap{}
	for floor, g := range w.Grids {
		if g == nil {
			return nil, invalid("nil grid for floor %q", floor)
		}
		maps[floor] = &floorMap{
			grid:   g,
			sight:  sync.OnceValues(func() (*grid.Grid, error) { return widen(g, cfg.SightCells) }),
			raster: sync.OnceValues(func() (*raster, error) { return newRaster(g, cfg) }),
		}
	}
	return build(w.Entities, cfg, maps)
}

// With returns an Index over other rows and the same grids and Config. What
// sight and path derived from the grids so far (several milliseconds of work
// per floor of an office) is shared, so this is the way to follow entities
// that change faster than the map does.
func (ix *Index) With(entities []spatial.Entity) (*Index, error) {
	return build(entities, ix.cfg, ix.maps)
}

func build(entities []spatial.Entity, cfg Config, maps map[spatial.FloorID]*floorMap) (*Index, error) {
	if len(entities) > math.MaxInt32 {
		return nil, invalid("%d entities", len(entities))
	}
	ix := &Index{
		cfg: cfg, ents: slices.Clone(entities), maps: maps,
		byFloor: map[spatial.FloorID]*floorIndex{}, byID: map[string][]int32{},
	}
	rows := map[spatial.FloorID][]int32{}
	// Most IDs identify one row. Give those buckets a shared backing array
	// instead of allocating a separate one-element slice for every entity.
	// Cap each slice at one so duplicate IDs cannot overwrite another bucket.
	ids := make([]int32, len(ix.ents))
	for i := range ix.ents {
		e := &ix.ents[i]
		if err := e.Validate(); err != nil {
			return nil, invalid("entity %d (%s): %v", i, e.Key(), err)
		}
		rows[e.Floor] = append(rows[e.Floor], int32(i))
		if existing := ix.byID[e.ID]; len(existing) == 0 {
			ids[i] = int32(i)
			ix.byID[e.ID] = ids[i : i+1 : i+1]
		} else {
			ix.byID[e.ID] = append(existing, int32(i))
		}
		if e.Kind == spatial.EntityRegion && len(e.Extent.Polygon) > 0 {
			ix.regions = append(ix.regions, int32(i))
		}
	}
	for floor, r := range rows {
		f := newFloorIndex(floor, ix.ents, r, cfg)
		ix.floors, ix.byFloor[floor] = append(ix.floors, f), f
	}
	slices.SortFunc(ix.floors, func(a, b *floorIndex) int { return strings.Compare(string(a.id), string(b.id)) })
	return ix, nil
}

// Len is the number of rows.
func (ix *Index) Len() int { return len(ix.ents) }

func newFloorIndex(id spatial.FloorID, ents []spatial.Entity, rows []int32, cfg Config) *floorIndex {
	lo, hi := ents[rows[0]].Position.XZ(), ents[rows[0]].Position.XZ()
	for _, r := range rows {
		p := ents[r].Position
		lo.X, lo.Z, hi.X, hi.Z = math.Min(lo.X, p.X), math.Min(lo.Z, p.Z), math.Max(hi.X, p.X), math.Max(hi.Z, p.Z)
	}
	w, h := hi.X-lo.X, hi.Z-lo.Z
	f := &floorIndex{id: id, x0: lo.X, z0: lo.Z, cell: cfg.CellM}
	if f.cell == 0 {
		switch n := float64(len(rows)); {
		case w > 0 && h > 0:
			f.cell = math.Sqrt(w * h * cfg.TargetLoad / n)
		case w > 0 || h > 0: // rows on a line
			f.cell = math.Max(w, h) * cfg.TargetLoad / n
		}
		f.cell = math.Max(f.cell, minCellM)
	}
	for limit := 4*float64(len(rows)) + 64; ; f.cell *= 2 {
		cols, rws := math.Floor(w/f.cell)+1, math.Floor(h/f.cell)+1
		if cols*rws <= limit {
			f.cols, f.rows = int(cols), int(rws)
			break
		}
	}
	// Counting sort by cell keeps each cell's rows ascending.
	f.start = make([]int32, f.cols*f.rows+1)
	cells := make([]int32, len(rows))
	for i, r := range rows {
		p := ents[r].Position
		cells[i] = int32(f.along(p.Z, f.z0)*f.cols + f.along(p.X, f.x0))
		f.start[cells[i]+1]++
	}
	for c := range f.cols * f.rows {
		f.start[c+1] += f.start[c]
	}
	f.items = make([]int32, len(rows))
	next := slices.Clone(f.start[:f.cols*f.rows])
	for i, r := range rows {
		f.items[next[cells[i]]] = r
		next[cells[i]]++
	}
	return f
}

// along is the cell index of coordinate v on an axis that starts at origin.
// It is monotone in v, which is what makes pruning by cell exact: a row
// between two coordinates lies in a cell between theirs. For an indexed row
// the result is within the table; a query coordinate may fall outside.
func (f *floorIndex) along(v, origin float64) int { return int(math.Floor((v - origin) / f.cell)) }

// span is the inclusive range of cell indices on one axis of n cells that
// can hold a coordinate in [a, b]; either bound may be infinite, and a > b
// is the empty interval.
func (f *floorIndex) span(a, b, origin float64, n int) (lo, hi int, ok bool) {
	fa, fb := math.Floor((a-origin)/f.cell), math.Floor((b-origin)/f.cell)
	if !(a <= b && fb >= 0 && fa <= float64(n-1)) {
		return 0, 0, false
	}
	return int(math.Max(fa, 0)), int(math.Min(fb, float64(n-1))), true
}

// rect is an axis-aligned search window; the zero value is not valid, start
// from everywhere.
type rect struct{ x0, z0, x1, z1 float64 }

var everywhere = rect{math.Inf(-1), math.Inf(-1), math.Inf(1), math.Inf(1)}

func (r rect) intersect(o rect) rect {
	return rect{math.Max(r.x0, o.x0), math.Max(r.z0, o.z0), math.Min(r.x1, o.x1), math.Min(r.z1, o.z1)}
}

func (r rect) union(o rect) rect {
	return rect{math.Min(r.x0, o.x0), math.Min(r.z0, o.z0), math.Max(r.x1, o.x1), math.Max(r.z1, o.z1)}
}

// around is the window that holds every point within radius of p, padded
// against the rounding of the distance test it prunes for.
func around(p geom.Vec2, radius float64) rect {
	pad := radius + 1e-9*(1+math.Abs(p.X)+math.Abs(p.Z)+radius)
	return rect{p.X - pad, p.Z - pad, p.X + pad, p.Z + pad}
}

// bounds is the padded window of a polygon.
func bounds(poly []geom.Vec2) rect {
	r := rect{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
	for _, v := range poly {
		r = r.union(around(v, 0))
	}
	return r
}

// place is a resolved Ref.
type place struct {
	row   int32 // -1 for a point
	floor spatial.FloorID
	shape shape
	unc   float64 // position uncertainty, metres
}

func (ix *Index) resolve(r Ref, q *Query) (place, error) {
	if r.Point != nil {
		floor := r.Floor
		if floor == "" {
			floor = q.Floor
		}
		if floor == "" {
			if len(ix.floors) != 1 {
				return place{}, fmt.Errorf("%w: point (%g, %g) names no floor and the world has %d", ErrNoFloor, r.Point.X, r.Point.Z, len(ix.floors))
			}
			floor = ix.floors[0].id
		}
		return place{row: -1, floor: floor, shape: shape{pos: *r.Point}}, nil
	}
	rows := ix.byID[r.Entity]
	if kind, id, ok := strings.Cut(r.Entity, "/"); len(rows) == 0 && ok && spatial.EntityKind(kind).Valid() {
		for _, row := range ix.byID[id] {
			if ix.ents[row].Kind == spatial.EntityKind(kind) {
				rows = append(rows, row)
			}
		}
	}
	switch len(rows) {
	case 0:
		return place{}, fmt.Errorf("%w: %q", ErrUnknownEntity, r.Entity)
	case 1:
		e := &ix.ents[rows[0]]
		return place{row: rows[0], floor: e.Floor, shape: shapeOf(e), unc: e.UncertaintyM}, nil
	}
	keys := make([]string, len(rows))
	for i, row := range rows {
		keys[i] = ix.ents[row].Key()
	}
	return place{}, fmt.Errorf("%w: %q matches %d rows (%s)", ErrAmbiguous, r.Entity, len(rows), strings.Join(keys, ", "))
}
