package grid

import (
	"fmt"
	"math"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// walk visits, in order, every cell the segment from (ax, az) to (bx, bz)
// crosses, in cell units, except the cell holding the end point.
func walk(ax, az, bx, bz float64, visit func(ix, iz int32)) {
	ix, iz := cellIndex(ax), cellIndex(az)
	nx, nz := int(cellIndex(bx))-int(ix), int(cellIndex(bz))-int(iz)
	stepX, tMaxX, tDeltaX := axis(ax, bx-ax, ix)
	stepZ, tMaxZ, tDeltaZ := axis(az, bz-az, iz)
	if nx < 0 {
		nx = -nx
	}
	if nz < 0 {
		nz = -nz
	}
	for nx+nz > 0 {
		visit(ix, iz)
		// The remaining counts, not the float comparison alone, decide the
		// last steps, so rounding cannot overshoot the end cell.
		if nz == 0 || (nx > 0 && tMaxX < tMaxZ) {
			ix += stepX
			tMaxX += tDeltaX
			nx--
		} else {
			iz += stepZ
			tMaxZ += tDeltaZ
			nz--
		}
	}
}

// axis sets up one axis of a grid traversal from position a (cell units, in
// cell i) moving by d: the index step, the ray parameter at the first
// boundary and the parameter between boundaries.
func axis(a, d float64, i int32) (step int32, tMax, tDelta float64) {
	switch {
	case d > 0:
		return 1, (float64(i) + 1 - a) / d, 1 / d
	case d < 0:
		return -1, (a - float64(i)) / -d, -1 / d
	}
	return 0, math.Inf(1), math.Inf(1)
}

// InsertScan integrates one scan taken with the scanner at sensor, the pose
// of the scan's frame in the map frame. Each beam with a return clears the
// cells it crosses and marks the cell holding the return; a beam with no
// return, or a return beyond Config.MaxRange, only clears. Invalid beams are
// skipped. A cell is updated at most once per scan, and a return wins over
// any beam that crosses the same cell.
func (g *Grid) InsertScan(sensor geom.Pose2, s spatial.Scan) error {
	if !finite(sensor.X, sensor.Z, sensor.Yaw, s.AngleMin, s.AngleIncrement, s.RangeMax) || !(s.RangeMax > 0) {
		return fmt.Errorf("%w: scan geometry or sensor pose", ErrInvalid)
	}
	inv := 1 / g.cfg.CellSize
	ox, oz := sensor.X*inv, sensor.Z*inv
	limit := s.RangeMax
	if g.cfg.MaxRange > 0 && g.cfg.MaxRange < limit {
		limit = g.cfg.MaxRange
	}
	type end struct {
		x, z float64
		hit  bool
	}
	ends := make([]end, 0, len(s.Ranges))
	minX, maxX, minZ, maxZ := ox, ox, oz, oz
	for i := range s.Ranges {
		a, r, state := s.Beam(i)
		if state == spatial.BeamInvalid {
			continue
		}
		hit := state == spatial.BeamHit && r <= limit
		if !hit {
			r = limit
		}
		d := geom.Heading(sensor.Yaw + a)
		e := end{ox + d.X*r*inv, oz + d.Z*r*inv, hit}
		minX, maxX = math.Min(minX, e.x), math.Max(maxX, e.x)
		minZ, maxZ = math.Min(minZ, e.z), math.Max(maxZ, e.z)
		ends = append(ends, e)
	}
	if len(ends) == 0 {
		return nil
	}
	if !(minX > -maxIndex && maxX < maxIndex && minZ > -maxIndex && maxZ < maxIndex) {
		return fmt.Errorf("%w: scan beyond the addressable map", ErrInvalid)
	}
	x0, z0 := cellIndex(minX), cellIndex(minZ)
	w, h := int(cellIndex(maxX)-x0)+1, int(cellIndex(maxZ)-z0)+1
	if w*h > maxWindowCells {
		return fmt.Errorf("%w: scan footprint of %dx%d cells", ErrTooLarge, w, h)
	}
	if cap(g.mark) < w*h {
		g.mark = make([]uint8, w*h)
	}
	mark := g.mark[:w*h]
	touched := g.touched[:0]
	const crossed, returned = 1, 2
	for _, e := range ends {
		walk(ox, oz, e.x, e.z, func(ix, iz int32) {
			if i := int(iz-z0)*w + int(ix-x0); mark[i] == 0 {
				mark[i] = crossed
				touched = append(touched, int32(i))
			}
		})
		if e.hit {
			i := int(cellIndex(e.z)-z0)*w + int(cellIndex(e.x)-x0)
			if mark[i] == 0 {
				touched = append(touched, int32(i))
			}
			mark[i] = returned
		}
	}
	cu := cursor{g: g}
	var flipped []Cell
	for _, i := range touched {
		delta := -g.miss
		if mark[i] == returned {
			delta = g.hit
		}
		mark[i] = 0
		if c := (Cell{x0 + int32(int(i)%w), z0 + int32(int(i)/w)}); cu.add(c.IX, c.IZ, delta) {
			flipped = append(flipped, c)
		}
	}
	g.touched = touched
	g.stale(flipped...)
	return nil
}

// Raycast follows the ray from origin along yaw and returns the range in
// metres at which it enters the first Occupied cell, and that cell. Without
// one inside maxRange it returns maxRange and hit false. The reported range
// is to the cell boundary, so it is short of the surface inside the cell by
// up to one cell diagonal.
func (g *Grid) Raycast(origin geom.Vec2, yaw, maxRange float64) (r float64, c Cell, hit bool) {
	if !finite(origin.X, origin.Z, yaw, maxRange) || maxRange < 0 {
		return 0, Cell{}, false
	}
	s := g.cfg.CellSize
	ax, az := origin.X/s, origin.Z/s
	if math.Abs(ax) >= maxIndex || math.Abs(az) >= maxIndex {
		return maxRange, Cell{}, false
	}
	d := geom.Heading(yaw)
	ix, iz := cellIndex(ax), cellIndex(az)
	// d is a unit vector, so with it as the step the ray parameter is in
	// cells along the ray; multiply by s for metres.
	stepX, tMaxX, tDeltaX := axis(ax, d.X, ix)
	stepZ, tMaxZ, tDeltaZ := axis(az, d.Z, iz)
	cu := cursor{g: g}
	t := 0.0
	for t*s <= maxRange && ix > -maxIndex && ix < maxIndex && iz > -maxIndex && iz < maxIndex {
		if cu.get(ix, iz) >= g.occ {
			return t * s, Cell{ix, iz}, true
		}
		if tMaxX < tMaxZ {
			t = tMaxX
			ix += stepX
			tMaxX += tDeltaX
		} else {
			t = tMaxZ
			iz += stepZ
			tMaxZ += tDeltaZ
		}
	}
	return maxRange, Cell{}, false
}

// LineOfSight reports whether the segment from a to b crosses no Occupied
// cell, the cells holding a and b included.
func (g *Grid) LineOfSight(a, b geom.Vec2) bool {
	d := b.Sub(a)
	_, _, hit := g.Raycast(a, d.Angle(), d.Norm())
	return !hit
}
