package query

import (
	"cmp"
	"fmt"
	"math"
	"slices"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
)

// Match is one row of an answer.
type Match struct {
	// Entity is the row. It shares its polygon and attributes with the
	// World the Index was built from: read only.
	Entity spatial.Entity `json:"entity"`
	// DistanceM is the planar distance from the origin, and
	// DistanceSigmaM the root sum square of the position uncertainties of
	// the entity and of the origin. Both are zero without an origin.
	DistanceM      float64 `json:"distance_m"`
	DistanceSigmaM float64 `json:"distance_sigma_m"`
	// PathM is the path distance from the origin when the query asked for
	// one (PathWithinM or OrderPath) and Reachable is true.
	PathM     float64 `json:"path_m,omitempty"`
	Reachable bool    `json:"reachable,omitempty"`
}

// Result is the answer to a Query.
type Result struct {
	Matches []Match `json:"matches"`
	// Total is the number of rows that matched before Limit.
	Total int `json:"total"`
	// Examined is the number of rows the filters were evaluated on: all
	// of them for a query the index cannot narrow, few for one it can.
	Examined int `json:"examined"`
}

// cand is a row under evaluation.
type cand struct {
	row       int32
	dist      float64
	path      int64
	reachable bool
}

type circle struct {
	c geom.Vec2
	r float64
}

// plan is a Query resolved against an Index.
type plan struct {
	q        *Query
	empty    bool // the filters contradict each other: nothing can match
	floor    spatial.FloorID
	oneFloor bool
	origin   *place
	exclude  []int32   // rows named by the query
	circles  []circle  // within, near, inside a round footprint
	sets     [][]int32 // region rows: a match lies in one of each set
	sight    []shape
	grid     *grid.Grid // the floor's map widened for sight
	margin   float64    // how far from a footprint a sight line starts
	field    *pathField
	window   rect
}

func (p *plan) on(floor spatial.FloorID) {
	if p.oneFloor && p.floor != floor {
		p.empty = true
	}
	p.floor, p.oneFloor = floor, true
}

func (p *plan) use(pl place) {
	p.on(pl.floor)
	if pl.row >= 0 {
		p.exclude = append(p.exclude, pl.row)
	}
}

func (ix *Index) plan(q *Query) (*plan, error) {
	p := &plan{q: q, window: everywhere}
	if q.Floor != "" {
		p.on(q.Floor)
	}
	if q.Origin != nil {
		o, err := ix.resolve(*q.Origin, q)
		if err != nil {
			return nil, err
		}
		p.origin = &o
		p.use(o)
		if q.WithinM > 0 {
			p.circles = append(p.circles, circle{o.shape.pos, q.WithinM})
		}
		if q.Visible {
			p.sight = append(p.sight, o.shape)
		}
	}
	for _, rel := range q.Relations {
		t, err := ix.resolve(rel.To, q)
		if err != nil {
			return nil, err
		}
		p.use(t)
		switch rel.Kind {
		case RelNear:
			p.circles = append(p.circles, circle{t.shape.pos, rel.DistanceM})
		case RelVisibleFrom:
			p.sight = append(p.sight, t.shape)
		case RelInside:
			switch {
			case len(t.shape.poly) > 0:
				p.sets = append(p.sets, []int32{t.row})
			case t.shape.r > 0:
				p.circles = append(p.circles, circle{t.shape.pos, t.shape.r})
			default:
				return nil, fmt.Errorf("%w: %q", ErrNoExtent, rel.To.text())
			}
		case RelSameRegion:
			if len(ix.regions) == 0 {
				return nil, fmt.Errorf("%w: the world holds no regions", ErrUnknownRegion)
			}
			var set []int32
			for _, r := range ix.regions {
				if e := &ix.ents[r]; e.Floor == t.floor && geom.PointInPolygon(t.shape.pos, e.Extent.Polygon) {
					set = append(set, r)
				}
			}
			p.sets = append(p.sets, set)
		}
	}
	if q.Region != "" {
		var byID, byLabel []int32
		for _, r := range ix.regions {
			switch e := &ix.ents[r]; q.Region {
			case e.ID:
				byID = append(byID, r)
			case e.Label:
				byLabel = append(byLabel, r)
			}
		}
		if len(byID) == 0 {
			byID = byLabel
		}
		if len(byID) == 0 {
			return nil, fmt.Errorf("%w: %q", ErrUnknownRegion, q.Region)
		}
		p.sets = append(p.sets, byID)
	}
	for _, set := range p.sets {
		p.empty = p.empty || len(set) == 0
	}
	wantPath := q.PathWithinM > 0 || q.Order == OrderPath
	p.narrow()
	if p.empty || (len(p.sight) == 0 && !wantPath) {
		return p, nil
	}
	// Sight and path come with an origin or a relation, so one floor is set.
	m := ix.maps[p.floor]
	if m == nil {
		return nil, fmt.Errorf("%w: %q", ErrNoGrid, p.floor)
	}
	if len(p.sight) > 0 {
		g, err := m.sight()
		if err != nil {
			return nil, err
		}
		p.grid, p.margin = g, (ix.cfg.SurfaceCells+ix.cfg.SightCells)*g.CellSize()
	}
	if wantPath {
		r, err := m.raster()
		if err != nil {
			return nil, err
		}
		limit := int64(unreachable)
		if q.PathWithinM > 0 {
			limit = units(q.PathWithinM/r.cell) + 2
			// A path is no shorter than the straight line, up to the
			// rounding of its legs.
			p.window = p.window.intersect(around(p.origin.shape.pos, q.PathWithinM+r.cell))
		}
		// A point is within √2/2 cells of the centre of its own cell, so
		// 0.75 cells more than the surface always reaches that cell.
		tolerance := float64((ix.cfg.SurfaceCells+0.75)*r.cell) + ix.cfg.ClearanceM
		p.field = r.field(p.origin.shape, tolerance, limit)
	}
	return p, nil
}

// narrow intersects the search window with every filter that bounds it.
func (p *plan) narrow() {
	if b := p.q.Box; b != nil {
		p.window = p.window.intersect(rect{b.Min.X, b.Min.Z, b.Max.X, b.Max.Z})
	}
	if len(p.q.Polygon) > 0 {
		p.window = p.window.intersect(bounds(p.q.Polygon))
	}
	for _, c := range p.circles {
		p.window = p.window.intersect(around(c.c, c.r))
	}
}

// match evaluates every filter on a row, cheapest first. gate, when set, is
// asked before the map is consulted whether the row can still matter.
func (ix *Index) match(p *plan, row int32, gate func(cand) bool) (cand, bool) {
	q, e := p.q, &ix.ents[row]
	if slices.Contains(p.exclude, row) ||
		(len(q.Kinds) > 0 && !slices.Contains(q.Kinds, e.Kind)) ||
		(len(q.Classes) > 0 && !slices.Contains(q.Classes, e.Class)) {
		return cand{}, false
	}
	if !q.SeenSince.IsZero() || !q.SeenBefore.IsZero() || !q.ValidAt.IsZero() {
		at := e.ObservedAt
		if at.IsZero() ||
			(!q.SeenSince.IsZero() && at.Before(q.SeenSince)) ||
			(!q.SeenBefore.IsZero() && !at.Before(q.SeenBefore)) ||
			(!q.ValidAt.IsZero() && (at.After(q.ValidAt) || (!e.ValidUntil.IsZero() && !q.ValidAt.Before(e.ValidUntil)))) {
			return cand{}, false
		}
	}
	pos := e.Position.XZ()
	if b := q.Box; b != nil && (pos.X < b.Min.X || pos.X > b.Max.X || pos.Z < b.Min.Z || pos.Z > b.Max.Z) {
		return cand{}, false
	}
	if len(q.Polygon) > 0 && !geom.PointInPolygon(pos, q.Polygon) {
		return cand{}, false
	}
	for _, c := range p.circles {
		if c.c.Dist(pos) > c.r {
			return cand{}, false
		}
	}
sets:
	for _, set := range p.sets {
		for _, r := range set {
			if region := &ix.ents[r]; region.Floor == e.Floor && geom.PointInPolygon(pos, region.Extent.Polygon) {
				continue sets
			}
		}
		return cand{}, false
	}
	c := cand{row: row}
	if p.origin != nil {
		c.dist = p.origin.shape.pos.Dist(pos)
	}
	if gate != nil && !gate(c) {
		return cand{}, false
	}
	for _, from := range p.sight {
		if !visible(p.grid, p.margin, from, shapeOf(e)) {
			return cand{}, false
		}
	}
	if p.field != nil {
		c.path, c.reachable = p.field.to(shapeOf(e))
		if q.PathWithinM > 0 && (!c.reachable || p.field.metres(c.path) > q.PathWithinM) {
			return cand{}, false
		}
	}
	return c, true
}

// compare orders candidates by the key, then by spatial.CompareEntities,
// then by row.
func (ix *Index) compare(order Order, descending bool) func(a, b cand) int {
	return func(a, b cand) int {
		ea, eb := &ix.ents[a.row], &ix.ents[b.row]
		key := 0
		switch order {
		case OrderDistance:
			key = cmp.Compare(a.dist, b.dist)
		case OrderTime:
			key = ea.ObservedAt.Compare(eb.ObservedAt)
		case OrderPath:
			if a.reachable != b.reachable { // unreachable last, either direction
				if a.reachable {
					return -1
				}
				return 1
			}
			key = cmp.Compare(a.path, b.path)
		}
		if descending {
			key = -key
		}
		if key != 0 {
			return key
		}
		// The fields of spatial.CompareEntities, read in place: it takes its
		// arguments by value, a quarter of a kilobyte per call in a sort.
		if c := cmp.Compare(ea.Kind, eb.Kind); c != 0 {
			return c
		}
		if c := cmp.Compare(ea.ID, eb.ID); c != 0 {
			return c
		}
		if c := cmp.Compare(ea.Floor, eb.Floor); c != 0 {
			return c
		}
		if c := ea.ObservedAt.Compare(eb.ObservedAt); c != 0 {
			return c
		}
		return cmp.Or(cmp.Compare(ea.Source, eb.Source), cmp.Compare(a.row, b.row))
	}
}

// ringSearchRatio: "nearest K" searches outward from the origin when the
// floor holds more than this many rows per row asked for, and scans the floor
// otherwise (a ring search that ends up reading most cells is the slower of
// the two). Both give the same answer.
const ringSearchRatio = 8

// Run answers q. Errors wrap ErrInvalid for a query that fails Validate, and
// ErrUnknownEntity, ErrAmbiguous, ErrUnknownRegion, ErrNoFloor, ErrNoGrid or
// ErrNoExtent for one this world cannot answer; see the package comment for
// the semantics of each filter.
func (ix *Index) Run(q Query) (Result, error) {
	if err := q.Validate(); err != nil {
		return Result{}, err
	}
	p, err := ix.plan(&q)
	if err != nil {
		return Result{}, err
	}
	var res Result
	var found []cand
	nearest := ix.compare(OrderDistance, false)
	switch f := ix.byFloor[p.floor]; {
	case p.empty:
	case q.Nearest > 0 && f != nil && q.Nearest < len(f.items)/ringSearchRatio:
		found = ix.nearest(p, f, &res)
	case p.oneFloor:
		if f != nil {
			found = ix.scan(p, f, found, &res)
		}
	default:
		for _, f := range ix.floors {
			found = ix.scan(p, f, found, &res)
		}
	}
	if q.Nearest > 0 && len(found) > q.Nearest {
		slices.SortFunc(found, nearest)
		found = found[:q.Nearest]
	}
	order := q.Order
	if order == OrderDefault {
		order = OrderKey
		if p.origin != nil {
			order = OrderDistance
		}
	}
	slices.SortFunc(found, ix.compare(order, q.Descending))
	res.Total = len(found)
	if q.Limit > 0 && len(found) > q.Limit {
		found = found[:q.Limit]
	}
	res.Matches = make([]Match, len(found))
	for i, c := range found {
		m := Match{Entity: ix.ents[c.row]}
		if p.origin != nil {
			m.DistanceM, m.DistanceSigmaM = c.dist, math.Hypot(m.Entity.UncertaintyM, p.origin.unc)
		}
		if c.reachable {
			m.PathM, m.Reachable = p.field.metres(c.path), true
		}
		res.Matches[i] = m
	}
	return res, nil
}

// scan evaluates the rows of one floor that lie in the plan's window.
func (ix *Index) scan(p *plan, f *floorIndex, found []cand, res *Result) []cand {
	w := p.window
	for _, set := range p.sets {
		box := rect{math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)}
		for _, r := range set {
			if e := &ix.ents[r]; e.Floor == f.id {
				box = box.union(bounds(e.Extent.Polygon))
			}
		}
		w = w.intersect(box)
	}
	ix0, ix1, okX := f.span(w.x0, w.x1, f.x0, f.cols)
	iz0, iz1, okZ := f.span(w.z0, w.z1, f.z0, f.rows)
	if !okX || !okZ {
		return found
	}
	for iz := iz0; iz <= iz1; iz++ {
		for _, row := range f.items[f.start[iz*f.cols+ix0]:f.start[iz*f.cols+ix1+1]] {
			res.Examined++
			if c, ok := ix.match(p, row, nil); ok {
				found = append(found, c)
			}
		}
	}
	return found
}

// nearest finds the Query.Nearest closest matching rows of the origin's
// floor by growing square rings of cells around the origin. A row in ring r
// is more than (r−1) cells away, so the search stops at the first ring that
// cannot beat the K-th best found (ties included: it stops only when
// strictly farther).
func (ix *Index) nearest(p *plan, f *floorIndex, res *Result) []cand {
	k, origin := p.q.Nearest, p.origin.shape.pos
	closer := ix.compare(OrderDistance, false)
	best := make([]cand, 0, k+1) // ascending
	gate := func(c cand) bool { return len(best) < k || closer(c, best[k-1]) < 0 }
	visit := func(ix0, ix1, iz int) {
		for _, row := range f.items[f.start[iz*f.cols+ix0]:f.start[iz*f.cols+ix1+1]] {
			res.Examined++
			if c, ok := ix.match(p, row, gate); ok {
				at, _ := slices.BinarySearchFunc(best, c, closer)
				best = slices.Insert(best, at, c)
				best = best[:min(len(best), k)]
			}
		}
	}
	bound := math.Inf(1)
	if p.q.WithinM > 0 {
		bound = p.q.WithinM
	}
	cx, cz := f.along(origin.X, f.x0), f.along(origin.Z, f.z0)
	first := max(0, -cx, cx-(f.cols-1), -cz, cz-(f.rows-1))
	last := max(cx, f.cols-1-cx, cz, f.rows-1-cz)
	for r := first; r <= last; r++ {
		atLeast := float64(r-1)*f.cell - 1e-9*(1+math.Abs(origin.X)+math.Abs(origin.Z)+math.Abs(f.x0)+math.Abs(f.z0)+float64(r)*f.cell)
		if atLeast > bound || (len(best) == k && atLeast > best[k-1].dist) {
			break
		}
		if x0, x1 := max(cx-r, 0), min(cx+r, f.cols-1); x0 <= x1 {
			if iz := cz - r; iz >= 0 && iz < f.rows {
				visit(x0, x1, iz)
			}
			if iz := cz + r; r > 0 && iz >= 0 && iz < f.rows {
				visit(x0, x1, iz)
			}
		}
		for iz := max(cz-r+1, 0); iz <= min(cz+r-1, f.rows-1); iz++ {
			if x := cx - r; x >= 0 && x < f.cols {
				visit(x, x, iz)
			}
			if x := cx + r; r > 0 && x >= 0 && x < f.cols {
				visit(x, x, iz)
			}
		}
	}
	return best
}
