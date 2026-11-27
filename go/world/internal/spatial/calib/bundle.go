package calib

import (
	"fmt"
	"math"
	"sort"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// Shared parameters of a bundle, in the order of Estimate.Params and of the
// covariance. The rotation is a right perturbation of the optical-to-site
// rotation, R·exp(δ), so its covariance is in the optical frame.
const (
	parFX = iota
	parFY
	parCX
	parCY
	parK1
	parK2
	parRot              // 3 entries, radians
	parPos = parRot + 3 // 3 entries: camera centre, metres
	parDT  = parPos + 3 // sensor clock minus site time, seconds

	numShared = parDT + 1

	maxBlock = 9 // largest per-group block: offset, drift and spin
	// unprojectable is the residual, in sigmas, charged to a point that falls
	// behind the camera or outside the distortion model during the search.
	unprojectable = 1e3
)

var (
	freeIntrinsics = []int{parFX, parFY, parCX, parCY, parK1, parK2}
	freePose       = []int{parRot, parRot + 1, parRot + 2, parPos, parPos + 1, parPos + 2}
)

// camera is the shared state of a bundle.
type camera struct {
	in   geom.Intrinsics
	pose geom.Pose3 // optical frame to world
	dt   float64    // sensor clock minus site time, seconds
}

// point is one pixel observation of a known point.
type point struct {
	px geom.Pixel
	w  float64   // 1/sigma of each pixel axis
	p  geom.Vec3 // model point (rigid group) or world point at the frame's timestamp
	v  geom.Vec3 // world velocity of p, m/s: the point at site time is p − v·dt
	// tau is the point's time within its group, from −1 to 1, for groups whose
	// offset drifts.
	tau float64
	// l is the point's lever arm, in world axes, from the origin of the body
	// that carries it, for groups whose body may be turned (spin).
	l geom.Vec3
}

// group is a set of observations that share one block of nuisance parameters.
type group struct {
	// rigid: the points are in a model frame placed in the OPTICAL frame by a
	// free pose (a target view). Otherwise they are world points.
	rigid  bool
	points []point
	// prior, for world points, is the information matrix of a common unknown
	// offset of the points (metres, world axes): the localization error of the
	// robot that carried them, or a landmark's survey error. Nil means the
	// points are exact.
	prior *geom.Mat3
	// drift, with prior, lets the offset change linearly in time: a point's
	// offset is o + d·tau, and drift is the information matrix of d.
	drift *geom.Mat3
	// spin, with prior, lets the carrying body be turned by a small rotation
	// vector w (radians, world axes) about its origin: a point moves by w × l.
	// It is the information matrix of w: the heading error of a robot, the
	// orientation error of a surveyed landmark.
	spin *geom.Mat3
}

// dim is the size of the group's block: a pose (rigid), or the offset
// followed by the drift and the spin where present.
func (g *group) dim() int {
	switch {
	case g.rigid:
		return 6
	case g.prior == nil:
		return 0
	}
	n := 3
	if g.drift != nil {
		n += 3
	}
	if g.spin != nil {
		n += 3
	}
	return n
}

// state is everything a bundle optimises.
type state struct {
	cam    camera
	poses  []geom.Pose3 // per group: model to optical (rigid groups)
	offs   []geom.Vec3  // per group: world offset (groups with a prior)
	drifts []geom.Vec3  // per group: change of the offset per unit tau
	spins  []geom.Vec3  // per group: rotation vector of the carrying body
}

// newState is the starting state for n groups around a camera: zero offsets;
// the caller fills the poses of rigid groups.
func newState(cam camera, n int) state {
	return state{cam: cam, poses: make([]geom.Pose3, n), offs: make([]geom.Vec3, n), drifts: make([]geom.Vec3, n), spins: make([]geom.Vec3, n)}
}

func (s state) clone() state {
	return state{s.cam, append([]geom.Pose3(nil), s.poses...), append([]geom.Vec3(nil), s.offs...), append([]geom.Vec3(nil), s.drifts...), append([]geom.Vec3(nil), s.spins...)}
}

// bundle is the least-squares problem behind every estimator of the package.
type bundle struct {
	groups []group
	free   []int   // free shared parameters, ascending
	huber  float64 // Huber band of a point's residual norm, in sigmas
	iters  int
	// absolute: the pixel sigmas are trusted (priors in metres are weighed
	// against them), so the covariance is only ever inflated by the fit.
	absolute bool
	// lens, when set, is what an earlier solution knew about the intrinsics.
	// It needs all six of them free.
	lens *lensPrior
}

// numLens is the number of intrinsic parameters: parFX to parK2.
const numLens = parK2 + 1

// lensPrior is a Gaussian prior on the intrinsics: their mean and
// information matrix in the order fx, fy, cx, cy, k1, k2.
type lensPrior struct {
	mean [numLens]float64
	info [numLens][numLens]float64
}

// pull returns the gradient of the prior's cost at in, info·(in − mean), and
// twice that cost.
func (l *lensPrior) pull(in *geom.Intrinsics) (grad [numLens]float64, chi2 float64) {
	d := [numLens]float64{in.FX - l.mean[0], in.FY - l.mean[1], in.CX - l.mean[2], in.CY - l.mean[3], in.K1 - l.mean[4], in.K2 - l.mean[5]}
	for i := range grad {
		for j := range d {
			grad[i] += l.info[i][j] * d[j]
		}
		chi2 += d[i] * grad[i]
	}
	return grad, chi2
}

type bundleResult struct {
	state      state
	cov        geom.Mat  // over free, scaled by the fit
	chi2       float64   // reduced chi-square of the weighted residuals
	dist       []float64 // pixel distance of every point, in group order
	iterations int
}

func skew(a geom.Vec3) geom.Mat3 { return geom.Mat3{0, -a.Z, a.Y, a.Z, 0, -a.X, -a.Y, a.X, 0} }

// residual is the weighted pixel residual of one point and, when js is set,
// its derivatives with respect to the shared parameters and the group block.
func (b *bundle) residual(gi int, p *point, st *state, js *[2][numShared]float64, jl *[2][maxBlock]float64) (r [2]float64, ok bool) {
	g := &b.groups[gi]
	var q geom.Vec3
	rt := st.cam.pose.R.T()
	if g.rigid {
		q = st.poses[gi].Apply(p.p)
	} else {
		q = rt.MulVec(p.p.Add(st.offs[gi]).Add(st.drifts[gi].Scale(p.tau)).Add(st.spins[gi].Cross(p.l)).Sub(p.v.Scale(st.cam.dt)).Sub(st.cam.pose.T))
	}
	in := &st.cam.in
	px, ok := in.Project(q)
	if !ok {
		return r, false
	}
	r = [2]float64{(px.U - p.px.U) * p.w, (px.V - p.px.V) * p.w}
	if js == nil {
		return r, true
	}
	x, y := q.X/q.Z, q.Y/q.Z
	r2 := x*x + y*y
	d, e := 1+in.K1*r2+in.K2*r2*r2, in.K1+2*in.K2*r2
	*js, *jl = [2][numShared]float64{}, [2][maxBlock]float64{}
	js[0][parFX], js[0][parCX], js[0][parK1], js[0][parK2] = x*d*p.w, p.w, in.FX*x*r2*p.w, in.FX*x*r2*r2*p.w
	js[1][parFY], js[1][parCY], js[1][parK1], js[1][parK2] = y*d*p.w, p.w, in.FY*y*r2*p.w, in.FY*y*r2*r2*p.w
	// Pixel with respect to the optical-frame point, through (x, y).
	a := [2][2]float64{{in.FX * (d + 2*x*x*e), in.FX * 2 * x * y * e}, {in.FY * 2 * x * y * e, in.FY * (d + 2*y*y*e)}}
	var dq [2]geom.Vec3
	for i := range dq {
		dq[i] = geom.Vec3{X: a[i][0], Y: a[i][1], Z: -(a[i][0]*x + a[i][1]*y)}.Scale(p.w / q.Z)
	}
	// row·M for a row vector: Mᵀ·row.
	left := func(row geom.Vec3, m geom.Mat3) geom.Vec3 { return m.T().MulVec(row) }
	for i := range dq {
		if g.rigid {
			// q = R·exp(δ)·p + t: ∂q/∂δ = −R·[p]×, ∂q/∂t = I.
			rot := left(dq[i], st.poses[gi].R.Mul(skew(p.p))).Scale(-1)
			jl[i] = [maxBlock]float64{rot.X, rot.Y, rot.Z, dq[i].X, dq[i].Y, dq[i].Z}
			continue
		}
		// q = exp(−δ)·Rᵀ·(p + o + d·tau + w×l − v·dt − c):
		// ∂q/∂δ = [q]×, ∂q/∂c = −Rᵀ, ∂q/∂o = Rᵀ, ∂q/∂d = tau·Rᵀ and, since
		// a·(w×l) = w·(l×a), a row a of Rᵀ contributes l×a to ∂/∂w.
		rot, pos := left(dq[i], skew(q)), left(dq[i], rt)
		js[i][parRot], js[i][parRot+1], js[i][parRot+2] = rot.X, rot.Y, rot.Z
		js[i][parPos], js[i][parPos+1], js[i][parPos+2] = -pos.X, -pos.Y, -pos.Z
		js[i][parDT] = -pos.Dot(p.v)
		if g.prior != nil {
			jl[i][0], jl[i][1], jl[i][2] = pos.X, pos.Y, pos.Z
			k := 3
			if g.drift != nil {
				jl[i][k], jl[i][k+1], jl[i][k+2] = p.tau*pos.X, p.tau*pos.Y, p.tau*pos.Z
				k += 3
			}
			if g.spin != nil {
				w := p.l.Cross(pos)
				jl[i][k], jl[i][k+1], jl[i][k+2] = w.X, w.Y, w.Z
			}
		}
	}
	return r, true
}

// loss is the Huber loss of a point's residual norm and its IRLS weight.
func (b *bundle) loss(r [2]float64, ok bool) (cost, weight float64) {
	e := unprojectable
	if ok {
		e = math.Hypot(r[0], r[1])
	}
	if e <= b.huber {
		return e * e / 2, 1
	}
	return b.huber * (e - b.huber/2), b.huber / e
}

func priorCost(w *geom.Mat3, o geom.Vec3) float64 { return o.Dot(w.MulVec(o)) / 2 }

func (b *bundle) cost(st *state) float64 {
	total := 0.0
	if b.lens != nil {
		_, chi2 := b.lens.pull(&st.cam.in)
		total += chi2 / 2
	}
	for gi := range b.groups {
		g := &b.groups[gi]
		for i := range g.points {
			r, ok := b.residual(gi, &g.points[i], st, nil, nil)
			c, _ := b.loss(r, ok)
			total += c
		}
		if g.prior != nil {
			total += priorCost(g.prior, st.offs[gi])
			if g.drift != nil {
				total += priorCost(g.drift, st.drifts[gi])
			}
			if g.spin != nil {
				total += priorCost(g.spin, st.spins[gi])
			}
		}
	}
	return total
}

// blockNormal is one group's part of the normal equations.
type blockNormal struct {
	n int
	d [maxBlock][maxBlock]float64  // JlᵀJl (+ prior)
	g [maxBlock]float64            // Jlᵀr (+ prior gradient)
	c [numShared][maxBlock]float64 // JsᵀJl
}

type normal struct {
	a      [numShared][numShared]float64
	g      [numShared]float64
	blocks []blockNormal
	wss    float64 // Σ w·r² including priors
	rows   int     // residual rows including priors
}

func (b *bundle) linearize(st *state) *normal {
	nm := &normal{blocks: make([]blockNormal, len(b.groups))}
	var js [2][numShared]float64
	var jl [2][maxBlock]float64
	for gi := range b.groups {
		g := &b.groups[gi]
		bl := &nm.blocks[gi]
		bl.n = g.dim()
		for pi := range g.points {
			r, ok := b.residual(gi, &g.points[pi], st, &js, &jl)
			if !ok {
				continue
			}
			_, w := b.loss(r, true)
			nm.wss += w * (r[0]*r[0] + r[1]*r[1])
			nm.rows += 2
			for i := 0; i < 2; i++ {
				for fi, j := range b.free {
					v := w * js[i][j]
					if v == 0 {
						continue
					}
					nm.g[j] += v * r[i]
					for _, k := range b.free[fi:] { // upper triangle; mirrored below
						nm.a[j][k] += v * js[i][k]
					}
					for k := 0; k < bl.n; k++ {
						bl.c[j][k] += v * jl[i][k]
					}
				}
				for j := 0; j < bl.n; j++ {
					v := w * jl[i][j]
					bl.g[j] += v * r[i]
					for k := 0; k < bl.n; k++ {
						bl.d[j][k] += v * jl[i][k]
					}
				}
			}
		}
		addPrior := func(w *geom.Mat3, o geom.Vec3, at int) {
			grad := w.MulVec(o).Array()
			for j := 0; j < 3; j++ {
				bl.g[at+j] += grad[j]
				for k := 0; k < 3; k++ {
					bl.d[at+j][at+k] += w[j*3+k]
				}
			}
			nm.wss += 2 * priorCost(w, o)
			nm.rows += 3
		}
		if g.prior != nil {
			addPrior(g.prior, st.offs[gi], 0)
			k := 3
			if g.drift != nil {
				addPrior(g.drift, st.drifts[gi], k)
				k += 3
			}
			if g.spin != nil {
				addPrior(g.spin, st.spins[gi], k)
			}
		}
	}
	for fi, j := range b.free {
		for _, k := range b.free[fi+1:] {
			nm.a[k][j] = nm.a[j][k]
		}
	}
	if b.lens != nil {
		grad, chi2 := b.lens.pull(&st.cam.in)
		for i := 0; i < numLens; i++ {
			nm.g[i] += grad[i]
			for j := 0; j < numLens; j++ {
				nm.a[i][j] += b.lens.info[i][j]
			}
		}
		nm.wss += chi2
		nm.rows += numLens
	}
	return nm
}

// reduce eliminates every block from the damped normal equations. It returns
// the reduced matrix and right-hand side over the free shared parameters, and
// each block's inverse for the back-substitution.
func (b *bundle) reduce(nm *normal, lambda float64) (s geom.Mat, rhs []float64, inv []geom.Mat, err error) {
	n := len(b.free)
	s, rhs, inv = geom.NewMat(n, n), make([]float64, n), make([]geom.Mat, len(nm.blocks))
	damp := func(diag, largest float64) float64 { return diag + lambda*(diag+1e-12*largest) }
	largest := 0.0
	for _, j := range b.free {
		largest = math.Max(largest, nm.a[j][j])
	}
	for i, j := range b.free {
		for k, l := range b.free {
			s.Data[i*n+k] = nm.a[j][l]
		}
		s.Data[i*n+i] = damp(nm.a[j][j], largest)
		rhs[i] = -nm.g[j]
	}
	for gi := range nm.blocks {
		bl := &nm.blocks[gi]
		if bl.n == 0 {
			continue
		}
		d := geom.NewMat(bl.n, bl.n)
		largest := 0.0
		for j := 0; j < bl.n; j++ {
			largest = math.Max(largest, bl.d[j][j])
		}
		for j := 0; j < bl.n; j++ {
			copy(d.Data[j*bl.n:(j+1)*bl.n], bl.d[j][:bl.n])
			d.Data[j*bl.n+j] = damp(bl.d[j][j], largest)
		}
		if inv[gi], err = geom.Inverse(d); err != nil {
			return s, rhs, inv, fmt.Errorf("calib: observation group %d does not constrain its own parameters: %w", gi, geom.ErrSingular)
		}
		// w = C·D⁻¹ over the free rows; S −= w·Cᵀ; rhs += w·g.
		for i, j := range b.free {
			var w [maxBlock]float64
			for k := 0; k < bl.n; k++ {
				for l := 0; l < bl.n; l++ {
					w[k] += bl.c[j][l] * inv[gi].Data[l*bl.n+k]
				}
				rhs[i] += w[k] * bl.g[k]
			}
			for k, l := range b.free {
				for m := 0; m < bl.n; m++ {
					s.Data[i*n+k] -= w[m] * bl.c[l][m]
				}
			}
		}
	}
	return s, rhs, inv, nil
}

// plus returns st moved by an increment of the shared parameters and of
// every group block. Rotations take a right perturbation.
func (b *bundle) plus(st *state, shared [numShared]float64, blocks [][maxBlock]float64) state {
	out := st.clone()
	c := &out.cam
	c.in.FX, c.in.FY, c.in.CX, c.in.CY, c.in.K1, c.in.K2 = c.in.FX+shared[parFX], c.in.FY+shared[parFY], c.in.CX+shared[parCX], c.in.CY+shared[parCY], c.in.K1+shared[parK1], c.in.K2+shared[parK2]
	c.pose.R = c.pose.R.Mul(geom.RotExp(geom.Vec3{X: shared[parRot], Y: shared[parRot+1], Z: shared[parRot+2]}))
	c.pose.T = c.pose.T.Add(geom.Vec3{X: shared[parPos], Y: shared[parPos+1], Z: shared[parPos+2]})
	c.dt += shared[parDT]
	for gi, dl := range blocks {
		if b.groups[gi].rigid {
			p := &out.poses[gi]
			p.R = p.R.Mul(geom.RotExp(geom.Vec3{X: dl[0], Y: dl[1], Z: dl[2]}))
			p.T = p.T.Add(geom.Vec3{X: dl[3], Y: dl[4], Z: dl[5]})
		} else {
			g := &b.groups[gi]
			out.offs[gi] = out.offs[gi].Add(geom.Vec3{X: dl[0], Y: dl[1], Z: dl[2]})
			k := 3
			if g.drift != nil {
				out.drifts[gi] = out.drifts[gi].Add(geom.Vec3{X: dl[k], Y: dl[k+1], Z: dl[k+2]})
				k += 3
			}
			if g.spin != nil {
				out.spins[gi] = out.spins[gi].Add(geom.Vec3{X: dl[k], Y: dl[k+1], Z: dl[k+2]})
			}
		}
	}
	return out
}

// step applies the solution of the damped system to a copy of st.
func (b *bundle) step(nm *normal, st *state, lambda float64) (state, error) {
	s, rhs, inv, err := b.reduce(nm, lambda)
	if err != nil {
		return state{}, err
	}
	ds, err := geom.Solve(s, rhs)
	if err != nil {
		return state{}, err
	}
	var shared [numShared]float64
	for i, j := range b.free {
		shared[j] = ds[i]
	}
	blocks := make([][maxBlock]float64, len(nm.blocks))
	for gi := range nm.blocks {
		bl := &nm.blocks[gi]
		// dl = −D⁻¹·(g + Cᵀ·ds)
		var t [maxBlock]float64
		for k := 0; k < bl.n; k++ {
			t[k] = bl.g[k]
			for i, j := range b.free {
				t[k] += bl.c[j][k] * ds[i]
			}
		}
		for k := 0; k < bl.n; k++ {
			for l := 0; l < bl.n; l++ {
				blocks[gi][k] -= inv[gi].Data[k*bl.n+l] * t[l]
			}
		}
	}
	return b.plus(st, shared, blocks), nil
}

// solve runs Levenberg-Marquardt from st. It returns geom.ErrInsufficient
// when there are fewer residuals than parameters, an error wrapping
// geom.ErrSingular when some parameter is not observable, and
// geom.ErrNoConvergence with the best iterate when the iteration limit is hit.
func (b *bundle) solve(st state) (bundleResult, error) {
	params, points := len(b.free), 0
	for gi := range b.groups {
		params += b.groups[gi].dim()
		points += len(b.groups[gi].points)
	}
	if params == 0 || 2*points < params {
		return bundleResult{}, geom.ErrInsufficient
	}
	const relTol, maxLambda = 1e-10, 1e12
	cost := b.cost(&st)
	if math.IsNaN(cost) || math.IsInf(cost, 0) {
		return bundleResult{}, geom.ErrNonFinite
	}
	lambda, converged := 1e-3, false
	res := bundleResult{}
	for res.iterations < b.iters && !converged {
		res.iterations++
		nm := b.linearize(&st)
		accepted := false
		for lambda <= maxLambda && !accepted {
			used := lambda
			lambda *= 10
			trial, err := b.step(nm, &st, used)
			if err != nil {
				continue
			}
			c := b.cost(&trial)
			if !(c < cost) {
				continue
			}
			converged = cost-c <= relTol*cost
			st, cost, accepted = trial, c, true
			lambda = math.Max(used/10, 1e-12)
		}
		if !accepted {
			converged = true // no damping level improves: a minimum to working precision
		}
	}
	res.state = st
	nm := b.linearize(&st)
	s, _, _, err := b.reduce(nm, 0)
	if err == nil {
		res.cov, err = geom.Inverse(s)
	}
	if err != nil {
		return res, fmt.Errorf("calib: parameters not observable: %w", geom.ErrSingular)
	}
	res.chi2 = 1
	if dof := nm.rows - params; dof > 0 {
		res.chi2 = nm.wss / float64(dof)
	}
	scale := res.chi2
	if b.absolute {
		scale = math.Max(1, scale)
	}
	n := len(b.free)
	for i := 0; i < n; i++ {
		for j := i; j < n; j++ {
			v := (res.cov.Data[i*n+j] + res.cov.Data[j*n+i]) / 2 * scale
			res.cov.Data[i*n+j], res.cov.Data[j*n+i] = v, v
		}
	}
	for gi := range b.groups {
		for pi := range b.groups[gi].points {
			p := &b.groups[gi].points[pi]
			d := math.Inf(1)
			if r, ok := b.residual(gi, p, &st, nil, nil); ok {
				d = math.Hypot(r[0], r[1]) / p.w
			}
			res.dist = append(res.dist, d)
		}
	}
	if !converged {
		return res, geom.ErrNoConvergence
	}
	return res, nil
}

// residualStats summarises pixel distances. An unprojectable point counts as
// the largest finite distance.
func residualStats(dist []float64) spatial.ResidualStats {
	d := make([]float64, 0, len(dist))
	for _, v := range dist {
		if !math.IsInf(v, 0) {
			d = append(d, v)
		}
	}
	if len(d) == 0 {
		return spatial.ResidualStats{}
	}
	sort.Float64s(d)
	ss := 0.0
	for _, v := range d {
		ss += v * v
	}
	return spatial.ResidualStats{Count: len(d), RMSE: math.Sqrt(ss / float64(len(d))), Median: d[len(d)/2], Max: d[len(d)-1]}
}
