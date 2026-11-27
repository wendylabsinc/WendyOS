package geom

import (
	"errors"
	"math"
	"testing"
)

func TestAlignPoints2(t *testing.T) {
	const (
		noiseSigma = 0.02 // metres per axis
		poseTol    = 0.01 // metres and radians with 200 noisy points
	)
	r := newRand(30)
	for trial := 0; trial < 50; trial++ {
		truth := randPose2(r)
		src, dst, noisy := make([]Vec2, 200), make([]Vec2, 200), make([]Vec2, 200)
		for i := range src {
			src[i] = Vec2{r.Float64()*10 - 5, r.Float64()*10 - 5}
			dst[i] = truth.Apply(src[i])
			noisy[i] = dst[i].Add(Vec2{r.NormFloat64() * noiseSigma, r.NormFloat64() * noiseSigma})
		}
		got, rmse, err := AlignPoints2(src, dst, nil)
		if err != nil {
			t.Fatal(err)
		}
		nearPose2(t, got, truth, roundTol, "exact alignment")
		near(t, rmse, 0, roundTol, "exact rmse")
		got, rmse, err = AlignPoints2(src, noisy, nil)
		if err != nil {
			t.Fatal(err)
		}
		nearPose2(t, got, truth, poseTol, "noisy alignment")
		near(t, rmse, noiseSigma*math.Sqrt2, 0.2*noiseSigma, "rmse reflects the noise")
		// A zero-weight outlier has no influence.
		w := make([]float64, len(src))
		for i := range w {
			w[i] = 1 + float64(i%3)
		}
		w[7] = 0
		dst[7] = Vec2{1e3, -1e3}
		got, rmse, err = AlignPoints2(src, dst, w)
		if err != nil {
			t.Fatal(err)
		}
		nearPose2(t, got, truth, roundTol, "weighted alignment ignores zero-weight outlier")
		near(t, rmse, 0, roundTol, "weighted rmse")
	}
	two := []Vec2{{0, 0}, {1, 0}}
	if got, _, err := AlignPoints2(two, []Vec2{{2, 2}, {2, 3}}, nil); err != nil || math.Abs(AngleDiff(got.Yaw, math.Pi/2)) > roundTol {
		t.Fatalf("two-point alignment: %+v %v", got, err)
	}
}

func TestAlignPoints2Refusals(t *testing.T) {
	one := []Vec2{{1, 2}}
	if _, _, err := AlignPoints2(one, one, nil); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("one point: %v", err)
	}
	same := []Vec2{{1, 2}, {1, 2}, {1, 2}}
	if _, _, err := AlignPoints2(same, []Vec2{{0, 0}, {1, 0}, {2, 0}}, nil); !errors.Is(err, ErrDegenerate) {
		t.Fatalf("coincident source: %v", err)
	}
	pts := []Vec2{{0, 0}, {1, 0}, {0, 1}}
	if _, _, err := AlignPoints2(pts, pts[:2], nil); !errors.Is(err, ErrDimension) {
		t.Fatalf("length mismatch: %v", err)
	}
	if _, _, err := AlignPoints2(pts, pts, []float64{1, 0, 0}); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("one weighted point: %v", err)
	}
	if _, _, err := AlignPoints2(pts, pts, []float64{1, -1, 1}); !errors.Is(err, ErrDimension) {
		t.Fatalf("negative weight: %v", err)
	}
	if _, _, err := AlignPoints2(pts, []Vec2{{0, 0}, {math.NaN(), 0}, {0, 1}}, nil); !errors.Is(err, ErrNonFinite) {
		t.Fatalf("NaN point: %v", err)
	}
}

func TestAlignPoints3(t *testing.T) {
	const (
		noiseSigma = 0.01
		posTol     = 0.01
		rotTol     = 0.005
	)
	r := newRand(31)
	for trial := 0; trial < 50; trial++ {
		truth := randPose3(r)
		planar := trial%2 == 0 // coplanar sources are a valid configuration
		src, dst, noisy := make([]Vec3, 150), make([]Vec3, 150), make([]Vec3, 150)
		for i := range src {
			src[i] = randVec3(r, 3)
			if planar {
				src[i].Y = 0.5
			}
			dst[i] = truth.Apply(src[i])
			noisy[i] = dst[i].Add(Vec3{r.NormFloat64() * noiseSigma, r.NormFloat64() * noiseSigma, r.NormFloat64() * noiseSigma})
		}
		got, rmse, err := AlignPoints3(src, dst, nil)
		if err != nil {
			t.Fatal(err)
		}
		nearPose3(t, got, truth, 1e-8, "exact alignment")
		near(t, rmse, 0, 1e-8, "exact rmse")
		if !got.R.IsRotation(roundTol) {
			t.Fatal("alignment returned a reflection")
		}
		got, rmse, err = AlignPoints3(src, noisy, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got.T.Dist(truth.T) > posTol || RotAngle(got.R, truth.R) > rotTol {
			t.Fatalf("noisy alignment off by %g m, %g rad", got.T.Dist(truth.T), RotAngle(got.R, truth.R))
		}
		near(t, rmse, noiseSigma*math.Sqrt(3), 0.25*noiseSigma, "rmse reflects the noise")
	}
}

func TestAlignPoints3Refusals(t *testing.T) {
	line := []Vec3{{0, 0, 0}, {1, 1, 1}, {2, 2, 2}, {-3, -3, -3}}
	if _, _, err := AlignPoints3(line, line, nil); !errors.Is(err, ErrDegenerate) {
		t.Fatalf("collinear points: %v", err)
	}
	same := []Vec3{{1, 1, 1}, {1, 1, 1}, {1, 1, 1}}
	if _, _, err := AlignPoints3(same, same, nil); !errors.Is(err, ErrDegenerate) {
		t.Fatalf("coincident points: %v", err)
	}
	if _, _, err := AlignPoints3(line[:2], line[:2], nil); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("two points: %v", err)
	}
}

type line2 struct{ slope, intercept float64 }

func TestRansacLine(t *testing.T) {
	const (
		n          = 300
		outliers   = 120 // 40 %
		noiseSigma = 0.02
		modelTol   = 0.02
	)
	build := func(seed uint64) ([]Vec2, func([]int) (line2, error), func(line2, int) float64) {
		r := newRand(seed)
		pts := make([]Vec2, n)
		for i := range pts {
			x := r.Float64() * 10
			pts[i] = Vec2{x, 1.5*x - 2 + r.NormFloat64()*noiseSigma}
			if i < outliers {
				pts[i].Z = r.Float64()*40 - 20
			}
		}
		fit := func(idx []int) (line2, error) {
			a, b := NewMat(len(idx), 2), make([]float64, len(idx))
			for k, i := range idx {
				a.Set(k, 0, pts[i].X)
				a.Set(k, 1, 1)
				b[k] = pts[i].Z
			}
			x, err := LeastSquares(a, b)
			if err != nil {
				return line2{}, err
			}
			return line2{x[0], x[1]}, nil
		}
		residual := func(m line2, i int) float64 { return m.slope*pts[i].X + m.intercept - pts[i].Z }
		return pts, fit, residual
	}
	_, fit, residual := build(40)
	cfg := RansacConfig{SampleSize: 2, Threshold: 3 * noiseSigma, Iterations: 100}
	res, err := Ransac(n, cfg, newRand(41), fit, residual)
	if err != nil {
		t.Fatal(err)
	}
	near(t, res.Model.slope, 1.5, modelTol, "slope")
	near(t, res.Model.intercept, -2, modelTol, "intercept")
	trueInliers := 0
	for k, i := range res.Inliers {
		if k > 0 && i <= res.Inliers[k-1] {
			t.Fatal("inliers are not strictly ascending")
		}
		if i >= outliers {
			trueInliers++
		}
	}
	// Nearly every genuine point is kept; outliers land inside the band only by chance.
	if trueInliers < (n-outliers)*95/100 || len(res.Inliers)-trueInliers > outliers/10 {
		t.Fatalf("consensus has %d genuine and %d spurious points", trueInliers, len(res.Inliers)-trueInliers)
	}
	again, err := Ransac(n, cfg, newRand(41), fit, residual)
	if err != nil || again.Model != res.Model || len(again.Inliers) != len(res.Inliers) {
		t.Fatal("the same seed must reproduce the result")
	}
}

func TestRansacRefusals(t *testing.T) {
	r := newRand(42)
	pts := make([]Vec2, 50)
	for i := range pts {
		pts[i] = Vec2{r.Float64() * 10, r.Float64() * 10} // no structure at all
	}
	fit := func(idx []int) (line2, error) {
		a, b := pts[idx[0]], pts[idx[len(idx)-1]]
		if a.X == b.X {
			return line2{}, ErrDegenerate
		}
		s := (b.Z - a.Z) / (b.X - a.X)
		return line2{s, a.Z - s*a.X}, nil
	}
	residual := func(m line2, i int) float64 { return m.slope*pts[i].X + m.intercept - pts[i].Z }
	cfg := RansacConfig{SampleSize: 2, Threshold: 1e-3, MinInliers: 10}
	if _, err := Ransac(len(pts), cfg, r, fit, residual); !errors.Is(err, ErrNoConsensus) {
		t.Fatalf("unstructured data: %v", err)
	}
	if _, err := Ransac(1, cfg, r, fit, residual); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("too few points: %v", err)
	}
	if _, err := Ransac(len(pts), RansacConfig{SampleSize: 2}, r, fit, residual); !errors.Is(err, ErrDimension) {
		t.Fatalf("missing threshold: %v", err)
	}
	if _, err := Ransac(len(pts), cfg, nil, fit, residual); !errors.Is(err, ErrDimension) {
		t.Fatalf("missing rng: %v", err)
	}
	never := func([]int) (line2, error) { return line2{}, ErrDegenerate }
	res, err := Ransac(len(pts), cfg, r, never, residual)
	if !errors.Is(err, ErrNoConsensus) || res.Fits != 0 {
		t.Fatalf("all samples degenerate: %v fits=%d", err, res.Fits)
	}
}

func TestChiSquare(t *testing.T) {
	for _, c := range []struct {
		dof     int
		p, want float64
	}{
		{1, 0.95, 3.841459}, {2, 0.95, 5.991465}, {3, 0.95, 7.814728},
		{1, 0.99, 6.634897}, {2, 0.99, 9.210340}, {3, 0.99, 11.344867}, {3, 0.5, 2.365974},
	} {
		got, err := ChiSquareQuantile(c.dof, c.p)
		if err != nil {
			t.Fatal(err)
		}
		near(t, got, c.want, 1e-5, "chi-square quantile")
		cdf, err := ChiSquareCDF(c.dof, got)
		if err != nil {
			t.Fatal(err)
		}
		near(t, cdf, c.p, 1e-10, "cdf of quantile")
	}
	if c, _ := ChiSquareCDF(2, -1); c != 0 {
		t.Fatal("cdf below zero must be zero")
	}
	for _, bad := range []struct {
		dof int
		p   float64
	}{{0, 0.5}, {4, 0.5}, {2, 0}, {2, 1}, {2, math.NaN()}} {
		if _, err := ChiSquareQuantile(bad.dof, bad.p); !errors.Is(err, ErrDimension) {
			t.Fatalf("dof=%d p=%g: %v", bad.dof, bad.p, err)
		}
	}
}

func TestCov2(t *testing.T) {
	c := Cov2{4, 0, 0, 1}
	major, minor, yaw := c.Eigen()
	near(t, major, 4, exactTol, "major")
	near(t, minor, 1, exactTol, "minor")
	near(t, yaw, 0, exactTol, "axis")
	rot := c.Rotate(math.Pi / 2) // the long axis now lies along Z
	near(t, rot[0], 1, exactTol, "rotated xx")
	near(t, rot[3], 4, exactTol, "rotated zz")
	_, _, yaw = c.Rotate(0.3).Eigen()
	near(t, yaw, 0.3, roundTol, "major axis follows the rotation")
	near(t, c.Rotate(1.1).MaxSigma(), 2, roundTol, "max sigma is rotation invariant")
	near(t, TransformCov2(Pose2{5, 6, math.Pi / 2}, c)[3], 4, exactTol, "translation does not change a point covariance")
	d2, err := MahalanobisSq2(Vec2{2, 1}, c)
	if err != nil {
		t.Fatal(err)
	}
	near(t, d2, 2, exactTol, "mahalanobis 2d")
	d3, err := MahalanobisSq3([3]float64{1, 2, 3}, Mat3{1, 0, 0, 0, 4, 0, 0, 0, 9})
	if err != nil {
		t.Fatal(err)
	}
	near(t, d3, 3, exactTol, "mahalanobis 3d")
	if _, err := MahalanobisSq2(Vec2{1, 1}, Cov2{1, 1, 1, 1}); !errors.Is(err, ErrSingular) {
		t.Fatalf("singular covariance: %v", err)
	}
	if _, err := MahalanobisSq3([3]float64{1, 1, 1}, Mat3{1, 0, 0, 0, 1, 0, 0, 0, 0}); !errors.Is(err, ErrNotPositiveDefinite) {
		t.Fatalf("semi-definite covariance: %v", err)
	}
	if got := PositionCov(Mat3{1, 2, 3, 4, 5, 6, 7, 8, 9}); got != (Cov2{1, 2, 4, 5}) {
		t.Fatalf("position block: %v", got)
	}
}

// numericJacobian differentiates f: R^n -> R^m by central differences and
// returns J·c·Jᵀ, the reference for first-order covariance propagation.
func propagate(f func(x []float64) []float64, x []float64, c Mat) Mat {
	m := len(f(x))
	j := NewMat(m, len(x))
	for k := range x {
		const h = 1e-6
		xp, xm := append([]float64(nil), x...), append([]float64(nil), x...)
		xp[k] += h
		xm[k] -= h
		fp, fm := f(xp), f(xm)
		for i := 0; i < m; i++ {
			d := fp[i] - fm[i]
			if i == 2 { // yaw component of a pose output
				d = WrapAngle(d)
			}
			j.Set(i, k, d/(2*h))
		}
	}
	return j.Mul(c).Mul(j.T())
}

func randCov(r interface{ Float64() float64 }, n int) Mat {
	b := NewMat(n, n)
	for i := range b.Data {
		b.Data[i] = (r.Float64() - 0.5) * 0.2
	}
	return b.Mul(b.T())
}

func TestPoseCovariancePropagation(t *testing.T) {
	const jacTol = 1e-7
	r := newRand(50)
	for trial := 0; trial < 100; trial++ {
		a, b := randPose2(r), randPose2(r)
		q := Vec2{r.Float64()*6 - 3, r.Float64()*6 - 3}
		ca, cb, cq := randCov(r, 3), randCov(r, 3), randCov(r, 2)
		var ca3, cb3 Mat3
		copy(ca3[:], ca.Data)
		copy(cb3[:], cb.Data)
		cq2 := Cov2{cq.Data[0], cq.Data[1], cq.Data[2], cq.Data[3]}

		joint := NewMat(6, 6)
		for i := 0; i < 3; i++ {
			for j := 0; j < 3; j++ {
				joint.Set(i, j, ca.At(i, j))
				joint.Set(3+i, 3+j, cb.At(i, j))
			}
		}
		want := propagate(func(x []float64) []float64 {
			p := Pose2{x[0], x[1], x[2]}.Compose(Pose2{x[3], x[4], x[5]})
			return []float64{p.X, p.Z, p.Yaw}
		}, []float64{a.X, a.Z, a.Yaw, b.X, b.Z, b.Yaw}, joint)
		got := ComposeCov(a, ca3, b, cb3)
		for i := range got {
			near(t, got[i], want.Data[i], jacTol, "compose covariance")
		}

		want = propagate(func(x []float64) []float64 {
			p := Pose2{x[0], x[1], x[2]}.Inverse()
			return []float64{p.X, p.Z, p.Yaw}
		}, []float64{a.X, a.Z, a.Yaw}, ca)
		got = InverseCov(a, ca3)
		for i := range got {
			near(t, got[i], want.Data[i], jacTol, "inverse covariance")
		}

		// With an exact parent the composed covariance is just the rotated child covariance.
		got = TransformCov3(a, cb3)
		ref := ComposeCov(a, Mat3{}, b, cb3)
		for i := range got {
			near(t, got[i], ref[i], exactTol, "transform covariance")
		}

		joint = NewMat(5, 5)
		for i := 0; i < 3; i++ {
			for j := 0; j < 3; j++ {
				joint.Set(i, j, ca.At(i, j))
			}
		}
		for i := 0; i < 2; i++ {
			for j := 0; j < 2; j++ {
				joint.Set(3+i, 3+j, cq.At(i, j))
			}
		}
		wantPoint := propagate(func(x []float64) []float64 {
			p := Pose2{x[0], x[1], x[2]}.Apply(Vec2{x[3], x[4]})
			return []float64{p.X, p.Z}
		}, []float64{a.X, a.Z, a.Yaw, q.X, q.Z}, joint)
		gotPoint := ApplyCov(a, ca3, q, cq2)
		for i := range gotPoint {
			near(t, gotPoint[i], wantPoint.Data[i], jacTol, "apply covariance")
		}
	}
}
