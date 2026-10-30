package sitenav

import "math"

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
