package geom

import (
	"errors"
	"math"
	"testing"
)

var testCamera = Intrinsics{FX: 610, FY: 605, CX: 322, CY: 238, K1: -0.28, K2: 0.09, Width: 640, Height: 480}

func TestProjectKnown(t *testing.T) {
	plain := Intrinsics{FX: 500, FY: 400, CX: 320, CY: 240, Width: 640, Height: 480}
	px, ok := plain.Project(Vec3{0.2, -0.1, 2})
	if !ok {
		t.Fatal("point in front of the camera did not project")
	}
	near(t, px.U, 370, exactTol, "u = fx·x/z + cx")
	near(t, px.V, 220, exactTol, "v = fy·y/z + cy: optical +y is image down")
	// Radial distortion scales the offset from the principal point by 1 + k1·r² + k2·r⁴.
	dist := plain
	dist.K1, dist.K2 = 0.1, 0.01
	px, _ = dist.Project(Vec3{1, 0, 1})
	near(t, px.U, 320+500*1.11, exactTol, "distorted u")
	if _, ok := plain.Project(Vec3{0, 0, -1}); ok {
		t.Fatal("point behind the camera projected")
	}
	if _, ok := plain.Project(Vec3{0, 0, 0}); ok {
		t.Fatal("point at the optical centre projected")
	}
	if plain.K() != (Mat3{500, 0, 320, 0, 400, 240, 0, 0, 1}) {
		t.Fatal("calibration matrix layout")
	}
	if !plain.InImage(Pixel{0, 479.5}) || plain.InImage(Pixel{640, 10}) || plain.InImage(Pixel{-0.1, 10}) {
		t.Fatal("image bounds are [0, width) x [0, height)")
	}
}

func TestProjectUndistortRoundTrip(t *testing.T) {
	const pixelTol = 1e-8
	r := newRand(60)
	for _, in := range []Intrinsics{testCamera, {FX: 400, FY: 400, CX: 320, CY: 240, K1: 0.12, K2: 0.03, Width: 640, Height: 480}, {FX: 700, FY: 700, CX: 300, CY: 250, Width: 640, Height: 480}} {
		for i := 0; i < 2000; i++ {
			px := Pixel{r.Float64() * float64(in.Width), r.Float64() * float64(in.Height)}
			ray, err := in.Undistort(px)
			if err != nil {
				t.Fatalf("undistort %+v: %v", px, err)
			}
			if ray.Z != 1 {
				t.Fatal("undistorted ray must have unit depth")
			}
			back, ok := in.Project(ray.Scale(0.5 + r.Float64()*10))
			if !ok || back.Dist(px) > pixelTol {
				t.Fatalf("round trip %+v -> %+v", px, back)
			}
		}
	}
	centre, err := testCamera.Undistort(Pixel{testCamera.CX, testCamera.CY})
	if err != nil || centre != (Vec3{0, 0, 1}) {
		t.Fatalf("principal point: %+v %v", centre, err)
	}
}

// With barrel distortion the model folds back beyond a finite radius. Points
// out there must not project (they would land inside the image), and pixels
// beyond the fold have no ray.
func TestDistortionFoldBack(t *testing.T) {
	in := Intrinsics{FX: 300, FY: 300, CX: 320, CY: 240, K1: -0.3, Width: 640, Height: 480}
	limit := math.Sqrt(1 / 0.9) // d/dr r(1 − 0.3r²) = 0
	if _, ok := in.Project(Vec3{limit * 0.999, 0, 1}); !ok {
		t.Fatal("point just inside the monotonic radius must project")
	}
	if _, ok := in.Project(Vec3{limit * 1.001, 0, 1}); ok {
		t.Fatal("point beyond the monotonic radius must not project")
	}
	if _, ok := in.Project(Vec3{5, 0, 1}); ok {
		t.Fatal("far off-axis point folded back into the image")
	}
	maxU := in.CX + in.FX*limit*(1-0.3*limit*limit)
	if _, err := in.Undistort(Pixel{maxU + 5, in.CY}); !errors.Is(err, ErrDegenerate) {
		t.Fatalf("pixel beyond the fold: %v", err)
	}
	if _, err := in.Undistort(Pixel{math.NaN(), 0}); !errors.Is(err, ErrNonFinite) {
		t.Fatalf("NaN pixel: %v", err)
	}
	// With both terms the fold is the smaller positive root of 5·k2·s² + 3·k1·s + 1.
	both := Intrinsics{FX: 300, FY: 300, CX: 320, CY: 240, K1: -0.3, K2: 0.02, Width: 640, Height: 480}
	s := both.maxR2()
	near(t, 0.1*s*s-0.9*s+1, 0, roundTol, "fold radius is a root of the derivative")
	near(t, s, (0.9-math.Sqrt(0.41))/0.2, roundTol, "smaller root")
	edge, ok := both.Project(Vec3{math.Sqrt(s) * 0.99, 0, 1})
	if !ok {
		t.Fatal("point inside the fold must project")
	}
	ray, err := both.Undistort(edge)
	if err != nil {
		t.Fatal(err)
	}
	near(t, ray.X, math.Sqrt(s)*0.99, 1e-7, "undistort near the fold")
	// k1 < 0 with k2 > 0 large enough never folds.
	if got := (Intrinsics{K1: -0.2, K2: 0.1}).maxR2(); !math.IsInf(got, 1) {
		t.Fatalf("monotonic model reported a fold at %g", got)
	}
}

func TestIntrinsicsValidate(t *testing.T) {
	if err := testCamera.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Intrinsics){
		func(in *Intrinsics) { in.FX = 0 },
		func(in *Intrinsics) { in.FY = -1 },
		func(in *Intrinsics) { in.K1 = math.NaN() },
		func(in *Intrinsics) { in.Width = 0 },
		func(in *Intrinsics) { in.CX = math.Inf(1) },
	} {
		in := testCamera
		mutate(&in)
		if in.Validate() == nil {
			t.Fatalf("invalid intrinsics accepted: %+v", in)
		}
	}
}

func TestLookAtAndProjectWorld(t *testing.T) {
	eye, target, up := Vec3{2, 2.5, -1}, Vec3{5, 0.5, 3}, Vec3{0, 1, 0}
	cam, err := LookAt(eye, target, up)
	if err != nil {
		t.Fatal(err)
	}
	if !cam.R.IsRotation(roundTol) {
		t.Fatal("LookAt did not return a right-handed rotation")
	}
	px, ok := testCamera.ProjectWorld(cam, target)
	if !ok || px.Dist(Pixel{testCamera.CX, testCamera.CY}) > 1e-9 {
		t.Fatalf("target must project to the principal point: %+v", px)
	}
	// Optical y is down: a point above the target has a smaller v.
	above, _ := testCamera.ProjectWorld(cam, target.Add(up.Scale(0.3)))
	if above.V >= px.V || math.Abs(above.U-px.U) > 1e-6 {
		t.Fatalf("world up must be image up: %+v vs %+v", above, px)
	}
	// Optical x is right: forward × up.
	right := target.Sub(eye).Cross(up).Unit()
	side, _ := testCamera.ProjectWorld(cam, target.Add(right.Scale(0.3)))
	if side.U <= px.U {
		t.Fatalf("world right must be image right: %+v vs %+v", side, px)
	}
	if _, ok := testCamera.ProjectWorld(cam, eye.Sub(target.Sub(eye))); ok {
		t.Fatal("point behind the camera projected")
	}
	if _, err := LookAt(eye, eye, up); !errors.Is(err, ErrDegenerate) {
		t.Fatalf("eye == target: %v", err)
	}
	if _, err := LookAt(eye, eye.Add(up), up); !errors.Is(err, ErrDegenerate) {
		t.Fatalf("view along up: %v", err)
	}
}

func TestPixelRayToFloor(t *testing.T) {
	const floorTol = 1e-7 // metres
	cam, err := LookAt(Vec3{0, 2.8, 0}, Vec3{4, 0, 3}, Vec3{0, 1, 0})
	if err != nil {
		t.Fatal(err)
	}
	r := newRand(61)
	for i := 0; i < 500; i++ {
		foot := Vec3{2 + r.Float64()*4, 0.1, 1 + r.Float64()*4}
		px, ok := testCamera.ProjectWorld(cam, foot)
		if !ok {
			continue
		}
		origin, dir, err := testCamera.PixelRay(cam, px)
		if err != nil {
			t.Fatal(err)
		}
		near(t, dir.Norm(), 1, exactTol, "ray direction is a unit vector")
		got, ok := RayPlaneY(origin, dir, foot.Y)
		if !ok || got.Dist(foot) > floorTol {
			t.Fatalf("floor intersection %+v, want %+v", got, foot)
		}
	}
	if _, ok := RayPlaneY(Vec3{0, 1, 0}, Vec3{1, 0, 0}, 0); ok {
		t.Fatal("horizontal ray cannot reach the floor")
	}
	if _, ok := RayPlaneY(Vec3{0, 1, 0}, Vec3{0, 1, 0}, 0); ok {
		t.Fatal("upward ray cannot reach the floor below")
	}
}
