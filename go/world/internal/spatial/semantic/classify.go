package semantic

import (
	"cmp"
	"math"
	"slices"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
)

// Class is what one map cell is made of.
type Class uint8

const (
	ClassUnknown   Class = iota // never observed, or evidence undecided
	ClassFree                   // observed clear
	ClassWall                   // on a wall line
	ClassDoor                   // the leaf of a closed door: wall now, clear before
	ClassFurniture              // static structure that is not a wall
)

// ClassNames are the names of the classes after ClassUnknown, in order: the
// ClassNames of the semantic layer tiles, where code 0 is no data.
var ClassNames = []string{"free", "wall", "door", "furniture"}

// String is the class name; "unknown" for ClassUnknown.
func (c Class) String() string {
	if c == ClassUnknown || int(c) > len(ClassNames) {
		return "unknown"
	}
	return ClassNames[c-1]
}

// raster is the dense window of a floor's map that a segmentation works on:
// index iz·w+ix is grid cell (lo.IX+ix, lo.IZ+iz). It is padded so that its
// border is never observed.
type raster struct {
	lo   grid.Cell
	w, h int
	cell float64
}

const rasterPad = 2

func (r raster) inside(ix, iz int) bool { return ix >= 0 && iz >= 0 && ix < r.w && iz < r.h }

// index is the raster index of a grid cell, or −1 outside the window.
func (r raster) index(c grid.Cell) int {
	ix, iz := int(c.IX-r.lo.IX), int(c.IZ-r.lo.IZ)
	if !r.inside(ix, iz) {
		return -1
	}
	return iz*r.w + ix
}

func (r raster) gridCell(i int) grid.Cell {
	return grid.Cell{IX: r.lo.IX + int32(i%r.w), IZ: r.lo.IZ + int32(i/r.w)}
}

// centre is the site position of the middle of raster cell i.
func (r raster) centre(i int) geom.Vec2 {
	c := r.gridCell(i)
	return geom.Vec2{X: (float64(c.IX) + 0.5) * r.cell, Z: (float64(c.IZ) + 0.5) * r.cell}
}

// cells converts a length in metres to cells, at least 1.
func (r raster) cells(m float64) int { return max(int(math.Round(m/r.cell)), 1) }

// rasterise copies the cell states of a map into a dense window, with two
// readings of the evidence behind them. contested marks the observed cells
// that are neither occupied nor clear: their occupancy probability is above
// clearProbability. settled marks the cells that sit at the map's clear
// limit: observed so often without a hit that the evidence saturated.
func rasterise(g *grid.Grid, clearProbability float64) (r raster, state []grid.State, contested, settled []bool, ok bool) {
	lo, hi, ok := g.Bounds()
	if !ok {
		return raster{}, nil, nil, nil, false
	}
	r = raster{
		lo: grid.Cell{IX: lo.IX - rasterPad, IZ: lo.IZ - rasterPad},
		w:  int(hi.IX-lo.IX) + 1 + 2*rasterPad, h: int(hi.IZ-lo.IZ) + 1 + 2*rasterPad, cell: g.CellSize(),
	}
	state, contested, settled = make([]grid.State, r.w*r.h), make([]bool, r.w*r.h), make([]bool, r.w*r.h)
	limit := math.Log(clearProbability / (1 - clearProbability))
	floor := g.Config().Min + settledMargin
	g.Each(func(c grid.Cell, logOdds float64) bool {
		i := r.index(c)
		state[i] = g.State(c)
		contested[i] = state[i] != grid.Occupied && logOdds > limit
		settled[i] = logOdds <= floor
		return true
	})
	return r, state, contested, settled, true
}

// settledMargin is how far above the map's lower log-odds clamp a cell still
// counts as settled there: less than one miss.
const settledMargin = 0.25

// History is the persistence memory of one floor: the cells that some earlier
// map showed clear, with nothing occupied within a wall's thickness. A wall
// does not appear, so structure standing on such a cell later is a closed
// door leaf (a straight piece between two wall ends) or furniture that
// arrived. The zero value is not usable; see
// NewHistory.
type History struct {
	clear *grid.Grid // used as a sparse cell set: known means seen clear
	reach float64
}

// NewHistory returns an empty History for maps with the given cell size.
// wallThickness is Config.WallThickness; zero takes its default.
func NewHistory(cellSize, wallThickness float64) (*History, error) {
	g, err := grid.New(grid.Config{CellSize: cellSize})
	if err != nil {
		return nil, err
	}
	if wallThickness == 0 {
		wallThickness = DefaultConfig().WallThickness
	}
	return &History{clear: g, reach: wallThickness}, nil
}

// Observe records the clear cells of a map of the floor. Maps may be observed
// in any order.
func (h *History) Observe(g *grid.Grid) error {
	if g.CellSize() != h.clear.CellSize() {
		return grid.ErrIncompatible
	}
	r, state, _, _, ok := rasterise(g, 0.5)
	if !ok {
		return nil
	}
	near := dilate(r, func(i int) bool { return state[i] == grid.Occupied }, r.cells(h.reach))
	for i, s := range state {
		if s == grid.Free && !near[i] {
			if err := h.clear.Set(r.gridCell(i), 1); err != nil {
				return err
			}
		}
	}
	return nil
}

// dilate marks every cell within radius cells (Chebyshev) of a cell in the
// set, in two separable passes.
func dilate(r raster, in func(i int) bool, radius int) []bool {
	rows := make([]bool, r.w*r.h)
	for iz := 0; iz < r.h; iz++ {
		last := -1 << 30
		for ix := 0; ix < r.w; ix++ {
			if in(iz*r.w + ix) {
				for k := max(ix-radius, last+1, 0); k < ix; k++ {
					rows[iz*r.w+k] = true
				}
				last = ix
			}
			if ix-last <= radius {
				rows[iz*r.w+ix] = true
			}
		}
	}
	out := make([]bool, r.w*r.h)
	for ix := 0; ix < r.w; ix++ {
		last := -1 << 30
		for iz := 0; iz < r.h; iz++ {
			if rows[iz*r.w+ix] {
				for k := max(iz-radius, last+1, 0); k < iz; k++ {
					out[k*r.w+ix] = true
				}
				last = iz
			}
			if iz-last <= radius {
				out[iz*r.w+ix] = true
			}
		}
	}
	return out
}

// run is one straight stretch of occupied cells on a line.
type run struct {
	cells []int32 // raster indices, ordered along the line
	// long runs reach Config.WallMinLength and are classed as wall or
	// furniture; shorter ones are stubs, which matter only as the far jamb
	// of a door and are wall when they are one.
	long, furniture, jamb bool
}

// line is one line of the Hough search with the stretches of occupied cells
// found on it, in order along its direction.
type line struct {
	dx, dz float64 // unit direction, in cells
	runs   []run
}

// layer is the per-cell result of classification, with the intermediate
// rasters room segmentation reuses.
type layer struct {
	raster
	state []grid.State
	// contested cells were observed but are neither occupied nor clear.
	// hit marks those that stand among settled cells: in an area observed
	// often enough to settle, a cell that is not settled has collected
	// hits. A wall seen at a grazing angle or at range under heading error
	// dissolves into such cells, none of which gathers enough hits to be
	// occupied.
	contested, hit []bool
	class     []Class
	lines     []line
	// interior is room space: the free cells that were not hit and the
	// unobserved streaks between beams. barrier marks the cells that close
	// a gap in a wall line (see openings).
	interior, barrier []bool
	// unexplained is the share of occupied cells that lie on no straight
	// run and beside no wall. They are called furniture by default.
	unexplained float64
}

func (l *layer) occupied(ix, iz int) bool {
	return l.inside(ix, iz) && l.state[iz*l.w+ix] == grid.Occupied
}

func (l *layer) free(ix, iz int) bool {
	return l.inside(ix, iz) && l.state[iz*l.w+ix] == grid.Free
}

// classify assigns a class to every cell of the map from its geometry, and
// from persistence when a History is given.
//
// Occupied cells on a long straight run are wall unless the run is the face
// of a piece of furniture (see furnitureFace); so are the short stub that is
// the far jamb of a door and the blur beside a wall. Every other occupied
// cell is furniture.
func classify(cfg Config, g *grid.Grid, hist *History) (*layer, bool) {
	r, state, contested, settled, ok := rasterise(g, cfg.ClearProbability)
	if !ok {
		return nil, false
	}
	l := &layer{raster: r, state: state, contested: contested, hit: make([]bool, len(state)), class: make([]Class, len(state))}
	// Wall evidence: the occupied cells and the cells that were hit.
	var evidence []int32
	occupied := 0
	for i, s := range state {
		if contested[i] {
			n := 0
			for _, j := range [8]int{i - r.w - 1, i - r.w, i - r.w + 1, i - 1, i + 1, i + r.w - 1, i + r.w, i + r.w + 1} {
				if settled[j] {
					n++
				}
			}
			l.hit[i] = n >= cfg.SettledNeighbours
		}
		if s == grid.Occupied {
			occupied++
		}
		if s == grid.Occupied || l.hit[i] {
			evidence = append(evidence, int32(i))
		}
	}

	// Room space: an unobserved streak at most Config.BeamGap wide is a gap
	// between beams, found by a morphological opening of the unobserved
	// cells.
	half := max(int(math.Round(cfg.BeamGap/r.cell/2)), 1)
	solid := dilate(r, func(i int) bool { return state[i] != grid.Unknown }, half)
	wide := dilate(r, func(i int) bool { return !solid[i] }, half)
	l.interior = make([]bool, len(state))
	for i, s := range state {
		l.interior[i] = (s == grid.Free || (s == grid.Unknown && !wide[i])) && !l.hit[i]
	}

	l.lines = findLines(cfg, r, evidence)
	for k := range l.lines {
		ln := &l.lines[k]
		for j := range ln.runs {
			if rn := &ln.runs[j]; rn.long {
				rn.furniture = l.furnitureFace(cfg, ln, rn)
			}
		}
	}
	l.barrier = l.openings(cfg)
	wall, onRun := make([]bool, len(state)), make([]bool, len(state))
	for k := range l.lines {
		for j := range l.lines[k].runs {
			rn := &l.lines[k].runs[j]
			for _, i := range rn.cells {
				if state[i] == grid.Occupied {
					onRun[i] = onRun[i] || rn.long || rn.jamb
					wall[i] = wall[i] || (rn.long && !rn.furniture) || rn.jamb
				}
			}
		}
	}
	nearWall := dilate(r, func(i int) bool { return wall[i] }, r.cells(cfg.WallThickness))
	loose := 0
	for i, s := range state {
		switch {
		case s == grid.Free:
			l.class[i] = ClassFree
		case s != grid.Occupied:
		case wall[i] || (!onRun[i] && nearWall[i]):
			l.class[i] = ClassWall
		default:
			l.class[i] = ClassFurniture
			if !onRun[i] {
				loose++
			}
		}
	}
	if occupied > 0 {
		l.unexplained = float64(loose) / float64(occupied)
	}
	if hist != nil {
		l.persistence(cfg, hist)
	}
	return l, true
}

// along is the position of a raster cell along the line, in cells.
func (ln *line) along(r raster, i int32) float64 {
	return float64(int(i)%r.w)*ln.dx + float64(int(i)/r.w)*ln.dz
}

// What lies behind a run cell on one side of the run.
const (
	behindNothing = iota // unobserved as far as furniture is deep
	behindOpen           // observed space right beside the run
	behindBacked         // unobserved for the depth of a piece of furniture, then the map again
	behindSolid          // more structure than a wall is thick
)

// behind marches from raster cell (ix, iz) along (nx, nz) and reports what
// the map shows there.
func (l *layer) behind(cfg Config, ix, iz int, nx, nz float64) int {
	band, least, most := l.cells(cfg.WallThickness)+1, l.cells(cfg.FurnitureMinDepth), l.cells(cfg.FurnitureMaxDepth)
	at := func(j int) (int, int) {
		return int(math.Round(float64(ix) + float64(j)*nx)), int(math.Round(float64(iz) + float64(j)*nz))
	}
	for j := 1; j <= most; j++ {
		x, z := at(j)
		switch {
		case l.occupied(x, z):
			if j <= band {
				continue
			}
			if j < least {
				return behindSolid
			}
			return behindBacked
		case l.free(x, z):
			// One clear cell inside a wall's blur is noise: two in a row
			// are space.
			if x2, z2 := at(j + 1); !l.free(x2, z2) {
				continue
			}
			if j <= band+1 {
				return behindOpen
			}
			if j < least {
				return behindSolid
			}
			return behindBacked
		}
	}
	return behindNothing
}

// furnitureFace decides whether a long run is the face of a piece of
// furniture and not a wall. A wall is thin with observed space on both sides,
// or has nothing observed behind it. A furniture face has its own unobserved
// inside behind it and then the map again: its far face, the wall it stands
// near, or the room. Furniture that stands against an outer wall hides that
// wall, so nothing is observed behind its face either; there the wall is
// looked for on both flanks of the run, set back by the depth of the piece.
//
// ponytail: a chimney breast or pilaster (a short stretch of outer wall that
// stands proud of the wall on both sides) reads as furniture, and a cabinet
// that fills a wall from corner to corner reads as wall. Telling those apart
// needs height or a camera class per cell, neither of which a planar map has.
func (l *layer) furnitureFace(cfg Config, ln *line, rn *run) bool {
	nx, nz := -ln.dz, ln.dx
	var thin, outer, face, side int
	for _, i := range rn.cells {
		ix, iz := int(i)%l.w, int(i)/l.w
		a, b := l.behind(cfg, ix, iz, nx, nz), l.behind(cfg, ix, iz, -nx, -nz)
		switch {
		case a == behindOpen && b == behindOpen:
			thin++
		case (a == behindOpen && b == behindBacked) || (b == behindOpen && a == behindBacked):
			face++
		case a == behindOpen && b == behindNothing:
			outer++
			side--
		case b == behindOpen && a == behindNothing:
			outer++
			side++
		}
	}
	if total := thin + outer + face; total == 0 || float64(face) >= cfg.FurnitureShare*float64(total) {
		return total > 0
	}
	if float64(outer) < cfg.FurnitureShare*float64(thin+outer+face) || side == 0 {
		return false
	}
	// Flanks: structure set back behind the run past each of its ends, and
	// past at least one of them a wall running parallel to it.
	if side < 0 {
		nx, nz = -nx, -nz
	}
	band, most, reach := l.cells(cfg.WallThickness)+1, l.cells(cfg.FurnitureMaxDepth), l.cells(cfg.FlankReach)
	var some, parallel [2]bool
	for e, end := range [2]struct {
		from int32
		sign float64
	}{{rn.cells[len(rn.cells)-1], 1}, {rn.cells[0], -1}} {
		ex, ez := float64(int(end.from)%l.w), float64(int(end.from)/l.w)
		columns := 0
		for j := -band; j <= reach; j++ {
			for m := band + 1; m <= most; m++ {
				x := int(math.Round(ex + end.sign*float64(j)*ln.dx + float64(m)*nx))
				z := int(math.Round(ez + end.sign*float64(j)*ln.dz + float64(m)*nz))
				if l.occupied(x, z) {
					some[e] = true
					if j > band {
						columns++
					}
					break
				}
			}
		}
		parallel[e] = 2*columns >= reach-band
	}
	return some[0] && some[1] && (parallel[0] || parallel[1])
}

// persistence reclassifies structure that an earlier map showed clear. A
// straight piece as long as a door is wide that reaches older structure at
// both ends is a closed door leaf; anything else of that size or larger is
// furniture that arrived; a smaller patch is the flicker of an edge.
func (l *layer) persistence(cfg Config, hist *History) {
	appeared := make([]bool, len(l.class))
	any := false
	for i, s := range l.state {
		if s == grid.Occupied && hist.clear.Known(l.gridCell(i)) {
			appeared[i], any = true, true
		}
	}
	if !any {
		return
	}
	thick, jamb := float64(l.cells(cfg.WallThickness)), l.cells(cfg.TipClearance)
	older := func(i int32) bool {
		ix, iz := int(i)%l.w, int(i)/l.w
		for dz := -jamb; dz <= jamb; dz++ {
			for dx := -jamb; dx <= jamb; dx++ {
				if j := (iz+dz)*l.w + ix + dx; l.occupied(ix+dx, iz+dz) && !appeared[j] {
					return true
				}
			}
		}
		return false
	}
	for _, comp := range l.components(func(i int) bool { return appeared[i] }) {
		a, b, length := l.span(comp)
		if length < cfg.DoorMinWidth {
			continue
		}
		class := ClassFurniture
		if length <= cfg.DoorMaxWidth && older(a) && older(b) {
			class = ClassDoor
			ax, az := float64(int(a)%l.w), float64(int(a)/l.w)
			ux, uz := (float64(int(b)%l.w)-ax)*l.cell/length, (float64(int(b)/l.w)-az)*l.cell/length
			for _, i := range comp {
				if off := (float64(int(i)%l.w)-ax)*uz - (float64(int(i)/l.w)-az)*ux; math.Abs(off) > thick {
					class = ClassFurniture // not straight
					break
				}
			}
		}
		for _, i := range comp {
			l.class[i] = class
		}
	}
}

// components lists the 8-connected components of a cell set, in raster order.
func (l *layer) components(in func(i int) bool) [][]int32 {
	seen := make([]bool, l.w*l.h)
	var out [][]int32
	for start := range seen {
		if seen[start] || !in(start) {
			continue
		}
		comp := []int32{int32(start)}
		seen[start] = true
		for q := 0; q < len(comp); q++ {
			ix, iz := int(comp[q])%l.w, int(comp[q])/l.w
			for dz := -1; dz <= 1; dz++ {
				for dx := -1; dx <= 1; dx++ {
					if j := (iz+dz)*l.w + ix + dx; l.inside(ix+dx, iz+dz) && !seen[j] && in(j) {
						seen[j] = true
						comp = append(comp, int32(j))
					}
				}
			}
		}
		out = append(out, comp)
	}
	return out
}

// span finds two cells of a set that are far apart (the cell farthest from
// the first, and the cell farthest from that one) and the length of the set
// between them in metres, cell edge to cell edge.
func (l *layer) span(cells []int32) (a, b int32, length float64) {
	far := func(from int32) (int32, float64) {
		best, bestD := from, 0.0
		for _, i := range cells {
			if d := math.Hypot(float64(int(i)%l.w-int(from)%l.w), float64(int(i)/l.w-int(from)/l.w)); d > bestD {
				best, bestD = i, d
			}
		}
		return best, bestD
	}
	a, _ = far(cells[0])
	b, d := far(a)
	return a, b, (d + 1) * l.cell
}

// findLines finds the straight runs of occupied cells with a Hough search:
// for every line direction the cells are binned by their offset across it,
// and each well-filled bin is cut into stretches along it. A bin is three
// cells wide and bins overlap, because pose error smears a wall seen at a
// grazing angle over its neighbouring rows, none of which is filled on its
// own. A line is kept
// when it holds a run of at least Config.WallMinLength; its shorter stretches
// are kept with it down to Config.TipClearance.
//
// ponytail: bins are one cell apart and one AngleStep apart, so a wall comes
// out as several overlapping runs. That is enough to class cells and to find
// the gaps in a wall line; fit lines to the runs if wall geometry itself is
// wanted.
func findLines(cfg Config, r raster, occupied []int32) []line {
	type member struct {
		bin  int32
		t    float64
		cell int32
	}
	minLen, stub, gap := cfg.WallMinLength/r.cell, cfg.TipClearance/r.cell, cfg.LineGap/r.cell
	minCount := int32(math.Ceil(minLen * cfg.LineFill))
	offset := int32(r.w + r.h + 1)
	count := make([]int32, 2*offset+3)
	bins := make([]int32, len(occupied))
	var members []member
	var out []line
	for k := 0; k < int(math.Round(math.Pi/cfg.AngleStep)); k++ {
		sin, cos := math.Sincos(float64(k) * cfg.AngleStep)
		clear(count)
		for j, i := range occupied {
			ix, iz := float64(int(i)%r.w), float64(int(i)/r.w)
			bins[j] = int32(math.Round(ix*cos+iz*sin)) + offset
			for b := bins[j] - 1; b <= bins[j]+1; b++ {
				count[b]++
			}
		}
		members = members[:0]
		for j, i := range occupied {
			for b := bins[j] - 1; b <= bins[j]+1; b++ {
				if count[b] >= minCount {
					ix, iz := float64(int(i)%r.w), float64(int(i)/r.w)
					members = append(members, member{b, iz*cos - ix*sin, i})
				}
			}
		}
		slices.SortFunc(members, func(a, b member) int {
			return cmp.Or(cmp.Compare(a.bin, b.bin), cmp.Compare(a.t, b.t), cmp.Compare(a.cell, b.cell))
		})
		for a := 0; a < len(members); {
			ln := line{dx: -sin, dz: cos}
			keep := false
			end := a
			for end < len(members) && members[end].bin == members[a].bin {
				// One stretch, and how many cell positions along the line
				// it fills.
				b, filled := end+1, 1
				for b < len(members) && members[b].bin == members[end].bin && members[b].t-members[b-1].t <= gap+1 {
					if math.Round(members[b].t) != math.Round(members[b-1].t) {
						filled++
					}
					b++
				}
				if length := members[b-1].t - members[end].t + 1; length >= stub && float64(filled) >= cfg.LineFill*length {
					rn := run{cells: make([]int32, b-end), long: length >= minLen}
					for j := end; j < b; j++ {
						rn.cells[j-end] = members[j].cell
					}
					ln.runs = append(ln.runs, rn)
					keep = keep || rn.long
				}
				end = b
			}
			if keep {
				out = append(out, ln)
			}
			a = end
		}
	}
	return out
}
