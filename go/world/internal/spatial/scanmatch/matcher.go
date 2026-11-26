package scanmatch

import (
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
)

// Matcher matches scans against one occupancy grid. It follows the grid: the
// first match after the grid changes takes the grid's updated likelihood
// field (see grid.Grid.Field for what that costs per scan), and the first
// search after it rebuilds the score pyramid, Depth+1 bytes and passes per
// cell of the map's bounding box. A loop that searches while it maps should
// therefore search a Clone taken at keyframe rate. Matches may run
// concurrently with each other, not with a mutation of the grid.
type Matcher struct {
	g   *grid.Grid
	cfg Config

	mu    sync.Mutex
	field *grid.Field
	pyr   *grid.Pyramid
}

// NewMatcher returns a matcher for g.
func NewMatcher(g *grid.Grid, cfg Config) (*Matcher, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	if g == nil {
		return nil, fmt.Errorf("%w: nil grid", ErrConfig)
	}
	return &Matcher{g: g, cfg: cfg}, nil
}

// Config returns the configuration with defaults filled in.
func (m *Matcher) Config() Config { return m.cfg }

func (m *Matcher) prepare(pyramid bool) (*grid.Field, *grid.Pyramid, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f, err := m.g.Field()
	if err != nil {
		return nil, nil, err
	}
	if f.W == 0 {
		return nil, nil, ErrEmptyMap
	}
	if f != m.field {
		m.field, m.pyr = f, nil
	}
	if pyramid && m.pyr == nil {
		if m.pyr, err = f.Pyramid(m.cfg.Sigma, m.cfg.Depth+1); err != nil {
			return nil, nil, err
		}
	}
	return f, m.pyr, nil
}

// fieldProblem is the point-to-likelihood-field fit: the residual of a scan
// point is its interpolated distance to the nearest occupied cell, and the
// field's gradient steers the optimisation. Near the occupied cells that
// gradient follows the raster rather than the wall, so the information each
// point provides is taken along the normal of the line fitted to the scan
// itself.
func (m *Matcher) fieldProblem(f *grid.Field, s spatial.Scan) (*problem, []geom.Vec2, float64, error) {
	pts, normals, rms, far := m.cfg.scanPoints(s)
	if len(pts) < m.cfg.MinPoints || !(rms > 0) {
		return nil, nil, 0, fmt.Errorf("%w: %d returns, need %d", ErrInsufficient, len(pts), m.cfg.MinPoints)
	}
	res := func(pose geom.Pose2, emit func(float64, [3]float64, [3]float64)) {
		for i, p := range pts {
			arm := pose.Rotate(p)
			turn := arm.Perp()
			d, grad := f.Sample(pose.Pos().Add(arm))
			n := pose.Rotate(normals[i])
			emit(d, [3]float64{grad.X, grad.Z, grad.Dot(turn)}, [3]float64{n.X, n.Z, n.Dot(turn)})
		}
	}
	sigma := math.Max(s.RangeSigma, 0)
	return &problem{cfg: m.cfg, res: res, points: len(pts), scale: rms, sigma2Floor: sigma * sigma, translationFloor: m.cfg.TranslationFloor, yawFloor: m.cfg.YawFloor}, pts, far, nil
}

// Refine improves guess by damped Gauss-Newton on the likelihood field. It
// returns ErrNoMatch, with the rejected Result, when the refined pose scores
// below MinScore.
//
// The guess must be inside the basin of the true pose. Measured in a
// furnished building with 12 m scans: from 0.5 m and 10° off every refinement
// was right; from 0.8 m and 15° about 2 % settled in a wrong local minimum,
// tens of centimetres off, with a confident covariance and a score of 0.65
// to 0.8, above the default MinScore. With a prior worse than about 0.5 m
// or 10°, use Search over a Window of that size instead, or raise MinScore.
//
// Accuracy is bounded by the map, which places a wall at the centres of its
// cells: expect about a fifth of a cell on average and up to half a cell
// when every wall is parallel to the grid, whatever the number of beams.
// TranslationFloor accounts for it in the covariance. Use a finer grid, or
// ScanToScan for relative motion, when that is not enough.
func (m *Matcher) Refine(s spatial.Scan, guess geom.Pose2) (Result, error) {
	if !finitePose(guess) {
		return Result{}, fmt.Errorf("%w: non-finite guess", ErrConfig)
	}
	f, _, err := m.prepare(false)
	if err != nil {
		return Result{}, err
	}
	p, _, _, err := m.fieldProblem(f, s)
	if err != nil {
		return Result{}, err
	}
	out := p.run(guess, true)
	if out.Score < m.cfg.MinScore {
		return out, fmt.Errorf("%w: score %.3f", ErrNoMatch, out.Score)
	}
	return out, nil
}

// Score evaluates the scan at pose without refining it. Result.Iterations is
// zero.
func (m *Matcher) Score(s spatial.Scan, pose geom.Pose2) (Result, error) {
	if !finitePose(pose) {
		return Result{}, fmt.Errorf("%w: non-finite pose", ErrConfig)
	}
	f, _, err := m.prepare(false)
	if err != nil {
		return Result{}, err
	}
	p, _, _, err := m.fieldProblem(f, s)
	if err != nil {
		return Result{}, err
	}
	return p.evaluate(pose), nil
}

// Window is a search region around a pose.
type Window struct {
	Center geom.Pose2
	// Linear is the half-width in metres searched on both x and z.
	Linear float64
	// Angular is the half-width in radians searched in heading; π or more
	// searches every heading.
	Angular float64
}

// SearchResult is the outcome of a correlative search.
type SearchResult struct {
	// Best is the highest-scoring pose, refined.
	Best Result
	// Rivals are refined poses distinct from Best (see
	// Config.DistinctTranslation) that score at least AmbiguityRatio times
	// Best.Score, best first. The search over the cell lattice is exhaustive
	// by branch and bound, so an empty list means no such pose exists on it.
	Rivals []Result
	// ScoreRatio is Rivals[0].Score / Best.Score, or 0 without a rival.
	ScoreRatio float64
	// Evaluated counts the lattice poses scored, coarse nodes included.
	Evaluated int
}

// Global searches every pose whose position lies in the bounding box of the
// map's occupied cells, at every heading.
func (m *Matcher) Global(s spatial.Scan) (SearchResult, error) {
	f, _, err := m.prepare(false)
	if err != nil {
		return SearchResult{}, err
	}
	lo := grid.Cell{IX: f.Origin.IX + int32(f.Pad), IZ: f.Origin.IZ + int32(f.Pad)}
	hi := grid.Cell{IX: f.Origin.IX + int32(f.W-f.Pad-1), IZ: f.Origin.IZ + int32(f.H-f.Pad-1)}
	return m.search(s, lo, hi, 0, math.Pi)
}

// Search finds the best pose inside w by multi-resolution correlative
// matching, refines it and reports rivals. It returns ErrNoMatch when no
// pose reaches MinScore and ErrAmbiguous, with the full result, when a rival
// exists.
func (m *Matcher) Search(s spatial.Scan, w Window) (SearchResult, error) {
	if !finitePose(w.Center) || !(w.Linear >= 0) || !(w.Angular >= 0) || math.IsInf(w.Linear, 0) {
		return SearchResult{}, fmt.Errorf("%w: search window", ErrConfig)
	}
	size := m.g.CellSize()
	c := m.g.CellAt(w.Center.Pos())
	half := int32(math.Min(math.Ceil(w.Linear/size), 1<<20))
	return m.search(s, grid.Cell{IX: c.IX - half, IZ: c.IZ - half}, grid.Cell{IX: c.IX + half, IZ: c.IZ + half}, w.Center.Yaw, w.Angular)
}

// maxTopNodes bounds the coarsest level of one search (headings × blocks).
const maxTopNodes = 1 << 22

// node is a block of 2^level × 2^level sensor cells at one heading, with the
// upper bound of the score of every pose inside it.
type node struct {
	score   int32
	k, i, j int32
}

func nodeLess(a, b node) bool {
	if a.score != b.score {
		return a.score > b.score
	}
	if a.k != b.k {
		return a.k < b.k
	}
	if a.j != b.j {
		return a.j < b.j
	}
	return a.i < b.i
}

// lattice is one branch-and-bound search over sensor cells and headings.
// Cell coordinates are raster indices of the pyramid.
type lattice struct {
	pyr            *grid.Pyramid
	yaws           []float64
	offsets        [][]int32 // per heading: (dx, dz) cell offset of each point from the sensor cell
	i0, i1, j0, j1 int32     // inclusive sensor-cell window
	level          int       // coarsest level searched
	distinctCells  int32
	distinctYaw    float64
	found          []node // accepted leaves; later passes exclude their neighbourhoods
	best           node
	bound          int32 // a leaf must score above this
	ok             bool
	evaluated      int
}

func (l *lattice) score(level int, k, i, j int32) int32 {
	l.evaluated++
	cells, w, h := l.pyr.Levels[level], l.pyr.W, l.pyr.H
	off := l.offsets[k]
	sum := int32(0)
	for n := 0; n < len(off); n += 2 {
		x, z := int(i+off[n]), int(j+off[n+1])
		if uint(x) < uint(w) && uint(z) < uint(h) {
			sum += int32(cells[z*w+x])
		}
	}
	return sum
}

// excluded reports whether every pose of the block lies in the
// neighbourhood of an accepted leaf.
func (l *lattice) excluded(k, i, j, width int32) bool {
	for _, f := range l.found {
		if math.Abs(geom.AngleDiff(l.yaws[k], l.yaws[f.k])) <= l.distinctYaw &&
			i >= f.i-l.distinctCells && i+width-1 <= f.i+l.distinctCells &&
			j >= f.j-l.distinctCells && j+width-1 <= f.j+l.distinctCells {
			return true
		}
	}
	return false
}

func (l *lattice) branch(n node, level int) {
	if level == 0 {
		l.best, l.bound, l.ok = n, n.score, true
		return
	}
	half := int32(1) << (level - 1)
	var kids [4]node
	count := 0
	for _, d := range [4][2]int32{{0, 0}, {half, 0}, {0, half}, {half, half}} {
		i, j := n.i+d[0], n.j+d[1]
		if i > l.i1 || j > l.j1 || l.excluded(n.k, i, j, half) {
			continue
		}
		kid := node{l.score(level-1, n.k, i, j), n.k, i, j}
		at := count
		for at > 0 && nodeLess(kid, kids[at-1]) {
			kids[at] = kids[at-1]
			at--
		}
		kids[at] = kid
		count++
	}
	for _, kid := range kids[:count] {
		if kid.score <= l.bound {
			return
		}
		l.branch(kid, level-1)
	}
}

// pass finds the best leaf scoring above bound outside the neighbourhoods of
// the leaves already found.
func (l *lattice) pass(top []node, bound int32) (node, bool) {
	l.best, l.bound, l.ok = node{}, bound, false
	for _, n := range top {
		if n.score <= l.bound {
			break
		}
		if !l.excluded(n.k, n.i, n.j, int32(1)<<l.level) {
			l.branch(n, l.level)
		}
	}
	return l.best, l.ok
}

// newLattice lays out a search for the scan points pts, whose farthest is
// far metres away: the headings, the sensor-cell window lo..hi, the coarsest
// level and every point's cell offset at every heading.
func (c Config) newLattice(pyr *grid.Pyramid, pts []geom.Vec2, far float64, lo, hi grid.Cell, yaw, angular float64) (*lattice, error) {
	step := c.AngularStep
	if step == 0 {
		step = math.Max(c.Sigma/far, 0.002)
	}
	if 2*math.Pi/step > maxTopNodes {
		return nil, fmt.Errorf("%w: AngularStep %g is too fine", ErrConfig, step)
	}
	// A sensor farther from the raster than its longest return scores zero,
	// so the window is clipped to that reach.
	reach := int64(math.Min(math.Ceil(far/pyr.CellSize), 1<<20)) + 1
	clip := func(v, origin int32, size int) int32 {
		return int32(min(max(int64(v)-int64(origin), -reach), int64(size)+reach))
	}
	l := &lattice{
		pyr: pyr,
		i0:  clip(lo.IX, pyr.Origin.IX, pyr.W), i1: clip(hi.IX, pyr.Origin.IX, pyr.W),
		j0: clip(lo.IZ, pyr.Origin.IZ, pyr.H), j1: clip(hi.IZ, pyr.Origin.IZ, pyr.H),
		distinctCells: int32(math.Min(math.Ceil(c.DistinctTranslation/pyr.CellSize), 1<<20)),
		distinctYaw:   c.DistinctYaw,
	}
	if angular >= math.Pi {
		n := int(math.Ceil(2 * math.Pi / step))
		for k := 0; k < n; k++ {
			l.yaws = append(l.yaws, geom.WrapAngle(yaw+float64(k)*2*math.Pi/float64(n)))
		}
	} else {
		n := int(math.Ceil(angular / step))
		for k := -n; k <= n; k++ {
			l.yaws = append(l.yaws, geom.WrapAngle(yaw+float64(k)*step))
		}
	}
	for l.level < c.Depth && int32(1)<<l.level < max(l.i1-l.i0, l.j1-l.j0)+1 {
		l.level++
	}
	if blocks := l.blocks(); blocks > maxTopNodes/len(l.yaws) {
		return nil, fmt.Errorf("%w: search window of %d headings × %d blocks", ErrConfig, len(l.yaws), blocks)
	}
	l.offsets = make([][]int32, len(l.yaws))
	for k, a := range l.yaws {
		off := make([]int32, 0, 2*len(pts))
		for _, q := range pts {
			r := q.Rotate(a)
			// The sensor sits at a cell centre, hence the half cell.
			off = append(off, int32(math.Floor(r.X/pyr.CellSize+0.5)), int32(math.Floor(r.Z/pyr.CellSize+0.5)))
		}
		l.offsets[k] = off
	}
	return l, nil
}

// blocks is the number of coarsest-level blocks covering the window.
func (l *lattice) blocks() int {
	width := 1 << l.level
	return (int(l.i1-l.i0)/width + 1) * (int(l.j1-l.j0)/width + 1)
}

// candidates returns the best lattice pose scoring at least minScore and up
// to maxRivals more, each the best pose outside the neighbourhoods of those
// before it that scores at least ratio times the first. Scores are fractions
// of the full score of 255 per point. The result is exact on the lattice.
func (l *lattice) candidates(minScore, ratio float64, maxRivals int) []node {
	full := 255 * float64(len(l.offsets[0])/2)
	minimum := int32(math.Ceil(minScore*full)) - 1
	width := int32(1) << l.level
	top := make([]node, 0, l.blocks()*len(l.yaws))
	for k := range l.yaws {
		for j := l.j0; j <= l.j1; j += width {
			for i := l.i0; i <= l.i1; i += width {
				if n := (node{l.score(l.level, int32(k), i, j), int32(k), i, j}); n.score > minimum {
					top = append(top, n)
				}
			}
		}
	}
	sort.Slice(top, func(a, b int) bool { return nodeLess(top[a], top[b]) })
	best, ok := l.pass(top, minimum)
	if !ok {
		return nil
	}
	l.found = append(l.found, best)
	rivalBound := max(minimum, int32(math.Ceil(ratio*float64(best.score)))-1)
	for len(l.found) <= maxRivals {
		rival, ok := l.pass(top, rivalBound)
		if !ok {
			break
		}
		l.found = append(l.found, rival)
	}
	return l.found
}

func (m *Matcher) search(s spatial.Scan, lo, hi grid.Cell, yaw, angular float64) (SearchResult, error) {
	cfg := m.cfg
	f, pyr, err := m.prepare(true)
	if err != nil {
		return SearchResult{}, err
	}
	p, pts, far, err := m.fieldProblem(f, s)
	if err != nil {
		return SearchResult{}, err
	}
	l, err := cfg.newLattice(pyr, pts, far, lo, hi, yaw, angular)
	if err != nil {
		return SearchResult{}, err
	}
	// Refine every candidate and rank on the refined scores.
	var refined []Result
	for _, n := range l.candidates(cfg.MinScore, cfg.AmbiguityRatio, cfg.MaxRivals) {
		c := m.g.Center(grid.Cell{IX: n.i + pyr.Origin.IX, IZ: n.j + pyr.Origin.IZ})
		r := p.run(geom.Pose2{X: c.X, Z: c.Z, Yaw: l.yaws[n.k]}, true)
		if r.Score >= cfg.MinScore {
			refined = append(refined, r)
		}
	}
	out, err := cfg.rank(refined)
	out.Evaluated = l.evaluated
	return out, err
}

// rank orders refined candidates, drops those that collapsed onto a better
// one or fell below the ambiguity ratio, and reports the verdict.
func (c Config) rank(refined []Result) (SearchResult, error) {
	out := SearchResult{}
	if len(refined) == 0 {
		return out, ErrNoMatch
	}
	sort.SliceStable(refined, func(a, b int) bool { return refined[a].Score > refined[b].Score })
	out.Best = refined[0]
next:
	for _, r := range refined[1:] {
		if r.Score < c.AmbiguityRatio*out.Best.Score || !c.distinct(r.Pose, out.Best.Pose) {
			continue
		}
		for _, kept := range out.Rivals {
			if !c.distinct(r.Pose, kept.Pose) {
				continue next
			}
		}
		out.Rivals = append(out.Rivals, r)
	}
	if len(out.Rivals) > 0 {
		out.ScoreRatio = out.Rivals[0].Score / out.Best.Score
		return out, fmt.Errorf("%w: %d rival(s), score ratio %.3f", ErrAmbiguous, len(out.Rivals), out.ScoreRatio)
	}
	return out, nil
}
