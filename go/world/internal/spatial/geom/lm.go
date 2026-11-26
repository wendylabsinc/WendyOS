package geom

import (
	"fmt"
	"math"
)

// LMConfig tunes LevenbergMarquardt. Zero fields take the documented default.
type LMConfig struct {
	MaxIterations int     // Default 100 outer iterations.
	InitialLambda float64 // Default 1e-3, relative Marquardt damping.
	GradientTol   float64 // Default 1e-10: stop when max |Jᵀ·W·r| is at or below it.
	StepTol       float64 // Default 1e-10: stop when |dx| ≤ StepTol·(|x|+StepTol).
	CostTol       float64 // Default 1e-12: stop when the relative cost decrease is at or below it.
	HuberDelta    float64 // Residual units. 0 disables robust weighting.
	DiffStep      float64 // Default 1e-6: relative central-difference step of the numeric Jacobian.
}

func (c LMConfig) withDefaults() LMConfig {
	if c.MaxIterations <= 0 {
		c.MaxIterations = 100
	}
	if c.InitialLambda <= 0 {
		c.InitialLambda = 1e-3
	}
	if c.GradientTol <= 0 {
		c.GradientTol = 1e-10
	}
	if c.StepTol <= 0 {
		c.StepTol = 1e-10
	}
	if c.CostTol <= 0 {
		c.CostTol = 1e-12
	}
	if c.DiffStep <= 0 {
		c.DiffStep = 1e-6
	}
	return c
}

// LMProblem is a least-squares problem min Σ ρ(r_i(x)).
type LMProblem struct {
	// NumResiduals is the fixed length of the residual vector.
	NumResiduals int
	// Residuals fills r (length NumResiduals) for parameters x. It must not
	// retain either slice.
	Residuals func(x, r []float64)
	// Jacobian optionally fills j (NumResiduals x len(x)) with ∂r_i/∂x_j. When
	// nil a central-difference Jacobian is used.
	Jacobian func(x []float64, j Mat)
}

// LMResult is the outcome of LevenbergMarquardt.
type LMResult struct {
	X          []float64
	Residuals  []float64 // r(X), unweighted
	Cost       float64   // Σ ρ(r_i): ½r² inside the Huber band, δ(|r|−δ/2) outside
	RMSE       float64   // sqrt(mean r²), unweighted
	Iterations int
	Converged  bool
	// Sigma2 is the weighted residual variance Σ w·r²/(m−n). With no redundancy
	// (m == n) it is 1, i.e. the residuals are assumed to have unit variance.
	Sigma2 float64
	// Covariance is (Jᵀ·W·J)⁻¹·Sigma2, n x n. It is empty when the normal matrix
	// is singular, in which case the call also returns ErrSingular.
	Covariance Mat
}

const (
	lmMaxLambda = 1e12
	lmMinLambda = 1e-12
	// lmDiagFloor is the damping added to every diagonal entry of JᵀWJ,
	// relative to the largest one, on top of Marquardt's relative damping.
	lmDiagFloor = 1e-12
)

func huberCost(r []float64, delta float64) float64 {
	cost := 0.0
	for _, v := range r {
		a := math.Abs(v)
		if delta > 0 && a > delta {
			cost += delta * (a - delta/2)
		} else {
			cost += v * v / 2
		}
	}
	return cost
}

// HuberWeight is the IRLS weight of a residual: 1 inside the band, δ/|r|
// outside. δ ≤ 0 disables weighting.
func HuberWeight(r, delta float64) float64 {
	if a := math.Abs(r); delta > 0 && a > delta {
		return delta / a
	}
	return 1
}

func lmJacobian(p LMProblem, x []float64, jac Mat, step float64, rp, rm []float64) {
	if p.Jacobian != nil {
		for i := range jac.Data {
			jac.Data[i] = 0
		}
		p.Jacobian(x, jac)
		return
	}
	n := len(x)
	for j := 0; j < n; j++ {
		orig := x[j]
		h := step * math.Max(1, math.Abs(orig))
		x[j] = orig + h
		p.Residuals(x, rp)
		x[j] = orig - h
		p.Residuals(x, rm)
		x[j] = orig
		for i := range rp {
			jac.Data[i*n+j] = (rp[i] - rm[i]) / (2 * h)
		}
	}
}

// lmNormal fills a = Jᵀ·W·J and g = Jᵀ·W·r and returns Σ w·r².
func lmNormal(jac Mat, r []float64, delta float64, a Mat, g []float64) float64 {
	n := jac.Cols
	for i := range a.Data {
		a.Data[i] = 0
	}
	for i := range g {
		g[i] = 0
	}
	wss := 0.0
	for i, ri := range r {
		w := HuberWeight(ri, delta)
		wss += w * ri * ri
		row := jac.Data[i*n : (i+1)*n]
		for j, jj := range row {
			if jj == 0 {
				continue
			}
			g[j] += w * jj * ri
			for k := j; k < n; k++ {
				a.Data[j*n+k] += w * jj * row[k]
			}
		}
	}
	for j := 0; j < n; j++ {
		for k := j + 1; k < n; k++ {
			a.Data[k*n+j] = a.Data[j*n+k]
		}
	}
	return wss
}

// LevenbergMarquardt minimises Σ ρ(r_i(x)) from x0, where ρ is the square
// loss or, with HuberDelta > 0, the Huber loss applied by iteratively
// reweighted least squares.
//
// It returns ErrInsufficient when there are fewer residuals than parameters,
// ErrNonFinite when the residuals are not finite at x0, ErrNoConvergence when
// MaxIterations is reached (the best iterate is still returned), and an error
// wrapping ErrSingular when the normal matrix at the solution cannot be
// inverted, meaning some parameter combination is not observable from the
// data (the iterate is still returned, without a covariance). A parameter the
// residuals do not depend on at all keeps its starting value while the others
// are optimised.
func LevenbergMarquardt(p LMProblem, x0 []float64, cfg LMConfig) (LMResult, error) {
	n, m := len(x0), p.NumResiduals
	if p.Residuals == nil || n == 0 || m <= 0 {
		return LMResult{}, ErrDimension
	}
	if m < n {
		return LMResult{}, ErrInsufficient
	}
	cfg = cfg.withDefaults()
	x := append([]float64(nil), x0...)
	r := make([]float64, m)
	p.Residuals(x, r)
	if !finite(x...) || !finite(r...) {
		return LMResult{}, ErrNonFinite
	}
	cost := huberCost(r, cfg.HuberDelta)
	jac, a := NewMat(m, n), NewMat(n, n)
	g, negG := make([]float64, n), make([]float64, n)
	trial, rt, rs := make([]float64, n), make([]float64, m), make([]float64, m)
	lambda := cfg.InitialLambda
	res := LMResult{}
	for res.Iterations < cfg.MaxIterations && !res.Converged {
		res.Iterations++
		lmJacobian(p, x, jac, cfg.DiffStep, rt, rs)
		if !finite(jac.Data...) {
			return LMResult{}, ErrNonFinite
		}
		lmNormal(jac, r, cfg.HuberDelta, a, g)
		gmax := 0.0
		for i, v := range g {
			gmax = math.Max(gmax, math.Abs(v))
			negG[i] = -v
		}
		if gmax <= cfg.GradientTol {
			res.Converged = true
			break
		}
		diagMax := 0.0
		for i := 0; i < n; i++ {
			diagMax = math.Max(diagMax, a.Data[i*n+i])
		}
		accepted := false
		for lambda <= lmMaxLambda && !accepted {
			// The absolute term keeps the damped system solvable when a parameter
			// has a zero Jacobian column: that parameter then stays put while the
			// observable ones are optimised.
			damped := a.Clone()
			for i := 0; i < n; i++ {
				damped.Data[i*n+i] += lambda * (a.Data[i*n+i] + lmDiagFloor*diagMax)
			}
			used := lambda
			lambda *= 10
			dx, err := Solve(damped, negG)
			if err != nil {
				continue
			}
			for i := range trial {
				trial[i] = x[i] + dx[i]
			}
			p.Residuals(trial, rt)
			if !finite(rt...) {
				continue
			}
			c := huberCost(rt, cfg.HuberDelta)
			if c >= cost {
				continue
			}
			var step2, x2 float64
			for i := range dx {
				step2 += dx[i] * dx[i]
				x2 += x[i] * x[i]
			}
			// Under heavy damping a short step says nothing about the distance to
			// the minimum, so the step and cost tests apply only when lambda ≤ 1.
			if c == 0 || (used <= 1 && (cost-c <= cfg.CostTol*cost || math.Sqrt(step2) <= cfg.StepTol*(math.Sqrt(x2)+cfg.StepTol))) {
				res.Converged = true
			}
			copy(x, trial)
			copy(r, rt)
			cost = c
			lambda = math.Max(used/10, lmMinLambda)
			accepted = true
		}
		if !accepted {
			// No damping level reduces the cost: a minimum to working precision, or
			// an unobservable direction that the covariance step below reports.
			res.Converged = true
		}
	}
	res.X, res.Residuals, res.Cost = x, r, cost
	ss := 0.0
	for _, v := range r {
		ss += v * v
	}
	res.RMSE = math.Sqrt(ss / float64(m))
	lmJacobian(p, x, jac, cfg.DiffStep, rt, rs)
	wss := lmNormal(jac, r, cfg.HuberDelta, a, g)
	res.Sigma2 = 1
	if m > n {
		res.Sigma2 = wss / float64(m-n)
	}
	inv, err := Inverse(a)
	if err != nil {
		return res, fmt.Errorf("levenberg-marquardt: parameters not observable: %w", ErrSingular)
	}
	for i := 0; i < n; i++ {
		for j := i; j < n; j++ {
			v := (inv.Data[i*n+j] + inv.Data[j*n+i]) / 2 * res.Sigma2
			inv.Data[i*n+j], inv.Data[j*n+i] = v, v
		}
	}
	res.Covariance = inv
	if !res.Converged {
		return res, ErrNoConvergence
	}
	return res, nil
}
