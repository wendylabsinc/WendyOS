package posegraph

import (
	"math"
	"slices"
)

// pivotTol is the smallest Cholesky pivot accepted, relative to the diagonal
// entry of H it started from. That ratio is the information left on a state
// component once the others are accounted for, over its total: about 1e-8 for
// a long weakly anchored trajectory, rounding noise (below 1e-13) for a
// direction no edge observes.
const pivotTol = 1e-11

// system is the normal equations H·dx = −b of one linearisation, in the
// elimination order chosen by prepare. Every variable is a 3 x 3 block: a
// pose is (x, z, yaw), a landmark is (x, z) padded with a unit diagonal.
type system struct {
	n       int     // free variables
	nodeVar []int32 // block of each node, −1 when fixed or held for the gauge
	lmVar   []int32 // block of each landmark, −1 when held
	root    []int32 // connected component of each node; rootAbs when anchored
	rootAbs int32
	gauge   []NodeID
	dof     int

	// Column-compressed lower block structure of the factor: column k has
	// off-diagonal blocks colPtr[k]..colPtr[k+1], in rows rowIdx (ascending).
	// Block q holds 9 row-major values at 9q, rows indexed by the row variable.
	colPtr []int32
	rowIdx []int32
	// edgeA and edgeB are the blocks of each edge's two variables (−1 when
	// not free); edgeBlk is the off-diagonal block coupling them.
	edgeA, edgeB, edgeBlk []int32

	hd, hv []float64 // H: diagonal and off-diagonal blocks
	ld, lv []float64 // its factor L, same layout; ld holds lower-triangular blocks
	b      []float64 // Σ w·Jᵀ·Ω·r
}

// minDegree orders the variables of a symmetric sparsity graph for sparse
// elimination. adj[v] is v's sorted neighbour list and is consumed. It
// returns the elimination order and, for each variable, the variables
// eliminated later that its column of the factor reaches (its fill
// structure). Ties go to the lowest index, so the order is deterministic.
func minDegree(adj [][]int32) (order []int32, fill [][]int32) {
	n := len(adj)
	done := make([]bool, n)
	order = make([]int32, 0, n)
	fill = make([][]int32, n)
	for range n {
		// ponytail: linear scan for the minimum, O(n²) overall (0.1 s at
		// 10k variables). Use a degree-bucket queue beyond that.
		v := -1
		for i := range adj {
			if !done[i] && (v < 0 || len(adj[i]) < len(adj[v])) {
				v = i
			}
		}
		done[v] = true
		order = append(order, int32(v))
		fill[v] = adj[v]
		// Eliminating v makes its neighbours a clique.
		for _, u := range adj[v] {
			adj[u] = unionWithout(adj[u], adj[v], u, int32(v))
		}
	}
	return order, fill
}

// unionWithout merges two sorted lists, leaving out the values x and y.
func unionWithout(a, b []int32, x, y int32) []int32 {
	out := make([]int32, 0, len(a)+len(b))
	for len(a) > 0 || len(b) > 0 {
		var v int32
		switch {
		case len(b) == 0 || len(a) > 0 && a[0] < b[0]:
			v, a = a[0], a[1:]
		case len(a) == 0 || b[0] < a[0]:
			v, b = b[0], b[1:]
		default:
			v, a, b = a[0], a[1:], b[1:]
		}
		if v != x && v != y {
			out = append(out, v)
		}
	}
	return out
}

// structure orders the variables and allocates the factor. adj is the
// variable coupling graph in the caller's numbering; the result maps that
// numbering to elimination positions.
func (s *system) structure(adj [][]int32) (pos []int32) {
	for _, a := range adj {
		slices.Sort(a)
	}
	for i := range adj {
		adj[i] = slices.Compact(adj[i])
	}
	order, fill := minDegree(adj)
	s.n = len(order)
	pos = make([]int32, s.n)
	for p, v := range order {
		pos[v] = int32(p)
	}
	s.colPtr = make([]int32, s.n+1)
	for p, v := range order {
		for _, u := range fill[v] {
			s.rowIdx = append(s.rowIdx, pos[u])
		}
		slices.Sort(s.rowIdx[s.colPtr[p]:])
		s.colPtr[p+1] = int32(len(s.rowIdx))
	}
	s.hd, s.ld = make([]float64, 9*s.n), make([]float64, 9*s.n)
	s.hv, s.lv = make([]float64, 9*len(s.rowIdx)), make([]float64, 9*len(s.rowIdx))
	s.b = make([]float64, 3*s.n)
	return pos
}

// block returns the off-diagonal block coupling two distinct variables.
func (s *system) block(a, b int32) int32 {
	lo, hi := min(a, b), max(a, b)
	i, _ := slices.BinarySearch(s.rowIdx[s.colPtr[lo]:s.colPtr[lo+1]], hi)
	return s.colPtr[lo] + int32(i)
}

// factor computes L with L·Lᵀ = H + λ·diag(H) by right-looking block
// Cholesky. It reports false when a pivot falls to pivotTol of its diagonal
// entry: H is singular or indefinite to working precision.
func (s *system) factor(lambda float64) bool {
	copy(s.ld, s.hd)
	copy(s.lv, s.hv)
	for k := 0; k < s.n && lambda > 0; k++ {
		s.ld[9*k] *= 1 + lambda
		s.ld[9*k+4] *= 1 + lambda
		s.ld[9*k+8] *= 1 + lambda
	}
	for k := 0; k < s.n; k++ {
		d, h := s.ld[9*k:9*k+9], s.hd[9*k:9*k+9]
		if !(d[0] > pivotTol*h[0]) {
			return false
		}
		l00 := math.Sqrt(d[0])
		l10, l20 := d[3]/l00, d[6]/l00
		p11 := d[4] - l10*l10
		if !(p11 > pivotTol*h[4]) {
			return false
		}
		l11 := math.Sqrt(p11)
		l21 := (d[7] - l20*l10) / l11
		p22 := d[8] - l20*l20 - l21*l21
		if !(p22 > pivotTol*h[8]) {
			return false
		}
		l22 := math.Sqrt(p22)
		d[0], d[1], d[2], d[3], d[4], d[5], d[6], d[7], d[8] = l00, 0, 0, l10, l11, 0, l20, l21, l22

		lo, hi := s.colPtr[k], s.colPtr[k+1]
		// L_ik = A_ik · L_kk⁻ᵀ, one row at a time.
		for q := lo; q < hi; q++ {
			blk := s.lv[9*q : 9*q+9]
			for r := 0; r < 9; r += 3 {
				x0 := blk[r] / l00
				x1 := (blk[r+1] - x0*l10) / l11
				blk[r], blk[r+1], blk[r+2] = x0, x1, (blk[r+2]-x0*l20-x1*l21)/l22
			}
		}
		// Schur update of the trailing blocks: A_mi −= L_mk · L_ikᵀ. The rows
		// of column k below i are a subset of column i's rows, both ascending.
		for qa := lo; qa < hi; qa++ {
			i := s.rowIdx[qa]
			li := s.lv[9*qa : 9*qa+9]
			subMulT(s.ld[9*i:9*i+9], li, li)
			w := s.colPtr[i]
			for qb := qa + 1; qb < hi; qb++ {
				for s.rowIdx[w] != s.rowIdx[qb] {
					w++
				}
				subMulT(s.lv[9*w:9*w+9], s.lv[9*qb:9*qb+9], li)
			}
		}
	}
	return true
}

// subMulT computes dst −= a·bᵀ for row-major 3 x 3 blocks.
func subMulT(dst, a, b []float64) {
	_, _, _ = dst[8], a[8], b[8]
	for i := 0; i < 9; i += 3 {
		a0, a1, a2 := a[i], a[i+1], a[i+2]
		dst[i] -= a0*b[0] + a1*b[1] + a2*b[2]
		dst[i+1] -= a0*b[3] + a1*b[4] + a2*b[5]
		dst[i+2] -= a0*b[6] + a1*b[7] + a2*b[8]
	}
}

// solve overwrites x with the solution of L·Lᵀ·x = x.
func (s *system) solve(x []float64) {
	for k := 0; k < s.n; k++ {
		d, xk := s.ld[9*k:9*k+9], x[3*k:3*k+3]
		xk[0] /= d[0]
		xk[1] = (xk[1] - d[3]*xk[0]) / d[4]
		xk[2] = (xk[2] - d[6]*xk[0] - d[7]*xk[1]) / d[8]
		for q := s.colPtr[k]; q < s.colPtr[k+1]; q++ {
			i := s.rowIdx[q]
			l, xi := s.lv[9*q:9*q+9], x[3*i:3*i+3]
			xi[0] -= l[0]*xk[0] + l[1]*xk[1] + l[2]*xk[2]
			xi[1] -= l[3]*xk[0] + l[4]*xk[1] + l[5]*xk[2]
			xi[2] -= l[6]*xk[0] + l[7]*xk[1] + l[8]*xk[2]
		}
	}
	for k := s.n - 1; k >= 0; k-- {
		d, xk := s.ld[9*k:9*k+9], x[3*k:3*k+3]
		for q := s.colPtr[k]; q < s.colPtr[k+1]; q++ {
			i := s.rowIdx[q]
			l, xi := s.lv[9*q:9*q+9], x[3*i:3*i+3]
			xk[0] -= l[0]*xi[0] + l[3]*xi[1] + l[6]*xi[2]
			xk[1] -= l[1]*xi[0] + l[4]*xi[1] + l[7]*xi[2]
			xk[2] -= l[2]*xi[0] + l[5]*xi[1] + l[8]*xi[2]
		}
		xk[2] /= d[8]
		xk[1] = (xk[1] - d[7]*xk[2]) / d[4]
		xk[0] = (xk[0] - d[3]*xk[1] - d[6]*xk[2]) / d[0]
	}
}

// inverseColumns returns columns 3v..3v+2 of H⁻¹: three solves.
func (s *system) inverseColumns(v int32) [3][]float64 {
	var out [3][]float64
	for c := range out {
		x := make([]float64, 3*s.n)
		x[3*int(v)+c] = 1
		s.solve(x)
		out[c] = x
	}
	return out
}
