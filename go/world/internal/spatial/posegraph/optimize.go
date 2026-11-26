package posegraph

import (
	"fmt"
	"math"
	"slices"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

const (
	minLambda = 1e-7 // keeps a damped pivot well above pivotTol
	maxLambda = 1e8  // beyond this a step is a vanishing gradient step
)

// EdgeStat is the fit of one edge at the returned estimate.
type EdgeStat struct {
	// Residual is estimate minus measurement in the edge's leading Dim
	// components: metres, metres and wrapped radians for a relative edge or
	// pose prior, metres otherwise.
	Residual [3]float64
	// Chi2 is rᵀ·Info·r without the robust kernel. For a correct edge it is
	// roughly chi-square with Dim degrees of freedom, so compare it with
	// geom.ChiSquareQuantile(Dim, p) to reject a false loop closure.
	Chi2 float64
	// Weight is the kernel's weight on the edge, in (0, 1]; 1 when the edge
	// is not robust or lies inside the kernel's quadratic region.
	Weight float64
	// Dim is the residual dimension, 0 for a removed edge.
	Dim int
}

// Result is the outcome of Optimize. The optimised poses are read from the
// graph with Pose and Landmark.
type Result struct {
	Iterations int // Levenberg-Marquardt steps taken, rejected ones included
	Converged  bool
	// Chi2 is Σ rᵀ·Info·r over the edges, without the robust kernel;
	// InitialChi2 is the same before the first step.
	Chi2, InitialChi2 float64
	// RobustChi2 is Σ ρ(χ²), the quantity that was minimised.
	RobustChi2 float64
	// DOF is the residual dimension minus the free state dimension. With
	// correct information matrices and no outliers Chi2/DOF is near 1.
	DOF int
	// Edges is indexed by EdgeID.
	Edges []EdgeStat
	// Gauge lists the nodes held still because their connected component has
	// no fixed node and no prior edge: such a component floats, and its
	// absolute pose is whatever the held node's estimate was.
	Gauge []NodeID
}

func errUnobservable() error {
	return fmt.Errorf("posegraph: the edges leave part of the state unobserved: %w", geom.ErrSingular)
}

// Optimize minimises the robust cost from the current estimates and leaves
// the result in the graph. Fixed nodes do not move. A connected component
// with no fixed node and no prior has its lowest node held instead (see
// Result.Gauge).
//
// It returns geom.ErrInsufficient for a graph without nodes; an error
// wrapping geom.ErrSingular, with the estimates left as they were, when the
// edges do not determine the state (for example one position prior, which
// leaves the rotation about it free, or ranges alone); and an error wrapping
// geom.ErrNoConvergence, with the best iterate kept and reported, when
// Config.MaxIterations is reached. No step ever raises the cost.
func (g *Graph) Optimize() (Result, error) {
	if len(g.est) == 0 {
		return Result{}, fmt.Errorf("posegraph: empty graph: %w", geom.ErrInsufficient)
	}
	g.sys = nil
	s := g.prepare()
	startEst, startLms := slices.Clone(g.est), slices.Clone(g.lms)
	restore := func(est []geom.Pose2, lms []geom.Vec2) {
		copy(g.est, est)
		copy(g.lms, lms)
	}
	res := Result{Gauge: s.gauge, DOF: s.dof, Converged: s.n == 0}
	cost := s.linearize(g)
	res.InitialChi2, _ = g.stats(nil)

	lambda, nu := g.cfg.InitialLambda, 2.0
	dx := make([]float64, 3*s.n)
	prevEst, prevLms := make([]geom.Pose2, len(g.est)), make([]geom.Vec2, len(g.lms))
	for !res.Converged && res.Iterations < g.cfg.MaxIterations && lambda <= maxLambda {
		res.Iterations++
		if !s.factor(lambda) {
			lambda *= 10
			continue
		}
		for i, v := range s.b {
			dx[i] = -v
		}
		s.solve(dx)
		// Decrease of the quadratic model F + 2·bᵀdx + dxᵀ·H·dx for the damped step.
		step, predicted := 0.0, 0.0
		for i, d := range dx {
			step = math.Max(step, math.Abs(d))
			predicted += d * (lambda*s.hd[9*(i/3)+4*(i%3)]*d - s.b[i])
		}
		copy(prevEst, g.est)
		copy(prevLms, g.lms)
		s.apply(g, dx)
		_, next := g.stats(nil)
		if !(next <= cost) {
			restore(prevEst, prevLms)
			lambda *= nu
			nu *= 2
			res.Converged = step <= g.cfg.StepTol
			continue
		}
		gain := 0.0
		if predicted > 0 {
			gain = (cost - next) / predicted
		}
		res.Converged = step <= g.cfg.StepTol || cost-next <= g.cfg.CostTol*cost
		cost = s.linearize(g)
		lambda = math.Max(lambda*math.Max(1.0/3, 1-math.Pow(2*gain-1, 3)), minLambda)
		nu = 2
	}
	// The undamped factor is the observability test and the source of covariances.
	if !s.factor(0) {
		restore(startEst, startLms)
		return Result{}, errUnobservable()
	}
	g.sys = s
	res.Edges = make([]EdgeStat, len(g.edges))
	res.Chi2, res.RobustChi2 = g.stats(res.Edges)
	g.mergeFrames(s)
	if !res.Converged {
		return res, fmt.Errorf("posegraph: stopped after %d iterations: %w", res.Iterations, geom.ErrNoConvergence)
	}
	return res, nil
}

// prepare finds the free variables, their elimination order and the factor
// structure for the current set of edges.
func (g *Graph) prepare() *system {
	nn, nl := len(g.est), len(g.lms)
	abs := int32(nn + nl) // virtual node: the graph frame itself
	parent := make([]int32, nn+nl+1)
	for i := range parent {
		parent[i] = int32(i)
	}
	find := func(i int32) int32 {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	// second returns the element an edge joins its From node to.
	second := func(e *Edge) int32 {
		switch e.Kind {
		case EdgeRelative, EdgeRange:
			return int32(e.To)
		case EdgeLandmark:
			return int32(nn) + int32(e.Landmark)
		}
		return abs
	}
	for i, fixed := range g.fixed {
		if fixed {
			parent[find(int32(i))] = find(abs)
		}
	}
	s := &system{dof: 0}
	for k := range g.edges {
		if !g.gone[k] {
			parent[find(int32(g.edges[k].From))] = find(second(&g.edges[k]))
			s.dof += g.edges[k].Kind.Dim()
		}
	}
	s.rootAbs = find(abs)

	// Number the free variables: nodes, then landmarks. The first variable of
	// a floating component is held to fix its gauge.
	held := make([]bool, len(parent))
	free := int32(0)
	number := func(i int32, fixed bool) int32 {
		r := find(i)
		if fixed || r != s.rootAbs && !held[r] {
			if !fixed {
				held[r] = true
				if int(i) < nn {
					s.gauge = append(s.gauge, NodeID(i))
				}
			}
			return -1
		}
		free++
		return free - 1
	}
	s.nodeVar, s.lmVar, s.root = make([]int32, nn), make([]int32, nl), make([]int32, nn)
	for i := range s.nodeVar {
		s.nodeVar[i] = number(int32(i), g.fixed[i])
		s.root[i] = find(int32(i))
		if s.nodeVar[i] >= 0 {
			s.dof -= 3
		}
	}
	for i := range s.lmVar {
		s.lmVar[i] = number(int32(nn+i), false)
		if s.lmVar[i] >= 0 {
			s.dof -= 2
		}
	}
	variable := func(el int32) int32 {
		switch {
		case el == abs:
			return -1
		case int(el) < nn:
			return s.nodeVar[el]
		}
		return s.lmVar[int(el)-nn]
	}

	adj := make([][]int32, free)
	s.edgeA, s.edgeB, s.edgeBlk = make([]int32, len(g.edges)), make([]int32, len(g.edges)), make([]int32, len(g.edges))
	for k := range g.edges {
		a, b := int32(-1), int32(-1)
		if !g.gone[k] {
			a, b = s.nodeVar[g.edges[k].From], variable(second(&g.edges[k]))
		}
		s.edgeA[k], s.edgeB[k] = a, b
		if a >= 0 && b >= 0 {
			adj[a] = append(adj[a], b)
			adj[b] = append(adj[b], a)
		}
	}
	pos := s.structure(adj)
	remap := func(vars []int32) {
		for i, v := range vars {
			if v >= 0 {
				vars[i] = pos[v]
			}
		}
	}
	remap(s.nodeVar)
	remap(s.lmVar)
	remap(s.edgeA)
	remap(s.edgeB)
	for k := range g.edges {
		s.edgeBlk[k] = -1
		if s.edgeA[k] >= 0 && s.edgeB[k] >= 0 {
			s.edgeBlk[k] = s.block(s.edgeA[k], s.edgeB[k])
		}
	}
	return s
}

// jacobian returns an edge's residual at the current estimate and its
// derivatives with respect to the From node (ja) and to the To node or
// landmark (jb). Rows are residual components; unused rows and columns are zero.
func (g *Graph) jacobian(e *Edge) (r geom.Vec3, ja, jb geom.Mat3) {
	p := g.est[e.From]
	switch e.Kind {
	case EdgePosePrior:
		return geom.Vec3{X: p.X - e.Pose.X, Y: p.Z - e.Pose.Z, Z: geom.AngleDiff(p.Yaw, e.Pose.Yaw)}, geom.Identity3(), jb
	case EdgePositionPrior:
		return geom.Vec3{X: p.X - e.Point.X, Y: p.Z - e.Point.Z}, geom.Mat3{0: 1, 4: 1}, jb
	case EdgeRange:
		d := g.est[e.To].Pos().Sub(p.Pos())
		n := d.Norm()
		u := geom.Vec2{X: 1} // coincident nodes: any direction is a subgradient
		if n > 1e-12 {
			u = d.Scale(1 / n)
		}
		return geom.Vec3{X: n - e.Distance}, geom.Mat3{0: -u.X, 1: -u.Z}, geom.Mat3{0: u.X, 1: u.Z}
	}
	// A point q seen from p: l = Rᵀ(yaw)·(q − p), ∂l/∂yaw = (l.Z, −l.X).
	q, m := g.est[e.To].Pos(), e.Pose.Pos()
	if e.Kind == EdgeLandmark {
		q, m = g.lms[e.Landmark], e.Point
	}
	s, c := math.Sincos(p.Yaw)
	l := p.InverseApply(q)
	r = geom.Vec3{X: l.X - m.X, Y: l.Z - m.Z}
	ja = geom.Mat3{-c, -s, l.Z, s, -c, -l.X}
	jb = geom.Mat3{c, s, 0, -s, c, 0}
	if e.Kind == EdgeRelative {
		r.Z = geom.WrapAngle(g.est[e.To].Yaw - p.Yaw - e.Pose.Yaw)
		ja[8], jb[8] = -1, 1
	}
	return r, ja, jb
}

// stats returns Σ χ² and Σ ρ(χ²) over the edges at the current estimate and
// fills per-edge statistics when edges is not nil.
func (g *Graph) stats(edges []EdgeStat) (chi2, robust float64) {
	for k := range g.edges {
		if g.gone[k] {
			continue
		}
		e := &g.edges[k]
		r, _, _ := g.jacobian(e)
		c := r.Dot(e.Info.MulVec(r))
		rho, w := g.cfg.kernel(e.Robust, c)
		chi2 += c
		robust += rho
		if edges != nil {
			edges[k] = EdgeStat{Residual: r.Array(), Chi2: c, Weight: w, Dim: e.Kind.Dim()}
		}
	}
	return chi2, robust
}

// linearize assembles H = Σ w·Jᵀ·Ω·J and b = Σ w·Jᵀ·Ω·r at the current
// estimate, with w the kernel weight of each edge, and returns Σ ρ(χ²).
func (s *system) linearize(g *Graph) float64 {
	clear(s.hd)
	clear(s.hv)
	clear(s.b)
	add := func(dst []float64, m geom.Mat3) {
		for i, v := range m {
			dst[i] += v
		}
	}
	cost := 0.0
	for k := range g.edges {
		if g.gone[k] {
			continue
		}
		e := &g.edges[k]
		r, ja, jb := g.jacobian(e)
		or := e.Info.MulVec(r)
		rho, w := g.cfg.kernel(e.Robust, r.Dot(or))
		cost += rho
		a, b := s.edgeA[k], s.edgeB[k]
		or = or.Scale(w)
		wja, wjb := e.Info.Mul(ja).Scale(w), e.Info.Mul(jb).Scale(w)
		if a >= 0 {
			add(s.hd[9*a:9*a+9], ja.T().Mul(wja))
			ga := ja.T().MulVec(or)
			s.b[3*a], s.b[3*a+1], s.b[3*a+2] = s.b[3*a]+ga.X, s.b[3*a+1]+ga.Y, s.b[3*a+2]+ga.Z
		}
		if b >= 0 {
			add(s.hd[9*b:9*b+9], jb.T().Mul(wjb))
			gb := jb.T().MulVec(or)
			s.b[3*b], s.b[3*b+1], s.b[3*b+2] = s.b[3*b]+gb.X, s.b[3*b+1]+gb.Y, s.b[3*b+2]+gb.Z
		}
		if q := s.edgeBlk[k]; q >= 0 {
			// The stored block is (later variable, earlier variable).
			if a < b {
				add(s.hv[9*q:9*q+9], jb.T().Mul(wja))
			} else {
				add(s.hv[9*q:9*q+9], ja.T().Mul(wjb))
			}
		}
	}
	for _, v := range s.lmVar {
		if v >= 0 {
			s.hd[9*v+8] = 1 // padding of the two-dimensional landmark state
		}
	}
	return cost
}

// apply adds a step to the free variables.
func (s *system) apply(g *Graph, dx []float64) {
	for i, v := range s.nodeVar {
		if v >= 0 {
			p := &g.est[i]
			p.X, p.Z, p.Yaw = p.X+dx[3*v], p.Z+dx[3*v+1], geom.WrapAngle(p.Yaw+dx[3*v+2])
		}
	}
	for i, v := range s.lmVar {
		if v >= 0 {
			g.lms[i].X, g.lms[i].Z = g.lms[i].X+dx[3*v], g.lms[i].Z+dx[3*v+1]
		}
	}
}

// mergeFrames records that trajectories in one connected component, or
// anchored to the graph frame by fixed nodes or priors, now share a frame.
func (g *Graph) mergeFrames(s *system) {
	lead := map[int32]int{} // component → first trajectory in it; lookups only
	for i := range g.trajs {
		r := s.root[g.trajs[i].nodes[0]]
		if first, ok := lead[r]; ok {
			g.relabel(g.trajs[i].frame, g.trajs[first].frame)
		} else {
			lead[r] = i
		}
	}
}

// factored returns the undamped factorisation at the current estimate.
func (g *Graph) factored() (*system, error) {
	if g.sys == nil {
		s := g.prepare()
		s.linearize(g)
		if !s.factor(0) {
			return nil, errUnobservable()
		}
		g.sys = s
	}
	return g.sys, nil
}

// Marginal returns the covariance of a node's pose over (x, z, yaw) in the
// graph frame: the node's block of H⁻¹ for the Gauss-Newton approximation
// H = Σ w·Jᵀ·Ω·J at the current estimate. It is meaningful after Optimize.
//
// Approximations: first order; the information matrices are taken at face
// value (no rescaling by Chi2/DOF); robust weights are frozen at their current
// values; and the result is conditional on fixed and gauge-held nodes, which
// have zero covariance. In a floating component it is therefore the
// uncertainty relative to the held node. Each call costs three sparse solves.
func (g *Graph) Marginal(id NodeID) (geom.Mat3, error) {
	if !g.hasNode(id) {
		return geom.Mat3{}, fmt.Errorf("%w: node %d", ErrUnknownID, id)
	}
	s, err := g.factored()
	if err != nil {
		return geom.Mat3{}, err
	}
	var cov geom.Mat3
	if v := s.nodeVar[id]; v >= 0 {
		for c, col := range s.inverseColumns(v) {
			for r := 0; r < 3; r++ {
				cov[r*3+c] = col[3*int(v)+r]
			}
		}
	}
	return cov, nil
}
