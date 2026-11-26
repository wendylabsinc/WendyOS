package geom

import (
	"errors"
	"math"
)

var (
	// ErrSingular reports a singular or numerically rank-deficient system.
	ErrSingular = errors.New("geom: singular or ill-conditioned system")
	// ErrNotPositiveDefinite reports a matrix that is not symmetric positive definite.
	ErrNotPositiveDefinite = errors.New("geom: matrix is not positive definite")
	// ErrDegenerate reports input geometry that leaves the answer undetermined
	// (coincident or collinear points, a zero-area polygon, a pixel outside the
	// invertible range of the distortion model).
	ErrDegenerate = errors.New("geom: degenerate configuration")
	// ErrInsufficient reports too few observations for the requested estimate.
	ErrInsufficient = errors.New("geom: insufficient data")
	// ErrDimension reports mismatched or empty dimensions.
	ErrDimension = errors.New("geom: dimension mismatch")
	// ErrNonFinite reports a NaN or infinite input or intermediate value.
	ErrNonFinite = errors.New("geom: non-finite value")
	// ErrNoConvergence reports an iteration that hit its limit.
	ErrNoConvergence = errors.New("geom: did not converge")
	// ErrNoConsensus reports a RANSAC run in which no model reached MinInliers.
	ErrNoConsensus = errors.New("geom: no consensus model")
)

func finite(values ...float64) bool {
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}
