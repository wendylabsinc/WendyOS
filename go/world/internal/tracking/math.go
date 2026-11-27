package tracking

import "math"

func positive(c [4]float64) bool {
	return finite(c[:]...) && c[0] > 0 && c[3] > 0 && math.Abs(c[1]-c[2]) <= 1e-10 && finite(determinant(c)) && determinant(c) > 1e-18
}
func determinant(c [4]float64) float64 { return c[0]*c[3] - c[1]*c[2] }
func inverse(c [4]float64) [4]float64 {
	d := determinant(c)
	return [4]float64{c[3] / d, -c[1] / d, -c[2] / d, c[0] / d}
}
func multiply(c [4]float64, p [2]float64) [2]float64 {
	return [2]float64{c[0]*p[0] + c[1]*p[1], c[2]*p[0] + c[3]*p[1]}
}
func distance(a, b [2]float64) float64 { return math.Hypot(a[0]-b[0], a[1]-b[1]) }
func mahalanobis(a, b [2]float64, ac, bc [4]float64) float64 {
	var sum [4]float64
	for i := range sum {
		sum[i] = ac[i] + bc[i]
	}
	d := [2]float64{a[0] - b[0], a[1] - b[1]}
	v := multiply(inverse(sum), d)
	return d[0]*v[0] + d[1]*v[1]
}

// Covariance intersection makes no independent-error assumption. A fixed grid
// minimizes determinant; equal objectives prefer the balanced weight, then the
// lower weight. Member order is fixed before invoking this operation.
func intersection(a, b [2]float64, ac, bc [4]float64) ([2]float64, [4]float64) {
	ia, ib := inverse(ac), inverse(bc)
	va, vb := multiply(ia, a), multiply(ib, b)
	best, weight := math.Inf(1), .5
	var result [4]float64
	for n := 0; n <= 20; n++ {
		w := float64(n) / 20
		var precision [4]float64
		for i := range precision {
			precision[i] = w*ia[i] + (1-w)*ib[i]
		}
		cov := inverse(precision)
		det := determinant(cov)
		epsilon := 1e-12 * math.Max(1, math.Abs(best))
		if math.IsInf(best, 1) || det < best-epsilon || math.Abs(det-best) <= epsilon && math.Abs(w-.5) < math.Abs(weight-.5) {
			best, weight, result = det, w, cov
		}
	}
	v := [2]float64{weight*va[0] + (1-weight)*vb[0], weight*va[1] + (1-weight)*vb[1]}
	return multiply(result, v), result
}
