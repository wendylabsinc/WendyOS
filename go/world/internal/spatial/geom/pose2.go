package geom

import "math"

// WrapAngle maps an angle to (−π, π].
func WrapAngle(a float64) float64 {
	a = math.Mod(a, 2*math.Pi)
	if a > math.Pi {
		a -= 2 * math.Pi
	} else if a <= -math.Pi {
		a += 2 * math.Pi
	}
	return a
}

// AngleDiff is the signed shortest rotation from b to a, in (−π, π].
func AngleDiff(a, b float64) float64 { return WrapAngle(a - b) }

// Pose2 places a planar child frame in its parent: translation (X, Z) in
// metres and Yaw from +X toward +Z. The zero value is the identity.
type Pose2 struct {
	X   float64 `json:"x"`
	Z   float64 `json:"z"`
	Yaw float64 `json:"yaw"`
}

// Pose2FromROS converts a ROS REP-103 planar pose (x forward, y left, yaw
// counter-clockwise seen from above) to this package's convention (z right,
// yaw clockwise): y and yaw change sign. The same sign change applies to a
// lidar or bearing angle a ROS-style driver reports.
func Pose2FromROS(x, y, yaw float64) Pose2 { return Pose2{x, -y, WrapAngle(-yaw)} }

func (p Pose2) Pos() Vec2     { return Vec2{p.X, p.Z} }
func (p Pose2) Heading() Vec2 { return Heading(p.Yaw) }

// Apply maps a point from the child frame to the parent frame.
func (p Pose2) Apply(v Vec2) Vec2 {
	s, c := math.Sincos(p.Yaw)
	return Vec2{p.X + c*v.X - s*v.Z, p.Z + s*v.X + c*v.Z}
}

// InverseApply maps a point from the parent frame to the child frame.
func (p Pose2) InverseApply(v Vec2) Vec2 {
	s, c := math.Sincos(p.Yaw)
	dx, dz := v.X-p.X, v.Z-p.Z
	return Vec2{c*dx + s*dz, -s*dx + c*dz}
}

// Rotate maps a direction from the child frame to the parent frame.
func (p Pose2) Rotate(v Vec2) Vec2 { return v.Rotate(p.Yaw) }

// Compose returns p∘q: q is expressed in p's child frame, the result in p's
// parent frame. (p.Compose(q)).Apply(v) == p.Apply(q.Apply(v)).
func (p Pose2) Compose(q Pose2) Pose2 {
	t := p.Apply(q.Pos())
	return Pose2{t.X, t.Z, WrapAngle(p.Yaw + q.Yaw)}
}

// Inverse maps parent coordinates to child coordinates.
func (p Pose2) Inverse() Pose2 {
	s, c := math.Sincos(p.Yaw)
	return Pose2{-c*p.X - s*p.Z, s*p.X - c*p.Z, WrapAngle(-p.Yaw)}
}

// Between returns q expressed in p's frame: p.Compose(p.Between(q)) == q.
func (p Pose2) Between(q Pose2) Pose2 {
	t := p.InverseApply(q.Pos())
	return Pose2{t.X, t.Z, AngleDiff(q.Yaw, p.Yaw)}
}

// Interpolate blends linearly in translation and along the shortest arc in
// yaw. t=0 gives p, t=1 gives q; t outside [0, 1] extrapolates.
func (p Pose2) Interpolate(q Pose2, t float64) Pose2 {
	return Pose2{p.X + (q.X-p.X)*t, p.Z + (q.Z-p.Z)*t, WrapAngle(p.Yaw + t*AngleDiff(q.Yaw, p.Yaw))}
}

// Normalized wraps Yaw to (−π, π].
func (p Pose2) Normalized() Pose2 { return Pose2{p.X, p.Z, WrapAngle(p.Yaw)} }
