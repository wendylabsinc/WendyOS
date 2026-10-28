package sitenav

import "math"

// planarWallDirection tests the whole neighborhood, not a hand-picked strip
// inside it. A filled occupied patch contains many straight rows but is not a
// wall. Quantized thin surfaces may have up to 6 cm transverse standard deviation.
func planarWallDirection(points []Point) (Point, bool) {
	if len(points) < 6 {
		return Point{}, false
	}
	var mean Point
	for _, p := range points {
		mean.X += p.X
		mean.Y += p.Y
	}
	n := float64(len(points))
	mean.X /= n
	mean.Y /= n
	xx, xy, yy := 0.0, 0.0, 0.0
	for _, p := range points {
		x, y := p.X-mean.X, p.Y-mean.Y
		xx += x * x / n
		xy += x * y / n
		yy += y * y / n
	}
	delta := math.Hypot(xx-yy, 2*xy)
	major, minor := (xx+yy+delta)/2, (xx+yy-delta)/2
	if major < .025 || minor > .06*.06 || minor > .12*major {
		return Point{}, false
	}
	angle := math.Atan2(2*xy, xx-yy) / 2
	direction := Point{math.Cos(angle), math.Sin(angle)}
	low, high := math.Inf(1), math.Inf(-1)
	for _, p := range points {
		along := (p.X-mean.X)*direction.X + (p.Y-mean.Y)*direction.Y
		low, high = math.Min(low, along), math.Max(high, along)
	}
	return direction, high-low >= .60-1e-6
}
