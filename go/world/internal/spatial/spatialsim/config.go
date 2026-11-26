package spatialsim

import (
	"errors"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// Config holds every noise, rate and detection assumption of the simulator.
// Sigmas are 1σ. A rate of zero removes that sensor from every device.
type Config struct {
	LidarBeams    int     // beams per 360° sweep. Default 360.
	LidarHz       float64 // sweeps per second. Default 10.
	LidarRangeMin float64 // metres; a closer return reads RangeInvalid. Default 0.1.
	LidarRangeMax float64 // metres; a farther return reads RangeNoReturn. Default 20.
	LidarSigma    float64 // range noise, metres. Default 0.015.
	LidarDropout  float64 // probability that a beam reads RangeInvalid. Default 0.01.

	// Odometry error has two parts of equal size: a bias fixed per device
	// (wheel scale, heading drift per metre, turn scale) drawn once with the
	// sigma below, and white noise whose variance grows with the motion and
	// reaches the sigma after one metre or one radian.
	OdomHz            float64 // samples per second. Default 20.
	OdomTransPerMetre float64 // metres per metre travelled. Default 0.02 (2 %).
	OdomYawPerMetre   float64 // radians per metre travelled. Default 0.002.
	OdomYawPerRadian  float64 // radians per radian turned. Default 0.01.

	ImuHz       float64 // samples per second. Default 50.
	GyroSigma   float64 // yaw-rate noise, rad/s. Default 0.005.
	GyroBias    float64 // sigma of the constant per-device yaw-rate bias, rad/s. Default 0.002.
	AccelSigma  float64 // per-axis accelerometer noise, m/s². Default 0.05.
	JoltAccel   float64 // peak acceleration when a fixed camera is bumped, m/s². Default 6.
	JoltYawRate float64 // peak yaw rate of the same jolt, rad/s. Default 1.

	BaroHz          float64       // samples per second. Default 5.
	BaroSigma       float64       // altitude noise, metres. Default 0.1.
	BaroDrift       float64       // amplitude of the slow altitude drift, metres. Default 1.
	BaroDriftPeriod time.Duration // period of that drift. Default 20 min.

	GnssHz    float64 // fixes per second while outdoors. Default 1.
	GnssSigma float64 // white position noise per horizontal axis, metres. Default 1.5.
	GnssBias  float64 // amplitude of the slowly varying position error per axis, metres. Default 0.3.

	CameraHz           float64 // frames per second. Default 5.
	PixelSigma         float64 // per-axis noise of fiducial and target-corner pixels. Default 0.5.
	ObjectPixelSigma   float64 // per-axis noise of object foot pixels and box edges. Default 2.
	DetectRange        float64 // metres beyond which a camera detects nothing. Default 15.
	DetectMinFacing    float64 // smallest cosine between a wall tag's normal and the ray to the camera. Default 0.2.
	FiducialMissRate   float64 // probability a visible marker point or wall tag is missed. Default 0.02.
	ObjectMissRate     float64 // probability a visible object is missed. Default 0.05.
	FalsePositiveRate  float64 // probability a frame carries one spurious object. Default 0.02.
	ClassConfusionRate float64 // probability an object is reported with another class. Default 0.05.

	UwbHz        float64 // range rounds per second. Default 5.
	UwbSigma     float64 // line-of-sight range noise, metres. Default 0.1.
	UwbNlosSigma float64 // scale of the positive bias when a wall blocks the path, metres. Default 0.5.
	UwbRange     float64 // metres beyond which no range is produced. Default 30.

	MicHz        float64 // bearing estimates per second while a source is audible. Default 5.
	BearingSigma float64 // azimuth noise, radians. Default 0.05.
	MicRange     float64 // metres beyond which a source is not heard. Default 20.

	GasHz    float64 // samples per second. Default 1.
	GasSigma float64 // concentration noise, ppm. Default 5.

	// ClockOffsetMax bounds the per-sensor clock offset. Offsets are drawn
	// uniformly with magnitude in [10 ms, ClockOffsetMax] and a random sign;
	// zero disables them. Default 60 ms.
	ClockOffsetMax time.Duration
}

// DefaultConfig is the noise model the feature targets are stated for.
func DefaultConfig() Config {
	return Config{
		LidarBeams: 360, LidarHz: 10, LidarRangeMin: 0.1, LidarRangeMax: 20, LidarSigma: 0.015, LidarDropout: 0.01,
		OdomHz: 20, OdomTransPerMetre: 0.02, OdomYawPerMetre: 0.002, OdomYawPerRadian: 0.01,
		ImuHz: 50, GyroSigma: 0.005, GyroBias: 0.002, AccelSigma: 0.05, JoltAccel: 6, JoltYawRate: 1,
		BaroHz: 5, BaroSigma: 0.1, BaroDrift: 1, BaroDriftPeriod: 20 * time.Minute,
		GnssHz: 1, GnssSigma: 1.5, GnssBias: 0.3,
		CameraHz: 5, PixelSigma: 0.5, ObjectPixelSigma: 2, DetectRange: 15, DetectMinFacing: 0.2,
		FiducialMissRate: 0.02, ObjectMissRate: 0.05, FalsePositiveRate: 0.02, ClassConfusionRate: 0.05,
		UwbHz: 5, UwbSigma: 0.1, UwbNlosSigma: 0.5, UwbRange: 30,
		MicHz: 5, BearingSigma: 0.05, MicRange: 20,
		GasHz: 1, GasSigma: 5,
		ClockOffsetMax: 60 * time.Millisecond,
	}
}

// Noiseless returns c with every noise, bias, drift, dropout, miss and
// confusion term set to zero. Rates, ranges and clock offsets are kept, and
// the jolt of a bumped camera stays because it is signal, not noise.
func (c Config) Noiseless() Config {
	c.LidarSigma, c.LidarDropout = 0, 0
	c.OdomTransPerMetre, c.OdomYawPerMetre, c.OdomYawPerRadian = 0, 0, 0
	c.GyroSigma, c.GyroBias, c.AccelSigma = 0, 0, 0
	c.BaroSigma, c.BaroDrift = 0, 0
	c.GnssSigma, c.GnssBias = 0, 0
	c.PixelSigma, c.ObjectPixelSigma = 0, 0
	c.FiducialMissRate, c.ObjectMissRate, c.FalsePositiveRate, c.ClassConfusionRate = 0, 0, 0, 0
	c.UwbSigma, c.UwbNlosSigma = 0, 0
	c.BearingSigma, c.GasSigma = 0, 0
	return c
}

func (c Config) validate() error {
	nonNegative := []float64{
		c.LidarHz, c.LidarRangeMin, c.LidarSigma, c.LidarDropout, c.OdomHz, c.OdomTransPerMetre, c.OdomYawPerMetre, c.OdomYawPerRadian,
		c.ImuHz, c.GyroSigma, c.GyroBias, c.AccelSigma, c.JoltAccel, c.JoltYawRate, c.BaroHz, c.BaroSigma, c.BaroDrift,
		c.GnssHz, c.GnssSigma, c.GnssBias, c.CameraHz, c.PixelSigma, c.ObjectPixelSigma, c.DetectRange, c.DetectMinFacing,
		c.FiducialMissRate, c.ObjectMissRate, c.FalsePositiveRate, c.ClassConfusionRate,
		c.UwbHz, c.UwbSigma, c.UwbNlosSigma, c.UwbRange, c.MicHz, c.BearingSigma, c.MicRange, c.GasHz, c.GasSigma,
	}
	for _, v := range nonNegative {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return errors.New("spatialsim: config values must be finite and non-negative")
		}
	}
	for _, p := range []float64{c.LidarDropout, c.FiducialMissRate, c.ObjectMissRate, c.FalsePositiveRate, c.ClassConfusionRate, c.DetectMinFacing} {
		if p > 1 {
			return errors.New("spatialsim: config probabilities must not exceed 1")
		}
	}
	if c.LidarBeams < 8 || c.LidarBeams > spatial.MaxScanBeams || !(c.LidarRangeMax > c.LidarRangeMin) {
		return errors.New("spatialsim: invalid lidar beam count or range")
	}
	if c.BaroDriftPeriod < 0 || c.ClockOffsetMax < 0 {
		return errors.New("spatialsim: config durations must be non-negative")
	}
	for _, hz := range []float64{c.LidarHz, c.OdomHz, c.ImuHz, c.BaroHz, c.GnssHz, c.CameraHz, c.UwbHz, c.MicHz, c.GasHz} {
		if hz > 1000 {
			return errors.New("spatialsim: sensor rates above 1 kHz are not supported")
		}
	}
	return nil
}

// rate returns the configured sample rate of a sensor kind.
func (c Config) rate(kind spatial.SensorKind) float64 {
	switch kind {
	case spatial.KindLidar2D:
		return c.LidarHz
	case spatial.KindOdometry:
		return c.OdomHz
	case spatial.KindImu:
		return c.ImuHz
	case spatial.KindBaro:
		return c.BaroHz
	case spatial.KindGnss:
		return c.GnssHz
	case spatial.KindCamera:
		return c.CameraHz
	case spatial.KindUwb:
		return c.UwbHz
	case spatial.KindMic:
		return c.MicHz
	case spatial.KindGas:
		return c.GasHz
	}
	return 0
}

// TargetShow is one presentation of the calibration target: for Duration it
// is carried through the view of Camera at varied distances and tilts.
type TargetShow struct {
	Camera   spatial.DeviceID
	Start    time.Duration // offset from the site epoch
	Duration time.Duration
}

// Options selects variations of a scenario.
type Options struct {
	Config         *Config // nil means DefaultConfig()
	NoActors       bool    // no people or carts
	NoChanges      bool    // furniture and doors keep their initial state for the whole run
	NoDisturbances bool    // no camera bump and no lidar mount shift
	// TargetShows replaces the default schedule, in which the target is
	// shown to every fixed camera in turn. An empty non-nil slice shows it
	// to nobody.
	TargetShows []TargetShow
}

// Shape is a planar footprint with a height: a circle when Radius > 0,
// otherwise a box with half extents Half.X along Yaw and Half.Z across it.
type Shape struct {
	Centre geom.Vec2
	Yaw    float64
	Half   geom.Vec2
	Radius float64
	Height float64 // metres above the floor
}

// Corners returns the box corners in order, or for a circle the corners of
// its axis-aligned bounding square.
func (s Shape) Corners() [4]geom.Vec2 {
	p := geom.Pose2{X: s.Centre.X, Z: s.Centre.Z, Yaw: s.Yaw}
	h := s.Half
	if s.Radius > 0 {
		p.Yaw, h = 0, geom.Vec2{X: s.Radius, Z: s.Radius}
	}
	return [4]geom.Vec2{p.Apply(geom.Vec2{X: -h.X, Z: -h.Z}), p.Apply(geom.Vec2{X: h.X, Z: -h.Z}), p.Apply(h), p.Apply(geom.Vec2{X: -h.X, Z: h.Z})}
}

// Contains reports whether p lies in the footprint.
func (s Shape) Contains(p geom.Vec2) bool {
	if s.Radius > 0 {
		return p.Dist(s.Centre) <= s.Radius
	}
	l := geom.Pose2{X: s.Centre.X, Z: s.Centre.Z, Yaw: s.Yaw}.InverseApply(p)
	return math.Abs(l.X) <= s.Half.X && math.Abs(l.Z) <= s.Half.Z
}

// Bound is the radius of the circle around Centre that encloses the footprint.
func (s Shape) Bound() float64 {
	if s.Radius > 0 {
		return s.Radius
	}
	return s.Half.Norm()
}
