package geom

import (
	"errors"
	"math"
	"math/rand/v2"
	"testing"
)

const (
	exactTol = 1e-12 // closed-form identities
	roundTol = 1e-9  // round trips through trigonometry or iteration
)

func newRand(seed uint64) *rand.Rand { return rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)) }

func near(t *testing.T, got, want, tol float64, what string) {
	t.Helper()
	if math.IsNaN(got) || math.Abs(got-want) > tol {
		t.Fatalf("%s: got %.15g want %.15g (tol %g)", what, got, want, tol)
	}
}

func randPose2(r *rand.Rand) Pose2 {
	return Pose2{r.Float64()*20 - 10, r.Float64()*20 - 10, (r.Float64()*2 - 1) * math.Pi}
}

func randVec3(r *rand.Rand, scale float64) Vec3 {
	return Vec3{(r.Float64()*2 - 1) * scale, (r.Float64()*2 - 1) * scale, (r.Float64()*2 - 1) * scale}
}

func randPose3(r *rand.Rand) Pose3 {
	return Pose3FromRotVec(randVec3(r, 1).Unit().Scale(r.Float64()*math.Pi), randVec3(r, 10))
}

func nearPose2(t *testing.T, got, want Pose2, tol float64, what string) {
	t.Helper()
	if got.Pos().Dist(want.Pos()) > tol || math.Abs(AngleDiff(got.Yaw, want.Yaw)) > tol {
		t.Fatalf("%s: got %+v want %+v", what, got, want)
	}
}

func nearPose3(t *testing.T, got, want Pose3, tol float64, what string) {
	t.Helper()
	if got.T.Dist(want.T) > tol || RotAngle(got.R, want.R) > tol {
		t.Fatalf("%s: got %+v want %+v", what, got, want)
	}
}

func TestWrapAngle(t *testing.T) {
	for _, c := range []struct{ in, want float64 }{
		{0, 0}, {math.Pi, math.Pi}, {-math.Pi, math.Pi}, {3 * math.Pi, math.Pi}, {2 * math.Pi, 0},
		{math.Pi + 0.25, -math.Pi + 0.25}, {-math.Pi - 0.25, math.Pi - 0.25}, {7, 7 - 2*math.Pi},
	} {
		near(t, WrapAngle(c.in), c.want, exactTol, "wrap")
	}
	r := newRand(1)
	for i := 0; i < 1000; i++ {
		a := (r.Float64() - 0.5) * 200
		w := WrapAngle(a)
		if w <= -math.Pi || w > math.Pi {
			t.Fatalf("wrap(%g)=%g outside (-pi, pi]", a, w)
		}
		near(t, math.Sin(w), math.Sin(a), roundTol, "wrap preserves sine")
		near(t, math.Cos(w), math.Cos(a), roundTol, "wrap preserves cosine")
	}
	near(t, AngleDiff(-3, 3), 2*math.Pi-6, exactTol, "shortest arc across pi")
}

func TestVecBasics(t *testing.T) {
	a, b := Vec2{3, 4}, Vec2{-1, 2}
	near(t, a.Norm(), 5, exactTol, "norm")
	near(t, a.Dot(b), 5, exactTol, "dot")
	near(t, a.Cross(b), 10, exactTol, "cross")
	near(t, Vec2{1, 0}.Cross(Vec2{0, 1}), 1, exactTol, "cross is positive toward +Z")
	near(t, Vec2{0, 2}.Angle(), math.Pi/2, exactTol, "angle of +Z")
	if got := (Vec2{1, 0}).Rotate(math.Pi / 2); got.Dist(Vec2{0, 1}) > exactTol {
		t.Fatalf("rotate +X by +pi/2 = %+v, want +Z", got)
	}
	if a.Perp().Dist(a.Rotate(math.Pi/2)) > exactTol {
		t.Fatalf("perp must be a +pi/2 rotation: %+v", a.Perp())
	}
	if a.Lerp(b, 0.25) != (Vec2{2, 3.5}) || (Vec3{0, 0, 0}).Lerp(Vec3{4, 8, -4}, 0.25) != (Vec3{1, 2, -1}) {
		t.Fatal("lerp")
	}
	if (Vec2{}).Unit() != (Vec2{}) || (Vec3{}).Unit() != (Vec3{}) {
		t.Fatal("unit of zero must be zero")
	}
	x, y, z := Vec3{1, 0, 0}, Vec3{0, 1, 0}, Vec3{0, 0, 1}
	if x.Cross(y) != z || y.Cross(z) != x || z.Cross(x) != y {
		t.Fatal("cross product is not right-handed")
	}
	if got := Vec3FromArray(Vec3{1, 2, 3}.Array()); got != (Vec3{1, 2, 3}) || got.XZ() != (Vec2{1, 3}) || got.XZ().Lift(2) != got {
		t.Fatal("array, XZ and Lift must round trip")
	}
}

func TestPose2Convention(t *testing.T) {
	p := Pose2{2, 3, math.Pi / 2}
	if h := p.Heading(); h.Dist(Vec2{0, 1}) > exactTol {
		t.Fatalf("heading at yaw pi/2 = %+v, want +Z", h)
	}
	// One metre ahead of a body facing +Z is one metre further along +Z.
	if got := p.Apply(Vec2{1, 0}); got.Dist(Vec2{2, 4}) > exactTol {
		t.Fatalf("apply forward = %+v", got)
	}
	// Body +Z is to the right of travel: facing +Z, right is −X.
	if got := p.Apply(Vec2{0, 1}); got.Dist(Vec2{1, 3}) > exactTol {
		t.Fatalf("apply right = %+v", got)
	}
}

func TestPose2Algebra(t *testing.T) {
	r := newRand(2)
	for i := 0; i < 500; i++ {
		a, b, c := randPose2(r), randPose2(r), randPose2(r)
		v := Vec2{r.Float64()*4 - 2, r.Float64()*4 - 2}
		nearPose2(t, a.Compose(a.Inverse()), Pose2{}, roundTol, "a∘a⁻¹")
		nearPose2(t, a.Inverse().Compose(a), Pose2{}, roundTol, "a⁻¹∘a")
		nearPose2(t, a.Compose(b).Compose(c), a.Compose(b.Compose(c)), roundTol, "associativity")
		nearPose2(t, a.Compose(a.Between(b)), b, roundTol, "between")
		if got, want := a.Compose(b).Apply(v), a.Apply(b.Apply(v)); got.Dist(want) > roundTol {
			t.Fatalf("compose/apply: %+v vs %+v", got, want)
		}
		if got := a.InverseApply(a.Apply(v)); got.Dist(v) > roundTol {
			t.Fatalf("inverse apply: %+v vs %+v", got, v)
		}
		if got, want := a.Inverse().Apply(v), a.InverseApply(v); got.Dist(want) > roundTol {
			t.Fatalf("inverse vs inverse apply: %+v vs %+v", got, want)
		}
		if got, want := a.Rotate(v), a.Apply(v).Sub(a.Pos()); got.Dist(want) > roundTol {
			t.Fatalf("rotate: %+v vs %+v", got, want)
		}
		nearPose2(t, Pose2{a.X, a.Z, a.Yaw + 6*math.Pi}.Normalized(), a, roundTol, "normalized")
		nearPose2(t, a.Interpolate(b, 0), a, roundTol, "interpolate 0")
		nearPose2(t, a.Interpolate(b, 1), b, roundTol, "interpolate 1")
	}
	mid := Pose2{0, 0, 3}.Interpolate(Pose2{2, 4, -3}, 0.5)
	nearPose2(t, mid, Pose2{1, 2, math.Pi}, roundTol, "interpolation takes the short arc through pi")
}

func TestQuaternion(t *testing.T) {
	// A right-handed quarter turn about +Y takes +X to −Z.
	q := QuatFromRot(RotY(math.Pi / 2))
	want := [4]float64{0, math.Sqrt2 / 2, 0, math.Sqrt2 / 2}
	for i := range q {
		near(t, q[i], want[i], exactTol, "quaternion of RotY(90°)")
	}
	if got := RotY(math.Pi / 2).MulVec(Vec3{1, 0, 0}); got.Dist(Vec3{0, 0, -1}) > exactTol {
		t.Fatalf("RotY(90°)·x = %+v", got)
	}
	r := newRand(3)
	for i := 0; i < 500; i++ {
		p := randPose3(r)
		back, err := Pose3FromQuat(p.QuatXYZW(), p.T)
		if err != nil {
			t.Fatal(err)
		}
		nearPose3(t, back, p, roundTol, "quaternion round trip")
		q := p.QuatXYZW()
		near(t, q[0]*q[0]+q[1]*q[1]+q[2]*q[2]+q[3]*q[3], 1, exactTol, "unit quaternion")
		if q[3] < 0 {
			t.Fatal("quaternion w must be non-negative")
		}
	}
	// Every Shepperd branch: half turns about each axis have zero trace terms.
	for _, m := range []Mat3{RotX(math.Pi), RotY(math.Pi), RotZ(math.Pi), RotX(3)} {
		back, err := RotFromQuat(QuatFromRot(m))
		if err != nil || RotAngle(back, m) > roundTol {
			t.Fatalf("half-turn quaternion round trip failed: %v", err)
		}
	}
	if _, err := RotFromQuat([4]float64{}); !errors.Is(err, ErrDegenerate) {
		t.Fatalf("zero quaternion: %v", err)
	}
	if _, err := RotFromQuat([4]float64{math.NaN(), 0, 0, 1}); !errors.Is(err, ErrNonFinite) {
		t.Fatalf("NaN quaternion: %v", err)
	}
}

func TestPose3Algebra(t *testing.T) {
	r := newRand(4)
	for i := 0; i < 300; i++ {
		a, b, c := randPose3(r), randPose3(r), randPose3(r)
		v := randVec3(r, 5)
		nearPose3(t, a.Compose(a.Inverse()), IdentityPose3(), roundTol, "a∘a⁻¹")
		nearPose3(t, a.Compose(b).Compose(c), a.Compose(b.Compose(c)), roundTol, "associativity")
		nearPose3(t, a.Compose(a.Between(b)), b, roundTol, "between")
		if got, want := a.Compose(b).Apply(v), a.Apply(b.Apply(v)); got.Dist(want) > roundTol {
			t.Fatalf("compose/apply: %+v vs %+v", got, want)
		}
		if !a.R.IsRotation(roundTol) {
			t.Fatal("RotExp did not produce a rotation")
		}
		nearPose3(t, Pose3FromRotVec(a.RotVec(), a.T), a, 1e-7, "rotation vector round trip")
		m, err := Pose3FromMatrix4(a.Matrix4())
		if err != nil {
			t.Fatal(err)
		}
		nearPose3(t, m, a, exactTol, "matrix4 round trip")
	}
}

func TestMatrix4Layout(t *testing.T) {
	p := Pose3{RotZ(0.3), Vec3{1, 2, 3}}
	m := p.Matrix4()
	if m[3] != 1 || m[7] != 2 || m[11] != 3 || m[15] != 1 || m[12] != 0 {
		t.Fatalf("translation must sit in the last column: %v", m)
	}
	bad := p.Matrix4()
	bad[0] *= 1.01
	if _, err := Pose3FromMatrix4(bad); !errors.Is(err, ErrDegenerate) {
		t.Fatalf("non-orthonormal rotation accepted: %v", err)
	}
	mirror := IdentityPose3().Matrix4()
	mirror[0] = -1
	if _, err := Pose3FromMatrix4(mirror); !errors.Is(err, ErrDegenerate) {
		t.Fatalf("reflection accepted: %v", err)
	}
	if _, err := Pose3FromMatrix4(make([]float64, 12)); !errors.Is(err, ErrDimension) {
		t.Fatalf("short matrix accepted: %v", err)
	}
	row := p.Matrix4()
	row[12] = 0.1
	if _, err := Pose3FromMatrix4(row); !errors.Is(err, ErrDegenerate) {
		t.Fatalf("projective bottom row accepted: %v", err)
	}
}

func TestPose2Pose3Consistency(t *testing.T) {
	r := newRand(5)
	for i := 0; i < 300; i++ {
		a, b := randPose2(r), randPose2(r)
		elevA, elevB := r.Float64()*6, r.Float64()*2
		v := Vec2{r.Float64()*4 - 2, r.Float64()*4 - 2}
		a3 := Pose3FromPose2(a, elevA)
		// A planar point transforms identically in 2D and 3D.
		if got, want := a3.Apply(v.Lift(0)), a.Apply(v).Lift(elevA); got.Dist(want) > roundTol {
			t.Fatalf("lifted apply: %+v vs %+v", got, want)
		}
		// The body +X axis is the heading (cos yaw, 0, sin yaw); +Y stays up.
		if got, want := a3.Rotate(Vec3{1, 0, 0}), a.Heading().Lift(0); got.Dist(want) > roundTol {
			t.Fatalf("heading: %+v vs %+v", got, want)
		}
		if got := a3.Rotate(Vec3{0, 1, 0}); got.Dist(Vec3{0, 1, 0}) > roundTol {
			t.Fatalf("up axis moved: %+v", got)
		}
		// Lifting is a homomorphism; elevations add.
		nearPose3(t, Pose3FromPose2(a, elevA).Compose(Pose3FromPose2(b, elevB)), Pose3FromPose2(a.Compose(b), elevA+elevB), roundTol, "lift homomorphism")
		back, elev, ok := a3.Planar()
		if !ok {
			t.Fatal("planar projection refused a planar pose")
		}
		nearPose2(t, back, a, roundTol, "planar round trip")
		near(t, elev, elevA, exactTol, "elevation")
	}
	// A sensor pointing straight up has no planar heading.
	if _, _, ok := (Pose3{R: RotZ(math.Pi / 2)}).Planar(); ok {
		t.Fatal("vertical X axis must report an undefined yaw")
	}
}

func TestRotExpLog(t *testing.T) {
	r := newRand(6)
	for i := 0; i < 500; i++ {
		w := randVec3(r, 1).Unit().Scale(r.Float64() * (math.Pi - 1e-3))
		if got := RotLog(RotExp(w)); got.Dist(w) > 1e-8 {
			t.Fatalf("log(exp(w)): %+v vs %+v", got, w)
		}
	}
	for _, w := range []Vec3{{}, {1e-14, 0, 0}, {0, 1e-9, -1e-9}, {0, math.Pi - 1e-9, 0}, {0.3, -0.2, 0.9}} {
		m := RotExp(w)
		if !m.IsRotation(1e-9) {
			t.Fatalf("exp(%+v) is not a rotation", w)
		}
		if got := RotLog(m); got.Dist(w) > 1e-7 {
			t.Fatalf("log(exp(%+v)) = %+v", w, got)
		}
	}
	near(t, RotLog(RotExp(Vec3{0, math.Pi, 0})).Norm(), math.Pi, 1e-9, "half turn angle")
	near(t, RotAngle(RotX(0.2), RotX(0.5)), 0.3, roundTol, "rotation angle")
	near(t, RotAngle(RotY(1e-7), Identity3()), 1e-7, 1e-15, "small rotation angle keeps precision")
	near(t, RotAngle(RotY(0.4), RotY(0.4)), 0, exactTol, "identical rotations")
}

func TestMat3(t *testing.T) {
	r := newRand(7)
	for i := 0; i < 200; i++ {
		var m Mat3
		for j := range m {
			m[j] = r.Float64()*2 - 1
		}
		inv, err := m.Inverse()
		if err != nil {
			continue
		}
		p := m.Mul(inv)
		for j, v := range Identity3() {
			near(t, p[j], v, 1e-7, "m·m⁻¹")
		}
	}
	if _, err := (Mat3{1, 2, 3, 2, 4, 6, 0, 1, 0}).Inverse(); !errors.Is(err, ErrSingular) {
		t.Fatalf("singular inverse: %v", err)
	}
	near(t, Mat3{2, 0, 0, 0, 3, 0, 0, 0, 4}.Det(), 24, exactTol, "det")
}

func TestOrthonormalize(t *testing.T) {
	r := newRand(8)
	for i := 0; i < 200; i++ {
		truth := randPose3(r).R
		noisy := truth
		for j := range noisy {
			noisy[j] += (r.Float64() - 0.5) * 1e-3
		}
		got, err := noisy.Scale(3.7).Orthonormalize()
		if err != nil {
			t.Fatal(err)
		}
		if !got.IsRotation(roundTol) || RotAngle(got, truth) > 2e-3 {
			t.Fatalf("nearest rotation is %g rad from truth", RotAngle(got, truth))
		}
	}
	// A rank-two matrix still determines the rotation; rank one does not.
	rank2 := RotZ(0.4).Mul(Mat3{1, 0, 0, 0, 1, 0, 0, 0, 0})
	got, err := rank2.Orthonormalize()
	if err != nil || RotAngle(got, RotZ(0.4)) > roundTol {
		t.Fatalf("rank-two orthonormalize: %v", err)
	}
	if _, err := (Mat3{1, 0, 0, 0, 0, 0, 0, 0, 0}).Orthonormalize(); !errors.Is(err, ErrSingular) {
		t.Fatalf("rank-one orthonormalize: %v", err)
	}
}

func TestRQ3(t *testing.T) {
	r := newRand(9)
	for i := 0; i < 300; i++ {
		k := Mat3{400 + r.Float64()*800, r.Float64() * 2, 300 + r.Float64()*100, 0, 400 + r.Float64()*800, 200 + r.Float64()*100, 0, 0, 1}
		rot := randPose3(r).R
		gotK, gotQ, err := RQ3(k.Mul(rot))
		if err != nil {
			t.Fatal(err)
		}
		if RotAngle(gotQ, rot) > roundTol {
			t.Fatalf("RQ rotation off by %g rad", RotAngle(gotQ, rot))
		}
		for j := range k {
			near(t, gotK[j], k[j], 1e-7, "RQ calibration matrix")
		}
		if gotK[3] != 0 || gotK[6] != 0 || gotK[7] != 0 {
			t.Fatal("k is not upper triangular")
		}
	}
	// A negated input cannot keep both a positive diagonal and det(q) = +1.
	_, q, err := RQ3(RotX(0.3).Scale(-1))
	if err != nil || math.Abs(q.Det()+1) > roundTol {
		t.Fatalf("negated block: det(q)=%g err=%v", q.Det(), err)
	}
	if _, _, err := RQ3(Mat3{1, 2, 3, 2, 4, 6, 0, 0, 1}); !errors.Is(err, ErrSingular) {
		t.Fatalf("singular RQ: %v", err)
	}
}
