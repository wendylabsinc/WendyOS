package calib

import (
	"fmt"
	"math"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// nullVector returns the unit vector minimising |A·x| given AᵀA, and the
// ratio of the second smallest eigenvalue to the largest: near zero when the
// null space has more than one dimension and x is therefore arbitrary.
func nullVector(ata geom.Mat) (x []float64, gap float64, err error) {
	values, vecs, err := geom.SymEigen(ata)
	if err != nil {
		return nil, 0, err
	}
	n := ata.Rows
	if !(values[0] > 0) {
		return nil, 0, geom.ErrDegenerate
	}
	return vecs.Col(n - 1), values[n-2] / values[0], nil
}

// rankTol is the eigenvalue ratio below which a linear solve is rank
// deficient. It is a numerical floor for exactly degenerate input; noisy
// near-degenerate input is caught by the consensus and covariance checks.
const rankTol = 1e-12

// similarity2 is the Hartley normalisation of a point set: the offset and
// scale that centre it with mean distance √2.
func similarity2(n int, at func(i int) (float64, float64)) (mx, my, s float64, err error) {
	for i := 0; i < n; i++ {
		x, y := at(i)
		mx, my = mx+x, my+y
	}
	mx, my = mx/float64(n), my/float64(n)
	d := 0.0
	for i := 0; i < n; i++ {
		x, y := at(i)
		d += math.Hypot(x-mx, y-my)
	}
	if !(d > 0) || math.IsInf(d, 0) {
		return 0, 0, 0, geom.ErrDegenerate
	}
	return mx, my, math.Sqrt2 * float64(n) / d, nil
}

// homography returns H with pixel ~ H·(X, Y, 1) for the planar points of one
// target view, by the normalised DLT.
func homography(view []Correspondence) (geom.Mat3, error) {
	n := len(view)
	if n < 4 {
		return geom.Mat3{}, geom.ErrInsufficient
	}
	mx, my, ms, err := similarity2(n, func(i int) (float64, float64) { return view[i].Point.X, view[i].Point.Y })
	if err != nil {
		return geom.Mat3{}, err
	}
	px, py, ps, err := similarity2(n, func(i int) (float64, float64) { return view[i].Pixel.U, view[i].Pixel.V })
	if err != nil {
		return geom.Mat3{}, err
	}
	ata := geom.NewMat(9, 9)
	for _, c := range view {
		x, y := (c.Point.X-mx)*ms, (c.Point.Y-my)*ms
		u, v := (c.Pixel.U-px)*ps, (c.Pixel.V-py)*ps
		for _, row := range [2][9]float64{{-x, -y, -1, 0, 0, 0, u * x, u * y, u}, {0, 0, 0, -x, -y, -1, v * x, v * y, v}} {
			for i, a := range row {
				for j, b := range row {
					ata.Data[i*9+j] += a * b
				}
			}
		}
	}
	h, gap, err := nullVector(ata)
	if err != nil {
		return geom.Mat3{}, err
	}
	if gap < rankTol {
		return geom.Mat3{}, geom.ErrDegenerate // collinear points
	}
	var hn geom.Mat3
	copy(hn[:], h)
	model := geom.Mat3{ms, 0, -ms * mx, 0, ms, -ms * my, 0, 0, 1}
	pixel := geom.Mat3{1 / ps, 0, px, 0, 1 / ps, py, 0, 0, 1}
	return pixel.Mul(hn).Mul(model), nil
}

// zhang is the closed-form solution for a zero-skew pinhole camera from the
// homographies of three or more target views (Zhang 2000, section 3.1).
func zhang(hs []geom.Mat3, width, height int) (geom.Intrinsics, error) {
	// Work in pixels centred on the image and scaled to order one.
	s := 2 / float64(width+height)
	norm := geom.Mat3{s, 0, -s * float64(width) / 2, 0, s, -s * float64(height) / 2, 0, 0, 1}
	// Unknowns of B = K⁻ᵀK⁻¹ with B12 = 0: B11, B22, B13, B23, B33.
	vtv := geom.NewMat(5, 5)
	for _, h := range hs {
		h = norm.Mul(h)
		h1, h2 := geom.Vec3{X: h[0], Y: h[3], Z: h[6]}, geom.Vec3{X: h[1], Y: h[4], Z: h[7]}
		scale := math.Sqrt(h1.Norm() * h2.Norm())
		if !(scale > 0) {
			return geom.Intrinsics{}, geom.ErrDegenerate
		}
		h1, h2 = h1.Scale(1/scale), h2.Scale(1/scale)
		v := func(a, b geom.Vec3) [5]float64 {
			return [5]float64{a.X * b.X, a.Y * b.Y, a.Z*b.X + a.X*b.Z, a.Z*b.Y + a.Y*b.Z, a.Z * b.Z}
		}
		v12, v11, v22 := v(h1, h2), v(h1, h1), v(h2, h2)
		var diff [5]float64
		for i := range diff {
			diff[i] = v11[i] - v22[i]
		}
		for _, row := range [2][5]float64{v12, diff} {
			for i, a := range row {
				for j, b := range row {
					vtv.Data[i*5+j] += a * b
				}
			}
		}
	}
	b, gap, err := nullVector(vtv)
	if err != nil {
		return geom.Intrinsics{}, err
	}
	if b[0] < 0 {
		for i := range b {
			b[i] = -b[i]
		}
	}
	if gap < rankTol || !(b[0] > 0) || !(b[1] > 0) {
		return geom.Intrinsics{}, geom.ErrDegenerate
	}
	lambda := b[4] - b[2]*b[2]/b[0] - b[3]*b[3]/b[1]
	if !(lambda > 0) {
		return geom.Intrinsics{}, geom.ErrDegenerate
	}
	in := geom.Intrinsics{
		FX: math.Sqrt(lambda/b[0]) / s, FY: math.Sqrt(lambda/b[1]) / s,
		CX: -b[2]/b[0]/s + float64(width)/2, CY: -b[3]/b[1]/s + float64(height)/2,
		Width: width, Height: height,
	}
	if err := in.Validate(); err != nil {
		return geom.Intrinsics{}, geom.ErrDegenerate
	}
	return in, nil
}

// viewPose recovers the target-to-optical pose of a view from its homography
// and the camera matrix: K⁻¹·H = s·[r1 r2 t].
func viewPose(in geom.Intrinsics, h geom.Mat3) (geom.Pose3, error) {
	col := func(j int) geom.Vec3 {
		z := h[6+j]
		return geom.Vec3{X: (h[j] - in.CX*z) / in.FX, Y: (h[3+j] - in.CY*z) / in.FY, Z: z}
	}
	a1, a2, a3 := col(0), col(1), col(2)
	norm := (a1.Norm() + a2.Norm()) / 2
	if !(norm > 0) {
		return geom.Pose3{}, geom.ErrDegenerate
	}
	s := 1 / norm
	if a3.Z < 0 {
		s = -s // the target is in front of the camera
	}
	r1, r2, t := a1.Scale(s), a2.Scale(s), a3.Scale(s)
	r3 := r1.Cross(r2)
	r, err := geom.Mat3{r1.X, r2.X, r3.X, r1.Y, r2.Y, r3.Y, r1.Z, r2.Z, r3.Z}.Orthonormalize()
	if err != nil {
		return geom.Pose3{}, geom.ErrDegenerate
	}
	return geom.Pose3{R: r, T: t}, nil
}

// tiltSpread is the largest angle between the target normals of two views.
func tiltSpread(poses []geom.Pose3) float64 {
	spread := 0.0
	for i := range poses {
		ni := poses[i].Rotate(geom.Vec3{Z: 1})
		for j := i + 1; j < len(poses); j++ {
			c := ni.Dot(poses[j].Rotate(geom.Vec3{Z: 1}))
			spread = math.Max(spread, math.Acos(math.Max(-1, math.Min(1, c))))
		}
	}
	return spread
}

func viewGroup(view []Correspondence, sigma float64) group {
	g := group{rigid: true, points: make([]point, len(view))}
	for i, c := range view {
		g.points[i] = point{px: c.Pixel, w: 1 / sigma, p: c.Point}
	}
	return g
}

// zhangInit is the closed-form stage: intrinsics without distortion and the
// pose of every view.
func zhangInit(views [][]Correspondence, width, height int, cfg Config) (geom.Intrinsics, []geom.Pose3, error) {
	if width <= 0 || height <= 0 {
		return geom.Intrinsics{}, nil, geom.ErrDimension
	}
	if len(views) < cfg.MinViews {
		return geom.Intrinsics{}, nil, fmt.Errorf("calib: %d target views, need %d: %w", len(views), cfg.MinViews, geom.ErrInsufficient)
	}
	hs := make([]geom.Mat3, len(views))
	for i, v := range views {
		h, err := homography(v)
		if err != nil {
			return geom.Intrinsics{}, nil, fmt.Errorf("calib: target view %d: %w", i, err)
		}
		hs[i] = h
	}
	in, err := zhang(hs, width, height)
	if err != nil {
		return geom.Intrinsics{}, nil, fmt.Errorf("calib: the target views do not determine a camera: %w", err)
	}
	poses := make([]geom.Pose3, len(views))
	for i, h := range hs {
		if poses[i], err = viewPose(in, h); err != nil {
			return geom.Intrinsics{}, nil, fmt.Errorf("calib: target view %d: %w", i, err)
		}
	}
	return in, poses, nil
}

// CalibrateIntrinsics estimates fx, fy, cx, cy, k1 and k2 from views of a
// planar target by Zhang's method: a homography per view, the closed-form
// camera matrix, then a joint refinement of the intrinsics and every view's
// pose. Each view lists target-frame points (z = 0) with their pixels and
// needs at least four of them, not collinear.
//
// It returns geom.ErrInsufficient for fewer than Config.MinViews views and
// geom.ErrDegenerate when the views do not determine a camera: their target
// orientations differ by less than Config.MinTiltSpread, or the closed form
// has no solution. Estimate.Covariance is 6x6.
func CalibrateIntrinsics(views [][]Correspondence, width, height int, cfg Config) (Estimate, error) {
	e, _, err := calibrateIntrinsics(views, width, height, cfg.withDefaults())
	return e, err
}

// calibrateIntrinsics also returns the refined target-to-optical pose of
// every view.
func calibrateIntrinsics(views [][]Correspondence, width, height int, cfg Config) (Estimate, []geom.Pose3, error) {
	in, poses, err := zhangInit(views, width, height, cfg)
	if err != nil {
		return Estimate{}, nil, err
	}
	b := &bundle{free: freeIntrinsics, huber: cfg.HuberSigmas, iters: cfg.MaxIterations}
	for _, v := range views {
		b.groups = append(b.groups, viewGroup(v, cfg.PixelSigma))
	}
	start := newState(camera{in: in, pose: geom.IdentityPose3()}, len(views))
	copy(start.poses, poses)
	res, err := b.solve(start)
	if err != nil {
		return Estimate{}, nil, err
	}
	if spread := tiltSpread(res.state.poses); spread < cfg.MinTiltSpread {
		return Estimate{}, nil, fmt.Errorf("calib: target orientations span %.1f°, need %.1f°: %w", spread*180/math.Pi, cfg.MinTiltSpread*180/math.Pi, geom.ErrDegenerate)
	}
	return b.estimate(res), res.state.poses, nil
}
