package multifloor

import (
	"container/heap"
	"errors"
	"fmt"
	"math"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
)

// ErrNoRoute reports that no route exists: an end is not in mapped free
// space, or the floors are not joined by open portals that can be reached.
var ErrNoRoute = errors.New("multifloor: no route")

// RouteConfig tunes PlanRoute. A zero field takes its default.
type RouteConfig struct {
	// ClearanceM is the distance a path keeps from occupied cells: the radius
	// of what travels. It cannot exceed the maps' grid.Config.FieldDistance.
	// Default 0.25 m.
	ClearanceM float64
	// SnapM is how far a place or a portal end may lie from the nearest
	// passable cell and still be used. Default 1 m.
	SnapM float64
	// StairsFactor is the cost of a metre of stairs in metres of level
	// travel. Default 2.
	StairsFactor float64
	// LiftPenaltyM is the fixed cost of a lift ride (the wait and the doors)
	// in metres of level travel, added to its height. Default 15 m.
	LiftPenaltyM float64
}

func (c RouteConfig) withDefaults() RouteConfig {
	if c.ClearanceM == 0 {
		c.ClearanceM = 0.25
	}
	if c.SnapM == 0 {
		c.SnapM = 1
	}
	if c.StairsFactor == 0 {
		c.StairsFactor = 2
	}
	if c.LiftPenaltyM == 0 {
		c.LiftPenaltyM = 15
	}
	return c
}

// Place is a position on a floor.
type Place struct {
	Floor    spatial.FloorID
	Position geom.Vec2
}

// Leg is the part of a route on one floor.
type Leg struct {
	Floor spatial.FloorID
	// Path runs from where the leg starts to where it ends, with a vertex at
	// every change of direction.
	Path    []geom.Vec2
	LengthM float64
	// Portal is the id of the portal taken at the end of the leg; it is empty
	// on the last leg.
	Portal string
}

// Route is the cheapest way between two places.
type Route struct {
	Legs []Leg
	// LengthM is the distance travelled: the legs plus the length of every
	// portal between them.
	LengthM float64
	// Cost is LengthM with stairs weighted by StairsFactor and LiftPenaltyM
	// added per lift ride. It is what the route minimises.
	Cost float64
}

// maxRouteCells bounds the dense raster of one floor (about 400 x 400 m at
// 5 cm cells).
const maxRouteCells = 1 << 26

// PlanRoute returns the cheapest route between two places given one occupancy
// grid per floor and the portals that join the floors. Within a floor it
// walks cells that are Free and at least ClearanceM from an occupied cell, so
// closed doors and unmapped space block it. Between floors it uses the
// portals whose two ends are on different floors and whose State is not
// closed; portals inside one floor are ignored, since an open door is already
// free space in the map. The grids are only read.
//
// It returns an error wrapping ErrNoRoute when there is no route.
//
// ponytail: paths step between the eight neighbours of a cell, so a leg is up
// to 8 % longer than the true shortest path; smooth with line-of-sight
// shortcuts if lengths must be exact.
func PlanRoute(maps map[spatial.FloorID]*grid.Grid, portals []spatial.Portal, from, to Place, cfg RouteConfig) (Route, error) {
	cfg = cfg.withDefaults()
	rasters := map[spatial.FloorID]*raster{}
	rasterOf := func(f spatial.FloorID) (*raster, error) {
		if r, ok := rasters[f]; ok {
			return r, nil
		}
		g := maps[f]
		if g == nil {
			rasters[f] = nil
			return nil, nil
		}
		r, err := newRaster(g, cfg.ClearanceM)
		if err != nil {
			return nil, err
		}
		rasters[f] = r
		return r, nil
	}
	type node struct {
		place  Place
		r      *raster
		cell   int
		snap   float64 // metres from the place to the centre of its cell
		portal int     // index into portals, -1 for the two ends of the route
		twin   int
	}
	var nodes []node
	add := func(p Place, portal int) (bool, error) {
		r, err := rasterOf(p.Floor)
		if err != nil || r == nil {
			return false, err
		}
		cell, snap := r.snap(p.Position, cfg.SnapM)
		if cell < 0 {
			return false, nil
		}
		nodes = append(nodes, node{place: p, r: r, cell: cell, snap: snap, portal: portal, twin: -1})
		return true, nil
	}
	for i, p := range []Place{from, to} {
		ok, err := add(p, -1)
		if err != nil {
			return Route{}, err
		}
		if !ok {
			return Route{}, fmt.Errorf("%w: %s (%.2f, %.2f) on floor %q is not in mapped free space", ErrNoRoute, [2]string{"start", "goal"}[i], p.Position.X, p.Position.Z, p.Floor)
		}
	}
	for i, p := range portals {
		if p.A.Floor == p.B.Floor || p.State == spatial.PortalClosed {
			continue
		}
		okA, err := add(Place{p.A.Floor, geom.Vec3FromArray(p.A.PositionM).XZ()}, i)
		if err != nil {
			return Route{}, err
		}
		if !okA {
			continue
		}
		okB, err := add(Place{p.B.Floor, geom.Vec3FromArray(p.B.PositionM).XZ()}, i)
		if err != nil {
			return Route{}, err
		}
		if !okB {
			nodes = nodes[:len(nodes)-1]
			continue
		}
		n := len(nodes)
		nodes[n-2].twin, nodes[n-1].twin = n-1, n-2
	}

	// Dijkstra over the places, with the walks between them found on demand.
	type edge struct {
		from   int
		path   []geom.Vec2 // nil for a portal
		length float64
		cost   float64
	}
	best, via, done := make([]float64, len(nodes)), make([]edge, len(nodes)), make([]bool, len(nodes))
	for i := range best {
		best[i], via[i].from = math.Inf(1), -1
	}
	best[0] = 0
	for {
		u := -1
		for i := range nodes {
			if !done[i] && (u < 0 || best[i] < best[u]) {
				u = i
			}
		}
		if u < 0 || math.IsInf(best[u], 1) {
			return Route{}, fmt.Errorf("%w from floor %q to floor %q", ErrNoRoute, from.Floor, to.Floor)
		}
		if u == 1 {
			break
		}
		done[u] = true
		nu := nodes[u]
		var targets []int
		for v := range nodes {
			if !done[v] && nodes[v].r == nu.r {
				targets = append(targets, nodes[v].cell)
			}
		}
		dist, prev := nu.r.search(nu.cell, targets)
		for v := range nodes {
			nv := nodes[v]
			if done[v] || nv.r != nu.r || math.IsInf(dist[nv.cell], 1) {
				continue
			}
			if length := nu.snap + dist[nv.cell] + nv.snap; best[u]+length < best[v] {
				best[v] = best[u] + length
				via[v] = edge{from: u, path: nu.r.path(prev, nv.cell, nu.place.Position, nv.place.Position), length: length, cost: length}
			}
		}
		if v := nu.twin; v >= 0 && !done[v] {
			length, cost := portalCost(portals[nu.portal], cfg)
			if best[u]+cost < best[v] {
				best[v], via[v] = best[u]+cost, edge{from: u, length: length, cost: cost}
			}
		}
	}

	var chain []int
	for v := 1; v != 0; v = via[v].from {
		chain = append(chain, v)
	}
	var route Route
	for i := len(chain) - 1; i >= 0; i-- {
		v := chain[i]
		e := via[v]
		route.LengthM, route.Cost = route.LengthM+e.length, route.Cost+e.cost
		if e.path == nil {
			route.Legs[len(route.Legs)-1].Portal = portals[nodes[v].portal].ID
			continue
		}
		route.Legs = append(route.Legs, Leg{Floor: nodes[v].place.Floor, Path: e.path, LengthM: e.length})
	}
	return route, nil
}

// portalCost is the length of a portal between floors and its routing cost.
func portalCost(p spatial.Portal, cfg RouteConfig) (length, cost float64) {
	a, b := geom.Vec3FromArray(p.A.PositionM), geom.Vec3FromArray(p.B.PositionM)
	switch p.Kind {
	case spatial.PortalStairs:
		return a.Dist(b), a.Dist(b) * cfg.StairsFactor
	case spatial.PortalLift:
		return a.Dist(b), a.Dist(b) + cfg.LiftPenaltyM
	}
	return a.Dist(b), a.Dist(b)
}

// raster is the passable cells of one floor as a dense window over the grid.
type raster struct {
	g    *grid.Grid
	lo   grid.Cell
	w, h int
	ok   []bool
}

func newRaster(g *grid.Grid, clearance float64) (*raster, error) {
	if clearance > g.Config().FieldDistance {
		return nil, fmt.Errorf("multifloor: clearance %.2f m exceeds the map's field distance %.2f m", clearance, g.Config().FieldDistance)
	}
	r := &raster{g: g}
	lo, hi, ok := g.Bounds()
	if !ok {
		return r, nil
	}
	r.lo, r.w, r.h = lo, int(hi.IX-lo.IX)+1, int(hi.IZ-lo.IZ)+1
	if r.w*r.h > maxRouteCells {
		return nil, fmt.Errorf("multifloor: map spans %dx%d cells: %w", r.w, r.h, grid.ErrTooLarge)
	}
	field, err := g.Field()
	if err != nil {
		return nil, err
	}
	r.ok = make([]bool, r.w*r.h)
	g.Each(func(c grid.Cell, _ float64) bool {
		r.ok[r.index(c)] = g.State(c) == grid.Free && field.At(c) >= clearance
		return true
	})
	return r, nil
}

func (r *raster) index(c grid.Cell) int { return int(c.IZ-r.lo.IZ)*r.w + int(c.IX-r.lo.IX) }

func (r *raster) centre(i int) geom.Vec2 {
	return r.g.Center(grid.Cell{IX: r.lo.IX + int32(i%r.w), IZ: r.lo.IZ + int32(i/r.w)})
}

// snap is the passable cell nearest to p within radius, and its distance; the
// cell is -1 when there is none.
func (r *raster) snap(p geom.Vec2, radius float64) (int, float64) {
	c := r.g.CellAt(p)
	n := int32(math.Ceil(radius / r.g.CellSize()))
	best, at := -1, radius
	for dz := -n; dz <= n; dz++ {
		for dx := -n; dx <= n; dx++ {
			q := grid.Cell{IX: c.IX + dx, IZ: c.IZ + dz}
			x, z := int(q.IX-r.lo.IX), int(q.IZ-r.lo.IZ)
			if x < 0 || z < 0 || x >= r.w || z >= r.h || !r.ok[z*r.w+x] {
				continue
			}
			if d := r.g.Center(q).Dist(p); d < at || (best < 0 && d <= radius) {
				best, at = z*r.w+x, d
			}
		}
	}
	return best, at
}

type queued struct {
	d float64
	i int32
}

type queue []queued

func (q queue) Len() int { return len(q) }
func (q queue) Less(a, b int) bool {
	return q[a].d < q[b].d || (q[a].d == q[b].d && q[a].i < q[b].i)
}
func (q queue) Swap(a, b int) { q[a], q[b] = q[b], q[a] }
func (q *queue) Push(x any)   { *q = append(*q, x.(queued)) }
func (q *queue) Pop() any     { old := *q; x := old[len(old)-1]; *q = old[:len(old)-1]; return x }

// search is Dijkstra over the passable cells from src until every target is
// reached: the walking distance in metres to each cell it settled, and the
// cell before it on the way. A diagonal step needs both cells beside it
// passable, so a path never cuts a corner.
func (r *raster) search(src int, targets []int) (dist []float64, prev []int32) {
	dist, prev = make([]float64, len(r.ok)), make([]int32, len(r.ok))
	for i := range dist {
		dist[i], prev[i] = math.Inf(1), -1
	}
	want := map[int]bool{}
	for _, t := range targets {
		want[t] = true
	}
	settled := make([]bool, len(r.ok))
	step, diag := r.g.CellSize(), r.g.CellSize()*math.Sqrt2
	dist[src] = 0
	q := &queue{{0, int32(src)}}
	for q.Len() > 0 && len(want) > 0 {
		cur := heap.Pop(q).(queued)
		i := int(cur.i)
		if settled[i] {
			continue
		}
		settled[i] = true
		delete(want, i)
		x, z := i%r.w, i/r.w
		for dz := -1; dz <= 1; dz++ {
			for dx := -1; dx <= 1; dx++ {
				nx, nz := x+dx, z+dz
				if (dx == 0 && dz == 0) || nx < 0 || nz < 0 || nx >= r.w || nz >= r.h {
					continue
				}
				j, d := nz*r.w+nx, step
				if !r.ok[j] || settled[j] {
					continue
				}
				if dx != 0 && dz != 0 {
					if !r.ok[z*r.w+nx] || !r.ok[nz*r.w+x] {
						continue
					}
					d = diag
				}
				if nd := cur.d + d; nd < dist[j] {
					dist[j], prev[j] = nd, int32(i)
					heap.Push(q, queued{nd, int32(j)})
				}
			}
		}
	}
	return dist, prev
}

// path is the walk a search found to cell, from the place it started at to
// the place it serves, keeping only the vertices where it turns.
func (r *raster) path(prev []int32, cell int, from, to geom.Vec2) []geom.Vec2 {
	var cells []int
	for i := cell; i >= 0; i = int(prev[i]) {
		cells = append(cells, i)
	}
	out := []geom.Vec2{from}
	for k := len(cells) - 1; k >= 0; k-- {
		if k > 0 && k < len(cells)-1 && cells[k+1]-cells[k] == cells[k]-cells[k-1] {
			continue
		}
		out = append(out, r.centre(cells[k]))
	}
	return append(out, to)
}
