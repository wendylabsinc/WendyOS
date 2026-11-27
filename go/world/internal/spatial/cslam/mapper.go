package cslam

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/posegraph"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/scanmatch"
)

// maxPendingScans bounds the scans held while odometry catches up with them.
const maxPendingScans = 32

// Mapper is the streaming entry point: Ingest measurements, Solve for the
// current joint estimate. It is not safe for concurrent use.
type Mapper struct {
	cfg    Config
	robots []*robot // ordered by device id
	back   backend
	// priorFrame is the frame id of the absolute priors, "" without any.
	priorFrame string
}

// keyframe is one node of the shared graph, as the front end made it.
type keyframe struct {
	time      time.Time // site time of the scan
	floor     spatial.FloorID
	elevation float64
	// local is the base pose in the robot's own map frame. step is its pose
	// in the previous keyframe's frame and info the information of step; cov
	// is the dead-reckoned covariance of local.
	local, step geom.Pose2
	info, cov   geom.Mat3
	odom        geom.Pose2 // odometry pose at the keyframe
	mount       geom.Pose2 // scan frame in the base frame
	scan        spatial.Scan
	compact     spatial.CompactScan // scan, as published
	score       float64             // scan-to-submap match score, 0 when local is odometry only
	input       string
	prior       *prior
}

// prior is an absolute pose carried to a keyframe.
type prior struct {
	pose geom.Pose2
	cov  geom.Mat3
}

type pendingPrior struct {
	at       time.Time
	pose     geom.Pose2
	cov      geom.Mat3
	odom     geom.Pose2
	resolved bool
}

// submap is an occupancy raster in a robot's own map frame with its matcher.
type submap struct {
	g *grid.Grid
	m *scanmatch.Matcher
}

type robot struct {
	dev  spatial.DeviceID
	traj posegraph.Trajectory

	lidar      spatial.SensorID
	mount      geom.Pose2
	offset     time.Duration
	calibrated bool
	// mountSigmaM and mountSigmaRad are the calibration's 1σ mount errors.
	mountSigmaM, mountSigmaRad float64
	floor                      spatial.FloorID
	elevation                  float64

	odom   []spatial.Odometry // samples not older than the last placed scan
	scans  []spatial.Scan     // waiting for odometry to reach them
	last   time.Time          // site time of the last scan accepted
	priors []pendingPrior
	// ponytail: every keyframe keeps its scan for as long as the mapper
	// lives (about 4 kB per keyframe at 360 beams, two keyframes a second
	// while driving): fine for hours, not for weeks. Upgrade path: drop
	// keyframes that add nothing to an area already mapped, and merge
	// their edges.
	kfs []keyframe
	// old and cur are the active submaps: a keyframe is matched against old
	// (cur until the first rotation) and inserted into both.
	old, cur *submap

	// Back-end state, reset when the back end is rebuilt.
	nodes    []posegraph.NodeID // of kfs[:len(nodes)]
	anchored bool               // has an absolute prior in the graph
}

// New returns an empty mapper.
func New(cfg Config) (*Mapper, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	// Reject a scan-matching or grid configuration now, not at the first scan.
	g, err := grid.New(cfg.Scan.Grid)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConfig, err)
	}
	if _, err := scanmatch.NewMatcher(g, cfg.Scan); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrConfig, err)
	}
	m := &Mapper{cfg: cfg}
	m.back.reset(cfg)
	return m, nil
}

// LocalFrameID names a robot's own map frame: the frame of its
// spatial.Keyframe.Local poses and the child of its spatial.FrameAlignment.
func LocalFrameID(dev spatial.DeviceID) string { return "cslam/" + string(dev) }

func (m *Mapper) robot(dev spatial.DeviceID) *robot {
	i, ok := slices.BinarySearchFunc(m.robots, dev, func(r *robot, d spatial.DeviceID) int { return strings.Compare(string(r.dev), string(d)) })
	if !ok {
		r := &robot{dev: dev, traj: posegraph.Trajectory(dev), floor: m.cfg.DefaultFloor}
		m.robots = slices.Insert(m.robots, i, r)
	}
	return m.robots[i]
}

// SetCalibration supplies a lidar's mount on its device base and its clock
// offset. Scans of a device are refused until its lidar is calibrated; a
// later call replaces the calibration for the keyframes that follow. One
// lidar per device is used: the one calibrated last.
func (m *Mapper) SetCalibration(c spatial.Calibration) error {
	if err := c.Validate(); err != nil {
		return err
	}
	mount, _, ok := c.SensorToParent.Planar()
	if c.Kind != spatial.KindLidar2D || c.Parent != spatial.ParentDeviceBase || c.ParentDevice != c.Device || !ok {
		return fmt.Errorf("cslam: calibration of %s/%s is not a planar lidar mount on its own device base", c.Device, c.Sensor)
	}
	r := m.robot(c.Device)
	r.lidar, r.mount, r.offset, r.calibrated = c.Sensor, mount, c.TimeOffset, true
	r.mountSigmaM, r.mountSigmaRad = c.TranslationSigmaM, c.RotationSigmaRad
	return nil
}

// SetFloor names the floor a device is on and that floor's elevation in the
// shared frame, for the keyframes that follow. Submaps and closures never
// span two floors.
func (m *Mapper) SetFloor(dev spatial.DeviceID, floor spatial.FloorID, elevation float64) error {
	if floor == "" || math.IsNaN(elevation) || math.IsInf(elevation, 0) {
		return fmt.Errorf("cslam: floor of %s needs an id and a finite elevation", dev)
	}
	r := m.robot(dev)
	if floor != r.floor {
		r.old, r.cur = nil, nil // the next keyframe starts a fresh submap
	}
	r.floor, r.elevation = floor, elevation
	return nil
}

// AddPrior supplies an absolute pose of a device at site time at, for
// example from global localization. Priors fix the gauge: with one, the
// shared frame is p.FrameID instead of the first robot's own frame. Every
// prior must name the same frame, and at must not precede the device's
// odometry still buffered (priors arrive in time order with the stream).
func (m *Mapper) AddPrior(at time.Time, p spatial.DevicePose) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if m.priorFrame != "" && p.FrameID != m.priorFrame {
		return fmt.Errorf("cslam: prior in frame %q, earlier priors in %q", p.FrameID, m.priorFrame)
	}
	r := m.robot(p.Device)
	if len(r.odom) > 0 && at.Before(r.odom[0].Time) {
		return fmt.Errorf("%w: prior for %s", ErrOutOfOrder, p.Device)
	}
	m.priorFrame = p.FrameID
	r.priors = append(r.priors, pendingPrior{at: at, pose: p.Pose2(), cov: p.Covariance})
	r.resolvePriors()
	return nil
}

// Ingest feeds one measurement to its robot's front end. Odometry and lidar
// scans are used; every other kind is ignored. It returns ErrUncalibrated for
// a scan of an uncalibrated lidar and ErrOutOfOrder for a measurement older
// than its predecessor; neither changes the state.
func (m *Mapper) Ingest(meas spatial.Measurement) error {
	switch v := meas.(type) {
	case spatial.Odometry:
		if err := v.Validate(); err != nil {
			return err
		}
		r := m.robot(v.Device)
		if n := len(r.odom); n > 0 && !v.Time.After(r.odom[n-1].Time) {
			return fmt.Errorf("%w: odometry of %s", ErrOutOfOrder, v.Device)
		}
		r.odom = append(r.odom, v)
		r.resolvePriors()
		return m.drain(r)
	case spatial.Scan:
		if err := v.Validate(); err != nil {
			return err
		}
		r := m.robot(v.Device)
		if !r.calibrated || v.Sensor != r.lidar {
			return fmt.Errorf("%w: %s/%s", ErrUncalibrated, v.Device, v.Sensor)
		}
		at := v.Time.Add(-r.offset)
		if n := len(r.scans); !at.After(r.last) || n > 0 && !v.Time.After(r.scans[n-1].Time) {
			return fmt.Errorf("%w: scan of %s", ErrOutOfOrder, v.Device)
		}
		if len(r.scans) == maxPendingScans {
			r.scans = r.scans[1:] // odometry stalled: keep the newest
		}
		r.scans = append(r.scans, v)
		return m.drain(r)
	}
	return nil
}

// odomAt interpolates the odometry pose at site time t. state is -1 when t
// precedes the buffer (it can never be answered), +1 when odometry has not
// reached t yet, and 0 with the pose and the index of the sample before t.
func (r *robot) odomAt(t time.Time) (pose geom.Pose2, index, state int) {
	i := sort.Search(len(r.odom), func(i int) bool { return !r.odom[i].Time.Before(t) })
	switch {
	case i == len(r.odom):
		return pose, 0, +1
	case r.odom[i].Time.Equal(t):
		return r.odom[i].Pose, i, 0
	case i == 0:
		return pose, 0, -1
	}
	a, b := r.odom[i-1], r.odom[i]
	f := float64(t.Sub(a.Time)) / float64(b.Time.Sub(a.Time))
	return a.Pose.Interpolate(b.Pose, f), i - 1, 0
}

// resolvePriors notes the odometry pose at each waiting prior's instant and
// drops a prior that precedes all odometry.
func (r *robot) resolvePriors() {
	r.priors = slices.DeleteFunc(r.priors, func(p pendingPrior) bool {
		_, _, state := r.odomAt(p.at)
		return !p.resolved && state < 0
	})
	for i := range r.priors {
		if p := &r.priors[i]; !p.resolved {
			if o, _, state := r.odomAt(p.at); state == 0 {
				p.odom, p.resolved = o, true
			}
		}
	}
}

// drain places every waiting scan that odometry now brackets.
func (m *Mapper) drain(r *robot) error {
	for len(r.scans) > 0 {
		s := r.scans[0]
		at := s.Time.Add(-r.offset)
		o, index, state := r.odomAt(at)
		if state > 0 {
			return nil
		}
		r.scans = r.scans[1:]
		if state < 0 {
			continue // older than any odometry: it cannot be placed
		}
		r.last = at
		r.odom = r.odom[index:]
		if err := m.track(r, s, at, o); err != nil {
			return err
		}
	}
	return nil
}

// stepCov is the covariance of an odometry increment with the floors added.
func (m *Mapper) stepCov(n spatial.OdometryNoise, delta geom.Pose2) geom.Mat3 {
	c := n.Covariance(delta)
	t, y := m.cfg.OdometryTransFloor, m.cfg.OdometryYawFloor
	c[0] += t * t
	c[4] += t * t
	c[8] += y * y
	return c
}

func (m *Mapper) newSubmap() (*submap, error) {
	g, err := grid.New(m.cfg.Scan.Grid)
	if err != nil {
		return nil, err
	}
	mt, err := scanmatch.NewMatcher(g, m.cfg.Scan)
	if err != nil {
		return nil, err
	}
	return &submap{g, mt}, nil
}

// track is the front end for one scan taken at site time at with the
// odometry pose o: it decides whether the scan is a keyframe, matches it
// against the active submap, fuses the match with the odometry step and
// extends the submaps.
func (m *Mapper) track(r *robot, s spatial.Scan, at time.Time, o geom.Pose2) error {
	cfg := &m.cfg
	compact, err := spatial.Compact(s)
	if err != nil {
		return err
	}
	kf := keyframe{compact: compact, time: at, floor: r.floor, elevation: r.elevation, odom: o, mount: r.mount, scan: s, input: spatial.InputID(s)}
	noise := r.odom[0].Noise
	if n := len(r.kfs); n > 0 {
		prev := &r.kfs[n-1]
		delta := prev.odom.Between(o)
		if delta.Pos().Norm() < cfg.KeyframeDistance && math.Abs(delta.Yaw) < cfg.KeyframeAngle && r.cur != nil {
			return nil
		}
		// Everything below is in the previous keyframe's frame: the odometry
		// step predicts, the scan match measures.
		step, cov := delta, m.stepCov(noise, delta)
		if target := r.target(); target != nil {
			res, err := target.m.Refine(s, prev.local.Compose(delta).Compose(r.mount))
			meas := prev.local.Between(res.Pose.Compose(r.mount.Inverse()))
			if err == nil && res.Converged && meas.Pos().Dist(delta.Pos()) <= cfg.MatchGateM && math.Abs(geom.AngleDiff(meas.Yaw, delta.Yaw)) <= cfg.MatchGateRad {
				mc := geom.TransformCov3(prev.local.Inverse(), geom.ComposeCov(res.Pose, res.Covariance, r.mount.Inverse(), geom.Mat3{}))
				if fused, fc, ok := fuse(delta, cov, meas, mc); ok {
					step, cov, kf.score = fused, fc, res.Score
				}
			}
		}
		info, err := cov.Inverse()
		if err != nil {
			return fmt.Errorf("cslam: step covariance of %s: %w", r.dev, err)
		}
		kf.step, kf.info = step, info
		kf.local = prev.local.Compose(step).Normalized()
		kf.cov = geom.ComposeCov(prev.local, prev.cov, step, cov)
	}
	// Carry the latest absolute prior to this keyframe along the odometry.
	for len(r.priors) > 0 && r.priors[0].resolved && !r.priors[0].at.After(at) {
		p := r.priors[0]
		r.priors = r.priors[1:]
		d := p.odom.Between(o)
		kf.prior = &prior{pose: p.pose.Compose(d).Normalized(), cov: geom.ComposeCov(p.pose, p.cov, d, m.stepCov(noise, d))}
	}

	// Submaps rotate every half submap: the newer one becomes the match
	// target and a fresh one starts with this keyframe.
	if n := len(r.kfs); r.cur == nil || n%(cfg.SubmapKeyframes/2) == 0 {
		fresh, err := m.newSubmap()
		if err != nil {
			return err
		}
		r.old, r.cur = r.cur, fresh
	}
	pose := kf.local.Compose(kf.mount)
	for _, sm := range []*submap{r.old, r.cur} {
		if sm != nil {
			if err := sm.g.InsertScan(pose, s); err != nil {
				return err
			}
		}
	}
	r.kfs = append(r.kfs, kf)
	return nil
}

// target is the submap the next keyframe is matched against, nil when the
// robot has no map of its surroundings yet.
func (r *robot) target() *submap {
	if r.old != nil {
		return r.old
	}
	return r.cur
}

// fuse combines a predicted and a measured planar pose by their covariances.
// ok is false when either covariance cannot be inverted.
func fuse(pred geom.Pose2, cp geom.Mat3, meas geom.Pose2, cm geom.Mat3) (geom.Pose2, geom.Mat3, bool) {
	ip, err := cp.Inverse()
	if err != nil {
		return pred, cp, false
	}
	im, err := cm.Inverse()
	if err != nil {
		return pred, cp, false
	}
	c, err := ip.Add(im).Inverse()
	if err != nil {
		return pred, cp, false
	}
	d := c.MulVec(im.MulVec(geom.Vec3{X: meas.X - pred.X, Y: meas.Z - pred.Z, Z: geom.AngleDiff(meas.Yaw, pred.Yaw)}))
	return geom.Pose2{X: pred.X + d.X, Z: pred.Z + d.Y, Yaw: geom.WrapAngle(pred.Yaw + d.Z)}, c, true
}
