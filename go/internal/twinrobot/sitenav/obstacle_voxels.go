package sitenav

import (
	"math"
	"sort"
)

// HazardFootprint is the occupied area used by the collision checks, in odometry
// coordinates. Full-cell hazards represent detected drops; raised returns keep
// their measured voxel footprint rather than expanding to the lookup cell.
type HazardFootprint struct {
	Position Point
	Size     float64
	Drop     bool
}

func (g Grid) HazardFootprints() []HazardFootprint {
	unique := make(map[HazardFootprint]struct{})
	for c, occupied := range g.Obstacles {
		if !occupied {
			continue
		}
		if len(g.voxels[c]) == 0 {
			unique[HazardFootprint{Point{(float64(c.X) + .5) * CellSize, (float64(c.Y) + .5) * CellSize}, CellSize, true}] = struct{}{}
		} else {
			for _, v := range g.voxels[c] {
				unique[HazardFootprint{v.position, v.halfSize * 2, false}] = struct{}{}
			}
		}
	}
	result := make([]HazardFootprint, 0, len(unique))
	for v := range unique {
		result = append(result, v)
	}
	// Stable ordering lets the scene reuse geometry between unchanged polls.
	sort.Slice(result, func(i, j int) bool {
		a, b := result[i], result[j]
		if a.Position.X != b.Position.X {
			return a.Position.X < b.Position.X
		}
		if a.Position.Y != b.Position.Y {
			return a.Position.Y < b.Position.Y
		}
		if a.Size != b.Size {
			return a.Size < b.Size
		}
		return !a.Drop && b.Drop
	})
	return result
}

type obstacleVoxel struct {
	position Point
	halfSize float64
}

func (g Grid) addObstacleVoxel(p Point, resolution float64) {
	v := obstacleVoxel{p, resolution / 2}
	lo := cell(Point{p.X - v.halfSize, p.Y - v.halfSize})
	hi := cell(Point{p.X + v.halfSize, p.Y + v.halfSize})
	for y := lo.Y; y <= hi.Y; y++ {
		for x := lo.X; x <= hi.X; x++ {
			c := Cell{x, y}
			g.Obstacles[c] = true
			g.voxels[c] = append(g.voxels[c], v)
		}
	}
}

// The coarse cell is only a lookup bucket. Test the occupied voxel squares
// against the same oriented body sweep used for floor coverage. Retaining
// voxel size avoids treating sparse measurements as dimensionless points.
func (g Grid) obstacleOverlaps(k Cell, a, b Point, yaw, padding float64) bool {
	if !g.Obstacles[k] {
		return false
	}
	voxels := g.voxels[k]
	if len(voxels) == 0 {
		return true
	}
	s, c := math.Sincos(yaw)
	l, w := HalfLength+Clearance+padding, HalfWidth+Clearance+padding
	axes := []Point{{1, 0}, {0, 1}, {c, s}, {-s, c}}
	if d := a.Distance(b); d > 1e-9 {
		axes = append(axes, Point{-(b.Y - a.Y) / d, (b.X - a.X) / d})
	}
	for _, v := range voxels {
		overlap := true
		for _, axis := range axes {
			r := l*math.Abs(axis.X*c+axis.Y*s) + w*math.Abs(-axis.X*s+axis.Y*c)
			r += v.halfSize * (math.Abs(axis.X) + math.Abs(axis.Y))
			pa, pb := a.X*axis.X+a.Y*axis.Y, b.X*axis.X+b.Y*axis.Y
			q := v.position.X*axis.X + v.position.Y*axis.Y
			if q <= math.Min(pa, pb)-r+1e-9 || q >= math.Max(pa, pb)+r-1e-9 {
				overlap = false
				break
			}
		}
		if overlap {
			return true
		}
	}
	return false
}
