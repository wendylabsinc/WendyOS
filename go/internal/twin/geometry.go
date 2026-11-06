package twin

import "math"

// FloorTiles renders only locally supported horizontal samples. It does not
// fill unknown space or supply the robot's collision/navigation grid.
func FloorTiles(points []Point) []Point {
	cells := map[[3]int]Point{}
	for _, p := range points {
		cells[[3]int{int(math.Floor(p.X * 10)), int(math.Floor(p.Y * 10)), int(math.Floor(p.Z * 10))}] = p
	}
	result := []Point{}
	for key, p := range cells {
		supported := true
		for _, offset := range [][2]int{{-1, 0}, {1, 0}, {0, -1}, {0, 1}} {
			q, ok := cells[[3]int{key[0] + offset[0], key[1], key[2] + offset[1]}]
			if !ok || math.Abs(q.Y-p.Y) > .03 {
				supported = false
				break
			}
		}
		if !supported {
			continue
		}
		x, z := (float64(key[0])+.5)/10, (float64(key[2])+.5)/10
		a, b, c, d := Point{x - .05, p.Y, z - .05}, Point{x - .05, p.Y, z + .05}, Point{x + .05, p.Y, z + .05}, Point{x + .05, p.Y, z - .05}
		result = append(result, a, b, c, a, c, d)
		if len(result) >= 60000 {
			break
		}
	}
	return result
}
