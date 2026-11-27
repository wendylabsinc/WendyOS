package query

import (
	"fmt"
	"math"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
)

// shape is a position with its planar footprint: a circle of radius r, a
// polygon, or a point.
type shape struct {
	pos  geom.Vec2
	r    float64
	poly []geom.Vec2
}

func shapeOf(e *spatial.Entity) shape {
	return shape{pos: e.Position.XZ(), r: e.Extent.RadiusM, poly: e.Extent.Polygon}
}

// reach is how far the footprint extends from pos along the unit direction u
// toward a point at distance d: where a line leaving pos last crosses the
// boundary. A line that never leaves the polygon is inside it all the way.
func (s shape) reach(u geom.Vec2, d float64, end geom.Vec2) float64 {
	if len(s.poly) == 0 {
		return s.r
	}
	far := 0.0
	for i, j := 0, len(s.poly)-1; i < len(s.poly); j, i = i, i+1 {
		if t, ok := geom.RaySegment(s.pos, u, geom.Segment{A: s.poly[j], B: s.poly[i]}); ok && t <= d && t > far {
			far = t
		}
	}
	if far == 0 && geom.PointInPolygon(end, s.poly) {
		return d
	}
	return far
}

// gap is the distance from p to the footprint, zero inside it.
func (s shape) gap(p geom.Vec2) float64 {
	if len(s.poly) == 0 {
		return math.Max(0, p.Dist(s.pos)-s.r)
	}
	if geom.PointInPolygon(p, s.poly) {
		return 0
	}
	best := math.Inf(1)
	for i, j := 0, len(s.poly)-1; i < len(s.poly); j, i = i, i+1 {
		best = math.Min(best, geom.Segment{A: s.poly[j], B: s.poly[i]}.DistanceTo(p))
	}
	return best
}

// widen returns a grid whose Occupied cells are those of g and every cell
// within cells of one, centre to centre: casting a ray through it keeps a
// sight line that far from the map's obstacles. Below one cell it is g.
func widen(g *grid.Grid, cells float64) (*grid.Grid, error) {
	if cells < 1 {
		return g, nil
	}
	out, err := grid.New(g.Config())
	if err != nil {
		return nil, err
	}
	n, level := int32(cells), g.Config().Max
	g.Each(func(c grid.Cell, _ float64) bool {
		if g.State(c) != grid.Occupied {
			return true
		}
		for dz := -n; dz <= n && err == nil; dz++ {
			for dx := -n; dx <= n && err == nil; dx++ {
				if float64(dx*dx+dz*dz) <= cells*cells {
					err = out.Set(grid.Cell{IX: c.IX + dx, IZ: c.IZ + dz}, level)
				}
			}
		}
		return err == nil
	})
	return out, err
}

// visible reports whether the sight line from a to b is clear of the
// Occupied cells of g (the floor's map after widen) outside both footprints
// grown by margin.
//
// ponytail: planar, blind within margin of a footprint, and only as good as
// the map. An entity standing against the far side of a wall thinner than
// margin reads as visible, and a hole in a mapped wall wider than
// 2·SightCells cells is a window. The fix is a map that knows which cells
// belong to which entity (a per-entity mask, or occupancy with height) and a
// mapper that does not erode walls seen at grazing incidence.
func visible(g *grid.Grid, margin float64, a, b shape) bool {
	d := a.pos.Dist(b.pos)
	if d == 0 {
		return true
	}
	u := b.pos.Sub(a.pos).Scale(1 / d)
	sa := a.reach(u, d, b.pos) + margin
	sb := b.reach(u.Scale(-1), d, a.pos) + margin
	if sa+sb >= d {
		return true
	}
	_, _, hit := g.Raycast(a.pos.Add(u.Scale(sa)), u.Angle(), d-sa-sb)
	return !hit
}

// Path lengths are integers so that sums are exact whatever the order.
const (
	unitsPerCell  = 1_000_000 // one straight step
	diagonalUnits = 1_414_214 // one diagonal step, √2 rounded up
	unreachable   = math.MaxInt64
	// maxRasterCells bounds the dense copy of a floor's grid, as the grid
	// package bounds its own rasters.
	maxRasterCells = 1 << 26
)

// units converts a length in cells to path units.
func units(cells float64) int64 { return int64(math.Round(cells * unitsPerCell)) }

// raster is the dense traversability map of one floor over the bounding box
// of its known cells.
type raster struct {
	origin grid.Cell // cell at index 0
	w, h   int
	cell   float64
	free   []bool
}

func newRaster(g *grid.Grid, cfg Config) (*raster, error) {
	r := &raster{cell: g.CellSize()}
	lo, hi, ok := g.Bounds()
	if !ok {
		return r, nil
	}
	r.origin, r.w, r.h = lo, int(hi.IX-lo.IX)+1, int(hi.IZ-lo.IZ)+1
	if r.w*r.h > maxRasterCells {
		return nil, fmt.Errorf("%w: %d x %d cells", grid.ErrTooLarge, r.w, r.h)
	}
	var field *grid.Field
	if cfg.ClearanceM > 0 {
		f, err := g.Field()
		if err != nil {
			return nil, err
		}
		if cfg.ClearanceM > f.MaxDistance {
			return nil, fmt.Errorf("%w: clearance %g m exceeds the grid's field distance %g m", ErrConfig, cfg.ClearanceM, f.MaxDistance)
		}
		field = f
	}
	r.free = make([]bool, r.w*r.h)
	for i := range r.free {
		c := grid.Cell{IX: lo.IX + int32(i%r.w), IZ: lo.IZ + int32(i/r.w)}
		st := g.State(c)
		r.free[i] = (st == grid.Free || (cfg.TraverseUnknown && st != grid.Occupied)) && (field == nil || field.At(c) >= cfg.ClearanceM)
	}
	return r, nil
}

// snap calls fn for every traversable cell within tolerance of the footprint,
// with the length of the straight leg from the position to the cell centre.
//
// ponytail: the leg is not tested against the map, so a path can end across a
// wall thinner than tolerance from an entity standing against it (the same
// ceiling as visible).
func (r *raster) snap(s shape, tolerance float64, fn func(i int, leg int64)) {
	box := around(s.pos, s.r+tolerance)
	if len(s.poly) > 0 {
		b := bounds(s.poly)
		box = rect{b.x0 - tolerance, b.z0 - tolerance, b.x1 + tolerance, b.z1 + tolerance}
	}
	clamp := func(v float64, origin int32, n int) int {
		return int(math.Max(0, math.Min(float64(n-1), math.Floor(v/r.cell)-float64(origin))))
	}
	if r.w == 0 {
		return
	}
	ix0, ix1 := clamp(box.x0, r.origin.IX, r.w), clamp(box.x1, r.origin.IX, r.w)
	for iz, iz1 := clamp(box.z0, r.origin.IZ, r.h), clamp(box.z1, r.origin.IZ, r.h); iz <= iz1; iz++ {
		for ix := ix0; ix <= ix1; ix++ {
			i := iz*r.w + ix
			if !r.free[i] {
				continue
			}
			c := geom.Vec2{X: (float64(int(r.origin.IX)+ix) + 0.5) * r.cell, Z: (float64(int(r.origin.IZ)+iz) + 0.5) * r.cell}
			if s.gap(c) <= tolerance {
				fn(i, units(c.Dist(s.pos)/r.cell))
			}
		}
	}
}

// pathField holds the path length from one place to every traversable cell.
type pathField struct {
	r         *raster
	tolerance float64
	dist      []int64
}

// field runs Dijkstra from the footprint over cells whose cost stays within
// limit.
//
// ponytail: every query floods the map from its origin as far as limit
// allows (about 40 ms for 400,000 cells with no limit), and paths are
// 8-connected (up to 8.3 % long). Stop the flood once the rows asked for are
// settled, or keep the field per origin if the same origin is asked again;
// switch to an any-angle search (Theta*) if the overestimate matters.
func (r *raster) field(from shape, tolerance float64, limit int64) *pathField {
	f := &pathField{r: r, tolerance: tolerance, dist: make([]int64, len(r.free))}
	for i := range f.dist {
		f.dist[i] = unreachable
	}
	var open minHeap
	r.snap(from, tolerance, func(i int, leg int64) {
		if leg < f.dist[i] && leg <= limit {
			f.dist[i] = leg
			open.push(leg, int32(i))
		}
	})
	for len(open) > 0 {
		cost, i := open.pop()
		if cost > f.dist[i] {
			continue
		}
		x, z := int(i)%r.w, int(i)/r.w
		for dz := -1; dz <= 1; dz++ {
			for dx := -1; dx <= 1; dx++ {
				nx, nz := x+dx, z+dz
				if (dx == 0 && dz == 0) || nx < 0 || nz < 0 || nx >= r.w || nz >= r.h || !r.free[nz*r.w+nx] {
					continue
				}
				step := int64(unitsPerCell)
				if dx != 0 && dz != 0 {
					if !r.free[z*r.w+nx] || !r.free[nz*r.w+x] {
						continue // a diagonal step may not cut a corner
					}
					step = diagonalUnits
				}
				if j, nc := nz*r.w+nx, cost+step; nc < f.dist[j] && nc <= limit {
					f.dist[j] = nc
					open.push(nc, int32(j))
				}
			}
		}
	}
	return f
}

// to is the path length to the footprint in units.
func (f *pathField) to(s shape) (int64, bool) {
	best := int64(unreachable)
	f.r.snap(s, f.tolerance, func(i int, leg int64) {
		if d := f.dist[i]; d != unreachable && d+leg < best {
			best = d + leg
		}
	})
	return best, best != unreachable
}

func (f *pathField) metres(u int64) float64 { return float64(u) * f.r.cell / unitsPerCell }

// minHeap is a binary heap of (cost, cell) ordered by cost.
type minHeap []heapItem

type heapItem struct {
	cost int64
	i    int32
}

func (h *minHeap) push(cost int64, i int32) {
	*h = append(*h, heapItem{cost, i})
	s := *h
	for c := len(s) - 1; c > 0; {
		p := (c - 1) / 2
		if s[p].cost <= s[c].cost {
			break
		}
		s[p], s[c] = s[c], s[p]
		c = p
	}
}

func (h *minHeap) pop() (int64, int32) {
	s := *h
	top, last := s[0], len(s)-1
	s[0] = s[last]
	s = s[:last]
	for p := 0; ; {
		c := 2*p + 1
		if c >= last {
			break
		}
		if c+1 < last && s[c+1].cost < s[c].cost {
			c++
		}
		if s[p].cost <= s[c].cost {
			break
		}
		s[p], s[c] = s[c], s[p]
		p = c
	}
	*h = s
	return top.cost, top.i
}
