package posegraph

import (
	"fmt"
	"math"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// AlignConfig tunes Align. Zero fields take the documented default.
type AlignConfig struct {
	// TranslationTol and YawTol bound the disagreement between two
	// inter-trajectory edges that still count as supporting the same
	// alignment: the distance between where they place a node, in metres, and
	// the difference of the rotations they imply, in radians. They must cover
	// the drift of both trajectories between the matched nodes. Defaults 0.5 m
	// and 0.2 rad.
	TranslationTol float64
	YawTol         float64
	// MinInliers is the smallest set of mutually consistent edges accepted.
	// Default 2: a single unconfirmed match does not merge two frames.
	MinInliers int
}

// Alignment is the outcome of Align.
type Alignment struct {
	// Transform was applied on the left of every moved estimate: it maps the
	// moved trajectory's previous frame into the reference frame.
	Transform geom.Pose2
	// Inliers are the edges that support Transform. Outliers are the other
	// edges between the two frames: false matches, or true ones that drift
	// puts outside the tolerances. They stay in the graph; the χ² each gets
	// from Optimize decides which to remove.
	Inliers, Outliers []EdgeID
	// RMSE and YawRMSE are the disagreement left among the inliers after the
	// alignment, in metres and radians.
	RMSE, YawRMSE float64
}

// Align moves trajectory traj, and every trajectory already sharing its
// frame, into the frame of ref, using the relative edges between the two
// frames. It is the initial guess for a joint Optimize: each edge proposes a
// rigid transform, the proposal most others agree with wins, and the agreeing
// proposals are averaged. Landmarks seen only from moved nodes move too.
//
// It returns ErrUnknownID for an unknown trajectory, ErrAligned when the two
// already share a frame, geom.ErrInsufficient when no relative edge joins the
// frames, and geom.ErrNoConsensus when fewer than MinInliers edges agree. The
// graph changes only on success.
func (g *Graph) Align(ref, traj Trajectory, cfg AlignConfig) (Alignment, error) {
	if cfg.TranslationTol <= 0 {
		cfg.TranslationTol = 0.5
	}
	if cfg.YawTol <= 0 {
		cfg.YawTol = 0.2
	}
	if cfg.MinInliers <= 0 {
		cfg.MinInliers = 2
	}
	ri, ok := g.byName[ref]
	ti, ok2 := g.byName[traj]
	if !ok || !ok2 {
		return Alignment{}, fmt.Errorf("%w: trajectory", ErrUnknownID)
	}
	keep, move := g.trajs[ri].frame, g.trajs[ti].frame
	if keep == move {
		return Alignment{}, ErrAligned
	}
	frame := func(n NodeID) int32 { return g.trajs[g.trajOf[n]].frame }

	// Each edge says where a node of the moving frame should be (want) given
	// where it is now (src): the proposal is t = want∘src⁻¹.
	type proposal struct {
		edge      EdgeID
		src, want geom.Vec2
		t         geom.Pose2
	}
	var props []proposal
	for k, e := range g.edges {
		if g.gone[k] || e.Kind != EdgeRelative {
			continue
		}
		var src, want geom.Pose2
		switch fa, fb := frame(e.From), frame(e.To); {
		case fa == keep && fb == move:
			src, want = g.est[e.To], g.est[e.From].Compose(e.Pose)
		case fa == move && fb == keep:
			src, want = g.est[e.From], g.est[e.To].Compose(e.Pose.Inverse())
		default:
			continue
		}
		props = append(props, proposal{EdgeID(k), src.Pos(), want.Pos(), want.Compose(src.Inverse())})
	}
	if len(props) == 0 {
		return Alignment{}, fmt.Errorf("posegraph: no relative edge between %q and %q: %w", ref, traj, geom.ErrInsufficient)
	}
	agrees := func(t geom.Pose2, p proposal) (float64, bool) {
		d := t.Apply(p.src).Dist(p.want)
		return d, d <= cfg.TranslationTol && math.Abs(geom.AngleDiff(t.Yaw, p.t.Yaw)) <= cfg.YawTol
	}
	best, bestCount, bestSum := 0, 0, 0.0
	for i, c := range props {
		count, sum := 0, 0.0
		for _, p := range props {
			if d, ok := agrees(c.t, p); ok {
				count++
				sum += d * d
			}
		}
		if count > bestCount || count == bestCount && sum < bestSum {
			best, bestCount, bestSum = i, count, sum
		}
	}
	if bestCount < cfg.MinInliers {
		return Alignment{}, fmt.Errorf("posegraph: %d of %d edges between %q and %q agree, need %d: %w",
			bestCount, len(props), ref, traj, cfg.MinInliers, geom.ErrNoConsensus)
	}

	// Average the agreeing proposals: circular mean of the rotations, then the
	// translation that best maps src onto want under that rotation.
	var out Alignment
	var sin, cos float64
	var inliers []proposal
	for _, p := range props {
		if _, ok := agrees(props[best].t, p); ok {
			inliers = append(inliers, p)
			out.Inliers = append(out.Inliers, p.edge)
			s, c := math.Sincos(p.t.Yaw)
			sin, cos = sin+s, cos+c
		} else {
			out.Outliers = append(out.Outliers, p.edge)
		}
	}
	yaw := math.Atan2(sin, cos)
	var shift geom.Vec2
	for _, p := range inliers {
		shift = shift.Add(p.want.Sub(p.src.Rotate(yaw)))
	}
	shift = shift.Scale(1 / float64(len(inliers)))
	out.Transform = geom.Pose2{X: shift.X, Z: shift.Z, Yaw: yaw}
	for _, p := range inliers {
		out.RMSE += out.Transform.Apply(p.src).Sub(p.want).Norm2()
		dy := geom.AngleDiff(yaw, p.t.Yaw)
		out.YawRMSE += dy * dy
	}
	out.RMSE = math.Sqrt(out.RMSE / float64(len(inliers)))
	out.YawRMSE = math.Sqrt(out.YawRMSE / float64(len(inliers)))

	// A landmark moves when every node observing it moves.
	const (
		unseen = iota
		moving
		staying
	)
	seen := make([]uint8, len(g.lms))
	for k, e := range g.edges {
		if g.gone[k] || e.Kind != EdgeLandmark {
			continue
		}
		if frame(e.From) != move {
			seen[e.Landmark] = staying
		} else if seen[e.Landmark] == unseen {
			seen[e.Landmark] = moving
		}
	}
	for i, state := range seen {
		if state == moving {
			g.lms[i] = out.Transform.Apply(g.lms[i])
		}
	}
	for n := range g.est {
		if frame(NodeID(n)) == move {
			g.est[n] = out.Transform.Compose(g.est[n])
		}
	}
	g.relabel(move, keep)
	g.sys = nil
	return out, nil
}

// Aligned reports whether two trajectories share a frame: an Align or an
// Optimize over edges connecting them (or anchoring both to the graph frame)
// has put their estimates in common coordinates.
func (g *Graph) Aligned(a, b Trajectory) bool {
	ai, ok := g.byName[a]
	bi, ok2 := g.byName[b]
	return ok && ok2 && g.trajs[ai].frame == g.trajs[bi].frame
}

// FrameTransform returns the estimated pose of child's trajectory frame in
// parent's: a point p in child's own frame is at T.Apply(p) in parent's own
// frame. A trajectory's frame is the one in which its first node has the
// local pose it was added with, so the transform relates the two start
// frames and does not absorb drift accumulated later.
//
// The covariance is over (x, z, yaw) of T, propagated to first order from
// the joint marginal of the two first nodes under the approximations listed
// at Marginal. It returns ErrDisconnected until the trajectories share a
// frame (see Aligned), and an error wrapping geom.ErrSingular when the edges
// do not determine the state.
func (g *Graph) FrameTransform(parent, child Trajectory) (geom.Pose2, geom.Mat3, error) {
	pi, ok := g.byName[parent]
	ci, ok2 := g.byName[child]
	if !ok || !ok2 {
		return geom.Pose2{}, geom.Mat3{}, fmt.Errorf("%w: trajectory", ErrUnknownID)
	}
	if g.trajs[pi].frame != g.trajs[ci].frame {
		return geom.Pose2{}, geom.Mat3{}, ErrDisconnected
	}
	a, c := g.trajs[pi].nodes[0], g.trajs[ci].nodes[0]
	if a == c {
		return geom.Pose2{}, geom.Mat3{}, nil
	}
	s, err := g.factored()
	if err != nil {
		return geom.Pose2{}, geom.Mat3{}, err
	}
	// T = La ∘ D ∘ Lc⁻¹ with D = Pa⁻¹∘Pc, the quantity a relative edge
	// between the two first nodes would measure.
	d := g.est[a].Between(g.est[c])
	_, ja, jc := g.jacobian(&Edge{Kind: EdgeRelative, From: a, To: c})
	// Joint covariance of (Pa, Pc), then cov(D) = J·Σ·Jᵀ with J = [ja jc].
	jac, sigma := geom.NewMat(3, 6), geom.NewMat(6, 6)
	vars := [2]int32{s.nodeVar[a], s.nodeVar[c]}
	for i, j := range [2]geom.Mat3{ja, jc} {
		for r := 0; r < 3; r++ {
			copy(jac.Data[r*6+3*i:r*6+3*i+3], j[r*3:r*3+3])
		}
		if vars[i] < 0 {
			continue // fixed or held: no uncertainty
		}
		for col, x := range s.inverseColumns(vars[i]) {
			for k, v := range vars {
				for r := 0; r < 3 && v >= 0; r++ {
					sigma.Set(3*k+r, 3*i+col, x[3*int(v)+r])
				}
			}
		}
	}
	var covD geom.Mat3
	copy(covD[:], jac.Mul(sigma).Mul(jac.T()).Data)
	la, lci := g.local[a], g.local[c].Inverse()
	lad := la.Compose(d)
	cov := geom.ComposeCov(lad, geom.ComposeCov(la, geom.Mat3{}, d, covD), lci, geom.Mat3{})
	return lad.Compose(lci), cov, nil
}
