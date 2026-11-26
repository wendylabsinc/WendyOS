package geom

import (
	"errors"
	"math"
	"testing"
)

// A pose covariance mixes m² and rad², and a scan match in a corridor adds
// 1e4 m² along the axis it cannot observe: well-posed, far from equal scale.
func TestMat3InverseMixedScales(t *testing.T) {
	// Unobserved variance along a direction tilted off the x axis, as a
	// corridor that is not grid-aligned produces, plus the systematic floors.
	weak := Vec3{X: 0.99, Y: 0.1, Z: 0.01}.Unit().Array()
	var tilted Mat3
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			tilted[i*3+j] = 1e4 * weak[i] * weak[j]
		}
	}
	tilted[0], tilted[4], tilted[8] = tilted[0]+1e-4, tilted[4]+1e-4, tilted[8]+4e-6
	for name, m := range map[string]Mat3{
		"diag(1e4, 1e-3, 1e-5)": {1e4, 0, 0, 0, 1e-3, 0, 0, 0, 1e-5},
		"diag(1e4, 2e-4, 4e-6)": {1e4, 0, 0, 0, 2e-4, 0, 0, 0, 4e-6},
		"diag(1e10, 1, 1e-10)":  {1e10, 0, 0, 0, 1, 0, 0, 0, 1e-10},
		"tilted corridor":       tilted,
	} {
		inv, err := m.Inverse()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		p := m.Mul(inv)
		for j, v := range Identity3() {
			near(t, p[j], v, 1e-6, name+": m·m⁻¹")
		}
		if inv[1] != inv[3] || inv[2] != inv[6] || inv[5] != inv[7] {
			t.Fatalf("%s: inverse of a symmetric matrix is not symmetric: %v", name, inv)
		}
	}
	if _, err := (Mat3{}).Inverse(); !errors.Is(err, ErrSingular) {
		t.Fatalf("zero matrix: %v", err)
	}
	if _, err := (Mat3{math.NaN()}).Inverse(); !errors.Is(err, ErrNonFinite) {
		t.Fatalf("NaN: %v", err)
	}
}

// A parameter with an exactly zero Jacobian column (a mount offset while the
// device stands still) must not stop the observable ones from converging.
func TestLMOptimisesAroundAnUnusedParameter(t *testing.T) {
	xs := []float64{0, 1, 2, 3, 4}
	line := LMProblem{NumResiduals: len(xs), Residuals: func(x, r []float64) {
		for i, u := range xs {
			r[i] = x[0]*u + x[1] - (2*u + 0.5)
		}
	}}
	res, err := LevenbergMarquardt(line, []float64{0, 0, 7}, LMConfig{})
	if !errors.Is(err, ErrSingular) {
		t.Fatalf("an unused parameter must be reported: %v", err)
	}
	near(t, res.X[0], 2, 1e-8, "slope")
	near(t, res.X[1], 0.5, 1e-8, "intercept")
	near(t, res.X[2], 7, 0, "the unused parameter keeps its start")
	if !res.Converged || res.RMSE > 1e-8 || res.Covariance.Data != nil {
		t.Fatalf("result: %+v", res)
	}
}

// Squared column norms of entries near 1e±150 leave the float64 range.
func TestSVDExtremeScales(t *testing.T) {
	base := MatFrom(3, 3, []float64{4, 1, -2, 1, 3, 0.5, -2, 0.5, 5})
	want := checkSVD(t, base)
	for _, scale := range []float64{1e-200, 1e-150, 1e150, 1e200} {
		a := base.Clone()
		for i := range a.Data {
			a.Data[i] *= scale
		}
		got, err := SVD(a)
		if err != nil {
			t.Fatalf("scale %g: %v", scale, err)
		}
		for i, s := range got.S {
			near(t, s/scale, want.S[i], 1e-9, "scaled singular value")
		}
		if d := maxAbsDiff(got.U.T().Mul(got.U), Identity(3)); d > 1e-10 {
			t.Fatalf("scale %g: U is not orthonormal: %g", scale, d)
		}
	}
	zero, err := SVD(NewMat(2, 2))
	if err != nil || zero.S[0] != 0 || zero.S[1] != 0 {
		t.Fatalf("zero matrix: %+v %v", zero, err)
	}
}

func TestPose2FromROS(t *testing.T) {
	// REP-103: y is left and yaw counter-clockwise; here z is right and yaw
	// runs toward +z. A robot that drives forward while turning left ends up
	// at negative z with negative yaw.
	p := Pose2FromROS(2, 1, 0.3)
	near(t, p.X, 2, 0, "x")
	near(t, p.Z, -1, 0, "z")
	near(t, p.Yaw, -0.3, 0, "yaw")
	// A point one metre to the robot's left: ROS body (0, 1), here (0, −1).
	rosYaw := 0.3
	wantX, wantY := 2-math.Sin(rosYaw), 1+math.Cos(rosYaw)
	got := p.Apply(Vec2{X: 0, Z: -1})
	near(t, got.X, wantX, exactTol, "left point x")
	near(t, got.Z, -wantY, exactTol, "left point z")
	near(t, Pose2FromROS(0, 0, -3*math.Pi).Yaw, math.Pi, exactTol, "yaw is wrapped")
}

func TestViewYaw(t *testing.T) {
	cam, err := LookAt(Vec3{X: 1, Y: 2.5, Z: 2}, Vec3{X: 4, Y: 0, Z: 6}, Vec3{Y: 1})
	if err != nil {
		t.Fatal(err)
	}
	yaw, ok := cam.ViewYaw()
	near(t, yaw, math.Atan2(4, 3), roundTol, "a camera looks along its optical axis")
	if planar, _, _ := cam.Planar(); !ok || math.Abs(math.Abs(AngleDiff(planar.Yaw, yaw))-math.Pi/2) > roundTol {
		t.Fatalf("Planar yaw %g is the image-right axis, a quarter turn from the view yaw %g", planar.Yaw, yaw)
	}
	if _, ok := (Pose3{R: RotX(math.Pi / 2)}).ViewYaw(); ok {
		t.Fatal("a camera looking straight down has no view yaw")
	}
}
