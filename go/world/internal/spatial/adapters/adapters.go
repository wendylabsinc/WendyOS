// Package adapters converts real inputs into the device-agnostic measurement
// types of package spatial, so recorded or live hardware data runs through the
// estimators the simulator exercises: sensing.Sample (LiDAR, gas, camera), Go2
// pose, voxel-cloud and laser-scan observations, and recorded NDJSON.
//
// Conventions are those of package geom: X forward, Y up, Z right, metres,
// radians, positive yaw CLOCKWISE seen from above. ROS data (x forward, y
// left, z up, yaw counter-clockwise) crosses into that convention through
// exactly three functions, which agree with each other (see the handedness
// test): PointFromROS here for points, geom.Pose2FromROS for planar poses and
// spatial.ScanFromROS for scan angles. Nothing else in this package flips an
// axis.
//
// Nothing here reads a clock. A measurement's Header.Time is the source's own
// capture stamp, mapped as each function documents.
package adapters

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// Refusals. Every error a conversion returns wraps one of these (errors.Is).
var (
	// ErrConfig: a Config field is out of range.
	ErrConfig = errors.New("adapters: invalid configuration")
	// ErrInvalid: the input is malformed, or the converted measurement fails
	// its own validation.
	ErrInvalid = errors.New("adapters: invalid input")
	// ErrUnmapped: the input is well formed but has no honest counterpart
	// among the measurement types.
	ErrUnmapped = errors.New("adapters: input has no measurement mapping")
	// ErrNoPose: the conversion needs the sensor's pose and the input carries
	// none.
	ErrNoPose = errors.New("adapters: input carries no sensor pose")
	// ErrNoTime: the input carries no usable capture time.
	ErrNoTime = errors.New("adapters: input carries no capture time")
	// ErrSkew: a cloud and the pose it is viewed from are too far apart in
	// time (Config.MaxCloudSkew).
	ErrSkew = errors.New("adapters: cloud and pose are out of sync")
	// ErrTilted: a planar scan was taken further from level than
	// Config.MaxTilt.
	ErrTilted = errors.New("adapters: scan plane is tilted")
)

// DefaultClockDomain names the clock of recorded real measurements when the
// trial declares none. It is deliberately not a simulation clock domain.
const DefaultClockDomain = "wendy.spatial.capture"

// Config holds every assumption a conversion makes that real hardware could
// violate. A zero field takes the default documented on it; DefaultConfig
// returns them all. None of the defaults has been tuned on hardware.
type Config struct {
	// Beams is the number of equal angular bins a point cloud is flattened
	// into over the full turn. Default 360 (1 degree). A hit is reported on
	// its bin's centre line, so a point moves sideways by up to
	// range·π/Beams: 9 mm per metre of range at the default.
	Beams int
	// RangeMin is the shortest range reported from a point cloud, in metres.
	// A bin whose nearest point is closer is RangeInvalid, since that point
	// hides what lies behind it. Default 0.10. Raise it to about 0.45 if the
	// Go2's own body or payload shows up in its voxel map.
	RangeMin float64
	// RangeMax is the longest range reported from a cloud whose source
	// declares none (Go2 voxel maps), in metres. Default 10, the radius
	// sitenav keeps around the robot. Farther points are dropped.
	RangeMax float64
	// RangeSigma is the 1σ range noise reported when the source declares
	// none, which is every source converted here, in metres. Default 0.03:
	// about the Unitree L1's stated accuracy plus a 5 cm voxel's
	// quantisation. It does not include the sideways binning error.
	RangeSigma float64
	// BandLow and BandHigh bound the heights, relative to the scan origin
	// (the sample's sensor position, or the Go2 base), of the 3D points
	// flattened into a planar scan, in metres. Default −0.05 and +0.50 when
	// both are zero. A band that reaches the floor turns the floor into a
	// ring of phantom walls: BandLow must stay above −(origin height).
	BandLow, BandHigh float64
	// NoReturnIsFree reports a direction without a return as RangeNoReturn
	// (free space out to RangeMax) instead of RangeInvalid (no information).
	// Default false: an empty bin of a flattened cloud may be a beam that
	// left the height band, a direction never sampled or, for the Go2's
	// accumulated voxel map, a place never observed. Enable it only for a
	// planar sensor whose every bin is known to have been fired.
	NoReturnIsFree bool
	// MaxCloudSkew is the largest accepted gap between a Go2 cloud's capture
	// time and the capture time of the pose it is viewed from. Default
	// 200 ms, as sitenav requires.
	MaxCloudSkew time.Duration
	// MaxTilt is the largest roll or pitch, in radians, at which a Go2 laser
	// scan is accepted as planar. Default 0.05. A beam tilted by t leaves
	// the plane by range·sin(t) and strikes the floor beyond
	// height/sin(t): 7.6 m for the L1 at 0.38 m and the default.
	MaxTilt float64
	// OdometryNoise is declared on every Go2 odometry sample, since the
	// robot reports none. Default {0.05, 0.02, 0.05}: a guess for legged
	// odometry, to be replaced by the drift measured in a first trial.
	OdometryNoise spatial.OdometryNoise
	// AllowArrivalTime stamps a Go2 pose that carries no capture time with
	// the time it was received instead of refusing it. That time includes
	// the transport delay and its jitter. Default false.
	AllowArrivalTime bool
	// PixelSigma is the 1σ pixel noise written on a camera frame header,
	// since a sample declares none, in pixels. Default 1.
	PixelSigma float64
	// RotationSigma is the 1σ orientation uncertainty written on a camera
	// calibration prior, since a sample declares none, in radians. Default
	// 0.02.
	RotationSigma float64
	// ClockDomain names the clock the converted timestamps are on. It is
	// recorded as the original clock domain of replayed inputs. Default
	// DefaultClockDomain. The stamps themselves are never shifted: a constant
	// offset from site time is what Calibration.TimeOffset estimates.
	ClockDomain string
}

// DefaultConfig returns the documented defaults.
func DefaultConfig() Config {
	return Config{
		Beams: 360, RangeMin: 0.10, RangeMax: 10, RangeSigma: 0.03, BandLow: -0.05, BandHigh: 0.50,
		MaxCloudSkew: 200 * time.Millisecond, MaxTilt: 0.05,
		OdometryNoise: spatial.OdometryNoise{TransPerMetre: 0.05, YawPerMetre: 0.02, YawPerRadian: 0.05},
		PixelSigma:    1, RotationSigma: 0.02, ClockDomain: DefaultClockDomain,
	}
}

// resolved fills zero fields with their defaults and checks the result.
func (c Config) resolved() (Config, error) {
	d := DefaultConfig()
	if c.Beams == 0 {
		c.Beams = d.Beams
	}
	if c.RangeMin == 0 {
		c.RangeMin = d.RangeMin
	}
	if c.RangeMax == 0 {
		c.RangeMax = d.RangeMax
	}
	if c.RangeSigma == 0 {
		c.RangeSigma = d.RangeSigma
	}
	if c.BandLow == 0 && c.BandHigh == 0 {
		c.BandLow, c.BandHigh = d.BandLow, d.BandHigh
	}
	if c.MaxCloudSkew == 0 {
		c.MaxCloudSkew = d.MaxCloudSkew
	}
	if c.MaxTilt == 0 {
		c.MaxTilt = d.MaxTilt
	}
	if c.OdometryNoise == (spatial.OdometryNoise{}) {
		c.OdometryNoise = d.OdometryNoise
	}
	if c.PixelSigma == 0 {
		c.PixelSigma = d.PixelSigma
	}
	if c.RotationSigma == 0 {
		c.RotationSigma = d.RotationSigma
	}
	if c.ClockDomain == "" {
		c.ClockDomain = d.ClockDomain
	}
	n := c.OdometryNoise
	// Written so that a NaN fails every comparison.
	ok := c.Beams >= 4 && c.Beams <= spatial.MaxScanBeams &&
		c.RangeMin > 0 && c.RangeMax > c.RangeMin && !math.IsInf(c.RangeMax, 0) &&
		c.RangeSigma > 0 && !math.IsInf(c.RangeSigma, 0) &&
		c.BandLow < c.BandHigh && !math.IsInf(c.BandLow, 0) && !math.IsInf(c.BandHigh, 0) &&
		c.MaxCloudSkew > 0 && c.MaxTilt > 0 && c.MaxTilt < math.Pi/2 &&
		n.TransPerMetre >= 0 && n.YawPerMetre >= 0 && n.YawPerRadian >= 0 && finite(n.TransPerMetre, n.YawPerMetre, n.YawPerRadian) &&
		c.PixelSigma > 0 && !math.IsInf(c.PixelSigma, 0) && c.RotationSigma > 0 && !math.IsInf(c.RotationSigma, 0)
	if !ok {
		return c, ErrConfig
	}
	return c, nil
}

// PointFromROS maps a point in a ROS REP-103 frame (x forward, y left, z up)
// to the spatial axes (X forward, Y up, Z right). It is a rotation, so
// handedness is preserved, and it is the planar part of geom.Pose2FromROS:
// PointFromROS(x, y, z).XZ() == geom.Pose2FromROS(x, y, yaw).Pos().
func PointFromROS(x, y, z float64) geom.Vec3 { return geom.Vec3{X: x, Y: z, Z: -y} }

func finite(values ...float64) bool {
	for _, v := range values {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// checked wraps a measurement's own validation failure as ErrInvalid.
func checked[M spatial.Measurement](m M) (M, error) {
	if err := m.Validate(); err != nil {
		var zero M
		return zero, invalid("%v", err)
	}
	return m, nil
}

// planarScan flattens points given in the scan frame into a full-turn range
// scan: beam i looks along −π + i·2π/Beams and reports the planar distance of
// the nearest point within half a bin of that direction.
//
// ponytail: nearest point per angular bin. What is lost: everything but the
// nearest point of each bin (structure behind it, and a second surface at
// another height), each point's bearing within its bin (it is reported on the
// bin centre line) and each point's height. Ceiling: a 3D lidar is used as a
// 2D one. Upgrade path: a point-cloud measurement type in package spatial and
// estimators that consume it.
func (c Config) planarScan(h spatial.Header, rangeMax float64, points []geom.Vec2) (spatial.Scan, error) {
	inc := 2 * math.Pi / float64(c.Beams)
	ranges := make([]float64, c.Beams)
	for i := range ranges {
		ranges[i] = math.Inf(1)
	}
	for _, p := range points {
		// A point on the origin has no bearing; one beyond rangeMax would
		// read as free space along its whole beam.
		if r := p.Norm(); r > 0 && r <= rangeMax {
			i := int(math.Round((p.Angle()+math.Pi)/inc)) % c.Beams
			ranges[i] = math.Min(ranges[i], r)
		}
	}
	empty := spatial.RangeInvalid
	if c.NoReturnIsFree {
		empty = spatial.RangeNoReturn
	}
	for i, r := range ranges {
		switch {
		case math.IsInf(r, 1):
			ranges[i] = empty
		case r < c.RangeMin:
			ranges[i] = spatial.RangeInvalid
		}
	}
	return checked(spatial.Scan{Header: h, AngleMin: -math.Pi, AngleIncrement: inc, RangeMin: c.RangeMin, RangeMax: rangeMax, Ranges: ranges, RangeSigma: c.RangeSigma})
}
