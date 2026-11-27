package geom

import (
	"math/rand/v2"
	"sort"
)

// RansacConfig tunes Ransac. Threshold has no default: it is the inlier bound
// in the caller's residual units and depends on the sensor noise.
type RansacConfig struct {
	SampleSize int     // Points per minimal sample. Required.
	Threshold  float64 // A point is an inlier when |residual| ≤ Threshold. Required.
	Iterations int     // Default 200 random samples.
	MinInliers int     // Default SampleSize+1: smallest consensus accepted.
}

// RansacResult is the best model and its consensus set.
type RansacResult[M any] struct {
	Model   M
	Inliers []int // ascending indices into the caller's data
	Fits    int   // samples for which fit succeeded
}

// Ransac searches n data points for the model with the largest consensus.
// fit builds a model from the given indices and must accept any count of at
// least SampleSize (it is called once more on the final inlier set); it
// returns an error for a degenerate sample, which is skipped. residual returns
// the model's error at point i. Ties are broken by the lower sum of squared
// inlier residuals, so the result depends only on the inputs and the rng
// state.
//
// It returns ErrInsufficient when n < SampleSize and ErrNoConsensus when no
// model reaches MinInliers.
func Ransac[M any](n int, cfg RansacConfig, rng *rand.Rand, fit func(idx []int) (M, error), residual func(model M, i int) float64) (RansacResult[M], error) {
	var best RansacResult[M]
	if cfg.SampleSize < 1 || !(cfg.Threshold > 0) || rng == nil || fit == nil || residual == nil {
		return best, ErrDimension
	}
	if n < cfg.SampleSize {
		return best, ErrInsufficient
	}
	if cfg.Iterations <= 0 {
		cfg.Iterations = 200
	}
	if cfg.MinInliers <= 0 {
		cfg.MinInliers = cfg.SampleSize + 1
	}
	consensus := func(m M) ([]int, float64) {
		var in []int
		ss := 0.0
		for i := 0; i < n; i++ {
			if r := residual(m, i); r >= -cfg.Threshold && r <= cfg.Threshold {
				in = append(in, i)
				ss += r * r
			}
		}
		return in, ss
	}
	perm := make([]int, n)
	for i := range perm {
		perm[i] = i
	}
	sample := make([]int, cfg.SampleSize)
	bestSS, fits := 0.0, 0
	for it := 0; it < cfg.Iterations; it++ {
		for i := 0; i < cfg.SampleSize; i++ {
			j := i + rng.IntN(n-i)
			perm[i], perm[j] = perm[j], perm[i]
		}
		copy(sample, perm[:cfg.SampleSize])
		sort.Ints(sample)
		m, err := fit(sample)
		if err != nil {
			continue
		}
		fits++
		in, ss := consensus(m)
		if len(in) > len(best.Inliers) || (len(in) == len(best.Inliers) && len(in) > 0 && ss < bestSS) {
			best.Model, best.Inliers, bestSS = m, in, ss
		}
	}
	best.Fits = fits
	if len(best.Inliers) < cfg.MinInliers {
		return RansacResult[M]{Fits: fits}, ErrNoConsensus
	}
	// Refit on the consensus set; keep the refit only if it does not lose support.
	if m, err := fit(best.Inliers); err == nil {
		if in, _ := consensus(m); len(in) >= len(best.Inliers) {
			best.Model, best.Inliers = m, in
		}
	}
	return best, nil
}
