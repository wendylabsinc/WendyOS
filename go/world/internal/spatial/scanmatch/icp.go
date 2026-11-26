package scanmatch

import (
	"errors"
	"fmt"
	"math"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
)

// refPoint is a return of the reference scan with its fitted line.
type refPoint struct {
	p geom.Vec2 // the return, used to pair points
	line
}

// reference returns the returns of s that have a line, at most 4·MaxPoints
// of them.
func (c Config) reference(s spatial.Scan) []refPoint {
	pts, lines := c.fitLines(s)
	stride := max((len(pts)+4*c.MaxPoints-1)/(4*c.MaxPoints), 1)
	var out []refPoint
	for i := 0; i < len(pts); i += stride {
		if lines[i].normal != (geom.Vec2{}) {
			out = append(out, refPoint{pts[i], lines[i]})
		}
	}
	return out
}

// icp aligns cur to the reference lines by point-to-line ICP from guess.
func (c Config) icp(refs []refPoint, ref, cur spatial.Scan, guess geom.Pose2) (Result, error) {
	pts, _, rms, _ := c.scanPoints(cur)
	if len(pts) < c.MinPoints || len(refs) < c.MinPoints || !(rms > 0) {
		return Result{}, fmt.Errorf("%w: %d returns against %d reference lines, need %d", ErrInsufficient, len(pts), len(refs), c.MinPoints)
	}
	reach2 := c.MaxCorrespondence * c.MaxCorrespondence
	res := func(pose geom.Pose2, emit func(float64, [3]float64, [3]float64)) {
		for _, p := range pts {
			arm := pose.Rotate(p)
			q := pose.Pos().Add(arm)
			// ponytail: brute-force nearest neighbour, ≤ MaxPoints × 4·MaxPoints
			// distances per iteration; index the reference by beam angle if
			// scans with thousands of used beams make this the bottleneck.
			best, bestD := -1, reach2
			for i := range refs {
				if d := q.Sub(refs[i].p).Norm2(); d <= bestD {
					best, bestD = i, d
				}
			}
			if best < 0 {
				continue
			}
			n := refs[best].normal
			j := [3]float64{n.X, n.Z, n.Dot(arm.Perp())}
			emit(n.Dot(q.Sub(refs[best].anchor)), j, j)
		}
	}
	// Both scans carry range noise.
	rs, cs := math.Max(ref.RangeSigma, 0), math.Max(cur.RangeSigma, 0)
	p := &problem{cfg: c, res: res, points: len(pts), scale: rms, sigma2Floor: rs*rs + cs*cs, translationFloor: c.ScanTranslationFloor, yawFloor: c.ScanYawFloor}
	out := p.run(guess, false)
	if out.InlierFraction < c.MinOverlap {
		return out, fmt.Errorf("%w: %.0f%% of points matched, need %.0f%%", ErrNoOverlap, 100*out.InlierFraction, 100*c.MinOverlap)
	}
	return out, nil
}

// ScanToScan estimates the pose of cur's scan frame in ref's scan frame by
// point-to-line ICP from guess: the relative motion between two scans of one
// scanner, or the offset between two keyframes. The guess must be good to
// about MaxCorrespondence at the scan's returns; use ScanToScanSearch
// otherwise. It returns ErrNoOverlap, with the rejected Result, when fewer
// than MinOverlap of cur's points land on ref's surfaces.
//
// Along a direction the scans do not constrain (the axis of a corridor) the
// result keeps the guess and is flagged Degenerate, so the guess should be
// the best prior available, such as the odometry increment.
func ScanToScan(ref, cur spatial.Scan, guess geom.Pose2, cfg Config) (Result, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return Result{}, err
	}
	if !finitePose(guess) {
		return Result{}, fmt.Errorf("%w: non-finite guess", ErrConfig)
	}
	return cfg.icp(cfg.reference(ref), ref, cur, guess)
}

// ScanToScanSearch estimates the pose of cur's scan frame in ref's scan
// frame without a usable guess, for loop closure: a correlative search over
// w on a local grid built from ref, then ICP from the best pose and from
// each rival. It returns ErrNoMatch when nothing in the window reaches
// MinScore, ErrNoOverlap when no candidate survives ICP, and ErrAmbiguous,
// with the full result, when a rival remains after ICP.
func ScanToScanSearch(ref, cur spatial.Scan, w Window, cfg Config) (SearchResult, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return SearchResult{}, err
	}
	g, err := grid.New(cfg.Grid)
	if err != nil {
		return SearchResult{}, err
	}
	if err := g.InsertScan(geom.Pose2{}, ref); err != nil {
		return SearchResult{}, err
	}
	m := &Matcher{g: g, cfg: cfg}
	found, err := m.Search(cur, w)
	if err != nil && !errors.Is(err, ErrAmbiguous) {
		return found, err
	}
	refs := cfg.reference(ref)
	var refined []Result
	var last error
	for _, cand := range append([]Result{found.Best}, found.Rivals...) {
		r, err := cfg.icp(refs, ref, cur, cand.Pose)
		if err != nil {
			last = err
			continue
		}
		refined = append(refined, r)
	}
	if len(refined) == 0 {
		return SearchResult{Evaluated: found.Evaluated}, last
	}
	out, err := cfg.rank(refined)
	out.Evaluated = found.Evaluated
	return out, err
}
