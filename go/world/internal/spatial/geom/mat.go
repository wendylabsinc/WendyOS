package geom

import (
	"math"
	"sort"
)

// Mat is a dense row-major matrix: element (i, j) is Data[i*Cols+j]. It is
// meant for small systems (tens of columns, a few thousand rows). Methods
// panic on mismatched dimensions, which is a programming error; numerical
// failures are returned as errors.
type Mat struct {
	Rows, Cols int
	Data       []float64
}

func NewMat(rows, cols int) Mat {
	if rows < 0 || cols < 0 {
		panic("geom: negative matrix dimension")
	}
	return Mat{rows, cols, make([]float64, rows*cols)}
}

// MatFrom copies row-major data into a rows x cols matrix.
func MatFrom(rows, cols int, data []float64) Mat {
	if len(data) != rows*cols {
		panic("geom: matrix data length mismatch")
	}
	m := NewMat(rows, cols)
	copy(m.Data, data)
	return m
}

func Identity(n int) Mat {
	m := NewMat(n, n)
	for i := 0; i < n; i++ {
		m.Data[i*n+i] = 1
	}
	return m
}

func (m Mat) At(i, j int) float64     { return m.Data[i*m.Cols+j] }
func (m Mat) Set(i, j int, v float64) { m.Data[i*m.Cols+j] = v }
func (m Mat) Clone() Mat              { return MatFrom(m.Rows, m.Cols, m.Data) }

// Row and Col return copies.
func (m Mat) Row(i int) []float64 {
	return append([]float64(nil), m.Data[i*m.Cols:(i+1)*m.Cols]...)
}

func (m Mat) Col(j int) []float64 {
	out := make([]float64, m.Rows)
	for i := range out {
		out[i] = m.Data[i*m.Cols+j]
	}
	return out
}

// T is the transpose.
func (m Mat) T() Mat {
	t := NewMat(m.Cols, m.Rows)
	for i := 0; i < m.Rows; i++ {
		for j := 0; j < m.Cols; j++ {
			t.Data[j*m.Rows+i] = m.Data[i*m.Cols+j]
		}
	}
	return t
}

func (m Mat) Mul(o Mat) Mat {
	if m.Cols != o.Rows {
		panic("geom: matrix product dimension mismatch")
	}
	r := NewMat(m.Rows, o.Cols)
	for i := 0; i < m.Rows; i++ {
		for k := 0; k < m.Cols; k++ {
			a := m.Data[i*m.Cols+k]
			if a == 0 {
				continue
			}
			for j := 0; j < o.Cols; j++ {
				r.Data[i*o.Cols+j] += a * o.Data[k*o.Cols+j]
			}
		}
	}
	return r
}

func (m Mat) MulVec(v []float64) []float64 {
	if m.Cols != len(v) {
		panic("geom: matrix-vector dimension mismatch")
	}
	out := make([]float64, m.Rows)
	for i := range out {
		s := 0.0
		for j, x := range v {
			s += m.Data[i*m.Cols+j] * x
		}
		out[i] = s
	}
	return out
}

// luPivotTol is the smallest acceptable pivot after row and column
// equilibration, so it bounds the condition number rather than the magnitude.
const luPivotTol = 1e-13

type luFactor struct {
	n          int
	lu         []float64
	perm       []int
	rowS, colS []float64
}

func factorLU(a Mat) (luFactor, error) {
	n := a.Rows
	if n == 0 || a.Cols != n || len(a.Data) != n*n {
		return luFactor{}, ErrDimension
	}
	if !finite(a.Data...) {
		return luFactor{}, ErrNonFinite
	}
	f := luFactor{n: n, lu: append([]float64(nil), a.Data...), perm: make([]int, n), rowS: make([]float64, n), colS: make([]float64, n)}
	lu := f.lu
	for i := 0; i < n; i++ {
		mx := 0.0
		for j := 0; j < n; j++ {
			mx = math.Max(mx, math.Abs(lu[i*n+j]))
		}
		if mx == 0 {
			return luFactor{}, ErrSingular
		}
		f.rowS[i] = 1 / mx
		for j := 0; j < n; j++ {
			lu[i*n+j] *= f.rowS[i]
		}
	}
	for j := 0; j < n; j++ {
		mx := 0.0
		for i := 0; i < n; i++ {
			mx = math.Max(mx, math.Abs(lu[i*n+j]))
		}
		if mx == 0 {
			return luFactor{}, ErrSingular
		}
		f.colS[j] = 1 / mx
		for i := 0; i < n; i++ {
			lu[i*n+j] *= f.colS[j]
		}
	}
	for k := 0; k < n; k++ {
		p, best := k, math.Abs(lu[k*n+k])
		for i := k + 1; i < n; i++ {
			if v := math.Abs(lu[i*n+k]); v > best {
				p, best = i, v
			}
		}
		if best <= luPivotTol {
			return luFactor{}, ErrSingular
		}
		f.perm[k] = p
		if p != k {
			for j := 0; j < n; j++ {
				lu[k*n+j], lu[p*n+j] = lu[p*n+j], lu[k*n+j]
			}
		}
		for i := k + 1; i < n; i++ {
			m := lu[i*n+k] / lu[k*n+k]
			lu[i*n+k] = m
			if m == 0 {
				continue
			}
			for j := k + 1; j < n; j++ {
				lu[i*n+j] -= m * lu[k*n+j]
			}
		}
	}
	return f, nil
}

func (f luFactor) solve(b []float64) []float64 {
	n, lu := f.n, f.lu
	x := make([]float64, n)
	for i := range x {
		x[i] = b[i] * f.rowS[i]
	}
	for k, p := range f.perm {
		x[k], x[p] = x[p], x[k]
	}
	for i := 1; i < n; i++ {
		for j := 0; j < i; j++ {
			x[i] -= lu[i*n+j] * x[j]
		}
	}
	for i := n - 1; i >= 0; i-- {
		for j := i + 1; j < n; j++ {
			x[i] -= lu[i*n+j] * x[j]
		}
		x[i] /= lu[i*n+i]
	}
	for j := range x {
		x[j] *= f.colS[j]
	}
	return x
}

// Solve returns x with a·x = b for square a, by LU with partial pivoting on
// the row- and column-equilibrated matrix. ErrSingular means the equilibrated
// system has a condition number beyond roughly 1e13.
func Solve(a Mat, b []float64) ([]float64, error) {
	if len(b) != a.Rows {
		return nil, ErrDimension
	}
	if !finite(b...) {
		return nil, ErrNonFinite
	}
	f, err := factorLU(a)
	if err != nil {
		return nil, err
	}
	return f.solve(b), nil
}

// Inverse returns a⁻¹ under the same conditioning rule as Solve.
func Inverse(a Mat) (Mat, error) {
	f, err := factorLU(a)
	if err != nil {
		return Mat{}, err
	}
	n := f.n
	inv := NewMat(n, n)
	e := make([]float64, n)
	for j := 0; j < n; j++ {
		e[j] = 1
		for i, v := range f.solve(e) {
			inv.Data[i*n+j] = v
		}
		e[j] = 0
	}
	return inv, nil
}

// Cholesky returns lower-triangular L with a = L·Lᵀ. Only the lower triangle
// of a is read. It returns ErrNotPositiveDefinite when a pivot falls to or
// below 1e-14 of its diagonal entry.
func Cholesky(a Mat) (Mat, error) {
	n := a.Rows
	if n == 0 || a.Cols != n || len(a.Data) != n*n {
		return Mat{}, ErrDimension
	}
	if !finite(a.Data...) {
		return Mat{}, ErrNonFinite
	}
	l := NewMat(n, n)
	for j := 0; j < n; j++ {
		d := a.Data[j*n+j]
		for k := 0; k < j; k++ {
			d -= l.Data[j*n+k] * l.Data[j*n+k]
		}
		if d <= 1e-14*math.Abs(a.Data[j*n+j]) || d <= 0 {
			return Mat{}, ErrNotPositiveDefinite
		}
		d = math.Sqrt(d)
		l.Data[j*n+j] = d
		for i := j + 1; i < n; i++ {
			s := a.Data[i*n+j]
			for k := 0; k < j; k++ {
				s -= l.Data[i*n+k] * l.Data[j*n+k]
			}
			l.Data[i*n+j] = s / d
		}
	}
	return l, nil
}

// SolveCholesky returns x with a·x = b for symmetric positive definite a.
func SolveCholesky(a Mat, b []float64) ([]float64, error) {
	if len(b) != a.Rows {
		return nil, ErrDimension
	}
	if !finite(b...) {
		return nil, ErrNonFinite
	}
	l, err := Cholesky(a)
	if err != nil {
		return nil, err
	}
	n := a.Rows
	x := append([]float64(nil), b...)
	for i := 0; i < n; i++ {
		for k := 0; k < i; k++ {
			x[i] -= l.Data[i*n+k] * x[k]
		}
		x[i] /= l.Data[i*n+i]
	}
	for i := n - 1; i >= 0; i-- {
		for k := i + 1; k < n; k++ {
			x[i] -= l.Data[k*n+i] * x[k]
		}
		x[i] /= l.Data[i*n+i]
	}
	return x, nil
}

// SymEigen decomposes a symmetric matrix by cyclic Jacobi rotations. Values
// are sorted in descending order and vecs holds the matching unit eigenvectors
// as columns: a·vecs = vecs·diag(values). The input must be symmetric to
// within 1e-9 of its largest entry.
func SymEigen(a Mat) (values []float64, vecs Mat, err error) {
	n := a.Rows
	if n == 0 || a.Cols != n || len(a.Data) != n*n {
		return nil, Mat{}, ErrDimension
	}
	if !finite(a.Data...) {
		return nil, Mat{}, ErrNonFinite
	}
	scale := 0.0
	for _, v := range a.Data {
		scale = math.Max(scale, math.Abs(v))
	}
	w := a.Clone()
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if math.Abs(w.Data[i*n+j]-w.Data[j*n+i]) > 1e-9*scale {
				return nil, Mat{}, ErrDimension
			}
			m := (w.Data[i*n+j] + w.Data[j*n+i]) / 2
			w.Data[i*n+j], w.Data[j*n+i] = m, m
		}
	}
	v := Identity(n)
	converged := false
	for sweep := 0; sweep < 100; sweep++ {
		off := 0.0
		for i := 0; i < n; i++ {
			for j := i + 1; j < n; j++ {
				off += w.Data[i*n+j] * w.Data[i*n+j]
			}
		}
		if math.Sqrt(off) <= 1e-15*scale*float64(n) {
			converged = true
			break
		}
		for p := 0; p < n-1; p++ {
			for q := p + 1; q < n; q++ {
				apq := w.Data[p*n+q]
				if apq == 0 {
					continue
				}
				theta := (w.Data[q*n+q] - w.Data[p*n+p]) / (2 * apq)
				t := 1 / (math.Abs(theta) + math.Sqrt(theta*theta+1))
				if theta < 0 {
					t = -t
				}
				c := 1 / math.Sqrt(t*t+1)
				s := t * c
				for k := 0; k < n; k++ {
					akp, akq := w.Data[k*n+p], w.Data[k*n+q]
					w.Data[k*n+p], w.Data[k*n+q] = c*akp-s*akq, s*akp+c*akq
				}
				for k := 0; k < n; k++ {
					apk, aqk := w.Data[p*n+k], w.Data[q*n+k]
					w.Data[p*n+k], w.Data[q*n+k] = c*apk-s*aqk, s*apk+c*aqk
				}
				for k := 0; k < n; k++ {
					vkp, vkq := v.Data[k*n+p], v.Data[k*n+q]
					v.Data[k*n+p], v.Data[k*n+q] = c*vkp-s*vkq, s*vkp+c*vkq
				}
			}
		}
	}
	if !converged {
		return nil, Mat{}, ErrNoConvergence
	}
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool { return w.Data[order[i]*n+order[i]] > w.Data[order[j]*n+order[j]] })
	values = make([]float64, n)
	vecs = NewMat(n, n)
	for c, src := range order {
		values[c] = w.Data[src*n+src]
		for i := 0; i < n; i++ {
			vecs.Data[i*n+c] = v.Data[i*n+src]
		}
	}
	return values, vecs, nil
}

// svdZeroTol is the singular value, relative to the Frobenius norm, at or
// below which SVD treats a direction as rank-deficient.
const svdZeroTol = 1e-15

// SVDResult holds a = U·diag(S)·Vᵀ for an m x n matrix: U is m x n, S has n
// non-negative values in descending order and V is a full n x n orthogonal
// matrix, so the last columns of V span the null space. U columns whose
// singular value is at or below svdZeroTol·‖a‖ (rank deficiency, or the
// trailing n−m values when m < n) are zero rather than an arbitrary basis
// completion.
type SVDResult struct {
	U Mat
	S []float64
	V Mat
}

// SVD computes the decomposition by one-sided Jacobi rotations, which keeps
// small singular values accurate enough for DLT and homography null spaces.
func SVD(a Mat) (SVDResult, error) {
	m, n := a.Rows, a.Cols
	if m == 0 || n == 0 || len(a.Data) != m*n {
		return SVDResult{}, ErrDimension
	}
	if !finite(a.Data...) {
		return SVDResult{}, ErrNonFinite
	}
	// Work on a/unit so that squared column norms neither overflow nor
	// underflow whatever the magnitude of the entries.
	unit := 0.0
	for _, x := range a.Data {
		unit = math.Max(unit, math.Abs(x))
	}
	if unit == 0 {
		unit = 1
	}
	rows := max(m, n) // zero-padded rows give a full V when m < n
	w := make([][]float64, n)
	v := make([][]float64, n)
	frob2 := 0.0
	for j := 0; j < n; j++ {
		w[j] = make([]float64, rows)
		for i := 0; i < m; i++ {
			w[j][i] = a.Data[i*n+j] / unit
			frob2 += w[j][i] * w[j][i]
		}
		v[j] = make([]float64, n)
		v[j][j] = 1
	}
	// Columns are orthogonal once their cosine is below the rounding error of
	// a dot product. A column whose norm falls below svdZeroTol·‖a‖ is zero at
	// working precision: it is left alone, since no rotation can make more
	// columns mutually orthogonal than the matrix has rank.
	relTol := math.Max(1e-15, 2.3e-16*math.Sqrt(float64(rows)))
	zero2 := svdZeroTol * svdZeroTol * frob2
	converged := false
	for sweep := 0; sweep < 100 && !converged; sweep++ {
		converged = true
		for p := 0; p < n-1; p++ {
			for q := p + 1; q < n; q++ {
				wp, wq := w[p], w[q]
				var alpha, beta, gamma float64
				for i := range wp {
					alpha += wp[i] * wp[i]
					beta += wq[i] * wq[i]
					gamma += wp[i] * wq[i]
				}
				if alpha <= zero2 || beta <= zero2 || math.Abs(gamma) <= relTol*math.Sqrt(alpha*beta) {
					continue
				}
				converged = false
				zeta := (beta - alpha) / (2 * gamma)
				t := 1 / (math.Abs(zeta) + math.Sqrt(1+zeta*zeta))
				if zeta < 0 {
					t = -t
				}
				c := 1 / math.Sqrt(1+t*t)
				s := c * t
				for i := range wp {
					wp[i], wq[i] = c*wp[i]-s*wq[i], s*wp[i]+c*wq[i]
				}
				vp, vq := v[p], v[q]
				for i := range vp {
					vp[i], vq[i] = c*vp[i]-s*vq[i], s*vp[i]+c*vq[i]
				}
			}
		}
	}
	if !converged {
		return SVDResult{}, ErrNoConvergence
	}
	sv := make([]float64, n)
	order := make([]int, n)
	for j := range w {
		for _, x := range w[j] {
			sv[j] += x * x
		}
		sv[j] = math.Sqrt(sv[j])
		order[j] = j
	}
	sort.SliceStable(order, func(i, j int) bool { return sv[order[i]] > sv[order[j]] })
	out := SVDResult{U: NewMat(m, n), S: make([]float64, n), V: NewMat(n, n)}
	floor := svdZeroTol * math.Sqrt(frob2)
	for c, src := range order {
		out.S[c] = sv[src] * unit
		for i := 0; i < n; i++ {
			out.V.Data[i*n+c] = v[src][i]
		}
		if sv[src] <= floor {
			continue
		}
		for i := 0; i < m; i++ {
			out.U.Data[i*n+c] = w[src][i] / sv[src]
		}
	}
	return out, nil
}

// LeastSquares returns the x minimising |a·x − b| for a with full column
// rank. A smallest singular value at or below 1e-12 of the largest is
// reported as ErrSingular instead of returning a minimum-norm guess.
func LeastSquares(a Mat, b []float64) ([]float64, error) {
	if len(b) != a.Rows {
		return nil, ErrDimension
	}
	if a.Rows < a.Cols {
		return nil, ErrInsufficient
	}
	if !finite(b...) {
		return nil, ErrNonFinite
	}
	svd, err := SVD(a)
	if err != nil {
		return nil, err
	}
	n := a.Cols
	if svd.S[n-1] <= 1e-12*svd.S[0] {
		return nil, ErrSingular
	}
	x := make([]float64, n)
	for c := 0; c < n; c++ {
		dot := 0.0
		for i := 0; i < a.Rows; i++ {
			dot += svd.U.Data[i*n+c] * b[i]
		}
		dot /= svd.S[c]
		for i := 0; i < n; i++ {
			x[i] += svd.V.Data[i*n+c] * dot
		}
	}
	return x, nil
}
