package cslam

import (
	"errors"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/posegraph"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/scanmatch"
)

var (
	// ErrConfig reports a Config outside its documented ranges.
	ErrConfig = errors.New("cslam: invalid config")
	// ErrUncalibrated reports a scan from a lidar whose mount and clock
	// offset were never supplied with Mapper.SetCalibration.
	ErrUncalibrated = errors.New("cslam: lidar has no calibration")
	// ErrOutOfOrder reports a measurement older than one already ingested
	// from the same device and kind.
	ErrOutOfOrder = errors.New("cslam: measurement out of order")
)

// Config holds every threshold and noise assumption. A zero field takes its
// default; DefaultConfig lists them.
type Config struct {
	// KeyframeDistance (metres) and KeyframeAngle (radians) are the odometry
	// motion since the last keyframe at which a scan becomes a keyframe.
	// Defaults 0.5 and 0.5.
	KeyframeDistance float64
	KeyframeAngle    float64
	// SubmapKeyframes is the number of keyframes in one submap; consecutive
	// submaps overlap by half. Even, at least 4. Default 30, about 12 m of
	// travel: long enough to match against, short enough that front-end
	// drift inside one submap stays below a map cell.
	SubmapKeyframes int
	// OdometryTransFloor (metres) and OdometryYawFloor (radians) are 1σ
	// errors added to every odometry step on top of the noise the odometry
	// reports: wheel slip and timestamp interpolation do not shrink with the
	// step. Defaults 0.005 and 0.002.
	OdometryTransFloor float64
	OdometryYawFloor   float64
	// MatchGateM (metres) and MatchGateRad (radians) bound how far a scan
	// match may move a keyframe from its odometry prediction; a match beyond
	// them settled in a wrong minimum and is ignored. Defaults 0.25 and 0.15,
	// half the basin scanmatch.Matcher.Refine is good for.
	MatchGateM   float64
	MatchGateRad float64

	// ClosureEvery attempts closures on every n-th keyframe of a robot.
	// Default 2.
	ClosureEvery int
	// ClosureRadius is the distance in metres from a keyframe to the nearest
	// keyframe of a submap, in the current estimate, within which the submap
	// is a closure candidate. Default 3.
	ClosureRadius float64
	// ClosureWindow (metres) and ClosureAngle (radians) are the half-widths
	// of the correlative search around the predicted pose. They must cover
	// the drift between two robots, or around one loop, since their last
	// closure. Defaults 1.5 and 0.3.
	ClosureWindow float64
	ClosureAngle  float64
	// ClosureMinScore is the lowest scan-match score of a closure. Default
	// 0.6, above the score of a match that shares only half the scene.
	ClosureMinScore float64
	// ClosureSigmaM (metres) and ClosureSigmaRad (radians) are 1σ errors
	// added to every closure on top of the matcher's covariance: a submap is
	// built from keyframes that each carry a map-cell and a lidar-mount
	// error, which the match of one scan does not see. Defaults 0.03 and 0.01.
	ClosureSigmaM   float64
	ClosureSigmaRad float64
	// LoopMinKeyframes is how many keyframes ago a robot's own submap must
	// have been completed to be a loop-closure candidate. Default 30.
	LoopMinKeyframes int
	// GlobalEvery attempts a global match against robots in unrelated frames
	// on every n-th closure attempt. Default 2.
	GlobalEvery int
	// GlobalBeams is the number of evenly spaced beams of a scan used for a
	// global match, whose cost grows with them; the pose found is then
	// refined and scored with every beam. Default 120.
	GlobalBeams int
	// ConsistencyTranslation (metres) and ConsistencyYaw (radians) bound the
	// disagreement of two closures that still count as implying the same
	// transform between two robots' frames (or the same loop correction).
	// They must cover front-end drift over ConsistencyKeyframes. Defaults 0.3
	// and 0.1.
	ConsistencyTranslation float64
	ConsistencyYaw         float64
	// ConsistencyKeyframes is how many keyframes a closure stays available
	// as support for later ones. Default 40.
	ConsistencyKeyframes int
	// MinConsistent is the number of keyframes whose closures must agree
	// before any of them enters the graph. Default 2.
	MinConsistent int
	// MinAlignConsistent is the same for the closures that relate two frames
	// for the first time, where no prediction narrows the search. Default 3.
	MinAlignConsistent int
	// OutlierProbability is the χ² quantile beyond which a closure is removed
	// after optimisation. Default 0.999.
	OutlierProbability float64
	// OptimizeEvery is the number of keyframes after which a graph with new
	// closures is optimised. Default 10.
	OptimizeEvery int
	// ReorderWindow bounds how far behind the newest robot another robot's
	// scans may lag and still be waited for: Solve holds back keyframes newer
	// than the last scan of any robot within the window, so that a keyframe
	// arriving a moment later does not force the back end to be rebuilt.
	// A robot silent for longer is not waited for. Default 1 s.
	ReorderWindow time.Duration
	// SubmapCache is the number of finished submaps kept rasterised for
	// closure search; others are rebuilt from their keyframes on demand.
	// Default 32.
	SubmapCache int

	// DefaultFloor is the floor of a device until Mapper.SetFloor names
	// another. Default "ground".
	DefaultFloor spatial.FloorID

	// Scan configures scan matching and, through its Grid field, the submap
	// rasters.
	Scan scanmatch.Config
	// Graph configures the optimiser. Kernel defaults to posegraph.KernelHuber.
	Graph posegraph.Config
}

// DefaultConfig returns the defaults documented on Config.
func DefaultConfig() Config {
	return Config{
		KeyframeDistance: 0.5, KeyframeAngle: 0.5, SubmapKeyframes: 30,
		OdometryTransFloor: 0.005, OdometryYawFloor: 0.002,
		MatchGateM: 0.25, MatchGateRad: 0.15,
		ClosureEvery: 2, ClosureRadius: 3, ClosureWindow: 1.5, ClosureAngle: 0.3,
		ClosureMinScore: 0.6, ClosureSigmaM: 0.03, ClosureSigmaRad: 0.01,
		LoopMinKeyframes: 30, GlobalEvery: 2, GlobalBeams: 120,
		ConsistencyTranslation: 0.3, ConsistencyYaw: 0.1, ConsistencyKeyframes: 40,
		MinConsistent: 2, MinAlignConsistent: 3,
		OutlierProbability: 0.999, OptimizeEvery: 10, SubmapCache: 32,
		ReorderWindow: time.Second,
		DefaultFloor:  "ground",
		Scan:          scanmatch.DefaultConfig(),
		Graph:         posegraph.Config{Kernel: posegraph.KernelHuber},
	}
}

func (c Config) withDefaults() (Config, error) {
	d := DefaultConfig()
	for _, f := range []struct{ v, def *float64 }{
		{&c.KeyframeDistance, &d.KeyframeDistance}, {&c.KeyframeAngle, &d.KeyframeAngle},
		{&c.OdometryTransFloor, &d.OdometryTransFloor}, {&c.OdometryYawFloor, &d.OdometryYawFloor},
		{&c.MatchGateM, &d.MatchGateM}, {&c.MatchGateRad, &d.MatchGateRad},
		{&c.ClosureRadius, &d.ClosureRadius}, {&c.ClosureWindow, &d.ClosureWindow}, {&c.ClosureAngle, &d.ClosureAngle},
		{&c.ClosureMinScore, &d.ClosureMinScore}, {&c.ClosureSigmaM, &d.ClosureSigmaM}, {&c.ClosureSigmaRad, &d.ClosureSigmaRad},
		{&c.ConsistencyTranslation, &d.ConsistencyTranslation}, {&c.ConsistencyYaw, &d.ConsistencyYaw},
		{&c.OutlierProbability, &d.OutlierProbability},
	} {
		if *f.v == 0 {
			*f.v = *f.def
		}
		if !(*f.v > 0) {
			return c, fmt.Errorf("%w: negative or non-finite field", ErrConfig)
		}
	}
	for _, f := range []struct{ v, def *int }{
		{&c.SubmapKeyframes, &d.SubmapKeyframes}, {&c.ClosureEvery, &d.ClosureEvery},
		{&c.LoopMinKeyframes, &d.LoopMinKeyframes}, {&c.GlobalEvery, &d.GlobalEvery}, {&c.GlobalBeams, &d.GlobalBeams},
		{&c.ConsistencyKeyframes, &d.ConsistencyKeyframes}, {&c.MinConsistent, &d.MinConsistent},
		{&c.MinAlignConsistent, &d.MinAlignConsistent}, {&c.OptimizeEvery, &d.OptimizeEvery}, {&c.SubmapCache, &d.SubmapCache},
	} {
		if *f.v == 0 {
			*f.v = *f.def
		}
		if *f.v < 0 {
			return c, fmt.Errorf("%w: negative count", ErrConfig)
		}
	}
	if c.SubmapKeyframes < 4 || c.SubmapKeyframes%2 != 0 {
		return c, fmt.Errorf("%w: SubmapKeyframes %d must be even and at least 4", ErrConfig, c.SubmapKeyframes)
	}
	if c.OutlierProbability >= 1 || c.ClosureMinScore > 1 {
		return c, fmt.Errorf("%w: OutlierProbability and ClosureMinScore are below 1", ErrConfig)
	}
	if c.ReorderWindow == 0 {
		c.ReorderWindow = d.ReorderWindow
	}
	if c.ReorderWindow < 0 {
		return c, fmt.Errorf("%w: negative ReorderWindow", ErrConfig)
	}
	if c.DefaultFloor == "" {
		c.DefaultFloor = d.DefaultFloor
	}
	if c.Graph.Kernel == posegraph.KernelNone {
		c.Graph.Kernel = posegraph.KernelHuber
	}
	return c, nil
}
