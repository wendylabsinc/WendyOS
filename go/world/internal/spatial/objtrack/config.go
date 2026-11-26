package objtrack

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// Errors returned by this package, for errors.Is.
var (
	// ErrConfig is a Config value outside its range.
	ErrConfig = errors.New("objtrack: invalid config")
	// ErrInvalid is a malformed input: a non-finite detection, a covariance
	// that is not positive definite, an invalid payload.
	ErrInvalid = errors.New("objtrack: invalid input")
	// ErrNoCalibration means the sensor has no valid calibration, so its
	// measurement cannot be placed in the site frame.
	ErrNoCalibration = errors.New("objtrack: no valid calibration for the sensor")
	// ErrNoPose means the carrier of a mounted sensor has no pose close
	// enough in time to the measurement.
	ErrNoPose = errors.New("objtrack: no device pose at the measurement time")
	// ErrNoMap means there is no floor, or no static map of it, to interpret
	// the measurement against: a scan cannot be split into static and moving
	// returns, a foot pixel has no floor plane to land on.
	ErrNoMap = errors.New("objtrack: no floor or static map for the measurement")
	// ErrStale means the measurement is older than Config.MaxLateness
	// relative to what the tracker has already seen.
	ErrStale = errors.New("objtrack: measurement too old")
)

// Config holds every threshold and noise assumption. A zero field takes its
// default, so Config{} is DefaultConfig().
type Config struct {
	// StaticMarginM is how far from an occupied map cell a point still
	// counts as that cell's structure before any pose uncertainty is added:
	// the map's own blur, one cell plus the mapper's pose error. Default 0.10.
	StaticMarginM float64
	// StaticSigmas widens the margin of a lidar return by this many standard
	// deviations of its position (range noise, sensor position, range times
	// heading error). Default 3.
	StaticSigmas float64
	// ClusterGapM joins consecutive moving returns into one cluster when
	// they are closer than this (or than 2.5 beam spacings at their range,
	// whichever is larger). Default 0.20, under the gap between two people
	// who are not touching.
	ClusterGapM float64
	// MinClusterPoints is the fewest returns that make a detection. Default 3:
	// one or two returns are what noise at a wall edge leaves.
	MinClusterPoints int
	// MinRadiusM discards clusters narrower than this: the sliver of a body
	// or of a wall end that shows past an occluding edge. Default 0.05,
	// thinner than a leg.
	MinRadiusM float64
	// MaxRadiusM discards clusters wider than this: structure the map did
	// not know, not a trackable body. Default 1.0.
	MaxRadiusM float64
	// DepthFactor places a lidar detection's centre this many cluster
	// half-widths behind the centroid of its returns, along the view ray.
	// Default π/4, exact for a cylinder.
	DepthFactor float64
	// ShapeSigma is the 1σ uncertainty of a body's centre along the view
	// ray as a fraction of its radius, and twice that across the ray: a
	// cluster or a box shows the near surface of a shape whose depth is not
	// known (a box seen end-on reaches 1.7 half-widths back, side-on 0.6).
	// Default 0.5.
	ShapeSigma float64

	// FootPixelSigma is the 1σ noise of an object detection's foot pixel.
	// Default 2 px, a box-bottom detector at 720p; the frame's own
	// PixelSigma is used when it is larger.
	FootPixelSigma float64
	// MinConfidence drops camera detections below it. Default 0.5.
	MinConfidence float64
	// MaxDetectionSigmaM drops a detection whose 1σ position error along its
	// worst axis exceeds it: a foot pixel near the horizon does not locate a
	// body. Default 0.5, a body width.
	MaxDetectionSigmaM float64

	// ProcessNoise is the power spectral density of the acceleration a
	// tracked body is assumed to undergo, in m²/s³ (continuous white-noise
	// acceleration). Default 4: a walker reverses a velocity component of
	// 1.3 m/s within about half a second, a ≈ 2.7 m/s², q = a²·τ.
	ProcessNoise float64
	// InitialSpeedSigma is the per-axis 1σ velocity of a new track, m/s.
	// Default 1.5, a brisk walk in an unknown direction.
	InitialSpeedSigma float64
	// MaxSpeedMPS bounds how far from its last observed position a track
	// may be re-acquired: MaxSpeedMPS times the time since, plus
	// MinSeparationM. Default 3.
	MaxSpeedMPS float64
	// GateProbability is the chi-square confidence at which a tentative
	// track's travel must be significant. Default 0.99.
	GateProbability float64
	// ClutterDensity is the expected number of false or new detections per
	// square metre in one sensor sample. A detection is given to a track
	// only when the track explains it better than that: this is the
	// association gate. Default 1e-3, one spurious object in a 30 m by 30 m
	// view.
	ClutterDensity float64
	// ClassConfusion is the share of detections whose class is not the
	// body's. A detection whose class disagrees with a track's votes is that
	// much less likely to belong to it. Default 0.1.
	ClassConfusion float64

	// ConfirmHits is the fewest detections a confirmed track has. Default 3.
	ConfirmHits int
	// MinTravelM is how far beyond its own diameter one sensor must see a
	// track travel from where it first saw it, by way of a detection half as
	// far out, before the track is confirmed as moving. Each of those
	// distances must also be significant at GateProbability against the
	// covariances of the two detections compared. Default 0.5.
	MinTravelM float64
	// StaticAfter is how long a track may be watched without confirming
	// before it is taken for structure and never confirmed: the time the
	// slowest mover of interest needs to show its travel. Default 5 s, a
	// metre and a half at 0.3 m/s.
	StaticAfter time.Duration
	// MinSeparationM is the closest two bodies' centres can be. A track
	// within it of a device that reports its first pose is that device, and
	// it is the slack added to MaxSpeedMPS times the time since. Default
	// 0.5, two 0.25 m bodies touching.
	MinSeparationM float64
	// CoastAfter is how long after its last observation a confirmed track is
	// reported as coasting. Default 500 ms.
	CoastAfter time.Duration
	// DeleteAfter is how long a confirmed track is predicted without
	// observations before it is deleted. Default 3 s.
	DeleteAfter time.Duration
	// TentativeTimeout is how long a tentative track survives without
	// observations. Default 1 s.
	TentativeTimeout time.Duration

	// LevelToleranceM is the elevation difference within which floors share
	// a level (one floor plane, one static map) and within which a detection
	// may update a track. Default 1.8, half a storey.
	LevelToleranceM float64
	// MaxPoseAge is how far a device pose is extrapolated to the time of a
	// measurement from that device. Default 250 ms.
	MaxPoseAge time.Duration
	// MaxLateness is how far behind the newest measurement another may
	// arrive and still be used. Default 1 s.
	MaxLateness time.Duration
	// MaxTracks bounds live tracks; detections that would start more are
	// dropped. Default 512.
	MaxTracks int
}

// DefaultConfig returns the documented defaults.
func DefaultConfig() Config {
	return Config{
		StaticMarginM: 0.10, StaticSigmas: 3, ClusterGapM: 0.20, MinClusterPoints: 3, MinRadiusM: 0.05, MaxRadiusM: 1.0,
		DepthFactor: math.Pi / 4, ShapeSigma: 0.5,
		FootPixelSigma: 2, MinConfidence: 0.5, MaxDetectionSigmaM: 0.5,
		ProcessNoise: 4, InitialSpeedSigma: 1.5, MaxSpeedMPS: 3, GateProbability: 0.99, ClutterDensity: 1e-3, ClassConfusion: 0.1,
		ConfirmHits: 3, MinTravelM: 0.5, MinSeparationM: 0.5,
		StaticAfter: 5 * time.Second, CoastAfter: 500 * time.Millisecond, DeleteAfter: 3 * time.Second, TentativeTimeout: time.Second,
		LevelToleranceM: 1.8, MaxPoseAge: 250 * time.Millisecond, MaxLateness: time.Second, MaxTracks: 512,
	}
}

// withDefaults fills zero fields and validates the result.
func (c Config) withDefaults() (Config, error) {
	d := DefaultConfig()
	f := func(v *float64, def float64) {
		if *v == 0 {
			*v = def
		}
	}
	n := func(v *int, def int) {
		if *v == 0 {
			*v = def
		}
	}
	t := func(v *time.Duration, def time.Duration) {
		if *v == 0 {
			*v = def
		}
	}
	f(&c.StaticMarginM, d.StaticMarginM)
	f(&c.StaticSigmas, d.StaticSigmas)
	f(&c.ClusterGapM, d.ClusterGapM)
	n(&c.MinClusterPoints, d.MinClusterPoints)
	f(&c.MinRadiusM, d.MinRadiusM)
	f(&c.MaxRadiusM, d.MaxRadiusM)
	f(&c.DepthFactor, d.DepthFactor)
	f(&c.ShapeSigma, d.ShapeSigma)
	f(&c.FootPixelSigma, d.FootPixelSigma)
	f(&c.MinConfidence, d.MinConfidence)
	f(&c.MaxDetectionSigmaM, d.MaxDetectionSigmaM)
	f(&c.ProcessNoise, d.ProcessNoise)
	f(&c.InitialSpeedSigma, d.InitialSpeedSigma)
	f(&c.MaxSpeedMPS, d.MaxSpeedMPS)
	f(&c.GateProbability, d.GateProbability)
	f(&c.ClutterDensity, d.ClutterDensity)
	f(&c.ClassConfusion, d.ClassConfusion)
	n(&c.ConfirmHits, d.ConfirmHits)
	f(&c.MinTravelM, d.MinTravelM)
	f(&c.MinSeparationM, d.MinSeparationM)
	t(&c.StaticAfter, d.StaticAfter)
	t(&c.CoastAfter, d.CoastAfter)
	t(&c.DeleteAfter, d.DeleteAfter)
	t(&c.TentativeTimeout, d.TentativeTimeout)
	f(&c.LevelToleranceM, d.LevelToleranceM)
	t(&c.MaxPoseAge, d.MaxPoseAge)
	t(&c.MaxLateness, d.MaxLateness)
	n(&c.MaxTracks, d.MaxTracks)

	for _, v := range []float64{
		c.StaticMarginM, c.StaticSigmas, c.ClusterGapM, c.MinRadiusM, c.MaxRadiusM, c.DepthFactor, c.ShapeSigma, c.FootPixelSigma,
		c.MinConfidence, c.MaxDetectionSigmaM, c.ProcessNoise, c.InitialSpeedSigma, c.MaxSpeedMPS, c.MinTravelM,
		c.MinSeparationM, c.LevelToleranceM, c.ClutterDensity, c.ClassConfusion,
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return c, fmt.Errorf("%w: values must be finite and non-negative", ErrConfig)
		}
	}
	if c.MinConfidence > 1 || c.ClassConfusion >= 0.5 || !(c.GateProbability > 0 && c.GateProbability < 1) || c.MinClusterPoints < 1 || c.ConfirmHits < 1 || c.MaxTracks < 1 ||
		c.StaticAfter < 0 || c.CoastAfter < 0 || c.DeleteAfter < c.CoastAfter || c.TentativeTimeout < 0 || c.MaxPoseAge < 0 || c.MaxLateness < 0 {
		return c, fmt.Errorf("%w: probability, count or duration out of range", ErrConfig)
	}
	return c, nil
}
