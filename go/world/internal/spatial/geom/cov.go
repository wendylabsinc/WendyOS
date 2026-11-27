package geom

import "math"

// Cov2 is a row-major 2x2 covariance of a planar point, [xx, xz, zx, zz] in m².
type Cov2 [4]float64

func (c Cov2) Add(o Cov2) Cov2 { return Cov2{c[0] + o[0], c[1] + o[1], c[2] + o[2], c[3] + o[3]} }
func (c Cov2) Det() float64    { return c[0]*c[3] - c[1]*c[2] }

// Rotate returns R·c·Rᵀ for the planar rotation by yaw.
func (c Cov2) Rotate(yaw float64) Cov2 {
	s, k := math.Sincos(yaw)
	// R = [k −s; s k]
	a0, a1 := k*c[0]-s*c[2], k*c[1]-s*c[3]
	a2, a3 := s*c[0]+k*c[2], s*c[1]+k*c[3]
	return Cov2{a0*k - a1*s, a0*s + a1*k, a2*k - a3*s, a2*s + a3*k}
}

// Inverse returns ErrSingular unless the determinant is positive beyond 1e-14
// of the product of the diagonal terms.
func (c Cov2) Inverse() (Cov2, error) {
	if !finite(c[:]...) {
		return Cov2{}, ErrNonFinite
	}
	det := c.Det()
	if det <= 1e-14*math.Abs(c[0]*c[3]) || det <= 0 {
		return Cov2{}, ErrSingular
	}
	return Cov2{c[3] / det, -c[1] / det, -c[2] / det, c[0] / det}, nil
}

// Eigen returns the variances along the principal axes (major ≥ minor) and
// the yaw of the major axis. The off-diagonal terms are averaged.
func (c Cov2) Eigen() (major, minor, yaw float64) {
	b := (c[1] + c[2]) / 2
	mean, diff := (c[0]+c[3])/2, (c[0]-c[3])/2
	r := math.Hypot(diff, b)
	return mean + r, mean - r, math.Atan2(2*b, c[0]-c[3]) / 2
}

// MaxSigma is the standard deviation along the major axis, a conservative
// scalar for "uncertainty_m" fields.
func (c Cov2) MaxSigma() float64 {
	major, _, _ := c.Eigen()
	return math.Sqrt(math.Max(major, 0))
}

// PositionCov extracts the (x, z) block of a planar pose covariance, whose
// state order is (x, z, yaw).
func PositionCov(c Mat3) Cov2 { return Cov2{c[0], c[1], c[3], c[4]} }

func sandwich3(j, c Mat3) Mat3 { return j.Mul(c).Mul(j.T()) }

// TransformCov2 maps a point covariance from p's child frame to its parent.
func TransformCov2(p Pose2, c Cov2) Cov2 { return c.Rotate(p.Yaw) }

// TransformCov3 maps the covariance of a planar pose (x, z, yaw) from p's
// child frame to its parent, p being exact.
func TransformCov3(p Pose2, c Mat3) Mat3 {
	s, k := math.Sincos(p.Yaw)
	return sandwich3(Mat3{k, -s, 0, s, k, 0, 0, 0, 1}, c)
}

// ComposeCov is the first-order covariance of a.Compose(b) for independent a
// and b with covariances ca and cb over (x, z, yaw).
func ComposeCov(a Pose2, ca Mat3, b Pose2, cb Mat3) Mat3 {
	s, k := math.Sincos(a.Yaw)
	ja := Mat3{1, 0, -s*b.X - k*b.Z, 0, 1, k*b.X - s*b.Z, 0, 0, 1}
	jb := Mat3{k, -s, 0, s, k, 0, 0, 0, 1}
	return sandwich3(ja, ca).Add(sandwich3(jb, cb))
}

// InverseCov is the first-order covariance of p.Inverse() given p's covariance.
func InverseCov(p Pose2, c Mat3) Mat3 {
	s, k := math.Sincos(p.Yaw)
	inv := p.Inverse()
	return sandwich3(Mat3{-k, -s, inv.Z, s, -k, -inv.X, 0, 0, -1}, c)
}

// ApplyCov is the first-order covariance of p.Apply(q) for an uncertain pose
// (covariance cp) and an independent uncertain point in its frame (cq).
func ApplyCov(p Pose2, cp Mat3, q Vec2, cq Cov2) Cov2 {
	s, k := math.Sincos(p.Yaw)
	// J = [1 0 dx; 0 1 dz] with d = ∂(R·q)/∂yaw.
	dx, dz := -s*q.X-k*q.Z, k*q.X-s*q.Z
	out := Cov2{
		cp[0] + dx*(cp[2]+cp[6]) + dx*dx*cp[8],
		cp[1] + dx*cp[7] + dz*cp[2] + dx*dz*cp[8],
		cp[3] + dz*cp[6] + dx*cp[5] + dx*dz*cp[8],
		cp[4] + dz*(cp[5]+cp[7]) + dz*dz*cp[8],
	}
	return out.Add(cq.Rotate(p.Yaw))
}

// MahalanobisSq2 is dᵀ·c⁻¹·d, to be compared with ChiSquareQuantile(2, p).
func MahalanobisSq2(d Vec2, c Cov2) (float64, error) {
	inv, err := c.Inverse()
	if err != nil {
		return 0, err
	}
	return d.X*(inv[0]*d.X+inv[1]*d.Z) + d.Z*(inv[2]*d.X+inv[3]*d.Z), nil
}

// MahalanobisSq3 is dᵀ·c⁻¹·d for a three-component residual such as a planar
// pose difference (wrap the yaw component first); compare with
// ChiSquareQuantile(3, p).
func MahalanobisSq3(d [3]float64, c Mat3) (float64, error) {
	return MahalanobisSq(d[:], c.Mat())
}

// MahalanobisSq is dᵀ·c⁻¹·d for symmetric positive definite c.
func MahalanobisSq(d []float64, c Mat) (float64, error) {
	x, err := SolveCholesky(c, d)
	if err != nil {
		return 0, err
	}
	sum := 0.0
	for i := range d {
		sum += d[i] * x[i]
	}
	return sum, nil
}

// ChiSquareCDF is P(X ≤ x) for a chi-square variable with 1, 2 or 3 degrees
// of freedom.
func ChiSquareCDF(dof int, x float64) (float64, error) {
	if dof < 1 || dof > 3 || math.IsNaN(x) {
		return 0, ErrDimension
	}
	if x <= 0 {
		return 0, nil
	}
	switch dof {
	case 1:
		return math.Erf(math.Sqrt(x / 2)), nil
	case 2:
		return -math.Expm1(-x / 2), nil
	default:
		return math.Erf(math.Sqrt(x/2)) - math.Sqrt(2*x/math.Pi)*math.Exp(-x/2), nil
	}
}

// ChiSquareQuantile returns the gate g with P(X ≤ g) = p for 1, 2 or 3 degrees
// of freedom and p in (0, 1): a squared Mahalanobis distance above g is
// rejected at confidence p (for example 5.991 for 2 dof at 0.95).
func ChiSquareQuantile(dof int, p float64) (float64, error) {
	if dof < 1 || dof > 3 || !(p > 0 && p < 1) {
		return 0, ErrDimension
	}
	lo, hi := 0.0, 1.0
	for {
		c, _ := ChiSquareCDF(dof, hi)
		if c >= p {
			break
		}
		lo, hi = hi, hi*2
		if hi > 1e6 {
			return 0, ErrNoConvergence
		}
	}
	for i := 0; i < 200 && hi-lo > 1e-13*hi; i++ {
		mid := (lo + hi) / 2
		if c, _ := ChiSquareCDF(dof, mid); c < p {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2, nil
}
