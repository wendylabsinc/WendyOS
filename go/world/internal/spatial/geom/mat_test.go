package geom

import (
	"errors"
	"math"
	"math/rand/v2"
	"testing"
)

func randMat(r *rand.Rand, rows, cols int) Mat {
	m := NewMat(rows, cols)
	for i := range m.Data {
		m.Data[i] = r.Float64()*2 - 1
	}
	return m
}

func maxAbsDiff(a, b Mat) float64 {
	d := 0.0
	for i := range a.Data {
		d = math.Max(d, math.Abs(a.Data[i]-b.Data[i]))
	}
	return d
}

func TestMatBasics(t *testing.T) {
	a := MatFrom(2, 3, []float64{1, 2, 3, 4, 5, 6})
	b := MatFrom(3, 2, []float64{7, 8, 9, 10, 11, 12})
	if got := a.Mul(b); maxAbsDiff(got, MatFrom(2, 2, []float64{58, 64, 139, 154})) > exactTol {
		t.Fatalf("product: %v", got.Data)
	}
	if got := a.T(); got.Rows != 3 || got.At(2, 1) != 6 || got.At(0, 1) != 4 {
		t.Fatalf("transpose: %+v", got)
	}
	if got := a.MulVec([]float64{1, 0, -1}); got[0] != -2 || got[1] != -2 {
		t.Fatalf("matvec: %v", got)
	}
	if c := a.Col(1); c[0] != 2 || c[1] != 5 {
		t.Fatalf("col: %v", c)
	}
	row := a.Row(1)
	row[0] = 99
	if a.At(1, 0) != 4 {
		t.Fatal("Row must return a copy")
	}
}

func TestSolve(t *testing.T) {
	x, err := Solve(MatFrom(3, 3, []float64{2, 1, -1, -3, -1, 2, -2, 1, 2}), []float64{8, -11, -3})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []float64{2, 3, -1} {
		near(t, x[i], want, exactTol*10, "known solution")
	}
	r := newRand(10)
	for n := 1; n <= 12; n++ {
		for trial := 0; trial < 20; trial++ {
			a := randMat(r, n, n)
			want := randMat(r, n, 1).Data
			got, err := Solve(a, a.MulVec(want))
			if err != nil {
				continue // a random matrix may be ill-conditioned; refusing is allowed
			}
			inv, err := Inverse(a)
			if err != nil {
				t.Fatal(err)
			}
			if maxAbsDiff(a.Mul(inv), Identity(n)) > 1e-6 {
				t.Fatalf("n=%d: a·a⁻¹ is not the identity", n)
			}
			for i := range want {
				near(t, got[i], want[i], 1e-6, "random solve")
			}
		}
	}
}

func TestSolveScalingAndSingularity(t *testing.T) {
	// Badly scaled but perfectly conditioned after equilibration.
	x, err := Solve(MatFrom(2, 2, []float64{1e9, 0, 0, 1e-9}), []float64{2e9, 3e-9})
	if err != nil {
		t.Fatal(err)
	}
	near(t, x[0], 2, exactTol, "scaled solve")
	near(t, x[1], 3, exactTol, "scaled solve")
	for name, a := range map[string]Mat{
		"dependent rows": MatFrom(3, 3, []float64{1, 2, 3, 2, 4, 6, 1, 0, 1}),
		"zero row":       MatFrom(2, 2, []float64{1, 2, 0, 0}),
		"zero column":    MatFrom(2, 2, []float64{1, 0, 2, 0}),
	} {
		if _, err := Solve(a, make([]float64, a.Rows)); !errors.Is(err, ErrSingular) {
			t.Fatalf("%s: got %v, want ErrSingular", name, err)
		}
		if _, err := Inverse(a); !errors.Is(err, ErrSingular) {
			t.Fatalf("%s inverse: got %v, want ErrSingular", name, err)
		}
	}
	if _, err := Solve(NewMat(2, 3), []float64{1, 2}); !errors.Is(err, ErrDimension) {
		t.Fatalf("non-square: %v", err)
	}
	if _, err := Solve(Identity(2), []float64{1}); !errors.Is(err, ErrDimension) {
		t.Fatalf("short rhs: %v", err)
	}
	if _, err := Solve(MatFrom(1, 1, []float64{math.Inf(1)}), []float64{1}); !errors.Is(err, ErrNonFinite) {
		t.Fatalf("infinite entry: %v", err)
	}
}

func TestCholesky(t *testing.T) {
	r := newRand(11)
	for n := 1; n <= 10; n++ {
		b := randMat(r, n+3, n)
		a := b.T().Mul(b) // SPD with probability one
		l, err := Cholesky(a)
		if err != nil {
			t.Fatal(err)
		}
		if maxAbsDiff(l.Mul(l.T()), a) > 1e-10 {
			t.Fatalf("n=%d: L·Lᵀ != a", n)
		}
		want := randMat(r, n, 1).Data
		got, err := SolveCholesky(a, a.MulVec(want))
		if err != nil {
			t.Fatal(err)
		}
		for i := range want {
			near(t, got[i], want[i], 1e-7, "cholesky solve")
		}
	}
	for name, a := range map[string]Mat{
		"indefinite":    MatFrom(2, 2, []float64{1, 2, 2, 1}),
		"semi-definite": MatFrom(2, 2, []float64{1, 1, 1, 1}),
		"negative":      MatFrom(1, 1, []float64{-1}),
	} {
		if _, err := Cholesky(a); !errors.Is(err, ErrNotPositiveDefinite) {
			t.Fatalf("%s: got %v", name, err)
		}
	}
}

func TestSymEigen(t *testing.T) {
	vals, vecs, err := SymEigen(MatFrom(2, 2, []float64{2, 1, 1, 2}))
	if err != nil {
		t.Fatal(err)
	}
	near(t, vals[0], 3, exactTol, "largest eigenvalue first")
	near(t, vals[1], 1, exactTol, "smallest eigenvalue last")
	near(t, math.Abs(vecs.At(0, 0)), math.Sqrt2/2, exactTol, "eigenvector")
	r := newRand(12)
	for n := 1; n <= 12; n++ {
		b := randMat(r, n, n)
		a := b.Mul(b.T())
		for i := range a.Data { // mix in an indefinite part
			a.Data[i] -= 0.3
		}
		vals, vecs, err := SymEigen(a)
		if err != nil {
			t.Fatal(err)
		}
		lambda := NewMat(n, n)
		for i, v := range vals {
			lambda.Set(i, i, v)
			if i > 0 && v > vals[i-1] {
				t.Fatal("eigenvalues are not descending")
			}
		}
		if maxAbsDiff(a.Mul(vecs), vecs.Mul(lambda)) > 1e-9 {
			t.Fatalf("n=%d: a·V != V·Λ", n)
		}
		if maxAbsDiff(vecs.T().Mul(vecs), Identity(n)) > 1e-10 {
			t.Fatalf("n=%d: eigenvectors are not orthonormal", n)
		}
	}
	if _, _, err := SymEigen(MatFrom(2, 2, []float64{1, 2, 3, 4})); !errors.Is(err, ErrDimension) {
		t.Fatalf("asymmetric input: %v", err)
	}
}

func checkSVD(t *testing.T, a Mat) SVDResult {
	t.Helper()
	svd, err := SVD(a)
	if err != nil {
		t.Fatal(err)
	}
	n := a.Cols
	sigma := NewMat(n, n)
	for i, s := range svd.S {
		sigma.Set(i, i, s)
		if s < 0 || (i > 0 && s > svd.S[i-1]) {
			t.Fatalf("singular values not sorted non-negative: %v", svd.S)
		}
	}
	scale := math.Max(svd.S[0], 1)
	if d := maxAbsDiff(svd.U.Mul(sigma).Mul(svd.V.T()), a); d > 1e-10*scale {
		t.Fatalf("U·S·Vᵀ differs from a by %g", d)
	}
	if d := maxAbsDiff(svd.V.T().Mul(svd.V), Identity(n)); d > 1e-10 {
		t.Fatalf("V is not orthogonal: %g", d)
	}
	return svd
}

func TestSVD(t *testing.T) {
	svd := checkSVD(t, MatFrom(3, 2, []float64{3, 0, 0, -2, 0, 0}))
	near(t, svd.S[0], 3, exactTol, "known singular value")
	near(t, svd.S[1], 2, exactTol, "known singular value")
	r := newRand(13)
	for _, shape := range [][2]int{{1, 1}, {3, 3}, {9, 9}, {12, 12}, {40, 12}, {8, 9}, {2, 5}, {3000, 12}} {
		a := randMat(r, shape[0], shape[1])
		svd := checkSVD(t, a)
		if shape[0] >= shape[1] {
			if d := maxAbsDiff(svd.U.T().Mul(svd.U), Identity(shape[1])); d > 1e-10 {
				t.Fatalf("%v: U columns are not orthonormal: %g", shape, d)
			}
		}
	}
}

// A DLT-style system: rows are orthogonal to a known vector, so the last
// column of V must recover it, including when there are fewer rows than columns.
func TestSVDNullSpace(t *testing.T) {
	const nullTol = 1e-9
	r := newRand(14)
	for _, rows := range []int{8, 9, 50, 2000} {
		const n = 9
		truth := randMat(r, n, 1).Data
		norm := 0.0
		for _, v := range truth {
			norm += v * v
		}
		norm = math.Sqrt(norm)
		a := randMat(r, rows, n)
		for i := 0; i < rows; i++ {
			dot := 0.0
			for j := 0; j < n; j++ {
				dot += a.At(i, j) * truth[j]
			}
			for j := 0; j < n; j++ { // project the row off the null vector
				a.Set(i, j, a.At(i, j)-dot*truth[j]/(norm*norm))
			}
		}
		svd := checkSVD(t, a)
		if svd.S[n-1] > nullTol*svd.S[0] {
			t.Fatalf("rows=%d: smallest singular value %g is not zero", rows, svd.S[n-1])
		}
		if svd.S[n-2] < 1e-3*svd.S[0] {
			t.Fatalf("rows=%d: null space is not one-dimensional: %v", rows, svd.S)
		}
		null := svd.V.Col(n - 1)
		dot := 0.0
		for j := range null {
			dot += null[j] * truth[j] / norm
		}
		near(t, math.Abs(dot), 1, nullTol, "null vector direction")
		if rows < n { // structurally rank-deficient: no arbitrary basis completion
			for _, v := range svd.U.Col(n - 1) {
				if v != 0 {
					t.Fatal("U column of a zero singular value must be zero")
				}
			}
		}
	}
}

func TestSVDErrors(t *testing.T) {
	if _, err := SVD(Mat{}); !errors.Is(err, ErrDimension) {
		t.Fatalf("empty: %v", err)
	}
	if _, err := SVD(MatFrom(1, 2, []float64{1, math.NaN()})); !errors.Is(err, ErrNonFinite) {
		t.Fatalf("NaN: %v", err)
	}
	svd := checkSVD(t, NewMat(4, 3)) // the zero matrix is valid: all singular values zero
	if svd.S[0] != 0 {
		t.Fatalf("zero matrix singular values: %v", svd.S)
	}
}

func TestLeastSquares(t *testing.T) {
	// y = 2 + 3t sampled exactly.
	a, b := NewMat(5, 2), make([]float64, 5)
	for i := range b {
		a.Set(i, 0, 1)
		a.Set(i, 1, float64(i))
		b[i] = 2 + 3*float64(i)
	}
	x, err := LeastSquares(a, b)
	if err != nil {
		t.Fatal(err)
	}
	near(t, x[0], 2, roundTol, "intercept")
	near(t, x[1], 3, roundTol, "slope")
	for i := 0; i < 5; i++ { // second column becomes a multiple of the first
		a.Set(i, 1, 4)
	}
	if _, err := LeastSquares(a, b); !errors.Is(err, ErrSingular) {
		t.Fatalf("rank-deficient: %v", err)
	}
	if _, err := LeastSquares(NewMat(1, 2), []float64{1}); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("underdetermined: %v", err)
	}
}
