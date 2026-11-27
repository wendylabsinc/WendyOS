package query

import (
	"errors"
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
)

// This file is the reference the index is checked against: a second
// implementation of the semantics in the package comment that shares nothing
// with the package but the foundation primitives (geom, grid) and the
// exported model. It visits every row for every query, has no index, no
// window, no early exit, sorts everything, and finds paths by relaxing the
// whole map to a fixed point instead of running Dijkstra.

type bruteRow struct {
	row       int
	dist      float64
	path      int64
	reachable bool
}

type bruteAnswer struct {
	rows  []bruteRow
	total int
	errs  []error // every refusal that applies; the index may report any one
}

type brutePlace struct {
	row   int // -1 for a point
	floor spatial.FloorID
	pos   geom.Vec2
	ext   spatial.Extent
}

func bruteFloors(w World) []spatial.FloorID {
	seen := map[spatial.FloorID]bool{}
	var out []spatial.FloorID
	for _, e := range w.Entities {
		if !seen[e.Floor] {
			seen[e.Floor] = true
			out = append(out, e.Floor)
		}
	}
	return out
}

func bruteResolve(w World, q Query, r Ref) (brutePlace, error) {
	if r.Point != nil {
		floor := r.Floor
		if floor == "" {
			floor = q.Floor
		}
		if floor == "" {
			floors := bruteFloors(w)
			if len(floors) != 1 {
				return brutePlace{}, ErrNoFloor
			}
			floor = floors[0]
		}
		return brutePlace{row: -1, floor: floor, pos: *r.Point}, nil
	}
	var hits []int
	for i, e := range w.Entities {
		if e.ID == r.Entity {
			hits = append(hits, i)
		}
	}
	if len(hits) == 0 {
		for i, e := range w.Entities {
			if strings.HasPrefix(r.Entity, string(e.Kind)+"/") && r.Entity == string(e.Kind)+"/"+e.ID {
				hits = append(hits, i)
			}
		}
	}
	switch len(hits) {
	case 0:
		return brutePlace{}, ErrUnknownEntity
	case 1:
		e := w.Entities[hits[0]]
		return brutePlace{row: hits[0], floor: e.Floor, pos: e.Position.XZ(), ext: e.Extent}, nil
	}
	return brutePlace{}, ErrAmbiguous
}

// bruteReach is how far a footprint extends from pos toward end.
func bruteReach(pos geom.Vec2, ext spatial.Extent, u geom.Vec2, d float64, end geom.Vec2) float64 {
	if len(ext.Polygon) == 0 {
		return ext.RadiusM
	}
	far := 0.0
	n := len(ext.Polygon)
	for i := 0; i < n; i++ {
		edge := geom.Segment{A: ext.Polygon[(i+n-1)%n], B: ext.Polygon[i]}
		if t, ok := geom.RaySegment(pos, u, edge); ok && t <= d && t > far {
			far = t
		}
	}
	if far == 0 && geom.PointInPolygon(end, ext.Polygon) {
		return d
	}
	return far
}

// bruteWide is the map a sight line is cast through: a cell is Occupied when
// an Occupied cell of g lies within cells of it. It asks every cell for its
// neighbourhood; the package marks the neighbourhood of every Occupied cell.
// The result is kept per grid: tests do not change a grid once it is queried.
func bruteWide(t testing.TB, g *grid.Grid, cells float64) *grid.Grid {
	key := wideKey{g, cells}
	if w := wideMaps[key]; w != nil {
		return w
	}
	w, err := grid.New(g.Config())
	if err != nil {
		t.Fatal(err)
	}
	lo, hi, _ := g.Bounds()
	n := int32(math.Ceil(cells))
	for z := lo.IZ - n; z <= hi.IZ+n; z++ {
		for x := lo.IX - n; x <= hi.IX+n; x++ {
			near := false
			for dz := -n; dz <= n; dz++ {
				for dx := -n; dx <= n; dx++ {
					near = near || (math.Hypot(float64(dx), float64(dz)) <= cells && g.State(grid.Cell{IX: x + dx, IZ: z + dz}) == grid.Occupied)
				}
			}
			if near {
				if err := w.Set(grid.Cell{IX: x, IZ: z}, 3); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	wideMaps[key] = w
	return w
}

type wideKey struct {
	g     *grid.Grid
	cells float64
}

var wideMaps = map[wideKey]*grid.Grid{}

func bruteVisible(g *grid.Grid, margin float64, a, b brutePlace) bool {
	d := a.pos.Dist(b.pos)
	if d == 0 {
		return true
	}
	u := b.pos.Sub(a.pos).Scale(1 / d)
	sa := bruteReach(a.pos, a.ext, u, d, b.pos) + margin
	sb := bruteReach(b.pos, b.ext, u.Scale(-1), d, a.pos) + margin
	if sa+sb >= d {
		return true
	}
	start := a.pos.Add(u.Scale(sa))
	_, _, hit := g.Raycast(start, u.Angle(), d-sa-sb)
	return !hit
}

func bruteGap(pos geom.Vec2, ext spatial.Extent, p geom.Vec2) float64 {
	if len(ext.Polygon) == 0 {
		return math.Max(0, p.Dist(pos)-ext.RadiusM)
	}
	if geom.PointInPolygon(p, ext.Polygon) {
		return 0
	}
	best := math.Inf(1)
	n := len(ext.Polygon)
	for i := 0; i < n; i++ {
		best = math.Min(best, geom.Segment{A: ext.Polygon[(i+n-1)%n], B: ext.Polygon[i]}.DistanceTo(p))
	}
	return best
}

// bruteMap is a dense copy of a grid with path lengths from one place.
type bruteMap struct {
	g      *grid.Grid
	lo     grid.Cell
	w, h   int
	free   []bool
	dist   []int64
	reachM float64
}

const bruteInf = math.MaxInt64

func newBruteMap(t testing.TB, g *grid.Grid, cfg Config, from brutePlace) *bruteMap {
	m := &bruteMap{g: g, reachM: float64((cfg.SurfaceCells+0.75)*g.CellSize()) + cfg.ClearanceM}
	lo, hi, ok := g.Bounds()
	if !ok {
		return m
	}
	m.lo, m.w, m.h = lo, int(hi.IX-lo.IX)+1, int(hi.IZ-lo.IZ)+1
	m.free, m.dist = make([]bool, m.w*m.h), make([]int64, m.w*m.h)
	var field *grid.Field
	if cfg.ClearanceM > 0 {
		f, err := g.Field()
		if err != nil {
			t.Fatal(err)
		}
		field = f
	}
	for z := 0; z < m.h; z++ {
		for x := 0; x < m.w; x++ {
			c := grid.Cell{IX: lo.IX + int32(x), IZ: lo.IZ + int32(z)}
			st := g.State(c)
			ok := st == grid.Free
			if cfg.TraverseUnknown {
				ok = st != grid.Occupied
			}
			if field != nil && field.At(c) < cfg.ClearanceM {
				ok = false
			}
			m.free[z*m.w+x] = ok
			m.dist[z*m.w+x] = bruteInf
		}
	}
	m.each(from, func(i int, leg int64) { m.dist[i] = min(m.dist[i], leg) })
	// Relax every cell from its eight neighbours until nothing changes.
	for changed := true; changed; {
		changed = false
		sweep := func(z, x int) {
			if !m.free[z*m.w+x] {
				return
			}
			for dz := -1; dz <= 1; dz++ {
				for dx := -1; dx <= 1; dx++ {
					nx, nz := x+dx, z+dz
					if (dx == 0 && dz == 0) || nx < 0 || nz < 0 || nx >= m.w || nz >= m.h || m.dist[nz*m.w+nx] == bruteInf {
						continue
					}
					step := int64(1_000_000)
					if dx != 0 && dz != 0 {
						if !m.free[z*m.w+nx] || !m.free[nz*m.w+x] {
							continue
						}
						step = 1_414_214
					}
					if d := m.dist[nz*m.w+nx] + step; d < m.dist[z*m.w+x] {
						m.dist[z*m.w+x], changed = d, true
					}
				}
			}
		}
		for z := 0; z < m.h; z++ {
			for x := 0; x < m.w; x++ {
				sweep(z, x)
			}
		}
		for z := m.h - 1; z >= 0; z-- {
			for x := m.w - 1; x >= 0; x-- {
				sweep(z, x)
			}
		}
	}
	return m
}

// each visits every traversable cell within reach of the footprint.
func (m *bruteMap) each(p brutePlace, fn func(i int, leg int64)) {
	for i, ok := range m.free {
		if !ok {
			continue
		}
		c := m.g.Center(grid.Cell{IX: m.lo.IX + int32(i%m.w), IZ: m.lo.IZ + int32(i/m.w)})
		if bruteGap(p.pos, p.ext, c) <= m.reachM {
			fn(i, int64(math.Round(c.Dist(p.pos)/m.g.CellSize()*1_000_000)))
		}
	}
}

func (m *bruteMap) to(p brutePlace) (int64, bool) {
	best := int64(bruteInf)
	m.each(p, func(i int, leg int64) {
		if m.dist[i] != bruteInf {
			best = min(best, m.dist[i]+leg)
		}
	})
	return best, best != bruteInf
}

func bruteRegions(w World, floor spatial.FloorID, p geom.Vec2, keep func(spatial.Entity) bool) bool {
	for _, r := range w.Entities {
		if r.Kind == spatial.EntityRegion && len(r.Extent.Polygon) > 0 && r.Floor == floor && keep(r) && geom.PointInPolygon(p, r.Extent.Polygon) {
			return true
		}
	}
	return false
}

// brute answers q by definition. cfg must have its defaults filled in.
func brute(t testing.TB, w World, cfg Config, q Query) bruteAnswer {
	var ans bruteAnswer
	fail := func(err error) { ans.errs = append(ans.errs, err) }

	// What the query refers to.
	var origin *brutePlace
	var places []brutePlace // origin and relation targets, in query order
	if q.Origin != nil {
		p, err := bruteResolve(w, q, *q.Origin)
		if err != nil {
			fail(err)
		} else {
			origin = &p
			places = append(places, p)
		}
	}
	targets := make([]*brutePlace, len(q.Relations))
	anyRegion := false
	for _, e := range w.Entities {
		anyRegion = anyRegion || (e.Kind == spatial.EntityRegion && len(e.Extent.Polygon) > 0)
	}
	for i, rel := range q.Relations {
		p, err := bruteResolve(w, q, rel.To)
		if err != nil {
			fail(err)
			continue
		}
		targets[i] = &p
		places = append(places, p)
		if rel.Kind == RelInside && len(p.ext.Polygon) == 0 && p.ext.RadiusM == 0 {
			fail(ErrNoExtent)
		}
		if rel.Kind == RelSameRegion && !anyRegion {
			fail(ErrUnknownRegion)
		}
	}
	regionByID := false
	if q.Region != "" {
		known := false
		for _, e := range w.Entities {
			if e.Kind == spatial.EntityRegion && len(e.Extent.Polygon) > 0 {
				regionByID = regionByID || e.ID == q.Region
				known = known || e.ID == q.Region || e.Label == q.Region
			}
		}
		if !known {
			fail(ErrUnknownRegion)
		}
	}
	if len(ans.errs) > 0 {
		return ans
	}

	// Rows that pass everything that needs no map.
	sameFloor := func(e spatial.Entity) bool {
		if q.Floor != "" && e.Floor != q.Floor {
			return false
		}
		for _, p := range places {
			if e.Floor != p.floor {
				return false
			}
		}
		return true
	}
	var pass []int
	for i, e := range w.Entities {
		pos := e.Position.XZ()
		ok := sameFloor(e)
		for _, p := range places {
			ok = ok && p.row != i
		}
		if len(q.Kinds) > 0 {
			hit := false
			for _, k := range q.Kinds {
				hit = hit || k == e.Kind
			}
			ok = ok && hit
		}
		if len(q.Classes) > 0 {
			hit := false
			for _, c := range q.Classes {
				hit = hit || c == e.Class
			}
			ok = ok && hit
		}
		if !q.SeenSince.IsZero() {
			ok = ok && !e.ObservedAt.IsZero() && !e.ObservedAt.Before(q.SeenSince)
		}
		if !q.SeenBefore.IsZero() {
			ok = ok && !e.ObservedAt.IsZero() && e.ObservedAt.Before(q.SeenBefore)
		}
		if !q.ValidAt.IsZero() {
			ok = ok && !e.ObservedAt.IsZero() && !e.ObservedAt.After(q.ValidAt) && (e.ValidUntil.IsZero() || q.ValidAt.Before(e.ValidUntil))
		}
		if b := q.Box; b != nil {
			ok = ok && pos.X >= b.Min.X && pos.X <= b.Max.X && pos.Z >= b.Min.Z && pos.Z <= b.Max.Z
		}
		if len(q.Polygon) > 0 {
			ok = ok && geom.PointInPolygon(pos, q.Polygon)
		}
		if q.Region != "" {
			ok = ok && bruteRegions(w, e.Floor, pos, func(r spatial.Entity) bool {
				if regionByID {
					return r.ID == q.Region
				}
				return r.Label == q.Region
			})
		}
		if origin != nil && q.WithinM > 0 {
			ok = ok && origin.pos.Dist(pos) <= q.WithinM
		}
		for k, rel := range q.Relations {
			target := targets[k]
			switch rel.Kind {
			case RelNear:
				ok = ok && target.pos.Dist(pos) <= rel.DistanceM
			case RelInside:
				if len(target.ext.Polygon) > 0 {
					ok = ok && geom.PointInPolygon(pos, target.ext.Polygon)
				} else {
					ok = ok && target.pos.Dist(pos) <= target.ext.RadiusM
				}
			case RelSameRegion:
				ok = ok && bruteRegions(w, e.Floor, pos, func(r spatial.Entity) bool {
					return geom.PointInPolygon(target.pos, r.Extent.Polygon)
				})
			}
		}
		if ok {
			pass = append(pass, i)
		}
	}

	// The map is consulted only when something is left to decide. A query
	// whose floors contradict each other is empty whatever the map says.
	wantSight := q.Visible
	for _, rel := range q.Relations {
		wantSight = wantSight || rel.Kind == RelVisibleFrom
	}
	wantPath := q.PathWithinM > 0 || q.Order == OrderPath
	contradiction := false
	for _, p := range places {
		contradiction = contradiction || (q.Floor != "" && p.floor != q.Floor) || p.floor != places[0].floor
	}
	for k, rel := range q.Relations {
		if rel.Kind == RelSameRegion && !bruteRegions(w, targets[k].floor, targets[k].pos, func(spatial.Entity) bool { return true }) {
			contradiction = true
		}
	}
	var rows []bruteRow
	if contradiction {
		pass = nil
	} else if wantSight || wantPath {
		g := w.Grids[places[0].floor]
		if g == nil {
			fail(ErrNoGrid)
			return ans
		}
		margin := (cfg.SurfaceCells + cfg.SightCells) * g.CellSize()
		wide := g
		if wantSight {
			wide = bruteWide(t, g, cfg.SightCells)
		}
		var m *bruteMap
		if wantPath {
			m = newBruteMap(t, g, cfg, *origin)
		}
		kept := pass[:0]
		for _, i := range pass {
			e := w.Entities[i]
			here := brutePlace{row: i, floor: e.Floor, pos: e.Position.XZ(), ext: e.Extent}
			ok := !q.Visible || bruteVisible(wide, margin, *origin, here)
			for k, rel := range q.Relations {
				ok = ok && (rel.Kind != RelVisibleFrom || bruteVisible(wide, margin, *targets[k], here))
			}
			r := bruteRow{row: i}
			if ok && wantPath {
				r.path, r.reachable = m.to(here)
				if q.PathWithinM > 0 {
					ok = r.reachable && float64(r.path)*g.CellSize()/1_000_000 <= q.PathWithinM
				}
			}
			if ok {
				kept = append(kept, i)
				rows = append(rows, r)
			}
		}
		pass = kept
	}
	if rows == nil {
		for _, i := range pass {
			rows = append(rows, bruteRow{row: i})
		}
	}
	if origin != nil {
		for i := range rows {
			rows[i].dist = origin.pos.Dist(w.Entities[rows[i].row].Position.XZ())
		}
	}

	tie := func(a, b bruteRow) bool {
		if c := spatial.CompareEntities(w.Entities[a.row], w.Entities[b.row]); c != 0 {
			return c < 0
		}
		return a.row < b.row
	}
	if q.Nearest > 0 {
		sort.Slice(rows, func(i, j int) bool {
			if rows[i].dist != rows[j].dist {
				return rows[i].dist < rows[j].dist
			}
			return tie(rows[i], rows[j])
		})
		rows = rows[:min(len(rows), q.Nearest)]
	}
	order := q.Order
	if order == OrderDefault {
		order = OrderKey
		if origin != nil {
			order = OrderDistance
		}
	}
	sign := 1
	if q.Descending {
		sign = -1
	}
	sort.Slice(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		key := 0
		switch order {
		case OrderDistance:
			key = sign * compareFloat(a.dist, b.dist)
		case OrderTime:
			key = sign * w.Entities[a.row].ObservedAt.Compare(w.Entities[b.row].ObservedAt)
		case OrderPath:
			switch {
			case a.reachable && !b.reachable:
				return true
			case !a.reachable && b.reachable:
				return false
			case a.path != b.path:
				return (a.path < b.path) == (sign > 0)
			}
		}
		if key != 0 {
			return key < 0
		}
		return tie(a, b)
	})
	ans.total = len(rows)
	if q.Limit > 0 {
		rows = rows[:min(len(rows), q.Limit)]
	}
	ans.rows = rows
	return ans
}

func compareFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// rowAttr marks each row so that a Match can be traced back to its row even
// when two rows are equal in every field the engine looks at.
const rowAttr = "row"

// check runs q on the index and on the brute-force reference and reports the
// first difference.
func check(t testing.TB, ix *Index, w World, cfg Config, q Query) (Result, error) {
	t.Helper()
	cfg, err := cfg.withDefaults()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ix.Run(q)
	if invalid := q.Validate(); invalid != nil {
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("query %q is invalid (%v) but Run returned %v", q, invalid, err)
		}
		return got, err
	}
	want := brute(t, w, cfg, q)
	if len(want.errs) > 0 {
		for _, e := range want.errs {
			if errors.Is(err, e) {
				return got, err
			}
		}
		t.Fatalf("query %q: error %v, brute force expects one of %v", q, err, want.errs)
	}
	if err != nil {
		t.Fatalf("query %q: unexpected error %v", q, err)
	}
	if got.Total != want.total || len(got.Matches) != len(want.rows) {
		t.Fatalf("query %q: %d matches of %d, brute force %d of %d", q, len(got.Matches), got.Total, len(want.rows), want.total)
	}
	for i, m := range got.Matches {
		r := want.rows[i]
		e := w.Entities[r.row]
		if m.Entity.Key() != e.Key() || m.Entity.Attributes[rowAttr] != e.Attributes[rowAttr] {
			t.Fatalf("query %q: match %d is %s (row %s), brute force %s (row %s)", q, i, m.Entity.Key(), m.Entity.Attributes[rowAttr], e.Key(), e.Attributes[rowAttr])
		}
		if m.DistanceM != r.dist || m.Reachable != r.reachable {
			t.Fatalf("query %q: match %d (%s) distance %v reachable %v, brute force %v %v", q, i, e.Key(), m.DistanceM, m.Reachable, r.dist, r.reachable)
		}
		if g := w.Grids[e.Floor]; r.reachable && m.PathM != float64(r.path)*g.CellSize()/1_000_000 {
			t.Fatalf("query %q: match %d (%s) path %v m, brute force %v units", q, i, e.Key(), m.PathM, r.path)
		}
	}
	return got, nil
}
