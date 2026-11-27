package calib

import (
	"errors"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// The unit tests build their data by hand: a lens, a pose and points placed
// with geom, projected through geom.Intrinsics. No simulator is involved.

const deg = math.Pi / 180

func testLens() geom.Intrinsics {
	return geom.Intrinsics{FX: 812, FY: 795, CX: 651, CY: 352, K1: -0.11, K2: 0.014, Width: 1280, Height: 720}
}

func testPose(t *testing.T) geom.Pose3 {
	t.Helper()
	pose, err := geom.LookAt(geom.Vec3{X: 0.3, Y: 2.7, Z: 0.4}, geom.Vec3{X: 5, Y: 0.4, Z: 4}, geom.Vec3{Y: 1})
	if err != nil {
		t.Fatal(err)
	}
	return pose
}

func noisy(rng *rand.Rand, px geom.Pixel, sigma float64) geom.Pixel {
	return geom.Pixel{U: px.U + sigma*rng.NormFloat64(), V: px.V + sigma*rng.NormFloat64()}
}

// boardViews images a 6x8 target with 8 cm squares at random poses in front
// of the lens. tilt bounds the rotation about the target's x and y axes.
func boardViews(rng *rand.Rand, in geom.Intrinsics, n int, tilt, sigma float64) [][]Correspondence {
	const rows, cols, spacing = 6, 8, 0.08
	var views [][]Correspondence
	for len(views) < n {
		z := 1.3 + 1.8*rng.Float64()
		centre := geom.Vec3{X: 0.5 * z * (2*rng.Float64() - 1), Y: 0.25 * z * (2*rng.Float64() - 1), Z: z}
		rot := geom.RotX(tilt * (2*rng.Float64() - 1)).Mul(geom.RotY(tilt * (2*rng.Float64() - 1))).Mul(geom.RotZ(0.3 * (2*rng.Float64() - 1)))
		pose := geom.Pose3{R: rot, T: centre.Sub(rot.MulVec(geom.Vec3{X: (cols - 1) * spacing / 2, Y: (rows - 1) * spacing / 2}))}
		var view []Correspondence
		for i := 0; i < rows*cols; i++ {
			p := geom.Vec3{X: float64(i%cols) * spacing, Y: float64(i/cols) * spacing}
			if px, ok := in.Project(pose.Apply(p)); ok && in.InImage(px) {
				view = append(view, Correspondence{Point: p, Pixel: noisy(rng, px, sigma)})
			}
		}
		if len(view) == rows*cols {
			views = append(views, view)
		}
	}
	return views
}

// scenePoints scatters points through the volume the test camera looks at
// and images them. flat puts them all on the floor.
func scenePoints(rng *rand.Rand, in geom.Intrinsics, pose geom.Pose3, n int, flat bool, sigma float64) []Correspondence {
	var pts []Correspondence
	for len(pts) < n {
		p := geom.Vec3{X: 1 + 7*rng.Float64(), Y: 1.5 * rng.Float64(), Z: 7 * rng.Float64()}
		if flat {
			p.Y = 0
		}
		if px, ok := in.ProjectWorld(pose, p); ok && in.InImage(px) {
			pts = append(pts, Correspondence{Point: p, Pixel: noisy(rng, px, sigma)})
		}
	}
	return pts
}

func focalError(got, want geom.Intrinsics) float64 {
	return math.Max(math.Abs(got.FX-want.FX)/want.FX, math.Abs(got.FY-want.FY)/want.FY)
}

func principalError(got, want geom.Intrinsics) float64 {
	return math.Hypot(got.CX-want.CX, got.CY-want.CY)
}

// The optical frame is x right, y down, z forward, and in the planar
// convention a robot heading +X has +Z on its right. A camera looking along
// +X must therefore image a point at larger Z to the right and a higher point
// further up, and SolvePnP must hand back the pose that says so.
func TestConventionsByHand(t *testing.T) {
	in := geom.Intrinsics{FX: 800, FY: 800, CX: 640, CY: 360, Width: 1280, Height: 720}
	eye := geom.Vec3{Y: 1}
	pose, err := geom.LookAt(eye, geom.Vec3{X: 1, Y: 1}, geom.Vec3{Y: 1})
	if err != nil {
		t.Fatal(err)
	}
	centre, _ := in.ProjectWorld(pose, geom.Vec3{X: 4, Y: 1})
	right, _ := in.ProjectWorld(pose, geom.Vec3{X: 4, Y: 1, Z: 1})
	above, _ := in.ProjectWorld(pose, geom.Vec3{X: 4, Y: 2})
	if centre.Dist(geom.Pixel{U: 640, V: 360}) > 1e-9 || right.U-centre.U != 200 || above.V-centre.V != -200 {
		t.Fatalf("projection convention: centre %v right %v above %v", centre, right, above)
	}
	// Eight points placed by hand, off any common plane.
	world := []geom.Vec3{{X: 3, Y: 0, Z: -1}, {X: 3, Y: 0, Z: 1}, {X: 5, Y: 0, Z: -1.5}, {X: 5, Y: 0, Z: 1.5}, {X: 4, Y: 1.5, Z: -0.5}, {X: 6, Y: 2, Z: 1}, {X: 3.5, Y: 0.6, Z: 0.2}, {X: 7, Y: 0.3, Z: -2}}
	var pts []Correspondence
	for _, p := range world {
		px, ok := in.ProjectWorld(pose, p)
		if !ok {
			t.Fatalf("%v does not project", p)
		}
		pts = append(pts, Correspondence{Point: p, Pixel: px})
	}
	est, err := SolvePnP(in, pts, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if d := est.CameraToSite.T.Dist(eye); d > 1e-6 {
		t.Errorf("camera position off by %g m", d)
	}
	yaw, ok := est.CameraToSite.ViewYaw()
	if !ok || math.Abs(yaw) > 1e-6 {
		t.Errorf("camera looks along yaw %g, want 0 (+X)", yaw)
	}
	if a := geom.RotAngle(est.CameraToSite.R, pose.R); a > 1e-6 {
		t.Errorf("rotation off by %g rad", a)
	}
}

// The analytic Jacobian of the bundle must match central differences for
// both kinds of group and every parameter.
func TestResidualJacobian(t *testing.T) {
	in, pose := testLens(), testPose(t)
	prior := geom.Mat3{2500, 0, 0, 0, 10000, 0, 0, 0, 2500}
	b := &bundle{huber: 3, groups: []group{
		{rigid: true, points: []point{{px: geom.Pixel{U: 300, V: 200}, w: 2, p: geom.Vec3{X: 0.3, Y: 0.2}}}},
		{prior: &prior, drift: &prior, points: []point{{px: geom.Pixel{U: 700, V: 400}, w: 2, p: geom.Vec3{X: 4, Y: 0.5, Z: 3}, v: geom.Vec3{X: 1, Z: -0.4}, tau: -0.6}}},
		{prior: &prior, drift: &prior, spin: &prior, points: []point{{px: geom.Pixel{U: 500, V: 300}, w: 2, p: geom.Vec3{X: 3, Y: 0.3, Z: 4}, v: geom.Vec3{X: -0.5, Z: 1}, tau: 0.4, l: geom.Vec3{X: 0.2, Y: 0.1, Z: -0.15}}}},
		{prior: &prior, spin: &prior, points: []point{{px: geom.Pixel{U: 900, V: 250}, w: 2, p: geom.Vec3{X: 6, Y: 1.8, Z: 2}, l: geom.Vec3{X: -0.1, Y: 0.1, Z: 0.05}}}},
	}}
	st := newState(camera{in: in, pose: pose, dt: 0.03}, len(b.groups))
	st.offs[2], st.drifts[2], st.spins[2] = geom.Vec3{X: -0.01, Y: 0.02, Z: 0.01}, geom.Vec3{X: 0.01, Y: 0.01, Z: -0.02}, geom.Vec3{X: 0.02, Y: -0.03, Z: 0.01}
	st.offs[3], st.spins[3] = geom.Vec3{X: 0.01, Y: -0.02, Z: 0.03}, geom.Vec3{X: -0.02, Y: 0.01, Z: 0.03}
	st.poses[0] = geom.Pose3{R: geom.RotX(0.3).Mul(geom.RotY(-0.4)), T: geom.Vec3{X: -0.4, Y: 0.1, Z: 2}}
	st.offs[1], st.drifts[1] = geom.Vec3{X: 0.02, Y: -0.01, Z: 0.015}, geom.Vec3{X: -0.01, Y: 0.002, Z: 0.02}
	const h = 1e-6
	for gi := range b.groups {
		p := &b.groups[gi].points[0]
		var js [2][numShared]float64
		var jl [2][maxBlock]float64
		if _, ok := b.residual(gi, p, &st, &js, &jl); !ok {
			t.Fatalf("group %d: point does not project", gi)
		}
		numeric := func(shared [numShared]float64, block [maxBlock]float64) [2]float64 {
			blocks := make([][maxBlock]float64, len(b.groups))
			blocks[gi] = block
			var minusS [numShared]float64
			var minusB [maxBlock]float64
			for i := range shared {
				minusS[i] = -shared[i]
			}
			for i := range block {
				minusB[i] = -block[i]
			}
			up := b.plus(&st, shared, blocks)
			blocks[gi] = minusB
			down := b.plus(&st, minusS, blocks)
			ru, _ := b.residual(gi, p, &up, nil, nil)
			rd, _ := b.residual(gi, p, &down, nil, nil)
			return [2]float64{(ru[0] - rd[0]) / (2 * h), (ru[1] - rd[1]) / (2 * h)}
		}
		check := func(name string, got [2]float64, want [2]float64) {
			for i := range got {
				if math.Abs(got[i]-want[i]) > 1e-4*(1+math.Abs(want[i])) {
					t.Errorf("group %d %s row %d: analytic %g, numeric %g", gi, name, i, got[i], want[i])
				}
			}
		}
		for j := 0; j < numShared; j++ {
			var d [numShared]float64
			d[j] = h
			check("shared "+string(rune('a'+j)), [2]float64{js[0][j], js[1][j]}, numeric(d, [maxBlock]float64{}))
		}
		for j := 0; j < b.groups[gi].dim(); j++ {
			var d [maxBlock]float64
			d[j] = h
			check("block "+string(rune('a'+j)), [2]float64{jl[0][j], jl[1][j]}, numeric([numShared]float64{}, d))
		}
	}
}

func TestCalibrateIntrinsicsExact(t *testing.T) {
	in := testLens()
	views := boardViews(rand.New(rand.NewPCG(1, 2)), in, 12, 35*deg, 0)
	est, err := CalibrateIntrinsics(views, in.Width, in.Height, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if e := focalError(est.Intrinsics, in); e > 1e-6 {
		t.Errorf("focal error %g", e)
	}
	if e := principalError(est.Intrinsics, in); e > 1e-4 {
		t.Errorf("principal point error %g px", e)
	}
	if math.Abs(est.Intrinsics.K1-in.K1) > 1e-6 || math.Abs(est.Intrinsics.K2-in.K2) > 1e-6 {
		t.Errorf("distortion %g %g, want %g %g", est.Intrinsics.K1, est.Intrinsics.K2, in.K1, in.K2)
	}
	if est.Residuals.RMSE > 1e-6 || est.Residuals.Count != 12*48 {
		t.Errorf("residuals %+v", est.Residuals)
	}
}

// With pixel noise the error must be the size the covariance claims, and the
// residual must be the size of the noise.
func TestCalibrateIntrinsicsCovarianceIsCalibrated(t *testing.T) {
	const trials, sigma = 24, 0.5
	in := testLens()
	var z2 float64
	var count int
	for seed := uint64(0); seed < trials; seed++ {
		views := boardViews(rand.New(rand.NewPCG(seed, 7)), in, 20, 35*deg, sigma)
		est, err := CalibrateIntrinsics(views, in.Width, in.Height, Config{})
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if est.Residuals.RMSE < 0.8*sigma*math.Sqrt2 || est.Residuals.RMSE > 1.2*sigma*math.Sqrt2 {
			t.Errorf("seed %d: RMSE %.3f px for %.1f px noise per axis", seed, est.Residuals.RMSE, sigma)
		}
		errs := []float64{est.Intrinsics.FX - in.FX, est.Intrinsics.FY - in.FY, est.Intrinsics.CX - in.CX, est.Intrinsics.CY - in.CY, est.Intrinsics.K1 - in.K1, est.Intrinsics.K2 - in.K2}
		for i, e := range errs {
			z2 += e * e / est.Covariance.At(i, i)
			count++
		}
		if est.FocalSigma <= 0 || est.PrincipalSigmaPx <= 0 {
			t.Fatalf("seed %d: no uncertainty reported", seed)
		}
	}
	// The mean squared normalised error of a calibrated covariance is 1.
	if mean := z2 / float64(count); mean < 0.6 || mean > 1.6 {
		t.Errorf("normalised squared error %.2f over %d parameters, want about 1", mean, count)
	}
}

func TestCalibrateIntrinsicsRefuses(t *testing.T) {
	in := testLens()
	rng := rand.New(rand.NewPCG(3, 4))
	views := boardViews(rng, in, 12, 35*deg, 0.3)
	if _, err := CalibrateIntrinsics(views[:2], in.Width, in.Height, Config{}); !errors.Is(err, geom.ErrInsufficient) {
		t.Errorf("two views: %v, want ErrInsufficient", err)
	}
	// Twelve views that all face the camera squarely: the focal length is not
	// observable from fronto-parallel views, whatever their number.
	parallel := boardViews(rng, in, 12, 0, 0.3)
	if est, err := CalibrateIntrinsics(parallel, in.Width, in.Height, Config{}); !errors.Is(err, geom.ErrDegenerate) && !errors.Is(err, geom.ErrSingular) {
		t.Errorf("parallel views: err %v, focal %.1f ± %.1f%%, want a refusal", err, est.Intrinsics.FX, est.FocalSigma*100)
	}
	// A view whose points are collinear has no homography.
	line := [][]Correspondence{views[0], views[1], views[2][:8]}
	if _, err := CalibrateIntrinsics(line, in.Width, in.Height, Config{}); !errors.Is(err, geom.ErrDegenerate) {
		t.Errorf("collinear view: %v, want ErrDegenerate", err)
	}
}

func TestSolvePnPWithOutliers(t *testing.T) {
	in, pose := testLens(), testPose(t)
	rng := rand.New(rand.NewPCG(5, 6))
	pts := scenePoints(rng, in, pose, 60, false, 0.5)
	for i := 0; i < 15; i++ { // a quarter of the detections are wrong associations
		pts[i*4].Pixel = geom.Pixel{U: 1280 * rng.Float64(), V: 720 * rng.Float64()}
	}
	est, err := SolvePnP(in, pts, Config{PixelSigma: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if d := est.CameraToSite.T.Dist(pose.T); d > 0.01 || d > 5*est.TranslationSigmaM {
		t.Errorf("position off by %.4f m (1σ %.4f)", d, est.TranslationSigmaM)
	}
	if a := geom.RotAngle(est.CameraToSite.R, pose.R); a > 0.1*deg || a > 5*est.RotationSigmaRad {
		t.Errorf("rotation off by %.4f° (1σ %.4f°)", a/deg, est.RotationSigmaRad/deg)
	}
	if len(est.Inliers) < 43 || len(est.Inliers) > 47 {
		t.Errorf("%d inliers of 60 with 15 corrupted", len(est.Inliers))
	}
	if est.Residuals.RMSE > 1.2 {
		t.Errorf("RMSE %.2f px", est.Residuals.RMSE)
	}
}

func TestSolvePnPRefuses(t *testing.T) {
	in, pose := testLens(), testPose(t)
	rng := rand.New(rand.NewPCG(8, 9))
	if _, err := SolvePnP(in, scenePoints(rng, in, pose, 5, false, 0), Config{}); !errors.Is(err, geom.ErrInsufficient) {
		t.Errorf("five points: %v, want ErrInsufficient", err)
	}
	// Detections unrelated to the points: no camera explains them.
	junk := scenePoints(rng, in, pose, 40, false, 0)
	for i := range junk {
		junk[i].Pixel = geom.Pixel{U: 1280 * rng.Float64(), V: 720 * rng.Float64()}
	}
	if _, err := SolvePnP(in, junk, Config{}); !errors.Is(err, geom.ErrNoConsensus) {
		t.Errorf("unrelated detections: %v, want ErrNoConsensus", err)
	}
	if _, err := SolvePnP(geom.Intrinsics{}, junk, Config{}); err == nil {
		t.Error("invalid intrinsics accepted")
	}
}

func TestResect(t *testing.T) {
	in, pose := testLens(), testPose(t)
	exact, err := Resect(scenePoints(rand.New(rand.NewPCG(10, 11)), in, pose, 80, false, 0), in.Width, in.Height, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if focalError(exact.Intrinsics, in) > 1e-6 || principalError(exact.Intrinsics, in) > 1e-3 || exact.CameraToSite.T.Dist(pose.T) > 1e-6 || geom.RotAngle(exact.CameraToSite.R, pose.R) > 1e-6 {
		t.Errorf("exact data: intrinsics %+v pose %+v", exact.Intrinsics, exact.CameraToSite)
	}

	const trials, sigma = 16, 0.5
	var z2 float64
	var count int
	for seed := uint64(0); seed < trials; seed++ {
		est, err := Resect(scenePoints(rand.New(rand.NewPCG(seed, 12)), in, pose, 200, false, sigma), in.Width, in.Height, Config{Seed: seed})
		if err != nil {
			t.Fatalf("seed %d: %v", seed, err)
		}
		if est.Covariance.Rows != 12 {
			t.Fatalf("covariance is %dx%d", est.Covariance.Rows, est.Covariance.Cols)
		}
		d := est.CameraToSite.T.Sub(pose.T).Array()
		for i := range d {
			z2 += d[i] * d[i] / est.Covariance.At(9+i, 9+i)
			count++
		}
		z2 += (est.Intrinsics.FX - in.FX) * (est.Intrinsics.FX - in.FX) / est.Covariance.At(0, 0)
		count++
		if e := focalError(est.Intrinsics, in); e > 0.01 {
			t.Errorf("seed %d: focal error %.2f%%", seed, e*100)
		}
		if d := est.CameraToSite.T.Dist(pose.T); d > 0.05 {
			t.Errorf("seed %d: position off by %.3f m", seed, d)
		}
	}
	if mean := z2 / float64(count); mean < 0.5 || mean > 1.8 {
		t.Errorf("normalised squared error %.2f over %d parameters, want about 1", mean, count)
	}
}

// Points on the floor only: the linear solve has no unique answer and Resect
// must say so rather than return whichever camera the noise picked.
func TestResectRefusesPlanarScene(t *testing.T) {
	in, pose := testLens(), testPose(t)
	est, err := Resect(scenePoints(rand.New(rand.NewPCG(13, 14)), in, pose, 120, true, 0.5), in.Width, in.Height, Config{})
	if err == nil {
		t.Errorf("planar scene accepted: focal %.1f, position error %.3f m", est.Intrinsics.FX, est.CameraToSite.T.Dist(pose.T))
	}
}
