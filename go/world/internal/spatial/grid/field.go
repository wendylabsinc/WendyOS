package grid

import (
	"cmp"
	"fmt"
	"math"
	"slices"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// Field is the likelihood field of a grid: for every cell of a dense window
// around the Occupied cells, the exact Euclidean distance from the cell
// centre to the nearest Occupied cell centre, clipped at MaxDistance. It is a
// snapshot and is never modified after it is built: when the grid changes,
// Grid.Field returns a new one.
type Field struct {
	// Origin is the grid cell at raster index (0, 0).
	Origin Cell
	// W and H are the raster size in cells; both are zero when the grid has
	// no Occupied cell.
	W, H int
	// Pad is the margin in cells between the bounding box of the Occupied
	// cells and each raster edge.
	Pad         int
	CellSize    float64 // metres
	MaxDistance float64 // metres; the value everywhere outside the raster
	// Dist is row-major: Dist[iz*W+ix] is the distance in metres for the
	// cell Origin + (ix, iz).
	Dist []float32
}

// Field returns the likelihood field of the current Occupied cells. It is
// built on first use and reused until a cell crosses Config.Occupied. After
// that the next call folds the changed cells into a copy of the previous
// field, at a cost that grows with their number and not with the map, as
// long as the bounding box of the Occupied cells is unchanged; a map that
// grew, or changed in many places at once, is rebuilt from scratch. Both
// paths give the same field to the last bit.
func (g *Grid) Field() (*Field, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.field != nil && len(g.flips) == 0 {
		return g.field, nil
	}
	if g.field != nil {
		if f := g.refresh(g.field, g.flips); f != nil {
			g.field, g.flips = f, nil
			return f, nil
		}
	}
	f, err := g.build()
	if err != nil {
		return nil, err
	}
	g.field, g.flips = f, nil
	return f, nil
}

func (f *Field) clip(squaredCells float64) float32 {
	return float32(math.Min(math.Sqrt(squaredCells)*f.CellSize, f.MaxDistance))
}

// build computes the field from the cells.
func (g *Grid) build() (*Field, error) {
	f := &Field{CellSize: g.cfg.CellSize, MaxDistance: g.cfg.FieldDistance}
	lo, hi, ok := g.bounds(func(raw int) bool { return raw >= g.occ })
	if !ok {
		return f, nil
	}
	f.Pad = int(math.Ceil(f.MaxDistance/f.CellSize)) + 1
	f.Origin = Cell{lo.IX - int32(f.Pad), lo.IZ - int32(f.Pad)}
	f.W, f.H = int(hi.IX-lo.IX)+1+2*f.Pad, int(hi.IZ-lo.IZ)+1+2*f.Pad
	if f.W*f.H > maxWindowCells {
		return nil, fmt.Errorf("%w: occupied cells span %dx%d cells", ErrTooLarge, f.W, f.H)
	}
	site := make([]bool, f.W*f.H)
	for k, ch := range g.chunks {
		for i, v := range ch {
			if int(v) >= g.occ {
				ix := int(k.x<<chunkBits|int32(i&chunkMask)) - int(f.Origin.IX)
				iz := int(k.z<<chunkBits|int32(i>>chunkBits)) - int(f.Origin.IZ)
				site[iz*f.W+ix] = true
			}
		}
	}
	d2 := SquaredDistance(site, f.W, f.H)
	f.Dist = make([]float32, len(d2))
	for i, v := range d2 {
		f.Dist[i] = f.clip(v)
	}
	return f, nil
}

// refresh derives the current field from old, the field as it was before the
// cells in flips crossed the Occupied threshold. It returns nil when the
// bounding box of the Occupied cells may have changed, or when a rebuild is
// the cheaper way.
//
// The field is clipped at MaxDistance, so a cell that changed only matters
// within reach = Pad−1 cells of itself. A cell that became Occupied can only
// lower distances, by a direct minimum over that neighbourhood. A cell that
// stopped being Occupied raises the distance of the cells it was nearest to:
// each of those takes the minimum over every remaining site within reach of
// it. Distances are exact integers in squared cells either way, so the
// result equals build's.
func (g *Grid) refresh(old *Field, flips []Cell) *Field {
	if old.W == 0 {
		return nil
	}
	w, h, pad, reach := old.W, old.H, old.Pad, old.Pad-1
	flips = slices.Clone(flips)
	slices.SortFunc(flips, func(a, b Cell) int { return cmp.Or(cmp.Compare(a.IZ, b.IZ), cmp.Compare(a.IX, b.IX)) })
	flips = slices.Compact(flips)
	var added, removed []int // raster indices
	for _, c := range flips {
		ix, iz := int(c.IX)-int(old.Origin.IX), int(c.IZ)-int(old.Origin.IZ)
		occupied := g.raw(c) >= g.occ
		if ix < pad || iz < pad || ix >= w-pad || iz >= h-pad {
			if occupied {
				return nil // the map grew
			}
			continue // no site was ever out here
		}
		switch i := iz*w + ix; {
		case occupied && old.Dist[i] != 0:
			added = append(added, i)
		case !occupied && old.Dist[i] == 0:
			removed = append(removed, i)
		}
	}
	// Rough costs in cell visits: an addition its neighbourhood, a removal
	// about six of them, a rebuild eight passes over the raster.
	side := 2*reach + 1
	if (6*len(removed)+len(added))*side*side > 8*w*h {
		return nil
	}
	// ponytail: every refresh copies the raster (4 bytes per cell). If that
	// allocation ever dominates, share unchanged tiles between versions.
	f := &Field{Origin: old.Origin, W: w, H: h, Pad: pad, CellSize: old.CellSize, MaxDistance: old.MaxDistance, Dist: slices.Clone(old.Dist)}
	dist, far := f.Dist, float32(f.MaxDistance)
	// near[(dz+reach)·side + dx+reach] is the field value at offset (dx, dz) from a lone site.
	near := make([]float32, side*side)
	for dz := -reach; dz <= reach; dz++ {
		for dx := -reach; dx <= reach; dx++ {
			near[(dz+reach)*side+dx+reach] = f.clip(float64(dx*dx + dz*dz))
		}
	}
	// From here on a cell is a site exactly when its distance is zero.
	for _, i := range added {
		dist[i] = 0
	}
	for _, i := range removed {
		dist[i] = far
	}
	var orphans []int // cells that had the removed site as their nearest
	var sites []int   // x, z pairs
	for _, i := range removed {
		qx, qz := i%w, i/w
		orphans = orphans[:0]
		x0, x1, z0, z1 := qx, qx, qz, qz
		for z := max(qz-reach, 0); z <= min(qz+reach, h-1); z++ {
			for x := max(qx-reach, 0); x <= min(qx+reach, w-1); x++ {
				if d := old.Dist[z*w+x]; d < far && d == near[(z-qz+reach)*side+x-qx+reach] {
					orphans = append(orphans, z*w+x)
					x0, x1, z0, z1 = min(x0, x), max(x1, x), min(z0, z), max(z1, z)
				}
			}
		}
		sites = sites[:0]
		for z := max(z0-reach, 0); z <= min(z1+reach, h-1); z++ {
			for x := max(x0-reach, 0); x <= min(x1+reach, w-1); x++ {
				if dist[z*w+x] == 0 {
					sites = append(sites, x, z)
				}
			}
		}
		for _, p := range orphans {
			px, pz := p%w, p/w
			best := math.Inf(1)
			for k := 0; k < len(sites); k += 2 {
				dx, dz := sites[k]-px, sites[k+1]-pz
				best = math.Min(best, float64(dx*dx+dz*dz))
			}
			dist[p] = f.clip(best)
		}
	}
	for _, i := range added {
		qx, qz := i%w, i/w
		for z := max(qz-reach, 0); z <= min(qz+reach, h-1); z++ {
			for x := max(qx-reach, 0); x <= min(qx+reach, w-1); x++ {
				if d := near[(z-qz+reach)*side+x-qx+reach]; d < dist[z*w+x] {
					dist[z*w+x] = d
				}
			}
		}
	}
	if len(removed) > 0 && !f.tight() {
		return nil // the map shrank
	}
	return f
}

// tight reports whether each edge of the Occupied bounding box the raster
// was laid out around still holds an Occupied cell.
func (f *Field) tight() bool {
	edge := func(start, step, n int) bool {
		for k := 0; k < n; k++ {
			if f.Dist[start+k*step] == 0 {
				return true
			}
		}
		return false
	}
	bw, bh := f.W-2*f.Pad, f.H-2*f.Pad
	first := f.Pad*f.W + f.Pad
	return edge(first, 1, bw) && edge(first+(bh-1)*f.W, 1, bw) && edge(first, f.W, bh) && edge(first+bw-1, f.W, bh)
}

// SquaredDistance is the exact Euclidean distance transform of a w×h
// row-major raster (Felzenszwalb and Huttenlocher): for each cell, the
// squared distance in cells to the nearest cell where site is true, or +Inf
// when there is none. It panics when len(site) != w*h.
func SquaredDistance(site []bool, w, h int) []float64 {
	if len(site) != w*h {
		panic("grid: SquaredDistance raster size mismatch")
	}
	inf := math.Inf(1)
	out := make([]float64, w*h)
	// Columns: distance to the nearest site in the same column.
	for x := 0; x < w; x++ {
		d := inf
		for z := 0; z < h; z++ {
			if site[z*w+x] {
				d = 0
			} else {
				d++
			}
			out[z*w+x] = d
		}
		d = inf
		for z := h - 1; z >= 0; z-- {
			if site[z*w+x] {
				d = 0
			} else {
				d++
			}
			if d < out[z*w+x] {
				out[z*w+x] = d
			}
		}
	}
	// Rows: lower envelope of the parabolas (x−q)² + column²(q).
	f := make([]float64, w)
	v := make([]int, w)
	bound := make([]float64, w+1)
	for z := 0; z < h; z++ {
		row := out[z*w : (z+1)*w]
		k := -1
		for q := 0; q < w; q++ {
			f[q] = row[q] * row[q]
			if math.IsInf(f[q], 1) {
				continue
			}
			for k >= 0 {
				p := v[k]
				s := ((f[q] + float64(q*q)) - (f[p] + float64(p*p))) / float64(2*(q-p))
				if s > bound[k] {
					bound[k+1] = s
					break
				}
				k--
			}
			if k < 0 {
				bound[0] = math.Inf(-1)
			}
			k++
			v[k] = q
		}
		if k < 0 {
			for q := range row {
				row[q] = inf
			}
			continue
		}
		for q, j := 0, 0; q < w; q++ {
			for j < k && bound[j+1] < float64(q) {
				j++
			}
			dx := float64(q - v[j])
			row[q] = dx*dx + f[v[j]]
		}
	}
	return out
}

func (f *Field) at(ix, iz int) float64 {
	if uint(ix) >= uint(f.W) || uint(iz) >= uint(f.H) {
		return f.MaxDistance
	}
	return float64(f.Dist[iz*f.W+ix])
}

// At is the clipped distance in metres from the centre of c to the nearest
// Occupied cell centre.
func (f *Field) At(c Cell) float64 {
	return f.at(int(c.IX)-int(f.Origin.IX), int(c.IZ)-int(f.Origin.IZ))
}

// Sample interpolates the field bilinearly between cell centres at p and
// returns the distance in metres with its gradient (metres per metre). The
// gradient is zero wherever the field is clipped.
func (f *Field) Sample(p geom.Vec2) (float64, geom.Vec2) {
	u := p.X/f.CellSize - 0.5 - float64(f.Origin.IX)
	v := p.Z/f.CellSize - 0.5 - float64(f.Origin.IZ)
	if !(u > -2 && u < float64(f.W)+1 && v > -2 && v < float64(f.H)+1) {
		return f.MaxDistance, geom.Vec2{}
	}
	fu, fv := math.Floor(u), math.Floor(v)
	i, j := int(fu), int(fv)
	a, b := u-fu, v-fv
	d00, d10, d01, d11 := f.at(i, j), f.at(i+1, j), f.at(i, j+1), f.at(i+1, j+1)
	d := (d00*(1-a)+d10*a)*(1-b) + (d01*(1-a)+d11*a)*b
	grad := geom.Vec2{
		X: ((d10-d00)*(1-b) + (d11-d01)*b) / f.CellSize,
		Z: ((d01-d00)*(1-a) + (d11-d10)*a) / f.CellSize,
	}
	return d, grad
}

// Distance is the interpolated distance of Sample.
func (f *Field) Distance(p geom.Vec2) float64 {
	d, _ := f.Sample(p)
	return d
}

// MaxPyramidLevels bounds Field.Pyramid: the coarsest level pools 2^11 cells.
const MaxPyramidLevels = 12

// Pyramid is a multi-resolution score raster for branch-and-bound scan
// matching. Level 0 scores each cell by its distance d to the nearest
// Occupied cell as round(255·exp(−d²/2σ²)), and 0 where the field is clipped.
// Level h holds, at each cell, the maximum of level 0 over the block of
// 2^h × 2^h cells that starts at that cell and extends toward +x and +z. A
// pattern of cells summed at level h therefore bounds from above the level-0
// sum of the same pattern shifted by any offset in [0, 2^h) on each axis.
type Pyramid struct {
	// Origin is the grid cell at raster index (0, 0). The raster extends the
	// field's by 2^(levels−1) cells toward −x and −z, so a block that starts
	// before the field still sees it.
	Origin   Cell
	W, H     int
	CellSize float64
	// Levels[h][iz*W+ix] is the level-h score of the cell Origin + (ix, iz).
	// Every cell outside the raster scores 0 at every level.
	Levels [][]uint8
}

// Pyramid builds a score pyramid with the given kernel width in metres and
// number of levels (1 to MaxPyramidLevels).
func (f *Field) Pyramid(sigma float64, levels int) (*Pyramid, error) {
	if !(sigma > 0) || math.IsInf(sigma, 0) || levels < 1 || levels > MaxPyramidLevels {
		return nil, fmt.Errorf("%w: pyramid sigma or levels", ErrInvalid)
	}
	p := &Pyramid{CellSize: f.CellSize, Levels: make([][]uint8, levels)}
	if f.W == 0 {
		for h := range p.Levels {
			p.Levels[h] = []uint8{}
		}
		return p, nil
	}
	pad := 1 << (levels - 1)
	p.Origin = Cell{f.Origin.IX - int32(pad), f.Origin.IZ - int32(pad)}
	p.W, p.H = f.W+pad, f.H+pad
	if p.W*p.H > maxWindowCells {
		return nil, fmt.Errorf("%w: pyramid of %dx%d cells", ErrTooLarge, p.W, p.H)
	}
	base := make([]uint8, p.W*p.H)
	k := -1 / (2 * sigma * sigma)
	for iz := 0; iz < f.H; iz++ {
		row := base[(iz+pad)*p.W+pad:]
		for ix, d := range f.Dist[iz*f.W : (iz+1)*f.W] {
			if d := float64(d); d < f.MaxDistance {
				row[ix] = uint8(math.Round(255 * math.Exp(k*d*d)))
			}
		}
	}
	p.Levels[0] = base
	for h := 1; h < levels; h++ {
		prev, half := p.Levels[h-1], 1<<(h-1)
		cur := make([]uint8, len(prev))
		for iz := 0; iz < p.H; iz++ {
			for ix := 0; ix < p.W; ix++ {
				m := prev[iz*p.W+ix]
				right, down := ix+half < p.W, iz+half < p.H
				if right {
					m = max(m, prev[iz*p.W+ix+half])
				}
				if down {
					m = max(m, prev[(iz+half)*p.W+ix])
				}
				if right && down {
					m = max(m, prev[(iz+half)*p.W+ix+half])
				}
				cur[iz*p.W+ix] = m
			}
		}
		p.Levels[h] = cur
	}
	return p, nil
}

// At is the score of cell c at the given level; 0 outside the raster.
func (p *Pyramid) At(level int, c Cell) uint8 {
	ix, iz := int(c.IX)-int(p.Origin.IX), int(c.IZ)-int(p.Origin.IZ)
	if uint(ix) >= uint(p.W) || uint(iz) >= uint(p.H) {
		return 0
	}
	return p.Levels[level][iz*p.W+ix]
}
