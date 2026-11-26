// Package scanmatch estimates planar poses from 2D lidar scans: local
// refinement against an occupancy grid, global correlative search with
// branch and bound, and scan-to-scan matching by point-to-line ICP.
//
// Every pose is the pose of the SCAN frame (the scanner) in the reference
// frame: the map frame for scan-to-map, the reference scan's frame for
// scan-to-scan. Callers apply their own sensor-to-base calibration.
//
// Every estimate carries a covariance over (x, z, yaw), a score in [0, 1],
// an inlier fraction and a degeneracy indicator. A scan that does not
// constrain the answer is reported, not guessed at: too few points, an empty
// map, a score below Config.MinScore, insufficient overlap and several
// equally good poses are typed errors, and a direction the scan cannot
// observe is flagged on the Result and given Config.UnobservedSigma in the
// covariance.
//
// A nil error is not the whole verdict. Callers must also read
// Result.Converged (the refinement stopped by itself) and Result.Degenerate
// (some direction is not determined by the scan). For a Degenerate result
// the covariance is right as a matrix but its diagonal is not a set of
// per-axis sigmas: the unobserved direction is rarely an exact axis, so its
// huge variance shows up in every marginal. Use the whole matrix: its
// inverse (geom.Mat3.Inverse) is the information of a pose-graph edge, with
// next to nothing along the unobserved direction.
package scanmatch

import (
	"errors"
	"fmt"
	"math"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
)

var (
	// ErrConfig reports a Config or Window outside its documented ranges.
	ErrConfig = errors.New("scanmatch: invalid config")
	// ErrInsufficient reports a scan with fewer than Config.MinPoints usable
	// returns.
	ErrInsufficient = errors.New("scanmatch: too few scan points")
	// ErrEmptyMap reports a grid without occupied cells.
	ErrEmptyMap = errors.New("scanmatch: map has no occupied cells")
	// ErrNoMatch reports that no pose reaches Config.MinScore. A Result
	// returned with it describes the rejected pose.
	ErrNoMatch = errors.New("scanmatch: no pose reaches the minimum score")
	// ErrAmbiguous reports a search in which a distinct pose scores within
	// Config.AmbiguityRatio of the best. The SearchResult is fully populated.
	ErrAmbiguous = errors.New("scanmatch: distinct poses match equally well")
	// ErrNoOverlap reports two scans that share less than Config.MinOverlap
	// of their points. The Result describes the rejected alignment.
	ErrNoOverlap = errors.New("scanmatch: scans do not overlap enough")
)

// Config holds every threshold and noise assumption. A zero field takes its
// default; DefaultConfig lists them. To switch a floor or threshold off, set
// it to a tiny positive value instead of zero.
type Config struct {
	// Sigma is the width in metres of the kernel exp(−d²/2σ²) that scores a
	// scan point at distance d from the map (scan-to-map) or from the
	// reference line (scan-to-scan). It must cover range noise plus half a
	// search step. Default 0.10.
	Sigma float64
	// MaxPoints caps the returns used from one scan; more are subsampled
	// evenly by beam index. Default 360.
	MaxPoints int
	// MinPoints is the fewest usable returns a scan may have. Default 30.
	MinPoints int
	// InlierDistance is the residual in metres below which a point counts as
	// matched. Default 0.10.
	InlierDistance float64
	// HuberDelta is the residual in metres beyond which a point's influence
	// stops growing. Default 0.05.
	HuberDelta float64
	// MaxIterations bounds one local refinement. Default 50.
	MaxIterations int
	// MinScore is the lowest score accepted as a match. Default 0.4, which
	// admits a scan of which half is unmapped or changed. In a mapped, static
	// scene a correct match scores above 0.9 and a refinement that settled in
	// a wrong local minimum below 0.8, so a tracker that never expects large
	// changes can set 0.8 and have those refused.
	MinScore float64
	// AngularStep is the heading step in radians of the correlative search.
	// Zero derives it per scan as Sigma divided by the farthest return, so
	// one step moves no point by more than Sigma, but not below 0.002.
	AngularStep float64
	// Depth is the number of coarse levels above the cell lattice in the
	// branch-and-bound search; the coarsest block is 2^Depth cells wide.
	// Default 6, maximum 11.
	Depth int
	// AmbiguityRatio is the score, relative to the best pose, at or above
	// which a distinct pose makes a search ambiguous. Default 0.9: a rival is
	// dismissed only when a tenth of the scan contradicts it.
	AmbiguityRatio float64
	// DistinctTranslation (metres) and DistinctYaw (radians) separate two
	// poses into distinct candidates: a rival differs from every better
	// candidate by more than one of them. Defaults 0.5 and 0.35.
	DistinctTranslation float64
	DistinctYaw         float64
	// MaxRivals bounds the rivals reported by a search. Default 3.
	MaxRivals int
	// DegenerateEigen is the eigenvalue of the per-point information matrix
	// Σ nnᵀ / N over (x, z, L·yaw), n the surface normal at each matched
	// point and L the RMS range of the points, below which a direction
	// counts as unobserved. A direction every point constrains has
	// eigenvalue near 1; a corridor's long axis has near 0. Default 0.03.
	DegenerateEigen float64
	// UnobservedSigma is the 1σ in metres assigned in the covariance to an
	// unobserved direction. Default 100.
	UnobservedSigma float64
	// TranslationFloor (metres) and YawFloor (radians) are 1σ systematic
	// errors added to every scan-to-map covariance: map cell quantisation
	// does not average down with the number of beams. Defaults 0.01 (a fifth
	// of the default cell) and 0.001.
	TranslationFloor float64
	YawFloor         float64
	// ScanTranslationFloor and ScanYawFloor are the same for scan-to-scan
	// matching, which has no cell quantisation: what remains is correlated
	// error of the fitted lines. Defaults 0.002 and 0.0005.
	ScanTranslationFloor float64
	ScanYawFloor         float64
	// MaxCorrespondence is the farthest in metres a scan-to-scan point may
	// be from its nearest reference point to be paired. Default 0.5.
	MaxCorrespondence float64
	// MinOverlap is the inlier fraction below which a scan-to-scan match is
	// refused. Default 0.5.
	MinOverlap float64
	// NormalRadius is the radius in metres of the neighbourhood, contiguous
	// in beam order, from which a reference point's line is fitted.
	// Default 0.25.
	NormalRadius float64
	// Grid configures the local grid ScanToScanSearch builds from the
	// reference scan.
	Grid grid.Config
}

// DefaultConfig returns the defaults documented on Config.
func DefaultConfig() Config {
	return Config{
		Sigma: 0.10, MaxPoints: 360, MinPoints: 30, InlierDistance: 0.10, HuberDelta: 0.05, MaxIterations: 50,
		MinScore: 0.4, Depth: 6, AmbiguityRatio: 0.9, DistinctTranslation: 0.5, DistinctYaw: 0.35, MaxRivals: 3,
		DegenerateEigen: 0.03, UnobservedSigma: 100, TranslationFloor: 0.01, YawFloor: 0.001,
		ScanTranslationFloor: 0.002, ScanYawFloor: 0.0005,
		MaxCorrespondence: 0.5, MinOverlap: 0.5, NormalRadius: 0.25,
	}
}

func (c Config) withDefaults() (Config, error) {
	d := DefaultConfig()
	floats := []struct {
		v   *float64
		def float64
	}{
		{&c.Sigma, d.Sigma}, {&c.InlierDistance, d.InlierDistance}, {&c.HuberDelta, d.HuberDelta}, {&c.MinScore, d.MinScore},
		{&c.AmbiguityRatio, d.AmbiguityRatio}, {&c.DistinctTranslation, d.DistinctTranslation}, {&c.DistinctYaw, d.DistinctYaw},
		{&c.DegenerateEigen, d.DegenerateEigen}, {&c.UnobservedSigma, d.UnobservedSigma}, {&c.TranslationFloor, d.TranslationFloor},
		{&c.YawFloor, d.YawFloor}, {&c.ScanTranslationFloor, d.ScanTranslationFloor}, {&c.ScanYawFloor, d.ScanYawFloor},
		{&c.MaxCorrespondence, d.MaxCorrespondence}, {&c.MinOverlap, d.MinOverlap}, {&c.NormalRadius, d.NormalRadius},
	}
	for _, f := range floats {
		if *f.v == 0 {
			*f.v = f.def
		}
		if !(*f.v > 0) || math.IsInf(*f.v, 0) {
			return c, fmt.Errorf("%w: fields must be positive and finite", ErrConfig)
		}
	}
	for _, f := range []struct {
		v   *int
		def int
	}{{&c.MaxPoints, d.MaxPoints}, {&c.MinPoints, d.MinPoints}, {&c.MaxIterations, d.MaxIterations}, {&c.Depth, d.Depth}, {&c.MaxRivals, d.MaxRivals}} {
		if *f.v == 0 {
			*f.v = f.def
		}
		if *f.v < 0 {
			return c, fmt.Errorf("%w: counts must be positive", ErrConfig)
		}
	}
	switch {
	case c.MinPoints < 3 || c.MaxPoints < c.MinPoints:
		return c, fmt.Errorf("%w: need 3 ≤ MinPoints ≤ MaxPoints", ErrConfig)
	case c.Depth >= grid.MaxPyramidLevels:
		return c, fmt.Errorf("%w: Depth above %d", ErrConfig, grid.MaxPyramidLevels-1)
	case c.MinScore >= 1 || c.AmbiguityRatio > 1 || c.MinOverlap > 1:
		return c, fmt.Errorf("%w: MinScore, AmbiguityRatio and MinOverlap are fractions", ErrConfig)
	case !(c.AngularStep >= 0) || math.IsInf(c.AngularStep, 0):
		return c, fmt.Errorf("%w: AngularStep", ErrConfig)
	}
	return c, nil
}

// Result is one pose estimate with its quality measures.
type Result struct {
	// Pose is the scan frame in the reference frame.
	Pose geom.Pose2
	// Covariance is over (x, z, yaw) in m², m·rad and rad²: the residual
	// variance times the inverse information, with UnobservedSigma along
	// unobserved directions, plus the systematic floors. When Degenerate is
	// set its diagonal is not per-axis sigmas; see the package comment.
	Covariance geom.Mat3
	// Score is the mean of exp(−d²/2σ²) over the used points, in [0, 1].
	Score float64
	// InlierFraction is the share of used points with a residual below
	// InlierDistance.
	InlierFraction float64
	// RMSE is the root mean square inlier residual in metres.
	RMSE float64
	// Points is the number of scan points used.
	Points int
	// Iterations and Converged describe the local refinement. A result that
	// did not converge within Config.MaxIterations is returned without an
	// error and must not be trusted.
	Iterations int
	Converged  bool
	// MinEigen is the smallest eigenvalue of the per-point information
	// matrix (see Config.DegenerateEigen) and Weak its unit eigenvector over
	// (x, z, L·yaw): the direction the scan constrains least. The matrix is
	// built from the normals of the surfaces the scan points lie on.
	MinEigen float64
	Weak     [3]float64
	// Degenerate is set when MinEigen is below Config.DegenerateEigen: the
	// pose is not determined along Weak and must not be trusted there.
	// Along a direction with next to no constraint, such as the axis of a
	// corridor, scan-to-scan matching leaves the pose at the guess, and
	// scan-to-map matching settles wherever the map raster happens to hold it.
	Degenerate bool
}

// line is the straight surface fitted through the neighbourhood of one
// return, in the scan frame.
type line struct {
	anchor geom.Vec2 // centroid of the neighbourhood, a point on the line
	normal geom.Vec2 // unit normal; zero when the neighbourhood is not a line
}

// maxNeighbourBeams bounds the returns taken on each side of a point when
// fitting its line.
const maxNeighbourBeams = 64

// fitLines fits a line to the neighbourhood of every return of s: the
// returns contiguous in beam order within NormalRadius of it. A return with
// fewer than two such neighbours, or whose neighbourhood is not straight to
// within InlierDistance/2 (corners, clutter), gets no line.
func (c Config) fitLines(s spatial.Scan) (pts []geom.Vec2, lines []line) {
	pts = s.Points()
	n := len(pts)
	lines = make([]line, n)
	wrap := math.Abs(s.AngleIncrement)*(float64(len(s.Ranges))+0.5) >= 2*math.Pi
	for i := range pts {
		var sum geom.Vec2
		var sxx, sxz, szz float64
		count := 0
		add := func(q geom.Vec2) {
			d := q.Sub(pts[i]) // relative to the point, for precision
			sum = sum.Add(d)
			sxx, sxz, szz = sxx+d.X*d.X, sxz+d.X*d.Z, szz+d.Z*d.Z
			count++
		}
		add(pts[i])
		for _, dir := range []int{-1, 1} {
			for k := 1; k <= maxNeighbourBeams && k < n; k++ {
				j := i + dir*k
				if wrap {
					j = (j%n + n) % n
				} else if j < 0 || j >= n {
					break
				}
				if !(pts[j].Dist(pts[i]) <= c.NormalRadius) {
					break
				}
				add(pts[j])
			}
		}
		if count < 3 {
			continue
		}
		m := sum.Scale(1 / float64(count))
		cov := geom.Cov2{sxx/float64(count) - m.X*m.X, sxz/float64(count) - m.X*m.Z, sxz/float64(count) - m.X*m.Z, szz/float64(count) - m.Z*m.Z}
		major, minor, yaw := cov.Eigen()
		if !(major > 0) || math.Sqrt(math.Max(minor, 0)) > c.InlierDistance/2 {
			continue
		}
		lines[i] = line{anchor: pts[i].Add(m), normal: geom.Heading(yaw).Perp()}
	}
	return pts, lines
}

// scanPoints returns up to MaxPoints returns of the scan in its own frame,
// evenly subsampled by beam order, each with the normal of its fitted line
// (zero without one), and the RMS and largest range among them.
func (c Config) scanPoints(s spatial.Scan) (pts, normals []geom.Vec2, rms, far float64) {
	all, lines := c.fitLines(s)
	stride := max((len(all)+c.MaxPoints-1)/c.MaxPoints, 1)
	sum := 0.0
	for i := 0; i < len(all); i += stride {
		p := all[i]
		if n := p.Norm2(); !math.IsInf(n, 0) && !math.IsNaN(n) {
			pts, normals = append(pts, p), append(normals, lines[i].normal)
			sum += n
			far = math.Max(far, math.Sqrt(n))
		}
	}
	if len(pts) > 0 {
		rms = math.Sqrt(sum / float64(len(pts)))
	}
	return pts, normals, rms, far
}

// residuals reports, for a candidate pose, one residual per matched scan
// point with two gradients over (x, z, yaw): j steers the optimisation, and
// info is the constraint the point really provides, along the normal of the
// surface it lies on (zero when that is unknown). Points that have no
// counterpart are not reported.
type residuals func(pose geom.Pose2, emit func(r float64, j, info [3]float64))

// problem is a robust least-squares pose fit over the points of one scan.
type problem struct {
	cfg    Config
	res    residuals
	points int
	// scale is the RMS point range in metres. Yaw is solved as scale·yaw so
	// the three parameters share a unit and the information matrix is
	// comparable across directions.
	scale float64
	// sigma2Floor is the least residual variance to assume (m²).
	sigma2Floor float64
	// translationFloor and yawFloor are the 1σ systematic errors added to
	// the covariance.
	translationFloor, yawFloor float64
}

// normal accumulates the Huber-weighted normal equations and cost at pose,
// and the sum of the weights.
func (p *problem) normal(pose geom.Pose2) (h geom.Mat3, g [3]float64, cost, weight float64) {
	delta := p.cfg.HuberDelta
	p.res(pose, func(r float64, j, _ [3]float64) {
		j[2] /= p.scale
		w := geom.HuberWeight(r, delta)
		weight += w
		if a := math.Abs(r); a > delta {
			cost += delta * (a - delta/2)
		} else {
			cost += r * r / 2
		}
		for a := 0; a < 3; a++ {
			g[a] += w * j[a] * r
			for b := 0; b < 3; b++ {
				h[a*3+b] += w * j[a] * j[b]
			}
		}
	})
	return h, g, cost, weight
}

// frozenEigenFraction is the share of Config.DegenerateEigen below which ICP
// does not step along a direction at all. Mid-iteration pairings are poor, so
// the bar for moving is an order of magnitude below the bar for trusting the
// result: a corridor's axis sits another order of magnitude lower still.
const frozenEigenFraction = 0.1

// observedStep is the Gauss-Newton step restricted to the directions the
// matched points constrain: those whose eigenvalue of h, per unit of weight,
// reaches frozenEigenFraction·Config.DegenerateEigen. Along any other the
// normal equations hold only noise, and a step would be arbitrary.
func (p *problem) observedStep(h geom.Mat3, g [3]float64, lambda, weight float64) (step geom.Vec3, ok bool) {
	vals, vecs, err := geom.SymEigen(h.Mat())
	if err != nil {
		return step, false
	}
	for i, val := range vals {
		if !(val >= frozenEigenFraction*p.cfg.DegenerateEigen*weight) || !(val > 0) {
			continue
		}
		v := geom.Vec3{X: vecs.At(0, i), Y: vecs.At(1, i), Z: vecs.At(2, i)}
		step = step.Add(v.Scale(-(g[0]*v.X + g[1]*v.Y + g[2]*v.Z) / (val * (1 + lambda))))
		ok = true
	}
	return step, ok
}

// stepTolerance ends a refinement once a step moves the pose by less than
// this many metres (yaw measured at the RMS point range).
const stepTolerance = 1e-6

// solve runs damped Gauss-Newton from pose. With monotone set a step must
// lower the cost, which holds for a fixed objective such as the likelihood
// field. ICP re-pairs points every step, takes each step as it comes, and
// moves only along the directions its points constrain, so the pose stays at
// the guess along a corridor instead of drifting with the noise.
func (p *problem) solve(pose geom.Pose2, monotone bool) (geom.Pose2, int, bool) {
	h, g, cost, weight := p.normal(pose)
	lambda := 1e-4
	for it := 1; it <= p.cfg.MaxIterations; it++ {
		var step geom.Vec3
		if monotone {
			a := h
			trace := h[0] + h[4] + h[8]
			for d := 0; d < 9; d += 4 {
				a[d] += lambda * (h[d] + 1e-6*trace)
			}
			inv, err := a.Inverse()
			if err != nil { // no point constrains the pose at all
				return pose, it, false
			}
			step = inv.MulVec(geom.Vec3{X: -g[0], Y: -g[1], Z: -g[2]})
		} else {
			var ok bool
			if step, ok = p.observedStep(h, g, lambda, weight); !ok { // no direction is constrained
				return pose, it, false
			}
		}
		next := geom.Pose2{X: pose.X + step.X, Z: pose.Z + step.Y, Yaw: geom.WrapAngle(pose.Yaw + step.Z/p.scale)}
		nh, ng, ncost, nweight := p.normal(next)
		small := step.Norm() < stepTolerance
		if monotone && !(ncost < cost) {
			if lambda *= 10; small || lambda > 1e10 {
				return pose, it, true
			}
			continue
		}
		pose, h, g, cost, weight = next, nh, ng, ncost, nweight
		lambda = math.Max(lambda/10, 1e-9)
		if small {
			return pose, it, true
		}
	}
	return pose, p.cfg.MaxIterations, false
}

// evaluate scores pose and derives its covariance and degeneracy.
func (p *problem) evaluate(pose geom.Pose2) Result {
	cfg := p.cfg
	out := Result{Pose: pose.Normalized(), Points: p.points}
	var h geom.Mat3
	var score, ss float64
	inliers := 0
	k := -1 / (2 * cfg.Sigma * cfg.Sigma)
	p.res(pose, func(r float64, _, j [3]float64) {
		score += math.Exp(k * r * r)
		if math.Abs(r) >= cfg.InlierDistance {
			return
		}
		inliers++
		ss += r * r
		j[2] /= p.scale
		w := geom.HuberWeight(r, cfg.HuberDelta)
		for a := 0; a < 3; a++ {
			for b := 0; b < 3; b++ {
				h[a*3+b] += w * j[a] * j[b]
			}
		}
	})
	n := float64(p.points)
	out.Score, out.InlierFraction = score/n, float64(inliers)/n
	unobserved := cfg.UnobservedSigma * cfg.UnobservedSigma
	variance := [3]float64{unobserved, unobserved, unobserved}
	vecs := geom.Identity(3)
	out.Degenerate, out.Weak = true, [3]float64{1, 0, 0}
	if inliers > 0 {
		out.RMSE = math.Sqrt(ss / float64(inliers))
		sigma2 := math.Max(ss/float64(max(inliers-3, 1)), p.sigma2Floor)
		if vals, v, err := geom.SymEigen(h.Mat()); err == nil {
			vecs = v
			out.MinEigen = math.Max(vals[2], 0) / float64(inliers)
			out.Weak = [3]float64{v.At(0, 2), v.At(1, 2), v.At(2, 2)}
			out.Degenerate = out.MinEigen < cfg.DegenerateEigen
			for i, val := range vals {
				if val/float64(inliers) >= cfg.DegenerateEigen {
					variance[i] = math.Min(sigma2/val, unobserved)
				}
			}
		}
	}
	// Σ varᵢ·vᵢvᵢᵀ over (x, z, scale·yaw), then back to (x, z, yaw).
	unit := [3]float64{1, 1, 1 / p.scale}
	for a := 0; a < 3; a++ {
		for b := 0; b < 3; b++ {
			sum := 0.0
			for i := 0; i < 3; i++ {
				sum += variance[i] * vecs.At(a, i) * vecs.At(b, i)
			}
			out.Covariance[a*3+b] = sum * unit[a] * unit[b]
		}
	}
	out.Covariance[0] += p.translationFloor * p.translationFloor
	out.Covariance[4] += p.translationFloor * p.translationFloor
	out.Covariance[8] += p.yawFloor * p.yawFloor
	return out
}

// run refines guess and evaluates the outcome.
func (p *problem) run(guess geom.Pose2, monotone bool) Result {
	pose, iterations, converged := p.solve(guess, monotone)
	out := p.evaluate(pose)
	out.Iterations, out.Converged = iterations, converged
	return out
}

func finitePose(p geom.Pose2) bool {
	return !math.IsNaN(p.X+p.Z+p.Yaw) && !math.IsInf(p.X+p.Z+p.Yaw, 0)
}

// distinct reports whether two poses differ by more than the configured
// separation in position or in heading.
func (c Config) distinct(a, b geom.Pose2) bool {
	return math.Max(math.Abs(a.X-b.X), math.Abs(a.Z-b.Z)) > c.DistinctTranslation || math.Abs(geom.AngleDiff(a.Yaw, b.Yaw)) > c.DistinctYaw
}
