package geom

import "math"

// alignWeights validates optional weights and returns their sum and the
// number of points with positive weight.
func alignWeights(n int, w []float64) (sum float64, used int, err error) {
	if w == nil {
		return float64(n), n, nil
	}
	if len(w) != n {
		return 0, 0, ErrDimension
	}
	for _, v := range w {
		if !finite(v) {
			return 0, 0, ErrNonFinite
		}
		if v < 0 {
			return 0, 0, ErrDimension
		}
		if v > 0 {
			sum += v
			used++
		}
	}
	return sum, used, nil
}

func weightAt(w []float64, i int) float64 {
	if w == nil {
		return 1
	}
	return w[i]
}

// AlignPoints2 returns the rigid transform t minimising
// Σ w_i·|t.Apply(src_i) − dst_i|² (Kabsch/Umeyama without scale) and the
// weighted RMSE of the fit in metres. w may be nil for uniform weights;
// weights must be non-negative. It needs at least two points with positive
// weight (ErrInsufficient) that are not coincident (ErrDegenerate).
//
// The degeneracy test is numerical only: two points a micrometre apart give
// a yaw. With point noise σ the yaw is known to about
// σ / sqrt(Σ w_i·|src_i − centroid|²) radians, so a caller with noisy points
// must check their spread before trusting the rotation.
func AlignPoints2(src, dst []Vec2, w []float64) (Pose2, float64, error) {
	if len(src) != len(dst) {
		return Pose2{}, 0, ErrDimension
	}
	sum, used, err := alignWeights(len(src), w)
	if err != nil {
		return Pose2{}, 0, err
	}
	if used < 2 {
		return Pose2{}, 0, ErrInsufficient
	}
	var ms, md Vec2
	scale := 0.0
	for i := range src {
		if !finite(src[i].X, src[i].Z, dst[i].X, dst[i].Z) {
			return Pose2{}, 0, ErrNonFinite
		}
		wi := weightAt(w, i)
		ms = ms.Add(src[i].Scale(wi))
		md = md.Add(dst[i].Scale(wi))
	}
	ms, md = ms.Scale(1/sum), md.Scale(1/sum)
	var dot, cross, vs, vd float64
	for i := range src {
		wi := weightAt(w, i)
		a, b := src[i].Sub(ms), dst[i].Sub(md)
		dot += wi * a.Dot(b)
		cross += wi * a.Cross(b)
		vs += wi * a.Norm2()
		vd += wi * b.Norm2()
		scale = math.Max(scale, math.Max(src[i].Norm2(), dst[i].Norm2()))
	}
	tol := 1e-18 * sum * (1 + scale)
	if vs <= tol || vd <= tol || math.Hypot(dot, cross) <= tol {
		return Pose2{}, 0, ErrDegenerate
	}
	yaw := math.Atan2(cross, dot)
	t := md.Sub(ms.Rotate(yaw))
	pose := Pose2{t.X, t.Z, yaw}
	ss := 0.0
	for i := range src {
		d := pose.Apply(src[i]).Sub(dst[i])
		ss += weightAt(w, i) * d.Norm2()
	}
	return pose, math.Sqrt(ss / sum), nil
}

// AlignPoints3 is the 3D counterpart of AlignPoints2. It needs at least three
// points with positive weight (ErrInsufficient) that are not collinear
// (ErrDegenerate); coplanar points are fine.
func AlignPoints3(src, dst []Vec3, w []float64) (Pose3, float64, error) {
	if len(src) != len(dst) {
		return Pose3{}, 0, ErrDimension
	}
	sum, used, err := alignWeights(len(src), w)
	if err != nil {
		return Pose3{}, 0, err
	}
	if used < 3 {
		return Pose3{}, 0, ErrInsufficient
	}
	var ms, md Vec3
	for i := range src {
		if !finite(src[i].X, src[i].Y, src[i].Z, dst[i].X, dst[i].Y, dst[i].Z) {
			return Pose3{}, 0, ErrNonFinite
		}
		wi := weightAt(w, i)
		ms = ms.Add(src[i].Scale(wi))
		md = md.Add(dst[i].Scale(wi))
	}
	ms, md = ms.Scale(1/sum), md.Scale(1/sum)
	h := NewMat(3, 3) // Σ w·a·bᵀ
	for i := range src {
		wi := weightAt(w, i)
		a, b := src[i].Sub(ms).Array(), dst[i].Sub(md).Array()
		for r := 0; r < 3; r++ {
			for c := 0; c < 3; c++ {
				h.Data[r*3+c] += wi * a[r] * b[c]
			}
		}
	}
	svd, err := SVD(h)
	if err != nil {
		return Pose3{}, 0, err
	}
	// The second singular value vanishes for collinear points, which leaves
	// the rotation about that line free.
	if svd.S[0] == 0 || svd.S[1] <= 1e-9*svd.S[0] {
		return Pose3{}, 0, ErrDegenerate
	}
	var u, v Mat3
	copy(u[:], svd.U.Data)
	copy(v[:], svd.V.Data)
	if svd.S[2] <= 1e-12*svd.S[0] {
		c := Vec3{u[0], u[3], u[6]}.Cross(Vec3{u[1], u[4], u[7]})
		u[2], u[5], u[8] = c.X, c.Y, c.Z
	}
	rot := v.Mul(u.T())
	if rot.Det() < 0 {
		v[2], v[5], v[8] = -v[2], -v[5], -v[8]
		rot = v.Mul(u.T())
	}
	pose := Pose3{rot, md.Sub(rot.MulVec(ms))}
	ss := 0.0
	for i := range src {
		ss += weightAt(w, i) * pose.Apply(src[i]).Sub(dst[i]).Norm2()
	}
	return pose, math.Sqrt(ss / sum), nil
}
