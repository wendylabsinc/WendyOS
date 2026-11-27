package geom

import "math"

// Segment is a line segment in the planar map.
type Segment struct {
	A Vec2 `json:"a"`
	B Vec2 `json:"b"`
}

func (s Segment) Length() float64 { return s.A.Dist(s.B) }
func (s Segment) Midpoint() Vec2  { return s.A.Lerp(s.B, 0.5) }

// Dir is the unit direction from A to B, zero for a point segment.
func (s Segment) Dir() Vec2 { return s.B.Sub(s.A).Unit() }

// ClosestPoint returns the point of the segment nearest to p.
func (s Segment) ClosestPoint(p Vec2) Vec2 {
	d := s.B.Sub(s.A)
	n := d.Norm2()
	if n == 0 {
		return s.A
	}
	t := math.Max(0, math.Min(1, p.Sub(s.A).Dot(d)/n))
	return s.A.Add(d.Scale(t))
}

// DistanceTo is the distance from p to the segment.
func (s Segment) DistanceTo(p Vec2) float64 { return s.ClosestPoint(p).Dist(p) }

// Intersect returns the crossing point of two segments, endpoints included.
// Parallel segments report no intersection even when they overlap.
func (s Segment) Intersect(o Segment) (Vec2, bool) {
	d1, d2 := s.B.Sub(s.A), o.B.Sub(o.A)
	den := d1.Cross(d2)
	if math.Abs(den) <= 1e-12*d1.Norm()*d2.Norm() {
		return Vec2{}, false
	}
	w := o.A.Sub(s.A)
	t, u := w.Cross(d2)/den, w.Cross(d1)/den
	if t < 0 || t > 1 || u < 0 || u > 1 {
		return Vec2{}, false
	}
	return s.A.Add(d1.Scale(t)), true
}

// SegmentDistance is the smallest distance between two segments, zero when
// they cross.
func SegmentDistance(a, b Segment) float64 {
	if _, ok := a.Intersect(b); ok {
		return 0
	}
	return math.Min(math.Min(a.DistanceTo(b.A), a.DistanceTo(b.B)), math.Min(b.DistanceTo(a.A), b.DistanceTo(a.B)))
}

// RaySegment returns the parameter t ≥ 0 at which origin + t·dir meets the
// segment. With a unit dir, t is the range in metres. A ray parallel to the
// segment reports no hit.
func RaySegment(origin, dir Vec2, s Segment) (float64, bool) {
	d := s.B.Sub(s.A)
	den := dir.Cross(d)
	if math.Abs(den) <= 1e-12*dir.Norm()*d.Norm() {
		return 0, false
	}
	w := s.A.Sub(origin)
	t, u := w.Cross(d)/den, w.Cross(dir)/den
	if t < 0 || u < 0 || u > 1 {
		return 0, false
	}
	return t, true
}

// RayCircle returns the smallest t ≥ 0 at which origin + t·dir meets the
// circle. From inside the circle that is the exit point.
func RayCircle(origin, dir, center Vec2, radius float64) (float64, bool) {
	a := dir.Norm2()
	if a == 0 || !(radius >= 0) {
		return 0, false
	}
	w := origin.Sub(center)
	b := w.Dot(dir)
	disc := b*b - a*(w.Norm2()-radius*radius)
	if disc < 0 {
		return 0, false
	}
	sq := math.Sqrt(disc)
	if t := (-b - sq) / a; t >= 0 {
		return t, true
	}
	if t := (-b + sq) / a; t >= 0 {
		return t, true
	}
	return 0, false
}

// PointInPolygon tests p against a simple polygon given by its vertices in
// either winding (even-odd rule). Points exactly on the boundary may report
// either side.
func PointInPolygon(p Vec2, poly []Vec2) bool {
	in := false
	for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
		a, b := poly[i], poly[j]
		if (a.Z > p.Z) != (b.Z > p.Z) && p.X < a.X+(b.X-a.X)*(p.Z-a.Z)/(b.Z-a.Z) {
			in = !in
		}
	}
	return in
}

// PolygonSignedArea is the shoelace area in m²: positive when the vertices
// turn from +X toward +Z, the same sense as positive yaw.
func PolygonSignedArea(poly []Vec2) float64 {
	sum := 0.0
	for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
		sum += poly[j].Cross(poly[i])
	}
	return sum / 2
}

// PolygonArea is the unsigned area in m².
func PolygonArea(poly []Vec2) float64 { return math.Abs(PolygonSignedArea(poly)) }

// PolygonCentroid is the area centroid of a simple polygon. ErrDegenerate for
// fewer than three vertices or zero area.
func PolygonCentroid(poly []Vec2) (Vec2, error) {
	if len(poly) < 3 {
		return Vec2{}, ErrDegenerate
	}
	// Accumulate relative to the first vertex to keep precision far from the origin.
	o := poly[0]
	var cx, cz, area, scale float64
	for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
		a, b := poly[j].Sub(o), poly[i].Sub(o)
		cr := a.Cross(b)
		area += cr
		cx += (a.X + b.X) * cr
		cz += (a.Z + b.Z) * cr
		scale = math.Max(scale, b.Norm2())
	}
	if !finite(area, cx, cz) {
		return Vec2{}, ErrNonFinite
	}
	if math.Abs(area) <= 1e-12*scale {
		return Vec2{}, ErrDegenerate
	}
	return Vec2{o.X + cx/(3*area), o.Z + cz/(3*area)}, nil
}
