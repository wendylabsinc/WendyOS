package semantic

import (
	"math"
	"slices"
	"sort"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
)

// basins is the room segmentation of a layer: region[i] is 0 where cell i
// belongs to no room and k for the k-th room, rooms ordered by the radius of
// their widest inscribed circle, largest first.
type basins struct {
	region    []int32
	inscribed []float64 // metres, per room
	enclosure []float64 // per room, see Room.Enclosure
	// clearance is the squared distance, in cells, from each room-space
	// cell to the nearest cell that is not room space.
	clearance []float64
}

// segmentRooms splits the free space of a layer into rooms.
//
// The distance transform of the free space is flooded from its maxima down (a
// watershed), and two basins that meet are merged when the passage between
// them is nearly as wide as the narrower basin, which is how
// over-segmentation by ridge noise, room shape and furniture is undone; a
// door is much narrower than either room. An opening wider than the room
// behind it has no saddle, so gaps in wall lines are closed first (see
// openings). What stands free inside a room is taken out of the way first
// (see islands). Basins that border mostly on unobserved space are dropped and
// small ones join a neighbour.
func segmentRooms(cfg Config, l *layer) basins {
	n := l.w * l.h
	interior, barrier := slices.Clone(l.interior), l.barrier
	l.islands(cfg, interior)
	site := make([]bool, n)
	for i := range site {
		site[i] = !interior[i]
	}
	out := basins{region: make([]int32, n), clearance: grid.SquaredDistance(site, l.w, l.h)}
	for i, b := range barrier {
		site[i] = site[i] || b
	}
	d2 := grid.SquaredDistance(site, l.w, l.h)

	// Cells in descending distance, ties in raster order (a counting sort:
	// the squared distances are integers).
	top := 0
	for i, d := range d2 {
		if !site[i] {
			top = max(top, int(d))
		}
	}
	start := make([]int32, top+2)
	for i, d := range d2 {
		if !site[i] {
			start[top-int(d)+1]++
		}
	}
	for k := 1; k < len(start); k++ {
		start[k] += start[k-1]
	}
	order := make([]int32, start[top+1])
	for i, d := range d2 {
		if !site[i] {
			order[start[top-int(d)]] = int32(i)
			start[top-int(d)]++
		}
	}

	parent, peak := []int32{0}, []float64{0}
	var find func(b int32) int32
	find = func(b int32) int32 {
		if parent[b] != b {
			parent[b] = find(parent[b])
		}
		return parent[b]
	}
	label := make([]int32, n)
	ratio2 := cfg.MergeRatio * cfg.MergeRatio
	for _, c := range order {
		i := int(c)
		nb := [4]int{i - 1, i + 1, i - l.w, i + l.w}
		best := int32(0)
		for _, j := range nb {
			if label[j] == 0 {
				continue
			}
			if b := find(label[j]); best == 0 || peak[b] > peak[best] || (peak[b] == peak[best] && b < best) {
				best = b
			}
		}
		if best == 0 {
			parent, peak = append(parent, int32(len(parent))), append(peak, d2[i])
			label[i] = int32(len(parent) - 1)
			continue
		}
		label[i] = best
		for _, j := range nb {
			if label[j] == 0 {
				continue
			}
			if b := find(label[j]); b != best && d2[i] >= ratio2*peak[b] {
				parent[b] = best
			}
		}
	}

	// Per basin: its area, how much of its outline is accounted for, and
	// the length of the border it shares with each neighbour.
	nearStructure := dilate(l.raster, func(i int) bool { return l.state[i] == grid.Occupied || l.hit[i] }, l.cells(cfg.WallThickness))
	area, edge, closed := make([]int, len(parent)), make([]int, len(parent)), make([]int, len(parent))
	shared := map[[2]int32]float64{} // widest clearance on the border of two basins, squared cells
	for _, c := range order {
		i := int(c)
		a := find(label[i])
		area[a]++
		for _, j := range [4]int{i - 1, i + 1, i - l.w, i + l.w} {
			b := int32(0)
			if label[j] != 0 {
				b = find(label[j])
			}
			if b == a {
				continue
			}
			edge[a]++
			// The outline is accounted for where it meets another basin, a
			// closed opening or structure, holes in a wall's blur included.
			if b != 0 || barrier[j] || nearStructure[j] || nearStructure[i] {
				closed[a]++
			}
			if k := [2]int32{min(a, b), max(a, b)}; b != 0 {
				shared[k] = max(shared[k], out.clearance[i])
			}
		}
	}
	type border struct {
		a, b int32
		wide float64
	}
	borders := make([]border, 0, len(shared))
	for k, v := range shared {
		borders = append(borders, border{k[0], k[1], v})
	}
	sort.Slice(borders, func(i, j int) bool {
		if borders[i].a != borders[j].a {
			return borders[i].a < borders[j].a
		}
		return borders[i].b < borders[j].b
	})

	// A basin that borders mostly on unobserved space is no room.
	dropped := make([]bool, len(parent))
	for b := int32(1); b < int32(len(parent)); b++ {
		dropped[b] = parent[b] == b && float64(closed[b]) < cfg.MinEnclosure*float64(edge[b])
	}
	// Basins below the minimum area join the neighbour they open widest
	// into, smallest first: a leak through a wall's blur is a border too,
	// but a narrow one.
	var small []int32
	minCells := int(math.Ceil(cfg.MinRoomArea / (l.cell * l.cell)))
	for b := int32(1); b < int32(len(parent)); b++ {
		if parent[b] == b && !dropped[b] && area[b] < minCells {
			small = append(small, b)
		}
	}
	sort.Slice(small, func(i, j int) bool {
		if area[small[i]] != area[small[j]] {
			return area[small[i]] < area[small[j]]
		}
		return small[i] < small[j]
	})
	for _, s := range small {
		a := find(s)
		if area[a] >= minCells {
			continue
		}
		into, most := int32(0), 0.0
		for _, e := range borders {
			ea, eb := find(e.a), find(e.b)
			if ea == eb || (ea != a && eb != a) {
				continue
			}
			if eb == a {
				eb = ea
			}
			if e.wide > most && !dropped[eb] {
				into, most = eb, e.wide
			}
		}
		if into != 0 {
			parent[a] = into
			area[into] += area[a]
			peak[into] = max(peak[into], peak[a])
		}
	}

	// The rooms that are left. The cells of a closed opening go to a room
	// beside them.
	room := make([]int32, n) // basin root per cell
	for _, c := range order {
		if b := find(label[c]); !dropped[b] && area[b] >= minCells {
			room[c] = b
		}
	}
	for pass := 0; pass < 3; pass++ {
		for i, b := range barrier {
			if !b || !interior[i] || room[i] != 0 {
				continue
			}
			for _, j := range [4]int{i - 1, i + 1, i - l.w, i + l.w} {
				if room[j] != 0 && !barrier[j] {
					room[i] = room[j]
					break
				}
			}
		}
	}
	// Dropping a basin opens the outline of its neighbours, so the test is
	// repeated on what is left, least enclosed first, until every room
	// passes: a room is enclosed by structure and by rooms that are rooms.
	enclosure := make([]float64, len(parent))
	for {
		clear(edge)
		clear(closed)
		for i, a := range room {
			if a == 0 {
				continue
			}
			for _, j := range [4]int{i - 1, i + 1, i - l.w, i + l.w} {
				if room[j] == a {
					continue
				}
				edge[a]++
				if room[j] != 0 || nearStructure[j] || nearStructure[i] {
					closed[a]++
				}
			}
		}
		worst := int32(0)
		for b := int32(1); b < int32(len(parent)); b++ {
			if edge[b] == 0 {
				continue
			}
			enclosure[b] = float64(closed[b]) / float64(edge[b])
			if enclosure[b] < cfg.MinEnclosure && (worst == 0 || enclosure[b] < enclosure[worst]) {
				worst = b
			}
		}
		if worst == 0 {
			break
		}
		for i, a := range room {
			if a == worst {
				room[i] = 0
			}
		}
	}
	// Number them, widest first.
	number := make([]int32, len(parent))
	for _, c := range order {
		b := room[c]
		if b == 0 {
			continue
		}
		if number[b] == 0 {
			out.inscribed = append(out.inscribed, math.Sqrt(peak[b])*l.cell)
			out.enclosure = append(out.enclosure, enclosure[b])
			number[b] = int32(len(out.inscribed))
		}
	}
	for i, b := range room {
		out.region[i] = number[b]
	}
	return out
}

// islands takes what stands free inside a room out of the way of the
// segmentation: a patch of cells that are not room space, is surrounded by
// room space, holds no wall and has an area between Config.FurnitureMinArea
// and Config.FurnitureMaxArea becomes room space, and its occupied cells are
// furniture. A table in the middle of a room would otherwise pinch the room
// into basins of its own. A smaller patch is noise or what is left of a
// dissolved wall, and stays in the way.
func (l *layer) islands(cfg Config, interior []bool) {
	minCells, maxCells := int(cfg.FurnitureMinArea/(l.cell*l.cell)), int(cfg.FurnitureMaxArea/(l.cell*l.cell))
	seen := make([]bool, len(interior))
	var queue []int32
	for start := range interior {
		if interior[start] || seen[start] {
			continue
		}
		queue = append(queue[:0], int32(start))
		seen[start] = true
		ok := true
		for q := 0; q < len(queue); q++ {
			i := int(queue[q])
			ix, iz := i%l.w, i/l.w
			if ix == 0 || iz == 0 || ix == l.w-1 || iz == l.h-1 {
				ok = false // reaches the edge of the map; not expanded past it
				continue
			}
			if l.class[i] == ClassWall || l.class[i] == ClassDoor {
				ok = false
			}
			for _, j := range [4]int{i - 1, i + 1, i - l.w, i + l.w} {
				if !interior[j] && !seen[j] {
					seen[j] = true
					queue = append(queue, int32(j))
				}
			}
		}
		if !ok || len(queue) < minCells || len(queue) > maxCells {
			continue
		}
		for _, i := range queue {
			interior[i] = true
			if l.state[i] == grid.Occupied {
				l.class[i] = ClassFurniture
			}
		}
	}
}

// openings finds the gaps in wall lines and returns the cells that close
// them. A gap lies between two stretches of structure on one line, the nearer
// of them a wall run, with room space between them. Both ends must be free
// ends with no other structure around them, which tells a doorway from two
// corners that face each other across a corridor, and the wall must have
// observed space on both of its sides, which the face of a piece of
// furniture has not. A stub that serves as the far end is marked as a jamb.
func (l *layer) openings(cfg Config) []bool {
	interior := l.interior
	barrier := make([]bool, len(interior))
	thick, clear := l.cells(cfg.WallThickness), l.cells(cfg.TipClearance)
	minGap, maxGap := cfg.DoorMinWidth/l.cell, cfg.OpeningMax/l.cell
	for k := range l.lines {
		ln := &l.lines[k]
		nx, nz := -ln.dz, ln.dx
		// tip reports whether the end of a run at cell from, with the run
		// lying towards (bx, bz), is a free end of a two-sided wall.
		tip := func(from int32, bx, bz float64) bool {
			fx, fz := int(from)%l.w, int(from)/l.w
			for dz := -clear; dz <= clear; dz++ {
				for dx := -clear; dx <= clear; dx++ {
					if off := float64(dx)*nx + float64(dz)*nz; math.Abs(off) > float64(2*thick) && l.occupied(fx+dx, fz+dz) {
						return false
					}
				}
			}
			both := 0
			for j := 0; j <= clear; j++ {
				x, z := float64(fx)+float64(j)*bx, float64(fz)+float64(j)*bz
				off := float64(2*thick + 1)
				if l.free(int(math.Round(x+off*nx)), int(math.Round(z+off*nz))) && l.free(int(math.Round(x-off*nx)), int(math.Round(z-off*nz))) {
					both++
				}
			}
			return 2*both > clear
		}
		for j := 0; j+1 < len(ln.runs); j++ {
			a, b := &ln.runs[j], &ln.runs[j+1]
			if !(a.long || b.long) || (a.long && a.furniture) || (b.long && b.furniture) {
				continue
			}
			from, to := a.cells[len(a.cells)-1], b.cells[0]
			gap := ln.along(l.raster, to) - ln.along(l.raster, from) - 1
			if gap < minGap || gap > maxGap {
				continue
			}
			// The cells between the two ends, and whether the way is clear:
			// room space on the line that is not contested, and no structure
			// beside it, away from the blur of the two ends.
			fx, fz := float64(int(from)%l.w), float64(int(from)/l.w)
			steps := int(math.Round(gap))
			path := make([]int32, 0, steps)
			open := true
			for s := 1; s <= steps && open; s++ {
				x, z := fx+float64(s)*ln.dx, fz+float64(s)*ln.dz
				ix, iz := int(math.Round(x)), int(math.Round(z))
				if !l.inside(ix, iz) {
					open = false
					break
				}
				path = append(path, int32(iz*l.w+ix))
				if s <= thick || s > steps-thick {
					continue
				}
				open = interior[iz*l.w+ix] && !l.contested[iz*l.w+ix]
				for m := -thick; m <= thick && open; m++ {
					open = !l.occupied(int(math.Round(x+float64(m)*nx)), int(math.Round(z+float64(m)*nz)))
				}
			}
			if !open || !tip(from, -ln.dx, -ln.dz) || !tip(to, ln.dx, ln.dz) {
				continue
			}
			if debugOpenings != nil {
				debugOpenings(l.centre(int(from)), l.centre(int(to)), a.long, b.long)
			}
			a.jamb, b.jamb = !a.long, !b.long
			for _, i := range path {
				barrier[i] = true
			}
		}
	}
	return barrier
}

// boundary is one connected stretch of the border between two rooms.
type boundary struct {
	a, b   int32 // rooms, a < b
	centre geom.Vec2
	width  float64
}

// boundaries finds where rooms touch. The width of a passage is that of the
// widest circle that passes through it; one below Config.DoorMinWidth is a
// leak in a wall and is left out.
func (l *layer) boundaries(cfg Config, b basins) []boundary {
	region := b.region
	pairs := map[[2]int32][]int32{}
	var keys [][2]int32
	for i, a := range region {
		if a == 0 {
			continue
		}
		for _, j := range [2]int{i + 1, i + l.w} {
			if o := region[j]; o != 0 && o != a {
				k := [2]int32{min(a, o), max(a, o)}
				if pairs[k] == nil {
					keys = append(keys, k)
				}
				pairs[k] = append(pairs[k], int32(i))
			}
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	var out []boundary
	for _, k := range keys {
		cells := pairs[k]
		// Single linkage: border cells within two cells of each other are
		// one stretch.
		done := make([]bool, len(cells))
		for s := range cells {
			if done[s] {
				continue
			}
			members := []int{s}
			done[s] = true
			for q := 0; q < len(members); q++ {
				ax, az := int(cells[members[q]])%l.w, int(cells[members[q]])/l.w
				for t := range cells {
					if bx, bz := int(cells[t])%l.w, int(cells[t])/l.w; !done[t] && max(abs(ax-bx), abs(az-bz)) <= 2 {
						done[t] = true
						members = append(members, t)
					}
				}
			}
			var sum geom.Vec2
			widest := 0.0
			for _, m := range members {
				sum = sum.Add(l.centre(int(cells[m])))
				widest = max(widest, b.clearance[cells[m]])
			}
			// A cell at distance d from the nearest cell that is not room
			// space sits in a gap of 2d − 1 cells.
			if width := (2*math.Sqrt(widest) - 1) * l.cell; width >= cfg.DoorMinWidth {
				out = append(out, boundary{a: k[0], b: k[1], centre: sum.Scale(1 / float64(len(members))), width: width})
			}
		}
	}
	return out
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// outline traces the outer boundary of room k along cell edges and simplifies
// it to at most spatial.MaxPolygonVertices vertices, in site metres.
func (l *layer) outline(region []int32, k int32, tolerance float64) []geom.Vec2 {
	in := func(ix, iz int) bool { return l.inside(ix, iz) && region[iz*l.w+ix] == k }
	// Directed edges with the room on the right: 0 +x, 1 +z, 2 −x, 3 −z,
	// which is clockwise on a raster drawn with z down. edges[v] is the set
	// of directions leaving lattice vertex v.
	vw := l.w + 1
	edges := map[int32]uint8{}
	var starts []int32
	add := func(vx, vz, dir int) {
		v := int32(vz*vw + vx)
		if edges[v] == 0 {
			starts = append(starts, v)
		}
		edges[v] |= 1 << dir
	}
	for i, r := range region {
		if r != k {
			continue
		}
		ix, iz := i%l.w, i/l.w
		if !in(ix, iz-1) {
			add(ix, iz, 0)
		}
		if !in(ix+1, iz) {
			add(ix+1, iz, 1)
		}
		if !in(ix, iz+1) {
			add(ix+1, iz+1, 2)
		}
		if !in(ix-1, iz) {
			add(ix, iz+1, 3)
		}
	}
	step := [4][2]int{{1, 0}, {0, 1}, {-1, 0}, {0, -1}}
	var best []geom.Vec2
	bestArea := 0.0
	for _, s := range starts {
		for edges[s] != 0 {
			// Follow one loop, turning right first where two loops touch
			// at a vertex so that each stays around its own cells.
			var loop []geom.Vec2
			v, dir := s, 0
			for edges[v]&(1<<dir) == 0 {
				dir++
			}
			for {
				edges[v] &^= 1 << dir
				vx, vz := int(v)%vw, int(v)/vw
				loop = append(loop, geom.Vec2{X: float64(vx), Z: float64(vz)})
				v = int32((vz+step[dir][1])*vw + vx + step[dir][0])
				next := -1
				for _, turn := range [3]int{1, 0, 3} {
					if d := (dir + turn) % 4; edges[v]&(1<<d) != 0 {
						next = d
						break
					}
				}
				if v == s || next < 0 {
					break
				}
				dir = next
			}
			if a := geom.PolygonArea(loop); a > bestArea {
				best, bestArea = loop, a
			}
		}
	}
	if len(best) < 3 {
		return nil
	}
	for i, p := range best {
		best[i] = geom.Vec2{X: (float64(l.lo.IX) + p.X) * l.cell, Z: (float64(l.lo.IZ) + p.Z) * l.cell}
	}
	for tol := tolerance; ; tol *= 2 {
		if poly := simplify(best, tol); len(poly) <= spatial.MaxPolygonVertices {
			return poly
		}
	}
}

// simplify is Douglas-Peucker on a closed polygon: it is split at vertex 0
// and at the vertex farthest from it, and each half is reduced on its own.
func simplify(poly []geom.Vec2, tolerance float64) []geom.Vec2 {
	if len(poly) <= 4 || tolerance <= 0 {
		return poly
	}
	far := 0
	for i, p := range poly {
		if p.Dist(poly[0]) > poly[far].Dist(poly[0]) {
			far = i
		}
	}
	at := func(i int) geom.Vec2 { return poly[i%len(poly)] }
	// The vertex farthest from that split is kept too, so the result is
	// never degenerate whatever the tolerance.
	third, chord := 0, geom.Segment{A: poly[0], B: poly[far]}
	for i, p := range poly {
		if chord.DistanceTo(p) > chord.DistanceTo(poly[third]) {
			third = i
		}
	}
	keep := make([]bool, len(poly)+1)
	keep[0], keep[far], keep[third], keep[len(poly)] = true, true, true, true
	var reduce func(a, b int)
	reduce = func(a, b int) {
		worst, dist := -1, tolerance
		chord := geom.Segment{A: at(a), B: at(b)}
		for i := a + 1; i < b; i++ {
			if d := chord.DistanceTo(at(i)); d > dist {
				worst, dist = i, d
			}
		}
		if worst >= 0 {
			keep[worst] = true
			reduce(a, worst)
			reduce(worst, b)
		}
	}
	cuts := []int{0, min(far, third), max(far, third), len(poly)}
	for i := 1; i < len(cuts); i++ {
		reduce(cuts[i-1], cuts[i])
	}
	out := make([]geom.Vec2, 0, len(poly))
	for i := range poly {
		if keep[i] {
			out = append(out, poly[i])
		}
	}
	return out
}

var debugOpenings func(a, b geom.Vec2, la, lb bool)
