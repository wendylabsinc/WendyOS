package geom

import "math"

// Mat3 is a row-major 3x3 matrix: element (row i, column j) is m[i*3+j].
type Mat3 [9]float64

func Identity3() Mat3 { return Mat3{1, 0, 0, 0, 1, 0, 0, 0, 1} }

func (m Mat3) Mul(o Mat3) Mat3 {
	var r Mat3
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			r[i*3+j] = m[i*3]*o[j] + m[i*3+1]*o[3+j] + m[i*3+2]*o[6+j]
		}
	}
	return r
}

func (m Mat3) MulVec(v Vec3) Vec3 {
	return Vec3{
		m[0]*v.X + m[1]*v.Y + m[2]*v.Z,
		m[3]*v.X + m[4]*v.Y + m[5]*v.Z,
		m[6]*v.X + m[7]*v.Y + m[8]*v.Z,
	}
}

// T is the transpose.
func (m Mat3) T() Mat3 { return Mat3{m[0], m[3], m[6], m[1], m[4], m[7], m[2], m[5], m[8]} }

func (m Mat3) Add(o Mat3) Mat3 {
	for i := range m {
		m[i] += o[i]
	}
	return m
}

func (m Mat3) Scale(s float64) Mat3 {
	for i := range m {
		m[i] *= s
	}
	return m
}

func (m Mat3) Det() float64 {
	return m[0]*(m[4]*m[8]-m[5]*m[7]) - m[1]*(m[3]*m[8]-m[5]*m[6]) + m[2]*(m[3]*m[7]-m[4]*m[6])
}

// Inverse returns m⁻¹ under the conditioning rule of Solve: LU on the row- and
// column-equilibrated matrix, so entries of mixed units (a pose covariance
// with 1e4 m² along an unobserved axis and 1e-6 rad² in yaw) do not make a
// well-posed matrix look singular. The inverse of an exactly symmetric m is
// exactly symmetric, as a covariance turned into an information matrix must be.
func (m Mat3) Inverse() (Mat3, error) {
	inv, err := Inverse(m.Mat())
	if err != nil {
		return Mat3{}, err
	}
	var out Mat3
	copy(out[:], inv.Data)
	if m[1] == m[3] && m[2] == m[6] && m[5] == m[7] {
		out[1], out[2], out[5] = (out[1]+out[3])/2, (out[2]+out[6])/2, (out[5]+out[7])/2
		out[3], out[6], out[7] = out[1], out[2], out[5]
	}
	return out, nil
}

// Mat returns the general-matrix copy.
func (m Mat3) Mat() Mat { return MatFrom(3, 3, m[:]) }

// Orthonormalize returns the rotation nearest to m in the Frobenius norm
// (U·Vᵀ of the SVD with the determinant forced to +1). It fails when m has
// rank below two, which leaves the rotation undetermined.
func (m Mat3) Orthonormalize() (Mat3, error) {
	svd, err := SVD(m.Mat())
	if err != nil {
		return Mat3{}, err
	}
	if svd.S[1] <= 1e-12*svd.S[0] {
		return Mat3{}, ErrSingular
	}
	var u, v Mat3
	copy(u[:], svd.U.Data)
	copy(v[:], svd.V.Data)
	if svd.S[2] <= 1e-12*svd.S[0] {
		// Rank two: the third left vector is not defined by m; complete the basis.
		c := Vec3{u[0], u[3], u[6]}.Cross(Vec3{u[1], u[4], u[7]})
		u[2], u[5], u[8] = c.X, c.Y, c.Z
	}
	r := u.Mul(v.T())
	if r.Det() < 0 {
		u[2], u[5], u[8] = -u[2], -u[5], -u[8]
		r = u.Mul(v.T())
	}
	return r, nil
}

// RQ3 factors m = k·q with k upper triangular with a positive diagonal and q
// orthogonal. For a projection matrix P = K[R|t] pass the left 3x3 block; if
// det(q) comes back −1 the block was negated (P is defined up to sign), so
// negate P and factor again.
func RQ3(m Mat3) (k, q Mat3, err error) {
	if !finite(m[:]...) {
		return k, q, ErrNonFinite
	}
	scale := 0.0
	for _, v := range m {
		scale = math.Max(scale, math.Abs(v))
	}
	// Gram-Schmidt on the rows from the bottom up: row i of m is a combination
	// of rows i..2 of q, which makes k upper triangular.
	var rows [3]Vec3
	for i := 2; i >= 0; i-- {
		v := Vec3{m[i*3], m[i*3+1], m[i*3+2]}
		for pass := 0; pass < 2; pass++ { // second pass removes rounding residue
			for j := i + 1; j < 3; j++ {
				v = v.Sub(rows[j].Scale(v.Dot(rows[j])))
			}
		}
		n := v.Norm()
		if n <= 1e-13*scale {
			return k, q, ErrSingular
		}
		rows[i] = v.Scale(1 / n)
	}
	for i := 0; i < 3; i++ {
		q[i*3], q[i*3+1], q[i*3+2] = rows[i].X, rows[i].Y, rows[i].Z
		mi := Vec3{m[i*3], m[i*3+1], m[i*3+2]}
		for j := i; j < 3; j++ {
			k[i*3+j] = mi.Dot(rows[j])
		}
	}
	return k, q, nil
}

// RotX, RotY and RotZ are right-handed rotations about the named axis.
func RotX(a float64) Mat3 {
	s, c := math.Sincos(a)
	return Mat3{1, 0, 0, 0, c, -s, 0, s, c}
}

func RotY(a float64) Mat3 {
	s, c := math.Sincos(a)
	return Mat3{c, 0, s, 0, 1, 0, -s, 0, c}
}

func RotZ(a float64) Mat3 {
	s, c := math.Sincos(a)
	return Mat3{c, -s, 0, s, c, 0, 0, 0, 1}
}

// RotExp is the exponential map: the rotation by |w| radians about w.
func RotExp(w Vec3) Mat3 {
	theta := w.Norm()
	if theta < 1e-12 {
		return Mat3{1, -w.Z, w.Y, w.Z, 1, -w.X, -w.Y, w.X, 1}
	}
	a := w.Scale(1 / theta)
	s, c := math.Sincos(theta)
	t := 1 - c
	return Mat3{
		c + t*a.X*a.X, t*a.X*a.Y - s*a.Z, t*a.X*a.Z + s*a.Y,
		t*a.X*a.Y + s*a.Z, c + t*a.Y*a.Y, t*a.Y*a.Z - s*a.X,
		t*a.X*a.Z - s*a.Y, t*a.Y*a.Z + s*a.X, c + t*a.Z*a.Z,
	}
}

// RotLog is the inverse of RotExp: the axis-angle vector with |w| in [0, π].
func RotLog(r Mat3) Vec3 {
	q := QuatFromRot(r)
	v := Vec3{q[0], q[1], q[2]}
	n := v.Norm()
	if n < 1e-12 {
		return v.Scale(2)
	}
	return v.Scale(2 * math.Atan2(n, q[3]) / n)
}

// RotAngle is the angle in [0, π] of the rotation taking a to b.
func RotAngle(a, b Mat3) float64 { return RotLog(a.T().Mul(b)).Norm() }

// QuatFromRot converts a rotation matrix to a unit quaternion [x, y, z, w]
// with w ≥ 0, the layout of world.LocalFrame.RotationXYZW.
func QuatFromRot(m Mat3) [4]float64 {
	var x, y, z, w float64
	switch tr := m[0] + m[4] + m[8]; {
	case tr > 0:
		s := math.Sqrt(tr+1) * 2
		w, x, y, z = s/4, (m[7]-m[5])/s, (m[2]-m[6])/s, (m[3]-m[1])/s
	case m[0] > m[4] && m[0] > m[8]:
		s := math.Sqrt(1+m[0]-m[4]-m[8]) * 2
		w, x, y, z = (m[7]-m[5])/s, s/4, (m[1]+m[3])/s, (m[2]+m[6])/s
	case m[4] > m[8]:
		s := math.Sqrt(1+m[4]-m[0]-m[8]) * 2
		w, x, y, z = (m[2]-m[6])/s, (m[1]+m[3])/s, s/4, (m[5]+m[7])/s
	default:
		s := math.Sqrt(1+m[8]-m[0]-m[4]) * 2
		w, x, y, z = (m[3]-m[1])/s, (m[2]+m[6])/s, (m[5]+m[7])/s, s/4
	}
	n := math.Sqrt(x*x + y*y + z*z + w*w)
	if w < 0 {
		n = -n
	}
	return [4]float64{x / n, y / n, z / n, w / n}
}

// RotFromQuat converts a quaternion [x, y, z, w]. The input is normalised; a
// zero or non-finite quaternion is an error.
func RotFromQuat(q [4]float64) (Mat3, error) {
	if !finite(q[:]...) {
		return Mat3{}, ErrNonFinite
	}
	n := math.Sqrt(q[0]*q[0] + q[1]*q[1] + q[2]*q[2] + q[3]*q[3])
	if n < 1e-12 {
		return Mat3{}, ErrDegenerate
	}
	x, y, z, w := q[0]/n, q[1]/n, q[2]/n, q[3]/n
	return Mat3{
		1 - 2*(y*y+z*z), 2 * (x*y - z*w), 2 * (x*z + y*w),
		2 * (x*y + z*w), 1 - 2*(x*x+z*z), 2 * (y*z - x*w),
		2 * (x*z - y*w), 2 * (y*z + x*w), 1 - 2*(x*x+y*y),
	}, nil
}

// Pose3 places a child frame in its parent: parent = R·child + T. R is a
// rotation matrix, T is in metres.
type Pose3 struct {
	R Mat3 `json:"r"`
	T Vec3 `json:"t"`
}

func IdentityPose3() Pose3 { return Pose3{R: Identity3()} }

// Apply maps a point from the child frame to the parent frame.
func (p Pose3) Apply(v Vec3) Vec3 { return p.R.MulVec(v).Add(p.T) }

// Rotate maps a direction from the child frame to the parent frame.
func (p Pose3) Rotate(v Vec3) Vec3 { return p.R.MulVec(v) }

// Compose returns p∘q: (p.Compose(q)).Apply(v) == p.Apply(q.Apply(v)).
func (p Pose3) Compose(q Pose3) Pose3 { return Pose3{p.R.Mul(q.R), p.Apply(q.T)} }

// Inverse maps parent coordinates to child coordinates.
func (p Pose3) Inverse() Pose3 {
	rt := p.R.T()
	return Pose3{rt, rt.MulVec(p.T).Scale(-1)}
}

// Between returns q expressed in p's frame: p.Compose(p.Between(q)) == q.
func (p Pose3) Between(q Pose3) Pose3 { return p.Inverse().Compose(q) }

// QuatXYZW is the rotation as a unit quaternion [x, y, z, w], w ≥ 0.
func (p Pose3) QuatXYZW() [4]float64 { return QuatFromRot(p.R) }

// Pose3FromQuat builds a pose from a quaternion [x, y, z, w] and translation.
func Pose3FromQuat(q [4]float64, t Vec3) (Pose3, error) {
	r, err := RotFromQuat(q)
	if err != nil {
		return Pose3{}, err
	}
	if !finite(t.X, t.Y, t.Z) {
		return Pose3{}, ErrNonFinite
	}
	return Pose3{r, t}, nil
}

// Pose3FromRotVec builds a pose from an axis-angle vector and translation: a
// minimal six-parameter form for optimisation.
func Pose3FromRotVec(w, t Vec3) Pose3 { return Pose3{RotExp(w), t} }

// RotVec is the rotation as an axis-angle vector, |w| in [0, π].
func (p Pose3) RotVec() Vec3 { return RotLog(p.R) }

// Matrix4 is the row-major homogeneous 4x4 form used by
// sensing.Camera.CameraToSite: translation at indices 3, 7 and 11.
func (p Pose3) Matrix4() []float64 {
	r, t := p.R, p.T
	return []float64{
		r[0], r[1], r[2], t.X,
		r[3], r[4], r[5], t.Y,
		r[6], r[7], r[8], t.Z,
		0, 0, 0, 1,
	}
}

// Pose3FromMatrix4 reads a row-major 4x4 rigid transform. It rejects a wrong
// length, a bottom row other than [0 0 0 1], and a rotation block that is not
// orthonormal with determinant +1 to within 1e-6.
func Pose3FromMatrix4(m []float64) (Pose3, error) {
	if len(m) != 16 {
		return Pose3{}, ErrDimension
	}
	if !finite(m...) {
		return Pose3{}, ErrNonFinite
	}
	if math.Abs(m[12]) > 1e-9 || math.Abs(m[13]) > 1e-9 || math.Abs(m[14]) > 1e-9 || math.Abs(m[15]-1) > 1e-9 {
		return Pose3{}, ErrDegenerate
	}
	r := Mat3{m[0], m[1], m[2], m[4], m[5], m[6], m[8], m[9], m[10]}
	if !r.IsRotation(1e-6) {
		return Pose3{}, ErrDegenerate
	}
	return Pose3{r, Vec3{m[3], m[7], m[11]}}, nil
}

// IsRotation reports whether m·mᵀ = I and det m = +1 to within tol.
func (m Mat3) IsRotation(tol float64) bool {
	if !finite(m[:]...) {
		return false
	}
	p := m.Mul(m.T())
	id := Identity3()
	for i := range p {
		if math.Abs(p[i]-id[i]) > tol {
			return false
		}
	}
	return math.Abs(m.Det()-1) <= tol
}

// Pose3FromPose2 lifts a planar pose to 3D at World elevation y. The rotation
// is RotY(−Yaw): body +X (forward) maps to (cos Yaw, 0, sin Yaw), body +Y stays
// up and body +Z (right) maps to (−sin Yaw, 0, cos Yaw), so a planar point
// (x, z) lifted to (x, 0, z) transforms exactly as Pose2.Apply.
func Pose3FromPose2(p Pose2, elevation float64) Pose3 {
	return Pose3{RotY(-p.Yaw), Vec3{p.X, elevation, p.Z}}
}

// Planar projects the pose onto the map plane: Yaw is the planar direction of
// the child +X axis, and the second result is the World elevation. ok is false
// when that axis is vertical and the yaw is therefore undefined.
//
// +X is forward for a body or a planar sensor. For a camera's optical frame
// it is image-right, a quarter turn from where the camera looks: use ViewYaw.
func (p Pose3) Planar() (pose Pose2, elevation float64, ok bool) {
	hx, hz := p.R[0], p.R[6]
	if math.Hypot(hx, hz) < 1e-9 {
		return Pose2{X: p.T.X, Z: p.T.Z}, p.T.Y, false
	}
	return Pose2{p.T.X, p.T.Z, math.Atan2(hz, hx)}, p.T.Y, true
}

// ViewYaw is the planar direction of the child +Z axis: where a camera's
// optical frame looks, or the way a wall tag faces. ok is false when that
// axis is vertical.
func (p Pose3) ViewYaw() (yaw float64, ok bool) {
	hx, hz := p.R[2], p.R[8]
	if math.Hypot(hx, hz) < 1e-9 {
		return 0, false
	}
	return math.Atan2(hz, hx), true
}
