package adapters

import (
	"fmt"
	"math"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/twinrobot/sitenav"
	"github.com/wendylabsinc/wendy/go/world/internal/go2nav"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// go2Base reads a Go2 pose observation: the base pose in the robot's odom
// frame, converted from ROS axes, and its capture time. sitenav has already
// shifted Pose.Captured from the robot clock onto the gateway client's clock
// by the offset measured when connecting; what remains of that offset is for
// Calibration.TimeOffset to absorb.
func (c Config) go2Base(p sitenav.Pose) (geom.Pose2, time.Time, error) {
	if !finite(p.Position.X, p.Position.Y, p.Height, p.Yaw) {
		return geom.Pose2{}, time.Time{}, invalid("non-finite Go2 pose")
	}
	at := p.Captured
	if at.IsZero() && c.AllowArrivalTime {
		at = p.Received
	}
	if at.IsZero() {
		return geom.Pose2{}, time.Time{}, fmt.Errorf("%w: Go2 pose has no capture stamp", ErrNoTime)
	}
	return geom.Pose2FromROS(p.Position.X, p.Position.Y, p.Yaw).Normalized(), spatial.Stamp(at.UnixNano()), nil
}

// Go2Odometry converts a Go2 pose observation (topic rt/lf/sportmodestate as
// decoded by sitenav) to an Odometry sample.
//
// Fields used: Position (odom x, y), Yaw (counter-clockwise, from the IMU
// quaternion), Height (odom z of the base, reported as Elevation, so it
// follows the robot's stance as well as the floor) and Captured, or Received
// under Config.AllowArrivalTime. The robot declares no noise:
// Config.OdometryNoise is written on every sample.
func Go2Odometry(cfg Config, device spatial.DeviceID, sensor spatial.SensorID, p sitenav.Pose) (spatial.Odometry, error) {
	c, err := cfg.resolved()
	if err != nil {
		return spatial.Odometry{}, err
	}
	base, at, err := c.go2Base(p)
	if err != nil {
		return spatial.Odometry{}, err
	}
	return checked(spatial.Odometry{Header: spatial.Header{Device: device, Sensor: sensor, Time: at}, Pose: base, ElevationKnown: true, Elevation: p.Height, Noise: c.OdometryNoise})
}

// go2Scan flattens n odom-frame points (ROS axes) into a scan in the robot's
// BASE frame at the instant of pose p, keeping those within the height band
// around the base.
func (c Config) go2Scan(device spatial.DeviceID, sensor spatial.SensorID, p sitenav.Pose, cloudAt time.Time, n int, point func(i int) (x, y, z float64)) (spatial.Scan, error) {
	base, at, err := c.go2Base(p)
	if err != nil {
		return spatial.Scan{}, err
	}
	if cloudAt.IsZero() {
		return spatial.Scan{}, fmt.Errorf("%w: Go2 cloud has no capture time", ErrNoTime)
	}
	if skew := cloudAt.Sub(at); skew > c.MaxCloudSkew || skew < -c.MaxCloudSkew {
		return spatial.Scan{}, fmt.Errorf("%w: cloud is %v from its pose, limit %v", ErrSkew, skew, c.MaxCloudSkew)
	}
	points := make([]geom.Vec2, 0, n)
	for i := 0; i < n; i++ {
		x, y, z := point(i)
		if !finite(x, y, z) {
			return spatial.Scan{}, invalid("non-finite Go2 cloud point")
		}
		if q := PointFromROS(x, y, z-p.Height); q.Y >= c.BandLow && q.Y <= c.BandHigh {
			points = append(points, base.InverseApply(q.XZ()))
		}
	}
	return c.planarScan(spatial.Header{Device: device, Sensor: sensor, Time: at}, c.RangeMax, points)
}

// Go2CloudScan converts decoded Go2 voxels (topic
// rt/utlidar/voxel_map_compressed: odom frame, z up) to a planar scan in the
// robot's BASE frame, so the scan's mount on the device is the identity. The
// voxels within Config.BandLow..BandHigh of the base height p.Height are
// flattened (see planarScan for what that loses).
//
// The scan is stamped with the POSE's capture time, not the cloud's: the
// voxel map is expressed in odom, so viewing it from pose p is exact for p's
// instant. cloudAt only has to lie within Config.MaxCloudSkew of it.
//
// ponytail: a virtual scan of Unitree's ACCUMULATED voxel map, not one sweep.
// It can contain voxels the robot cannot currently see, it is registered with
// the robot's own odometry (so matching consecutive scans confirms that
// odometry instead of correcting it), and an absent voxel is not free space
// (keep Config.NoReturnIsFree off). Ceiling: fine for localization against a
// map and for occupied evidence, not an independent motion estimate. Upgrade
// path: the single-sweep scan of Go2LaserScan.
func Go2CloudScan(cfg Config, device spatial.DeviceID, sensor spatial.SensorID, p sitenav.Pose, cloud []sitenav.Vertex, cloudAt time.Time) (spatial.Scan, error) {
	c, err := cfg.resolved()
	if err != nil {
		return spatial.Scan{}, err
	}
	return c.go2Scan(device, sensor, p, cloudAt, len(cloud), func(i int) (x, y, z float64) { return cloud[i].X, cloud[i].Y, cloud[i].Z })
}

// Go2LandmarkScan is Go2CloudScan over what a live sitenav.Observations keeps
// of the last accepted cloud: o.Landmarks, the planar points sitenav judged to
// be wall-like structure (the raw voxels are not retained). Those points have
// no height, so no height band applies; furniture and low clutter are already
// absent. It uses o.Pose and o.CloudAt and refuses with ErrNoTime while o
// holds no accepted cloud.
func Go2LandmarkScan(cfg Config, device spatial.DeviceID, sensor spatial.SensorID, o *sitenav.Observations) (spatial.Scan, error) {
	c, err := cfg.resolved()
	if err != nil {
		return spatial.Scan{}, err
	}
	if o == nil {
		return spatial.Scan{}, invalid("no Go2 observations")
	}
	c.BandLow, c.BandHigh = math.Inf(-1), math.Inf(1)
	return c.go2Scan(device, sensor, o.Pose, o.CloudAt, len(o.Landmarks), func(i int) (x, y, z float64) { return o.Landmarks[i].X, o.Landmarks[i].Y, o.Pose.Height })
}

// Go2LaserScan converts a go2nav.Scan, a ROS LaserScan from the Go2's lidar
// paired with the odometry it was taken at, to a Scan in the lidar frame, the
// paired Odometry sample (stamped under odometrySensor) and the lidar mount
// in the base frame. The mount is meaningful only when s.MountKnown; the map
// pose of the scan is odometry.Pose.Compose(mount).
//
// Fields used: Frame (must be "odom"), CapturedUnixNS, AngleMin,
// AngleIncrement, RangeMin, RangeMax, Ranges, Robot, SensorMount and, when
// present, TiltedPose.QuaternionWXYZ (the tilt test), OdomCapturedUnixNS (the
// odometry stamp) and OdomPositionXYZ[2] (the elevation). A scan without a
// TiltedPose is taken to be level. Not used: Sequence, WorldEpoch and the
// position and yaw uncertainties, which bound the pose in a commissioned map
// and not the odometry increment.
//
// A missing return (+Inf) becomes RangeInvalid unless Config.NoReturnIsFree:
// go2nav does not treat a missing return as evidence of free space. The
// source declares no range noise: Config.RangeSigma is written on the scan.
func Go2LaserScan(cfg Config, device spatial.DeviceID, lidarSensor, odometrySensor spatial.SensorID, s go2nav.Scan) (spatial.Scan, spatial.Odometry, geom.Pose2, error) {
	fail := func(err error) (spatial.Scan, spatial.Odometry, geom.Pose2, error) {
		return spatial.Scan{}, spatial.Odometry{}, geom.Pose2{}, err
	}
	c, err := cfg.resolved()
	if err != nil {
		return fail(err)
	}
	if s.Frame != "odom" {
		return fail(invalid("Go2 scan pose frame %q, want odom", s.Frame))
	}
	if s.CapturedUnixNS <= 0 {
		return fail(fmt.Errorf("%w: Go2 scan has no capture stamp", ErrNoTime))
	}
	if !finite(s.Robot.X, s.Robot.Y, s.Robot.Yaw, s.SensorMount.X, s.SensorMount.Y, s.SensorMount.Yaw) {
		return fail(invalid("non-finite Go2 scan pose or mount"))
	}
	odometry := spatial.Odometry{
		Header: spatial.Header{Device: device, Sensor: odometrySensor, Time: spatial.Stamp(s.CapturedUnixNS)},
		Pose:   geom.Pose2FromROS(s.Robot.X, s.Robot.Y, s.Robot.Yaw).Normalized(), Noise: c.OdometryNoise,
	}
	if t := s.TiltedPose; t != nil {
		roll, pitch, _, err := go2nav.QuaternionRPY(t.QuaternionWXYZ)
		if err != nil || t.OdomCapturedUnixNS <= 0 {
			return fail(invalid("Go2 scan tilt record"))
		}
		// ponytail: a tilted sweep is refused, not projected onto the level
		// plane. Ceiling: scans taken while the body pitches beyond MaxTilt
		// are lost. Upgrade path: project each ray with the quaternion and
		// drop the ones that reach the floor.
		if tilt := math.Max(math.Abs(roll), math.Abs(pitch)); tilt > c.MaxTilt {
			return fail(fmt.Errorf("%w: %.3f rad, limit %.3f", ErrTilted, tilt, c.MaxTilt))
		}
		odometry.Time = spatial.Stamp(t.OdomCapturedUnixNS)
		odometry.ElevationKnown, odometry.Elevation = true, t.OdomPositionXYZ[2]
	}
	scan := spatial.ScanFromROS(spatial.Header{Device: device, Sensor: lidarSensor, Time: spatial.Stamp(s.CapturedUnixNS)}, s.AngleMin, s.AngleIncrement, s.RangeMin, s.RangeMax, c.RangeSigma, s.Ranges)
	if !c.NoReturnIsFree {
		for i := range scan.Ranges {
			if _, _, state := scan.Beam(i); state == spatial.BeamNoReturn {
				scan.Ranges[i] = spatial.RangeInvalid
			}
		}
	}
	if scan, err = checked(scan); err != nil {
		return fail(err)
	}
	if odometry, err = checked(odometry); err != nil {
		return fail(err)
	}
	return scan, odometry, geom.Pose2FromROS(s.SensorMount.X, s.SensorMount.Y, s.SensorMount.Yaw).Normalized(), nil
}
