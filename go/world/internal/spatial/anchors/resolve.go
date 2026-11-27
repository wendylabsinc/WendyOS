package anchors

import (
	"errors"
	"fmt"
	"math"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/scanmatch"
)

// signatureSensor names the scans rebuilt from signatures and keyframes.
const signatureSensor spatial.SensorID = "keyframe"

// Resolution is the outcome for one anchor of a Resolve call.
type Resolution struct {
	// ID is the anchor's identifier.
	ID string
	// Err is nil when the anchor was found, and otherwise why it was refused:
	// ErrSignature, ErrNoMatch or ErrAmbiguous. The remaining fields are
	// zero when it is set.
	Err error
	// Anchor is the anchor as the device sees it: position and yaw in the
	// frame of the device's keyframes, UncertaintyM the 1σ of that position.
	// Elevation and every other field are as stored; a planar match says
	// nothing about height.
	Anchor spatial.Anchor
	// Covariance is of the anchor pose over (x, z, yaw) in the device frame.
	// It covers the match only, not the device's own drift between its
	// keyframes nor the error of the anchor's offset from its keyframe.
	Covariance geom.Mat3
	// Score is the scan-match score in [0, 1] of the signature scan the pose
	// comes from: the share of it that lies on surfaces the device has seen.
	Score float64
	// Matched is the number of signature scans found, all at this pose.
	Matched int
}

// Resolve finds anchors in a device's own frame. The keyframes are the
// device's, with Local poses in any one frame; anchors come from a Store. It
// builds a map from the keyframes and searches each scan of an anchor's
// signature in it at every position and heading. An anchor is refused with
// ErrAmbiguous when a signature scan fits two distinct places or two of its
// scans disagree, and with ErrNoMatch when none of its scans is found with a
// score of Match.MinScore, a converged refinement, every direction
// determined and at most MaxContradiction of it across floor the device saw
// empty. The last test is what tells a room from a larger one with the same
// corner: the score counts the walls that agree, not the wall that is not
// there.
//
// The result has one entry per anchor, in order. The error is ErrConfig,
// ErrInsufficient or a keyframe's validation error.
//
// ponytail: an anchor in one of two places that look alike is found wherever
// the device is, when the device has only seen one of them. Nothing in the
// device's scans can tell; give Resolve a coarse prior (the floor, a UWB fix)
// and drop anchors outside it when a site has such places.
func Resolve(cfg Config, keyframes []spatial.Keyframe, anchors []spatial.Anchor) ([]Resolution, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	g, err := grid.New(cfg.Match.Grid)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConfig, err)
	}
	for _, kf := range keyframes {
		if err := kf.Validate(); err != nil {
			return nil, err
		}
		scan := kf.Scan.Expand(spatial.Header{Device: kf.Device, Sensor: signatureSensor, Time: kf.Time}, cfg.RangeSigma)
		if err := g.InsertScan(kf.Local.Compose(kf.SensorToBase), scan); err != nil {
			return nil, err
		}
	}
	if g.Counts().Occupied == 0 {
		return nil, fmt.Errorf("%w: %d keyframes without a return", ErrInsufficient, len(keyframes))
	}
	m, err := scanmatch.NewMatcher(g, cfg.Match)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConfig, err)
	}
	field, err := g.Field()
	if err != nil {
		return nil, err
	}
	seen := deviceMap{g, field}
	out := make([]Resolution, len(anchors))
	for i, a := range anchors {
		out[i] = resolve(m, seen, cfg, a)
		if out[i].Err != nil {
			out[i] = Resolution{ID: a.ID, Err: fmt.Errorf("%s: %w", a.ID, out[i].Err)}
		}
	}
	return out, nil
}

// deviceMap is what the device has seen: the grid of its keyframes and the
// distance from any point to the nearest surface in it.
type deviceMap struct {
	grid  *grid.Grid
	field *grid.Field
}

// contradiction is the share of a scan's returns that, with the scan at pose,
// lie where the device saw empty floor, farther than reach from any surface
// it saw. Returns in space the device has not seen contradict nothing.
func (d deviceMap) contradiction(scan spatial.Scan, pose geom.Pose2, reach float64) float64 {
	points := scan.Points()
	n := 0
	for _, p := range points {
		if p = pose.Apply(p); d.grid.StateAt(p) == grid.Free && d.field.Distance(p) > reach {
			n++
		}
	}
	return float64(n) / float64(max(len(points), 1))
}

func resolve(m *scanmatch.Matcher, seen deviceMap, cfg Config, a spatial.Anchor) Resolution {
	patches, err := decodeSignature(a.Signature)
	if err != nil {
		return Resolution{Err: err}
	}
	var best Resolution
	var poses []geom.Pose2
	refused := "no signature scan was searched"
	for i, p := range patches {
		scan := p.scan.Expand(spatial.Header{Device: "anchor", Sensor: signatureSensor, Time: spatial.Stamp(1)}, cfg.RangeSigma)
		found, err := m.Global(scan)
		r := found.Best
		switch {
		case errors.Is(err, scanmatch.ErrAmbiguous):
			return Resolution{Err: fmt.Errorf("%w: scan %d scores %.2f here and %.2f at %.1f m from it",
				ErrAmbiguous, i, r.Score, found.Rivals[0].Score, found.Rivals[0].Pose.Pos().Dist(r.Pose.Pos()))}
		case err != nil:
			refused = fmt.Sprintf("scan %d: %v (score %.2f)", i, err, r.Score)
			continue
		case !r.Converged || r.Degenerate:
			refused = fmt.Sprintf("scan %d: converged %t, direction undetermined %t", i, r.Converged, r.Degenerate)
			continue
		}
		against := seen.contradiction(scan, r.Pose, cfg.ContradictionDistance)
		if against > cfg.MaxContradiction {
			refused = fmt.Sprintf("scan %d: %.0f %% of it lies where the device saw empty floor (score %.2f)", i, 100*against, r.Score)
			continue
		}
		pose := r.Pose.Compose(p.anchor).Normalized()
		poses = append(poses, pose)
		if r.Score > best.Score {
			cov := geom.ComposeCov(r.Pose, r.Covariance, p.anchor, geom.Mat3{})
			best = Resolution{Anchor: a, Covariance: cov, Score: r.Score}
			best.Anchor.Location = spatial.At(pose.Pos().Lift(a.PositionM[1]), geom.PositionCov(cov).MaxSigma())
			best.Anchor.Yaw = pose.Yaw
		}
	}
	if len(poses) == 0 {
		return Resolution{Err: fmt.Errorf("%w: %s", ErrNoMatch, refused)}
	}
	at := best.Anchor.Pose2()
	for _, p := range poses {
		if d, turn := p.Pos().Dist(at.Pos()), math.Abs(geom.AngleDiff(p.Yaw, at.Yaw)); d > cfg.Match.DistinctTranslation || turn > cfg.Match.DistinctYaw {
			return Resolution{Err: fmt.Errorf("%w: its scans put it %.2f m and %.2f rad apart", ErrAmbiguous, d, turn)}
		}
	}
	best.ID, best.Matched = a.ID, len(poses)
	return best
}
