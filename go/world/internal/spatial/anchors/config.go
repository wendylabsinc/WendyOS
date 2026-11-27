package anchors

import (
	"errors"
	"fmt"
	"math"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/scanmatch"
)

var (
	// ErrConfig reports a Config value outside its documented range.
	ErrConfig = errors.New("anchors: invalid config")
	// ErrCorrupt reports a journal damaged somewhere other than its tail.
	// The store refuses to open rather than drop the records behind it.
	ErrCorrupt = errors.New("anchors: journal is corrupt")
	// ErrNotFound reports an anchor identifier the store does not hold.
	ErrNotFound = errors.New("anchors: unknown anchor")
	// ErrNoKeyframe reports a placement the map has no keyframe for.
	ErrNoKeyframe = errors.New("anchors: no keyframe to bind to")
	// ErrSignature reports a signature this package cannot decode.
	ErrSignature = errors.New("anchors: invalid signature")
	// ErrInsufficient reports device data that cannot support a search: no
	// keyframes, or keyframes without a single return.
	ErrInsufficient = errors.New("anchors: too little device data")
	// ErrNoMatch reports an anchor whose signature was not found in the
	// device's scans: no pose reached the minimum score, the match left a
	// direction undetermined, or it put surfaces across observed free space.
	ErrNoMatch = errors.New("anchors: signature not found in the device's scans")
	// ErrAmbiguous reports an anchor whose signature fits more than one
	// place, or whose match contradicts another match of the same call.
	ErrAmbiguous = errors.New("anchors: signature fits more than one place")
)

// Config holds every threshold and noise assumption. A zero field takes its
// default.
type Config struct {
	// MaxBindings is the number of keyframes an anchor is bound to: the one
	// it was placed against and its nearest neighbours in sequence. Each
	// contributes one scan to the signature. Default 3, at most
	// spatial.MaxAnchorBindings.
	MaxBindings int
	// BindRadius is how far in metres a sequence neighbour may be from the
	// keyframe an anchor was placed against and still be bound, and how far
	// Bind looks for a keyframe around a site pose. Default 3.
	BindRadius float64
	// MinShiftM (metres) and MinShiftYaw (radians) are the changes of an
	// anchor's site pose below which a keyframe update creates no revision.
	// Defaults 0.001 and 0.001.
	MinShiftM   float64
	MinShiftYaw float64

	// RangeSigma is the 1σ range noise in metres assumed for keyframe scans
	// and signature scans, which do not carry their own. Default 0.02.
	RangeSigma float64
	// Match configures the signature search and its refinement. Zero fields
	// take the scanmatch defaults except four, chosen because the search
	// covers the whole device map at every heading: MaxPoints 120 (a third
	// of a typical sweep), AngularStep 0.01 rad (one step moves a return
	// 10 m away by one Sigma; the pose is refined afterwards), MaxRivals 1
	// (one rival is enough to refuse) and MinScore 0.5 (at least half of the
	// signature scan must lie on surfaces the device has seen).
	// DistinctTranslation and DistinctYaw also decide whether two matches of
	// one call agree.
	Match scanmatch.Config
	// ContradictionDistance is how far in metres a signature return must be
	// from every surface of the device's map, in a cell the device saw
	// empty, to contradict the match. Default 0.2.
	ContradictionDistance float64
	// MaxContradiction is the largest share of signature returns that may
	// contradict the device's map. Default 0.15: two people or robots 2 m
	// away take 8 % of a sweep, a wall across open floor takes more. (In the
	// simulated office a right match has 0 to 5 % and a room matched into a
	// wider one 24 to 33 %.)
	MaxContradiction float64

	// FrameDistance (metres) and FrameYaw (radians) are the motion after
	// which a Resolver takes the next keyframe. Defaults 0.5 and 0.5.
	FrameDistance float64
	FrameYaw      float64
	// Window is the number of most recent keyframes a Resolver keeps: the
	// device's map for the search. More of them see around more furniture;
	// fewer hold less of what has moved since and less accumulated drift.
	// Default 12, a turn and a few metres either side of it.
	Window int
}

// DefaultConfig returns the defaults documented on Config.
func DefaultConfig() Config {
	m := scanmatch.DefaultConfig()
	m.MaxPoints, m.AngularStep, m.MaxRivals, m.MinScore = 120, 0.01, 1, 0.5
	return Config{
		MaxBindings: 3, BindRadius: 3, MinShiftM: 0.001, MinShiftYaw: 0.001,
		RangeSigma: 0.02, Match: m, ContradictionDistance: 0.2, MaxContradiction: 0.15,
		FrameDistance: 0.5, FrameYaw: 0.5, Window: 12,
	}
}

func (c Config) withDefaults() (Config, error) {
	d := DefaultConfig()
	for _, f := range []struct{ v, def *float64 }{
		{&c.BindRadius, &d.BindRadius}, {&c.MinShiftM, &d.MinShiftM}, {&c.MinShiftYaw, &d.MinShiftYaw},
		{&c.RangeSigma, &d.RangeSigma}, {&c.ContradictionDistance, &d.ContradictionDistance}, {&c.MaxContradiction, &d.MaxContradiction},
		{&c.FrameDistance, &d.FrameDistance}, {&c.FrameYaw, &d.FrameYaw},
		{&c.Match.AngularStep, &d.Match.AngularStep}, {&c.Match.MinScore, &d.Match.MinScore},
	} {
		if *f.v == 0 {
			*f.v = *f.def
		}
		if !(*f.v > 0) || math.IsInf(*f.v, 0) {
			return c, fmt.Errorf("%w: fields must be positive and finite", ErrConfig)
		}
	}
	for _, f := range []struct{ v, def *int }{
		{&c.MaxBindings, &d.MaxBindings}, {&c.Window, &d.Window}, {&c.Match.MaxPoints, &d.Match.MaxPoints}, {&c.Match.MaxRivals, &d.Match.MaxRivals},
	} {
		if *f.v == 0 {
			*f.v = *f.def
		}
		if *f.v < 0 {
			return c, fmt.Errorf("%w: counts must be positive", ErrConfig)
		}
	}
	if c.MaxBindings > spatial.MaxAnchorBindings {
		return c, fmt.Errorf("%w: MaxBindings %d, limit %d", ErrConfig, c.MaxBindings, spatial.MaxAnchorBindings)
	}
	// The remaining Match fields are defaulted and checked by their owners.
	g, err := grid.New(c.Match.Grid)
	if err != nil {
		return c, fmt.Errorf("%w: %w", ErrConfig, err)
	}
	m, err := scanmatch.NewMatcher(g, c.Match)
	if err != nil {
		return c, fmt.Errorf("%w: %w", ErrConfig, err)
	}
	c.Match = m.Config()
	c.Match.Grid = g.Config()
	return c, nil
}
