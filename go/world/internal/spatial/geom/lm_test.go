package geom

import (
	"errors"
	"math"
	"testing"
)

func TestLMRosenbrock(t *testing.T) {
	p := LMProblem{NumResiduals: 2, Residuals: func(x, r []float64) {
		r[0] = 10 * (x[1] - x[0]*x[0])
		r[1] = 1 - x[0]
	}}
	res, err := LevenbergMarquardt(p, []float64{-1.2, 1}, LMConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Converged {
		t.Fatal("not converged")
	}
	near(t, res.X[0], 1, 1e-7, "rosenbrock x")
	near(t, res.X[1], 1, 1e-7, "rosenbrock y")
	near(t, res.Cost, 0, 1e-12, "rosenbrock cost")
	near(t, res.Sigma2, 1, exactTol, "no redundancy means unit variance is assumed")
}

// A linear model has an exact covariance σ²·(AᵀA)⁻¹, which the estimate must
// reproduce with σ² taken from the residuals.
func TestLMLinearCovariance(t *testing.T) {
	const (
		n          = 400
		noiseSigma = 0.05
		sigmaTol   = 0.1 // relative error allowed on the recovered noise level
	)
	r := newRand(20)
	ts, ys := make([]float64, n), make([]float64, n)
	a := NewMat(n, 2)
	for i := range ts {
		ts[i] = float64(i) / n * 10
		ys[i] = 1.5 - 0.7*ts[i] + r.NormFloat64()*noiseSigma
		a.Set(i, 0, 1)
		a.Set(i, 1, ts[i])
	}
	p := LMProblem{NumResiduals: n, Residuals: func(x, res []float64) {
		for i := range res {
			res[i] = x[0] + x[1]*ts[i] - ys[i]
		}
	}}
	res, err := LevenbergMarquardt(p, []float64{0, 0}, LMConfig{})
	if err != nil {
		t.Fatal(err)
	}
	exact, err := LeastSquares(a, ys)
	if err != nil {
		t.Fatal(err)
	}
	near(t, res.X[0], exact[0], 1e-7, "matches linear least squares")
	near(t, res.X[1], exact[1], 1e-7, "matches linear least squares")
	near(t, math.Sqrt(res.Sigma2), noiseSigma, sigmaTol*noiseSigma, "recovered noise sigma")
	near(t, res.RMSE, noiseSigma, sigmaTol*noiseSigma, "rmse")
	want, err := Inverse(a.T().Mul(a))
	if err != nil {
		t.Fatal(err)
	}
	for i := range want.Data {
		near(t, res.Covariance.Data[i], want.Data[i]*res.Sigma2, 1e-6*math.Abs(want.Data[i]*res.Sigma2), "covariance")
	}
	// The truth lies within a few standard deviations of the estimate.
	near(t, res.X[0], 1.5, 4*math.Sqrt(res.Covariance.At(0, 0)), "intercept within 4 sigma")
	near(t, res.X[1], -0.7, 4*math.Sqrt(res.Covariance.At(1, 1)), "slope within 4 sigma")
}

func TestLMAnalyticJacobianMatchesNumeric(t *testing.T) {
	r := newRand(21)
	const n = 60
	ts, ys := make([]float64, n), make([]float64, n)
	for i := range ts {
		ts[i] = float64(i) * 0.1
		ys[i] = 2.5*math.Exp(-0.4*ts[i]) + 0.3 + r.NormFloat64()*0.01
	}
	p := LMProblem{NumResiduals: n, Residuals: func(x, res []float64) {
		for i := range res {
			res[i] = x[0]*math.Exp(x[1]*ts[i]) + x[2] - ys[i]
		}
	}}
	numeric, err := LevenbergMarquardt(p, []float64{1, -1, 0}, LMConfig{})
	if err != nil {
		t.Fatal(err)
	}
	p.Jacobian = func(x []float64, j Mat) {
		for i := range ts {
			e := math.Exp(x[1] * ts[i])
			j.Set(i, 0, e)
			j.Set(i, 1, x[0]*ts[i]*e)
			j.Set(i, 2, 1)
		}
	}
	analytic, err := LevenbergMarquardt(p, []float64{1, -1, 0}, LMConfig{})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []float64{2.5, -0.4, 0.3} {
		near(t, analytic.X[i], numeric.X[i], 1e-6, "analytic vs numeric Jacobian")
		near(t, analytic.X[i], want, 5*math.Sqrt(analytic.Covariance.At(i, i)), "truth within 5 sigma")
		near(t, analytic.Covariance.At(i, i), numeric.Covariance.At(i, i), 1e-4*numeric.Covariance.At(i, i), "covariance diagonal")
	}
}

func TestLMHuberRejectsOutliers(t *testing.T) {
	const (
		n          = 200
		noiseSigma = 0.02
		robustTol  = 0.02 // slope and intercept error with a fifth of the data corrupted
	)
	r := newRand(22)
	ts, ys := make([]float64, n), make([]float64, n)
	for i := range ts {
		ts[i] = float64(i) / n * 4
		ys[i] = 0.5 + 2*ts[i] + r.NormFloat64()*noiseSigma
		if i%5 == 0 {
			ys[i] += 5 + r.Float64()*5 // one-sided gross outliers
		}
	}
	p := LMProblem{NumResiduals: n, Residuals: func(x, res []float64) {
		for i := range res {
			res[i] = x[0] + x[1]*ts[i] - ys[i]
		}
	}}
	plain, err := LevenbergMarquardt(p, []float64{0, 0}, LMConfig{})
	if err != nil {
		t.Fatal(err)
	}
	robust, err := LevenbergMarquardt(p, []float64{0, 0}, LMConfig{HuberDelta: 3 * noiseSigma})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(plain.X[0]-0.5) < 0.5 {
		t.Fatalf("test is vacuous: plain least squares was not pulled by the outliers (%g)", plain.X[0])
	}
	near(t, robust.X[0], 0.5, robustTol, "robust intercept")
	near(t, robust.X[1], 2, robustTol, "robust slope")
	near(t, HuberWeight(0.5, 1), 1, exactTol, "weight inside the band")
	near(t, HuberWeight(-4, 1), 0.25, exactTol, "weight outside the band")
	near(t, HuberWeight(100, 0), 1, exactTol, "delta zero disables weighting")
}

func TestLMRefusals(t *testing.T) {
	two := func(x, r []float64) { r[0], r[1] = x[0]-1, x[0]-2 }
	if _, err := LevenbergMarquardt(LMProblem{NumResiduals: 1, Residuals: two}, []float64{0, 0}, LMConfig{}); !errors.Is(err, ErrInsufficient) {
		t.Fatalf("fewer residuals than parameters: %v", err)
	}
	if _, err := LevenbergMarquardt(LMProblem{NumResiduals: 2}, []float64{0}, LMConfig{}); !errors.Is(err, ErrDimension) {
		t.Fatalf("missing residual function: %v", err)
	}
	nan := func(x, r []float64) { r[0], r[1] = math.NaN(), 0 }
	if _, err := LevenbergMarquardt(LMProblem{NumResiduals: 2, Residuals: nan}, []float64{0}, LMConfig{}); !errors.Is(err, ErrNonFinite) {
		t.Fatalf("NaN residual: %v", err)
	}
	// x[1] never appears: the data cannot constrain it.
	unobservable := func(x, r []float64) { r[0], r[1], r[2] = x[0]-1, x[0]-1.1, x[0]-0.9 }
	res, err := LevenbergMarquardt(LMProblem{NumResiduals: 3, Residuals: unobservable}, []float64{0, 7}, LMConfig{})
	if !errors.Is(err, ErrSingular) {
		t.Fatalf("unobservable parameter: %v", err)
	}
	if res.Covariance.Data != nil {
		t.Fatal("a singular problem must not report a covariance")
	}
	// Two parameters that only appear as a sum are jointly unobservable.
	sum := func(x, r []float64) { r[0], r[1], r[2] = x[0]+x[1]-1, x[0]+x[1]-1.1, x[0]+x[1]-0.9 }
	if _, err := LevenbergMarquardt(LMProblem{NumResiduals: 3, Residuals: sum}, []float64{0, 0}, LMConfig{}); !errors.Is(err, ErrSingular) {
		t.Fatalf("gauge freedom: %v", err)
	}
	slow := LMProblem{NumResiduals: 2, Residuals: func(x, r []float64) {
		r[0] = 10 * (x[1] - x[0]*x[0])
		r[1] = 1 - x[0]
	}}
	res, err = LevenbergMarquardt(slow, []float64{-1.2, 1}, LMConfig{MaxIterations: 2})
	if !errors.Is(err, ErrNoConvergence) || res.Converged || res.Iterations != 2 || len(res.X) != 2 {
		t.Fatalf("iteration limit: err=%v res=%+v", err, res)
	}
}
