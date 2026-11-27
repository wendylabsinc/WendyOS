package anchors

import (
	"fmt"
	"math"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/scanmatch"
)

// odometryKept is the number of odometry samples a Resolver keeps to place a
// scan between: at 20 Hz it spans 1.6 s, far more than any clock offset.
const odometryKept = 32

// Resolver is the streaming front of Resolve for one newly arrived device. It
// turns the device's scans and odometry into keyframes in the device's own
// frame and keeps the most recent Config.Window of them. The step from one
// keyframe to the next is the odometry's, refined by matching the two scans
// where they constrain it, so that the keyframes agree with each other to
// millimetres instead of to the odometry's heading error after a turn.
type Resolver struct {
	cfg     Config
	lidar   spatial.Calibration
	mount   geom.Pose2
	odom    []spatial.Odometry
	pending []spatial.Scan
	frames  []spatial.Keyframe
	next    uint64
	// The last keyframe: its odometry pose, scan, pose and covariance.
	lastOdom, lastLocal geom.Pose2
	lastScan            spatial.Scan
	lastCov             geom.Mat3
}

// NewResolver returns a Resolver for the device and lidar the calibration
// names. The calibration supplies the scanner's planar pose in the base frame
// and its clock offset; it must be relative to the device base.
func NewResolver(cfg Config, lidar spatial.Calibration) (*Resolver, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	if err := lidar.Validate(); err != nil {
		return nil, err
	}
	mount, _, planar := lidar.SensorToParent.Planar()
	if lidar.Kind != spatial.KindLidar2D || lidar.Parent != spatial.ParentDeviceBase || !planar {
		return nil, fmt.Errorf("%w: need a planar lidar calibration relative to the device base", ErrConfig)
	}
	return &Resolver{cfg: cfg, lidar: lidar, mount: mount}, nil
}

// Ingest takes the device's measurements in time order. Odometry and the
// calibrated lidar's scans are used; anything else, and anything from another
// device, is ignored. A scan is placed by interpolating the odometry at the
// scan's site time, so it becomes a keyframe once the odometry has passed it.
func (r *Resolver) Ingest(m spatial.Measurement) error {
	if device, _ := m.From(); device != r.lidar.Device {
		return nil
	}
	switch v := m.(type) {
	case spatial.Odometry:
		if err := v.Validate(); err != nil {
			return err
		}
		if n := len(r.odom); n > 0 && !v.Time.After(r.odom[n-1].Time) {
			return fmt.Errorf("anchors: odometry of %s at %s is not after %s", v.Device, v.Time, r.odom[n-1].Time)
		}
		r.odom = append(r.odom, v)
		if len(r.odom) > odometryKept {
			r.odom = r.odom[1:]
		}
	case spatial.Scan:
		if v.Sensor != r.lidar.Sensor {
			return nil
		}
		if err := v.Validate(); err != nil {
			return err
		}
		r.pending = append(r.pending, v)
	default:
		return nil
	}
	for len(r.pending) > 0 && len(r.odom) > 0 && !r.pending[0].Time.Add(-r.lidar.TimeOffset).After(r.odom[len(r.odom)-1].Time) {
		if err := r.place(r.pending[0]); err != nil {
			return err
		}
		r.pending = r.pending[1:]
	}
	return nil
}

// place turns a scan the odometry has passed into a keyframe when the device
// has moved far enough since the last one. A scan older than the kept
// odometry is dropped: the device was not yet reporting where it was.
func (r *Resolver) place(scan spatial.Scan) error {
	at := scan.Time.Add(-r.lidar.TimeOffset)
	i := len(r.odom) - 1 // the last sample at or before the scan
	for i >= 0 && r.odom[i].Time.After(at) {
		i--
	}
	if i < 0 {
		return nil
	}
	a, b := r.odom[i], r.odom[min(i+1, len(r.odom)-1)]
	t := 0.0
	if span := b.Time.Sub(a.Time); span > 0 {
		t = float64(at.Sub(a.Time)) / float64(span)
	}
	odom := a.Pose.Interpolate(b.Pose, t).Normalized()
	kf := spatial.Keyframe{Device: scan.Device, Sequence: r.next, Time: at, Local: odom, SensorToBase: r.mount}
	if r.next > 0 {
		step := r.lastOdom.Between(odom)
		if step.Pos().Norm() < r.cfg.FrameDistance && math.Abs(geom.WrapAngle(step.Yaw)) < r.cfg.FrameYaw {
			return nil
		}
		// The step as the two scans see it, from the odometry's as the guess.
		// A match that leaves a direction open keeps the odometry there, and
		// one that fails keeps all of it.
		cov := b.Noise.Covariance(step)
		match, err := scanmatch.ScanToScan(r.lastScan, scan, r.mount.Between(step.Compose(r.mount)), r.cfg.Match)
		if err == nil && match.Converged && !match.Degenerate {
			step, cov = r.mount.Compose(match.Pose).Compose(r.mount.Inverse()), geom.TransformCov3(r.mount, match.Covariance)
		}
		kf.Local = r.lastLocal.Compose(step).Normalized()
		kf.Covariance = geom.ComposeCov(r.lastLocal, r.lastCov, step, cov)
	}
	var err error
	if kf.Scan, err = spatial.Compact(scan); err != nil {
		return err
	}
	r.frames = append(r.frames, kf)
	if len(r.frames) > r.cfg.Window {
		r.frames = r.frames[1:]
	}
	r.next++
	r.lastOdom, r.lastScan, r.lastLocal, r.lastCov = odom, scan, kf.Local, kf.Covariance
	return nil
}

// Keyframes returns the kept keyframes, oldest first. Local is the base pose
// in the device's own frame, which is its odometry frame at the first
// keyframe, and Covariance what has accumulated since. The floor is not set:
// the device does not know it.
func (r *Resolver) Keyframes() []spatial.Keyframe {
	return append([]spatial.Keyframe(nil), r.frames...)
}

// Resolve finds the anchors in the device's own frame from the kept
// keyframes; see the function Resolve. The poses hold around the time of the
// last keyframe: relate them to the device through that keyframe's Local.
//
// The covariance of each also carries the lidar calibration's own error,
// which the match cannot see: the scanner's yaw error swings an anchor around
// the device by that angle, so at ten metres half a degree is nine
// centimetres across the line of sight. It vanishes as the device approaches.
func (r *Resolver) Resolve(anchors []spatial.Anchor) ([]Resolution, error) {
	out, err := Resolve(r.cfg, r.frames, anchors)
	if err != nil {
		return nil, err
	}
	base := r.frames[len(r.frames)-1].Local.Pos()
	turn, shift := r.lidar.RotationSigmaRad*r.lidar.RotationSigmaRad, r.lidar.TranslationSigmaM*r.lidar.TranslationSigmaM
	for i := range out {
		if out[i].Err != nil {
			continue
		}
		a, c := &out[i].Anchor, &out[i].Covariance
		arm := a.Pose2().Pos().Sub(base)
		swing := geom.Vec2{X: -arm.Z, Z: arm.X} // d(position)/d(yaw)
		c[0] += turn*swing.X*swing.X + shift
		c[4] += turn*swing.Z*swing.Z + shift
		c[1] += turn * swing.X * swing.Z
		c[3] += turn * swing.X * swing.Z
		c[2] += turn * swing.X
		c[6] += turn * swing.X
		c[5] += turn * swing.Z
		c[7] += turn * swing.Z
		c[8] += turn
		a.UncertaintyM = geom.PositionCov(*c).MaxSigma()
	}
	return out, nil
}
