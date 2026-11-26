package geom

import (
	"errors"
	"math"
)

// Pixel is an image coordinate in pixels: U to the right, V down.
type Pixel struct {
	U float64 `json:"u"`
	V float64 `json:"v"`
}

// Dist is the distance in pixels, the unit of reprojection error.
func (p Pixel) Dist(q Pixel) float64 { return math.Hypot(p.U-q.U, p.V-q.V) }

// Intrinsics is a pinhole camera with two radial distortion terms. For a
// point (X, Y, Z) in the optical frame (x right, y down, z forward):
//
//	x, y = X/Z, Y/Z;  r² = x²+y²;  d = 1 + K1·r² + K2·r⁴
//	u = FX·x·d + CX;  v = FY·y·d + CY
//
// FX, FY, CX, CY are in pixels. Width and Height are the image size in pixels.
type Intrinsics struct {
	FX     float64 `json:"fx"`
	FY     float64 `json:"fy"`
	CX     float64 `json:"cx"`
	CY     float64 `json:"cy"`
	K1     float64 `json:"k1"`
	K2     float64 `json:"k2"`
	Width  int     `json:"width"`
	Height int     `json:"height"`
}

// Validate checks for finite values, positive focal lengths and image size.
func (in Intrinsics) Validate() error {
	if !finite(in.FX, in.FY, in.CX, in.CY, in.K1, in.K2) || in.FX <= 0 || in.FY <= 0 || in.Width <= 0 || in.Height <= 0 {
		return errors.New("geom: invalid camera intrinsics")
	}
	return nil
}

// K is the 3x3 calibration matrix.
func (in Intrinsics) K() Mat3 { return Mat3{in.FX, 0, in.CX, 0, in.FY, in.CY, 0, 0, 1} }

// maxR2 is the squared normalised radius up to which r·d(r) is strictly
// increasing. Beyond it the distortion model folds back and is not invertible.
func (in Intrinsics) maxR2() float64 {
	a, b := 5*in.K2, 3*in.K1 // derivative of r·d(r) is a·s² + b·s + 1, s = r²
	if a == 0 {
		if b >= 0 {
			return math.Inf(1)
		}
		return -1 / b
	}
	disc := b*b - 4*a
	if disc < 0 {
		return math.Inf(1)
	}
	sq := math.Sqrt(disc)
	best := math.Inf(1)
	for _, root := range [2]float64{(-b - sq) / (2 * a), (-b + sq) / (2 * a)} {
		if root > 0 && root < best {
			best = root
		}
	}
	return best
}

// minDepth is the smallest forward distance, in metres, that projects.
const minDepth = 1e-6

// Project maps a point in the optical frame to a raw (distorted) pixel. ok is
// false when the point is not in front of the camera or lies outside the
// radius where the distortion model is monotonic. It does not test the image
// bounds; see InImage.
func (in Intrinsics) Project(p Vec3) (Pixel, bool) {
	if !(p.Z > minDepth) {
		return Pixel{}, false
	}
	x, y := p.X/p.Z, p.Y/p.Z
	r2 := x*x + y*y
	if !(r2 < in.maxR2()) {
		return Pixel{}, false
	}
	d := 1 + in.K1*r2 + in.K2*r2*r2
	return Pixel{in.FX*x*d + in.CX, in.FY*y*d + in.CY}, true
}

// InImage reports whether the pixel lies inside [0, Width) x [0, Height).
func (in Intrinsics) InImage(px Pixel) bool {
	return px.U >= 0 && px.V >= 0 && px.U < float64(in.Width) && px.V < float64(in.Height)
}

// ProjectWorld projects a parent-frame point through a camera whose optical
// frame is placed in that parent by cameraToWorld.
func (in Intrinsics) ProjectWorld(cameraToWorld Pose3, p Vec3) (Pixel, bool) {
	return in.Project(cameraToWorld.Inverse().Apply(p))
}

// Undistort inverts Project up to depth: it returns the ray (x, y, 1) in the
// optical frame for a raw pixel, solving the radial polynomial iteratively
// (safeguarded Newton). ErrDegenerate means the pixel lies beyond the radius
// the distortion model can produce.
func (in Intrinsics) Undistort(px Pixel) (Vec3, error) {
	if !finite(px.U, px.V) {
		return Vec3{}, ErrNonFinite
	}
	xd, yd := (px.U-in.CX)/in.FX, (px.V-in.CY)/in.FY
	rd := math.Hypot(xd, yd)
	if rd == 0 {
		return Vec3{0, 0, 1}, nil
	}
	f := func(r float64) float64 { r2 := r * r; return r * (1 + in.K1*r2 + in.K2*r2*r2) }
	lo, hi := 0.0, math.Sqrt(in.maxR2())
	if math.IsInf(hi, 1) {
		for hi = math.Max(rd, 1); f(hi) < rd; hi *= 2 {
			if hi > 1e12 {
				return Vec3{}, ErrDegenerate
			}
		}
	} else if f(hi) < rd {
		return Vec3{}, ErrDegenerate
	}
	r := math.Min(rd, hi)
	for i := 0; i < 100; i++ {
		e := f(r) - rd
		if e > 0 {
			hi = r
		} else {
			lo = r
		}
		r2 := r * r
		next := r - e/(1+3*in.K1*r2+5*in.K2*r2*r2)
		if !(next > lo && next < hi) {
			next = (lo + hi) / 2
		}
		if math.Abs(next-r) <= 1e-15*math.Max(r, 1e-300) || hi-lo <= 1e-16*hi {
			r = next
			break
		}
		r = next
	}
	s := r / rd
	return Vec3{xd * s, yd * s, 1}, nil
}

// PixelRay returns the parent-frame origin and unit direction of the ray
// through a raw pixel.
func (in Intrinsics) PixelRay(cameraToWorld Pose3, px Pixel) (origin, dir Vec3, err error) {
	ray, err := in.Undistort(px)
	if err != nil {
		return Vec3{}, Vec3{}, err
	}
	return cameraToWorld.T, cameraToWorld.Rotate(ray).Unit(), nil
}

// RayPlaneY intersects a ray with the horizontal plane at World elevation y,
// for example to place an object's foot pixel on its floor. ok is false when
// the ray is parallel to the plane or points away from it.
func RayPlaneY(origin, dir Vec3, y float64) (Vec3, bool) {
	if math.Abs(dir.Y) < 1e-12 {
		return Vec3{}, false
	}
	t := (y - origin.Y) / dir.Y
	if !(t > 0) {
		return Vec3{}, false
	}
	return origin.Add(dir.Scale(t)), true
}

// LookAt returns the camera-to-world pose of a camera at eye whose optical
// axis points at target, with the image "up" direction as close to up as the
// view allows. ErrDegenerate when eye equals target or the view is along up.
func LookAt(eye, target, up Vec3) (Pose3, error) {
	z := target.Sub(eye).Unit()
	x := z.Cross(up).Unit()
	if z == (Vec3{}) || x == (Vec3{}) || !finite(z.X, z.Y, z.Z, x.X, x.Y, x.Z) {
		return Pose3{}, ErrDegenerate
	}
	y := z.Cross(x)
	return Pose3{Mat3{x.X, y.X, z.X, x.Y, y.Y, z.Y, x.Z, y.Z, z.Z}, eye}, nil
}
