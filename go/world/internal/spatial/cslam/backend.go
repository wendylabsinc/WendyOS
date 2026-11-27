package cslam

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/posegraph"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/scanmatch"
)

// maxPruneRounds bounds the closures removed, one per round, in one optimisation.
const maxPruneRounds = 8

// candidate is a verified scan match of keyframe to of robot b against a
// submap of robot a: z is that keyframe's base pose in the frame of keyframe
// from of robot a.
type candidate struct {
	a, b     *robot
	from, to int
	z        geom.Pose2
	info     geom.Mat3
	inGraph  bool
}

// closure is a robust edge in the graph: a loop closure, an inter-robot
// match or an absolute prior (a == b, prior set).
type closure struct {
	id      posegraph.EdgeID
	a, b    *robot
	prior   bool
	removed bool
}

type rasterKey struct {
	r     *robot
	j     int
	floor spatial.FloorID
}

type raster struct {
	key  rasterKey
	m    *scanmatch.Matcher
	used int
}

// backend is the shared pose graph and what was derived from it. It is a
// function of the keyframes fed to it, in the order they were fed.
type backend struct {
	g         *posegraph.Graph
	processed int
	// cursor is the canonical position of the last keyframe processed.
	cursorTime time.Time
	cursorDev  spatial.DeviceID
	// recent holds the closure candidates still available as support.
	recent   []*candidate
	closures []closure
	// dirty: closures entered the graph since the last optimisation;
	// stale: nodes did.
	dirty, stale  bool
	lastOptimized int
	optimizations int
	removed       int
	result        posegraph.Result
	converged     bool
	gate          float64 // χ² above which a closure is removed
	rasters       []raster
	tick          int
}

func (b *backend) reset(cfg Config) {
	gate, _ := geom.ChiSquareQuantile(3, cfg.OutlierProbability)
	*b = backend{g: posegraph.New(cfg.Graph), gate: gate, converged: true}
}

// before orders keyframes canonically: time, then device. One device's
// keyframes are already in time order.
func before(ta time.Time, da spatial.DeviceID, tb time.Time, db spatial.DeviceID) bool {
	if !ta.Equal(tb) {
		return ta.Before(tb)
	}
	return da < db
}

// Solve brings the back end up to date with the keyframes the front ends
// have produced (all but the last moments, see Config.ReorderWindow),
// optimises the shared graph and returns the joint estimate. It returns an
// error wrapping geom.ErrInsufficient before the first keyframe.
//
// The result is a function of the measurements ingested, not of the order in
// which different robots' measurements arrived (see the package comment).
// Calling Solve more often along the way changes it only within the
// optimiser's convergence tolerance.
func (m *Mapper) Solve() (*Solution, error) {
	b := &m.back
	type item struct {
		r *robot
		i int
	}
	// Keyframes up to the horizon are final: every robot heard from within
	// ReorderWindow of the newest has placed its scans up to there, so no
	// keyframe can still arrive before it.
	var newest time.Time
	for _, r := range m.robots {
		if r.last.After(newest) {
			newest = r.last
		}
	}
	horizon := newest
	for _, r := range m.robots {
		if !r.last.IsZero() && newest.Sub(r.last) <= m.cfg.ReorderWindow && r.last.Before(horizon) {
			horizon = r.last
		}
	}
	collect := func() []item {
		var out []item
		for _, r := range m.robots {
			for i := len(r.nodes); i < len(r.kfs) && !r.kfs[i].time.After(horizon); i++ {
				out = append(out, item{r, i})
			}
		}
		slices.SortStableFunc(out, func(x, y item) int {
			tx, ty := x.r.kfs[x.i].time, y.r.kfs[y.i].time
			if c := tx.Compare(ty); c != 0 {
				return c
			}
			if c := cmp.Compare(x.r.dev, y.r.dev); c != 0 {
				return c
			}
			return cmp.Compare(x.i, y.i)
		})
		return out
	}
	todo := collect()
	if len(todo) == 0 && b.processed == 0 {
		return nil, fmt.Errorf("cslam: no keyframes yet: %w", geom.ErrInsufficient)
	}
	if len(todo) > 0 && b.processed > 0 && before(todo[0].r.kfs[todo[0].i].time, todo[0].r.dev, b.cursorTime, b.cursorDev) {
		// A keyframe that sorts before one already processed: replay.
		//
		// ponytail: the replay redoes every closure search, which is linear
		// in the keyframes so far. It only happens when a robot delivers
		// more than ReorderWindow late; upgrade path: checkpoint the back
		// end and replay from the checkpoint before the late keyframe.
		b.reset(m.cfg)
		for _, r := range m.robots {
			r.nodes, r.anchored = nil, false
		}
		todo = collect()
	}
	for _, it := range todo {
		if err := m.add(it.r, it.i); err != nil {
			return nil, err
		}
	}
	if b.dirty || b.stale {
		if err := m.optimize(); err != nil {
			return nil, err
		}
	}
	return m.snapshot()
}

func (m *Mapper) pose(n posegraph.NodeID) geom.Pose2 {
	p, _ := m.back.g.Pose(n) // ids come from AddNode
	return p
}

// add appends keyframe i of r to the graph and searches closures for it.
func (m *Mapper) add(r *robot, i int) error {
	b, kf := &m.back, &r.kfs[i]
	node, err := b.g.AddNode(r.traj, kf.local)
	if err != nil {
		return err
	}
	r.nodes = append(r.nodes, node)
	if i > 0 {
		if _, err := b.g.AddEdge(posegraph.Relative(r.nodes[i-1], node, kf.step, kf.info)); err != nil {
			return err
		}
	}
	b.cursorTime, b.cursorDev, b.processed, b.stale = kf.time, r.dev, b.processed+1, true
	if kf.prior != nil {
		if err := m.anchor(r, i); err != nil {
			return err
		}
	}
	if i%m.cfg.ClosureEvery == 0 {
		if err := m.search(r, i); err != nil {
			return err
		}
	}
	b.recent = slices.DeleteFunc(b.recent, func(c *candidate) bool { return c.b == r && i-c.to > m.cfg.ConsistencyKeyframes })
	if b.dirty && b.processed-b.lastOptimized >= m.cfg.OptimizeEvery {
		return m.optimize()
	}
	return nil
}

// related reports whether two robots' estimates share a frame.
func (m *Mapper) related(a, b *robot) bool { return a == b || m.back.g.Aligned(a.traj, b.traj) }

// anchor adds the absolute prior of keyframe i of r. The first prior of a
// group of related robots moves the group rigidly into the prior's frame, so
// the optimiser starts from an estimate the prior agrees with.
func (m *Mapper) anchor(r *robot, i int) error {
	b, p, node := &m.back, r.kfs[i].prior, r.nodes[i]
	if !slices.ContainsFunc(m.robots, func(a *robot) bool { return a.anchored && m.related(a, r) }) {
		t := p.pose.Compose(m.pose(node).Inverse())
		for _, a := range m.robots {
			if !m.related(a, r) {
				continue
			}
			for _, n := range a.nodes {
				if err := b.g.SetPose(n, t.Compose(m.pose(n))); err != nil {
					return err
				}
			}
		}
	}
	info, err := m.closureInfo(p.cov)
	if err != nil {
		return err
	}
	e := posegraph.PosePrior(node, p.pose, info)
	e.Robust = true
	id, err := b.g.AddEdge(e)
	if err != nil {
		return err
	}
	b.closures = append(b.closures, closure{id: id, a: r, b: r, prior: true})
	r.anchored = true
	return m.optimize()
}

// closureInfo is the information of a closure or prior with covariance cov,
// after the closure floors are added.
func (m *Mapper) closureInfo(cov geom.Mat3) (geom.Mat3, error) {
	s, y := m.cfg.ClosureSigmaM, m.cfg.ClosureSigmaRad
	cov[0] += s * s
	cov[4] += s * s
	cov[8] += y * y
	return cov.Inverse()
}

// submaps is the number of complete submaps among the first n keyframes.
func (m *Mapper) submaps(n int) int {
	if n < m.cfg.SubmapKeyframes {
		return 0
	}
	return (n-m.cfg.SubmapKeyframes)/(m.cfg.SubmapKeyframes/2) + 1
}

// search matches keyframe i of r against finished submaps of every robot.
func (m *Mapper) search(r *robot, i int) error {
	cfg, q := &m.cfg, &r.kfs[i]
	half := cfg.SubmapKeyframes / 2
	for _, a := range m.robots {
		// Only submaps completed before this keyframe in canonical order
		// are candidates; a robot's own must also be LoopMinKeyframes old.
		n := len(a.nodes)
		if a == r {
			n = i + 1 - cfg.LoopMinKeyframes
		}
		count := m.submaps(n)
		if count == 0 {
			continue
		}
		if !m.related(a, r) {
			// Unrelated frames: nothing predicts where the keyframe is in
			// a's map, so every submap is searched at every heading, over
			// the neighbourhood of the keyframes it was built from.
			//
			// ponytail: one global match per submap of every unrelated
			// robot, on every GlobalEvery-th attempt, until the frames are
			// related: fine for tens of submaps. Upgrade path: a scan
			// descriptor to pick the few submaps worth a global match.
			if (i/cfg.ClosureEvery)%cfg.GlobalEvery != 0 {
				continue
			}
			coarse := thin(q.scan, cfg.GlobalBeams)
			for j := 0; j < count && !m.related(a, r); j++ {
				mt, err := m.raster(a, j, q.floor)
				if err != nil {
					return err
				}
				if mt == nil {
					continue
				}
				sr, err := mt.Search(coarse, m.neighbourhood(a, j, q.floor))
				if err == nil {
					// The thinned scan found the pose; all beams verify it.
					sr.Best, err = mt.Refine(q.scan, sr.Best.Pose)
				}
				if errors.Is(err, scanmatch.ErrConfig) {
					return err
				}
				if err == nil {
					if err := m.propose(a, j, r, i, sr.Best); err != nil {
						return err
					}
				}
			}
			continue
		}
		// Related frames: search around the predicted pose in the submap
		// that holds the nearest keyframe.
		xq := m.pose(r.nodes[i])
		near, best := -1, cfg.ClosureRadius
		for k := 0; k < (count-1)*half+cfg.SubmapKeyframes; k++ {
			if a.kfs[k].floor != q.floor {
				continue
			}
			if d := m.pose(a.nodes[k]).Pos().Dist(xq.Pos()); d < best {
				near, best = k, d
			}
		}
		if near < 0 {
			continue
		}
		// near is in two submaps: take the one it is more central to.
		j := near / half
		if near%half < half/2 {
			j--
		}
		j = min(max(j, 0), count-1)
		mt, err := m.raster(a, j, q.floor)
		if err != nil {
			return err
		}
		if mt == nil {
			continue
		}
		guess := a.kfs[near].local.Compose(m.pose(a.nodes[near]).Between(xq)).Compose(q.mount)
		sr, err := mt.Search(q.scan, scanmatch.Window{Center: guess, Linear: cfg.ClosureWindow, Angular: cfg.ClosureAngle})
		if errors.Is(err, scanmatch.ErrConfig) {
			return err
		}
		if err == nil {
			if err := m.propose(a, j, r, i, sr.Best); err != nil {
				return err
			}
		}
	}
	return nil
}

// neighbourhood is the search window that covers every pose within
// ClosureRadius of a keyframe of submap j of robot a, at any heading.
func (m *Mapper) neighbourhood(a *robot, j int, floor spatial.FloorID) scanmatch.Window {
	half := m.cfg.SubmapKeyframes / 2
	lo, hi := geom.Vec2{X: math.Inf(1), Z: math.Inf(1)}, geom.Vec2{X: math.Inf(-1), Z: math.Inf(-1)}
	for k := j * half; k < j*half+m.cfg.SubmapKeyframes; k++ {
		if p := a.kfs[k].local; a.kfs[k].floor == floor {
			lo, hi = geom.Vec2{X: min(lo.X, p.X), Z: min(lo.Z, p.Z)}, geom.Vec2{X: max(hi.X, p.X), Z: max(hi.Z, p.Z)}
		}
	}
	c := lo.Lerp(hi, 0.5)
	return scanmatch.Window{Center: geom.Pose2{X: c.X, Z: c.Z}, Linear: max(hi.X-lo.X, hi.Z-lo.Z)/2 + m.cfg.ClosureRadius, Angular: math.Pi}
}

// thin keeps about n evenly spaced beams of a scan.
func thin(s spatial.Scan, n int) spatial.Scan {
	stride := len(s.Ranges) / n
	if stride < 2 {
		return s
	}
	out := s
	out.AngleIncrement *= float64(stride)
	out.Ranges = nil
	for i := 0; i < len(s.Ranges); i += stride {
		out.Ranges = append(out.Ranges, s.Ranges[i])
	}
	return out
}

// raster returns the matcher of submap j of robot a restricted to one floor,
// nil when the submap has no keyframe there. Rasters are rebuilt from the
// keyframes on demand and the most recently used are kept.
//
// ponytail: a submap is rasterised at its keyframes' front-end poses, so
// its internal distortion is the front end's drift over SubmapKeyframes
// (below a cell at the defaults) and closures never repair it. Upgrade
// path: rasterise at the optimised poses relative to the submap's first
// keyframe once the graph has moved them.
func (m *Mapper) raster(a *robot, j int, floor spatial.FloorID) (*scanmatch.Matcher, error) {
	b, key := &m.back, rasterKey{a, j, floor}
	b.tick++
	for k := range b.rasters {
		if b.rasters[k].key == key {
			b.rasters[k].used = b.tick
			return b.rasters[k].m, nil
		}
	}
	g, err := grid.New(m.cfg.Scan.Grid)
	if err != nil {
		return nil, err
	}
	half, members := m.cfg.SubmapKeyframes/2, 0
	for k := j * half; k < j*half+m.cfg.SubmapKeyframes; k++ {
		if kf := &a.kfs[k]; kf.floor == floor {
			if err := g.InsertScan(kf.local.Compose(kf.mount), kf.scan); err != nil {
				return nil, err
			}
			members++
		}
	}
	var mt *scanmatch.Matcher
	if members > 0 {
		if mt, err = scanmatch.NewMatcher(g, m.cfg.Scan); err != nil {
			return nil, err
		}
	}
	entry := raster{key, mt, b.tick}
	if len(b.rasters) < m.cfg.SubmapCache {
		b.rasters = append(b.rasters, entry)
	} else {
		oldest := 0
		for k := range b.rasters {
			if b.rasters[k].used < b.rasters[oldest].used {
				oldest = k
			}
		}
		b.rasters[oldest] = entry
	}
	return mt, nil
}

// propose turns a scan match of keyframe i of r against submap j of a into a
// closure candidate, when the match is good enough to be one.
func (m *Mapper) propose(a *robot, j int, r *robot, i int, res scanmatch.Result) error {
	cfg, q := &m.cfg, &r.kfs[i]
	if !res.Converged || res.Degenerate || res.Score < cfg.ClosureMinScore {
		return nil
	}
	// The edge runs from the submap keyframe nearest the matched pose: the
	// shorter the lever, the less the submap's own distortion matters.
	base := res.Pose.Compose(q.mount.Inverse())
	half, from := cfg.SubmapKeyframes/2, -1
	for k := j * half; k < j*half+cfg.SubmapKeyframes; k++ {
		if a.kfs[k].floor == q.floor && (from < 0 || a.kfs[k].local.Pos().Dist(base.Pos()) < a.kfs[from].local.Pos().Dist(base.Pos())) {
			from = k
		}
	}
	origin := a.kfs[from].local.Inverse()
	info, err := m.closureInfo(geom.TransformCov3(origin, geom.ComposeCov(res.Pose, res.Covariance, q.mount.Inverse(), geom.Mat3{})))
	if err != nil {
		return nil // a covariance that cannot be inverted constrains nothing
	}
	return m.consider(&candidate{a: a, b: r, from: from, to: i, z: origin.Compose(base).Normalized(), info: info})
}

// implied returns the transform a candidate implies between the current
// estimates of its two robots, oriented from the robot with the greater
// device id to the lesser (for one robot: from now to then), and the matched
// position in the source frame.
func (m *Mapper) implied(c *candidate) (geom.Pose2, geom.Vec2) {
	want := m.pose(c.a.nodes[c.from]).Compose(c.z) // keyframe to, as a's estimate has it
	have := m.pose(c.b.nodes[c.to])
	if c.a.dev > c.b.dev {
		return have.Compose(want.Inverse()), want.Pos()
	}
	return want.Compose(have.Inverse()), have.Pos()
}

// consistent reports whether two candidates between the same pair of robots
// imply the same transform, compared where each of them matched.
func (m *Mapper) consistent(c, d *candidate) bool {
	tc, pc := m.implied(c)
	td, pd := m.implied(d)
	tol := m.cfg.ConsistencyTranslation
	return math.Abs(geom.AngleDiff(tc.Yaw, td.Yaw)) <= m.cfg.ConsistencyYaw &&
		tc.Apply(pc).Dist(td.Apply(pc)) <= tol && tc.Apply(pd).Dist(td.Apply(pd)) <= tol
}

// consider admits a candidate, and the earlier candidates that agree with
// it, to the graph once enough distinct keyframes support the same
// transform. Until then it only waits as support for later candidates.
func (m *Mapper) consider(c *candidate) error {
	b := &m.back
	related := m.related(c.a, c.b)
	need := m.cfg.MinConsistent
	if !related {
		need = m.cfg.MinAlignConsistent
	}
	type query struct {
		r *robot
		i int
	}
	support, distinct := []*candidate{c}, []query{{c.b, c.to}}
	for _, d := range b.recent {
		if (d.a == c.a && d.b == c.b || d.a == c.b && d.b == c.a) && m.consistent(c, d) {
			support = append(support, d)
			if !slices.Contains(distinct, query{d.b, d.to}) {
				distinct = append(distinct, query{d.b, d.to})
			}
		}
	}
	b.recent = append(b.recent, c)
	if len(distinct) < need {
		return nil
	}
	first := len(b.closures)
	var added []*candidate
	for _, d := range support {
		if d.inGraph {
			continue
		}
		e := posegraph.Relative(d.a.nodes[d.from], d.b.nodes[d.to], d.z, d.info)
		e.Robust = true
		id, err := b.g.AddEdge(e)
		if err != nil {
			return err
		}
		d.inGraph = true
		added = append(added, d)
		b.closures = append(b.closures, closure{id: id, a: d.a, b: d.b})
	}
	if related {
		b.dirty = b.dirty || len(added) > 0
		return nil
	}
	// First relation between two frames: move one into the other, keeping
	// the frame that has an absolute prior, else the one that started first.
	ref, moved := c.a, c.b
	refAnchored, refFirst := m.lead(ref)
	if anchored, first := m.lead(moved); anchored && !refAnchored || anchored == refAnchored && first < refFirst {
		ref, moved = moved, ref
	}
	tol := posegraph.AlignConfig{TranslationTol: m.cfg.ConsistencyTranslation, YawTol: m.cfg.ConsistencyYaw, MinInliers: 1}
	if _, err := b.g.Align(ref.traj, moved.traj, tol); err != nil {
		// The estimates moved since the candidates were compared: withdraw.
		// The candidates stay available as support for a later attempt.
		for k, d := range added {
			if err := b.g.RemoveEdge(b.closures[first+k].id); err != nil {
				return err
			}
			d.inGraph = false
		}
		b.closures = b.closures[:first]
		return nil
	}
	return m.optimize()
}

// lead describes the group of robots related to r: whether it has an
// absolute prior, and its first node.
func (m *Mapper) lead(r *robot) (anchored bool, first posegraph.NodeID) {
	first = r.nodes[0]
	for _, a := range m.robots {
		if len(a.nodes) > 0 && m.related(a, r) {
			anchored = anchored || a.anchored
			first = min(first, a.nodes[0])
		}
	}
	return anchored, first
}

// optimize runs the optimiser and removes, worst first, closures whose χ²
// the rest of the graph contradicts.
func (m *Mapper) optimize() error {
	b := &m.back
	for round := 0; ; round++ {
		res, err := b.g.Optimize()
		if err != nil && !errors.Is(err, geom.ErrNoConvergence) {
			return err
		}
		b.result, b.converged, b.optimizations = res, err == nil, b.optimizations+1
		worst := -1
		for k := range b.closures {
			if c := &b.closures[k]; !c.removed && res.Edges[c.id].Chi2 > b.gate && (worst < 0 || res.Edges[c.id].Chi2 > res.Edges[b.closures[worst].id].Chi2) {
				worst = k
			}
		}
		if worst < 0 || round == maxPruneRounds {
			break
		}
		if err := b.g.RemoveEdge(b.closures[worst].id); err != nil {
			return err
		}
		b.closures[worst].removed = true
		b.removed++
	}
	b.dirty, b.stale, b.lastOptimized = false, false, b.processed
	return nil
}
