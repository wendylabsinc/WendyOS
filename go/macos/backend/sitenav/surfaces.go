package sitenav

import (
	"math"
	"math/bits"
	"sort"
)

// Surface keeps height and measured orientation. Horizontal samples can support
// furniture or a previously observed map, but never establish a wall direction.
type Surface struct {
	Position   Vertex `json:"position"`
	Direction  Point  `json:"direction"`
	Horizontal bool   `json:"horizontal"`
	Stable     bool   `json:"stable"`
}

type SurfaceCloud struct {
	Surfaces []Surface `json:"surfaces"`
	Rejected []Vertex  `json:"rejected"`
	Floor    float64   `json:"floor"`
}

var surfaceHeights = []float64{.25, .50, .75, 1.0, 1.25, 1.50}

// ExtractSurfaces pools vertical support over a whole planar neighborhood in
// overlapping height bands. A partially visible wall need not have 50 cm of
// returns in each column. Filled patches still fail the thin-surface test.
func ExtractSurfaces(points []Vertex, floor float64) SurfaceCloud {
	result := SurfaceCloud{Floor: floor}
	if !finite(floor) {
		return result
	}
	accepted := map[Cell]bool{}
	for _, height := range surfaceHeights {
		low, high := floor+height-.13, floor+height+.13
		columns := collectVerticalColumns(points, low, high)
		type support struct {
			direction Point
			distance  int
			source    Cell
		}
		supported := map[Cell]support{}
		for key := range columns {
			var neighbors []Point
			var levels uint64
			var minZ, maxZ = math.Inf(1), math.Inf(-1)
			for dy := -4; dy <= 4; dy++ {
				for dx := -4; dx <= 4; dx++ {
					if dx*dx+dy*dy > 20 {
						continue
					}
					if v, ok := columns[Cell{key.X + dx, key.Y + dy}]; ok {
						neighbors = append(neighbors, Point{float64(dx) * CellSize, float64(dy) * CellSize})
						levels |= v.levels
						minZ = math.Min(minZ, v.low)
						maxZ = math.Max(maxZ, v.high)
					}
				}
			}
			direction, planar := planarWallDirection(neighbors)
			if !planar || bits.OnesCount64(levels) < 3 || maxZ-minZ < .10-1e-6 {
				continue
			}
			// Retain the measured columns that established this plane. Keeping only
			// the center erodes short visible walls, then registration rejects their
			// shortened span even though this neighborhood passed the same test.
			for dy := -4; dy <= 4; dy++ {
				for dx := -4; dx <= 4; dx++ {
					distance := dx*dx + dy*dy
					if distance > 20 {
						continue
					}
					k := Cell{key.X + dx, key.Y + dy}
					if _, ok := columns[k]; !ok {
						continue
					}
					old, ok := supported[k]
					// Use the closest qualifying neighborhood, with a deterministic
					// tie break independent of map iteration and duplicate returns.
					if !ok || distance < old.distance || distance == old.distance && (key.X < old.source.X || key.X == old.source.X && key.Y < old.source.Y) {
						supported[k] = support{direction, distance, key}
					}
				}
			}
		}
		for k, support := range supported {
			v := columns[k]
			p := center(k)
			result.Surfaces = append(result.Surfaces, Surface{Position: Vertex{p.X, p.Y, (v.low + v.high) / 2}, Direction: support.direction})
			accepted[k] = true
		}
	}
	// Large level tops are a separate, weak class. Their footprint is never
	// interpreted as a filled box or as navigation free space beneath a desk.
	tops := map[Cell]float64{}
	for _, v := range points {
		if v.Z < floor+.35 || v.Z > floor+1.6 {
			continue
		}
		k := cell(Point{v.X, v.Y})
		if old, ok := tops[k]; !ok || v.Z > old {
			tops[k] = v.Z
		}
	}
	for k, z := range tops {
		var neighbors []Point
		for dy := -2; dy <= 2; dy++ {
			for dx := -2; dx <= 2; dx++ {
				if other, ok := tops[Cell{k.X + dx, k.Y + dy}]; ok && math.Abs(other-z) < .035 {
					neighbors = append(neighbors, Point{float64(dx) * CellSize, float64(dy) * CellSize})
				}
			}
		}
		if len(neighbors) < 20 {
			continue
		}
		p := center(k)
		result.Surfaces = append(result.Surfaces, Surface{Position: Vertex{p.X, p.Y, z}, Horizontal: true})
		accepted[k] = true
	}
	sort.Slice(result.Surfaces, func(i, j int) bool {
		a, b := result.Surfaces[i], result.Surfaces[j]
		if a.Position.Z != b.Position.Z {
			return a.Position.Z < b.Position.Z
		}
		if a.Position.X != b.Position.X {
			return a.Position.X < b.Position.X
		}
		return a.Position.Y < b.Position.Y
	})
	if len(result.Surfaces) > 900 {
		all := result.Surfaces
		result.Surfaces = make([]Surface, 900)
		for i := range result.Surfaces {
			result.Surfaces[i] = all[i*len(all)/900]
		}
	}
	// A bounded, deterministic sample makes rejected geometry inspectable.
	seen := map[Cell]bool{}
	for _, v := range points {
		k := cell(Point{v.X, v.Y})
		if v.Z > floor+.12 && !accepted[k] && !seen[k] {
			result.Rejected = append(result.Rejected, v)
			seen[k] = true
		}
	}
	if len(result.Rejected) > 500 {
		all := result.Rejected
		result.Rejected = make([]Vertex, 500)
		for i := range result.Rejected {
			result.Rejected[i] = all[i*len(all)/500]
		}
	}
	return result
}

func sortPoints(points []Point) {
	sort.Slice(points, func(i, j int) bool {
		if points[i].X == points[j].X {
			return points[i].Y < points[j].Y
		}
		return points[i].X < points[j].X
	})
}

func (c SurfaceCloud) Fingerprint() uint64 {
	hash := uint64(1469598103934665603)
	for _, p := range c.Surfaces {
		for _, value := range []float64{p.Position.X, p.Position.Y, p.Position.Z} {
			hash ^= uint64(int64(math.Round(value / .15)))
			hash *= 1099511628211
		}
	}
	return hash
}
