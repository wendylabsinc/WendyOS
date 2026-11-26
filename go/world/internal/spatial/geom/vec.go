package geom

import "math"

// Vec2 is a point or direction in the planar map: World X and World Z, metres.
type Vec2 struct {
	X float64 `json:"x"`
	Z float64 `json:"z"`
}

func (a Vec2) Add(b Vec2) Vec2      { return Vec2{a.X + b.X, a.Z + b.Z} }
func (a Vec2) Sub(b Vec2) Vec2      { return Vec2{a.X - b.X, a.Z - b.Z} }
func (a Vec2) Scale(s float64) Vec2 { return Vec2{a.X * s, a.Z * s} }
func (a Vec2) Dot(b Vec2) float64   { return a.X*b.X + a.Z*b.Z }

// Cross is a.X*b.Z − a.Z*b.X: positive when b lies in the +Yaw direction from a.
func (a Vec2) Cross(b Vec2) float64 { return a.X*b.Z - a.Z*b.X }
func (a Vec2) Norm() float64        { return math.Hypot(a.X, a.Z) }
func (a Vec2) Norm2() float64       { return a.X*a.X + a.Z*a.Z }
func (a Vec2) Dist(b Vec2) float64  { return math.Hypot(a.X-b.X, a.Z-b.Z) }

// Unit returns a/|a|, or the zero vector when a is zero.
func (a Vec2) Unit() Vec2 {
	n := a.Norm()
	if n == 0 {
		return Vec2{}
	}
	return Vec2{a.X / n, a.Z / n}
}

// Angle is the yaw of the direction a: atan2(Z, X).
func (a Vec2) Angle() float64 { return math.Atan2(a.Z, a.X) }

// Rotate turns a by yaw, from +X toward +Z.
func (a Vec2) Rotate(yaw float64) Vec2 {
	s, c := math.Sincos(yaw)
	return Vec2{c*a.X - s*a.Z, s*a.X + c*a.Z}
}

// Perp is a rotated by +π/2.
func (a Vec2) Perp() Vec2 { return Vec2{-a.Z, a.X} }

func (a Vec2) Lerp(b Vec2, t float64) Vec2 {
	return Vec2{a.X + (b.X-a.X)*t, a.Z + (b.Z-a.Z)*t}
}

// Lift places the planar point at World elevation y.
func (a Vec2) Lift(y float64) Vec3 { return Vec3{a.X, y, a.Z} }

// Heading is the unit vector (cos yaw, sin yaw) in (x, z).
func Heading(yaw float64) Vec2 {
	s, c := math.Sincos(yaw)
	return Vec2{c, s}
}

// Vec3 is a point or direction in a right-handed Y-up frame, metres.
type Vec3 struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

func (a Vec3) Add(b Vec3) Vec3      { return Vec3{a.X + b.X, a.Y + b.Y, a.Z + b.Z} }
func (a Vec3) Sub(b Vec3) Vec3      { return Vec3{a.X - b.X, a.Y - b.Y, a.Z - b.Z} }
func (a Vec3) Scale(s float64) Vec3 { return Vec3{a.X * s, a.Y * s, a.Z * s} }
func (a Vec3) Dot(b Vec3) float64   { return a.X*b.X + a.Y*b.Y + a.Z*b.Z }
func (a Vec3) Cross(b Vec3) Vec3 {
	return Vec3{a.Y*b.Z - a.Z*b.Y, a.Z*b.X - a.X*b.Z, a.X*b.Y - a.Y*b.X}
}
func (a Vec3) Norm() float64       { return math.Sqrt(a.Dot(a)) }
func (a Vec3) Norm2() float64      { return a.Dot(a) }
func (a Vec3) Dist(b Vec3) float64 { return a.Sub(b).Norm() }

// Unit returns a/|a|, or the zero vector when a is zero.
func (a Vec3) Unit() Vec3 {
	n := a.Norm()
	if n == 0 {
		return Vec3{}
	}
	return a.Scale(1 / n)
}

func (a Vec3) Lerp(b Vec3, t float64) Vec3 { return a.Add(b.Sub(a).Scale(t)) }

// XZ drops the elevation.
func (a Vec3) XZ() Vec2 { return Vec2{a.X, a.Z} }

// Array returns [x, y, z], the layout of World "position_m" fields.
func (a Vec3) Array() [3]float64 { return [3]float64{a.X, a.Y, a.Z} }

// Vec3FromArray reads [x, y, z].
func Vec3FromArray(p [3]float64) Vec3 { return Vec3{p[0], p[1], p[2]} }
