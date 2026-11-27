package cslam

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
)

// Feature is the name under which this package's records are published:
// spatial.InferredProvenance(Feature, ...) gives ModelID "wendy.spatial.cslam".
const Feature = "cslam"

// Solution is the joint estimate at one Solve. It does not change afterwards.
type Solution struct {
	// FrameID names the shared frame: LocalFrameID(Reference), or the frame
	// of the absolute priors when any were supplied (Reference is then empty).
	FrameID   string
	Reference spatial.DeviceID
	// Robots holds every robot with a keyframe, ordered by device id.
	Robots []Robot

	// Chi2 and DOF are the optimiser's residual: Chi2/DOF is near 1 when the
	// noise assumptions hold. Converged is false when the optimiser stopped
	// at its iteration limit; the estimate is then its best iterate.
	Chi2      float64
	DOF       int
	Converged bool
	// Closures counts the loop closures and inter-robot matches in the
	// graph; Removed those taken out again as outliers.
	Closures, Removed int
}

// Robot is one robot's part of a Solution.
type Robot struct {
	Device spatial.DeviceID
	// Shared reports whether the robot's own map frame is related to the
	// shared frame. Until it is, Pose is in LocalFrameID(Device), there is
	// no Alignment, and the robot is left out of Grid and Trajectory.
	Shared bool
	// Alignment places the robot's own map frame in the shared frame, with
	// its covariance (the graph's, plus the lidar mount errors the
	// calibrations declare), the number of inter-robot closures and priors
	// it rests on and their RMSE. It is nil for the Reference robot, whose own frame
	// is the shared frame, and for a robot that is not Shared.
	Alignment *spatial.FrameAlignment
	// Pose is the base pose at Time, the robot's latest odometry sample: the
	// last keyframe's estimate carried forward by odometry. Its covariance
	// is the graph marginal, relative to whatever fixes the gauge.
	Pose spatial.DevicePose
	Time time.Time
	// Keyframes are the optimised keyframes. Local is in the robot's own map
	// frame, corrected by every closure; Covariance is the front end's
	// accumulated covariance of that pose, an upper bound once closures
	// have tightened it.
	Keyframes []spatial.Keyframe

	// ownToShared maps the own map frame to the shared frame (to the graph
	// frame of the robot's own component when it is not Shared).
	ownToShared geom.Pose2
	inputs      []string  // spatial.InputID of each keyframe's scan
	elevations  []float64 // of each keyframe's floor
}

// Record is one payload ready for spatial.ToComponent.
type Record struct {
	Payload spatial.Payload
	// FrameID is the frame the payload is expressed in, which the component
	// frame must name.
	FrameID string
	// Time is the sample time and Inputs the spatial.InputID of measurements
	// the payload was derived from (the first and last scans it rests on).
	Time   time.Time
	Inputs []string
}

func (m *Mapper) snapshot() (*Solution, error) {
	b := &m.back
	s := &Solution{FrameID: m.priorFrame, Chi2: b.result.Chi2, DOF: b.result.DOF, Converged: b.converged, Removed: b.removed}
	for _, c := range b.closures {
		if !c.removed && !c.prior {
			s.Closures++
		}
	}
	// The shared frame: the priors' when a robot has one, else the own frame
	// of the robot holding the graph's first node.
	var ref *robot
	toShared := geom.Pose2{}
	if !slices.ContainsFunc(m.robots, func(r *robot) bool { return r.anchored }) {
		for _, r := range m.robots {
			if len(r.nodes) > 0 && (ref == nil || r.nodes[0] < ref.nodes[0]) {
				ref = r
			}
		}
		s.FrameID, s.Reference = LocalFrameID(ref.dev), ref.dev
		toShared = m.pose(ref.nodes[0]).Compose(ref.kfs[0].local.Inverse()).Inverse()
	}
	for _, r := range m.robots {
		if len(r.nodes) == 0 {
			continue
		}
		own := m.pose(r.nodes[0]).Compose(r.kfs[0].local.Inverse()) // own map frame in the graph frame
		out := Robot{Device: r.dev, ownToShared: own}
		if ref != nil {
			out.Shared = m.related(r, ref)
		} else {
			// Every group of robots holding a prior is in the priors' frame.
			out.Shared, _ = m.lead(r)
		}
		frame := LocalFrameID(r.dev)
		toFrame := own.Inverse()
		if out.Shared {
			frame, toFrame, out.ownToShared = s.FrameID, toShared, toShared.Compose(own)
		}
		for i, n := range r.nodes {
			kf := &r.kfs[i]
			out.inputs, out.elevations = append(out.inputs, kf.input), append(out.elevations, kf.elevation)
			out.Keyframes = append(out.Keyframes, spatial.Keyframe{
				Device: r.dev, Sequence: uint64(i), Floor: kf.floor, Time: kf.time,
				Local: own.Between(m.pose(n)).Normalized(), Covariance: kf.cov, SensorToBase: kf.mount, Scan: kf.compact,
			})
		}
		last := len(r.nodes) - 1

		// The device now: the last keyframe carried forward by odometry.
		kf := &r.kfs[last]
		pose := m.pose(r.nodes[last])
		cov, err := b.g.Marginal(r.nodes[last])
		if err != nil {
			return nil, err
		}
		out.Time = kf.time
		if o := r.odom[len(r.odom)-1]; o.Time.After(kf.time) {
			d := kf.odom.Between(o.Pose)
			cov = geom.ComposeCov(pose, cov, d, m.stepCov(o.Noise, d))
			pose, out.Time = pose.Compose(d), o.Time
		}
		pose, cov = toFrame.Compose(pose).Normalized(), geom.TransformCov3(toFrame, cov)
		out.Pose = spatial.DevicePose{
			Device: r.dev, Floor: kf.floor, FrameID: frame,
			Location: spatial.At(pose.Pos().Lift(kf.elevation), geom.PositionCov(cov).MaxSigma()),
			Yaw:      pose.Yaw, Covariance: cov, Method: Feature, Score: kf.score,
		}

		if out.Shared && r != ref {
			a := spatial.FrameAlignment{
				ChildFrameID: LocalFrameID(r.dev), ParentFrameID: s.FrameID,
				ChildToParent: out.ownToShared.Normalized(), Revision: fmt.Sprintf("r%d", b.optimizations),
			}
			if ref != nil {
				_, a.Covariance, err = b.g.FrameTransform(ref.traj, r.traj)
			} else {
				a.Covariance, err = b.g.Marginal(r.nodes[0])
			}
			if err != nil {
				return nil, err
			}
			a.Covariance = withMountError(a.Covariance, a.ChildToParent.Pos(), ref, r)
			for _, c := range b.closures {
				if !c.removed && (c.a == r || c.b == r) && (c.a != c.b || c.prior) {
					res := b.result.Edges[c.id].Residual
					a.Inliers++
					a.RMSE += res[0]*res[0] + res[1]*res[1]
				}
			}
			if a.Inliers > 0 {
				a.RMSE = math.Sqrt(a.RMSE / float64(a.Inliers))
			}
			out.Alignment = &a
		}
		s.Robots = append(s.Robots, out)
	}
	return s, nil
}

// withMountError adds to an alignment covariance what the graph cannot see:
// the lidar mount errors the calibrations declare. A mount error of the
// reference robot turns and shifts its whole map about its own origin, so
// its yaw error moves a frame at t by |t| times that error; the child's own
// mount error shifts and turns the child frame in place. ref is nil when
// absolute priors fix the gauge: they pin base poses, so only the child's
// share remains.
func withMountError(cov geom.Mat3, t geom.Vec2, ref, child *robot) geom.Mat3 {
	trans := child.mountSigmaM * child.mountSigmaM
	cov[8] += child.mountSigmaRad * child.mountSigmaRad
	if ref != nil {
		trans += ref.mountSigmaM * ref.mountSigmaM
		lever := t.Rotate(math.Pi / 2) // d/dyaw of t turned about the reference origin
		j, v := [3]float64{lever.X, lever.Z, 1}, ref.mountSigmaRad*ref.mountSigmaRad
		for row := range 3 {
			for col := range 3 {
				cov[row*3+col] += v * j[row] * j[col]
			}
		}
	}
	cov[0] += trans
	cov[4] += trans
	return cov
}

// Robot returns one robot's part of the solution.
func (s *Solution) Robot(dev spatial.DeviceID) (Robot, bool) {
	for _, r := range s.Robots {
		if r.Device == dev {
			return r, true
		}
	}
	return Robot{}, false
}

// Trajectory returns the robot's keyframe poses in the shared frame, in
// sequence order. For a robot that is not Shared they are in the frame of
// its Pose instead.
func (r Robot) Trajectory() []geom.Pose2 {
	out := make([]geom.Pose2, len(r.Keyframes))
	for i, kf := range r.Keyframes {
		out[i] = r.ownToShared.Compose(kf.Local).Normalized()
	}
	return out
}

// Grid is the merged occupancy map of one floor in the shared frame: every
// keyframe scan of every Shared robot on that floor, inserted at its
// optimised pose. It returns an error wrapping geom.ErrInsufficient when no
// such keyframe exists.
func (s *Solution) Grid(floor spatial.FloorID, cfg grid.Config) (*grid.Grid, error) {
	g, err := grid.New(cfg)
	if err != nil {
		return nil, err
	}
	n := 0
	for _, r := range s.Robots {
		if !r.Shared {
			continue
		}
		for _, kf := range r.Keyframes {
			if kf.Floor != floor {
				continue
			}
			scan := kf.Scan.Expand(spatial.Header{Device: kf.Device, Time: kf.Time}, 0)
			if err := g.InsertScan(r.ownToShared.Compose(kf.Local).Compose(kf.SensorToBase), scan); err != nil {
				return nil, err
			}
			n++
		}
	}
	if n == 0 {
		return nil, fmt.Errorf("cslam: no shared keyframe on floor %q: %w", floor, geom.ErrInsufficient)
	}
	return g, nil
}

// Records returns what the solution publishes: a spatial.DevicePose per
// robot, a spatial.FrameAlignment per aligned robot and every optimised
// spatial.Keyframe, each with the frame, sample time and input ancestry
// spatial.ToComponent and spatial.InferredProvenance(Feature, ...) need. Map
// tiles come from MapRecords.
func (s *Solution) Records() []Record {
	var out []Record
	for _, r := range s.Robots {
		span := []string{r.inputs[0], r.inputs[len(r.inputs)-1]}
		out = append(out, Record{Payload: r.Pose, FrameID: r.Pose.FrameID, Time: r.Time, Inputs: span})
		if r.Alignment != nil {
			out = append(out, Record{Payload: *r.Alignment, FrameID: r.Alignment.ParentFrameID, Time: r.Time, Inputs: span})
		}
	}
	for _, r := range s.Robots {
		for i, kf := range r.Keyframes {
			// The keyframe's own scan, and the first keyframe's, which
			// defines the frame Local is expressed in.
			out = append(out, Record{Payload: kf, FrameID: LocalFrameID(r.Device), Time: kf.Time, Inputs: []string{r.inputs[0], r.inputs[i]}})
		}
	}
	return out
}

// MapRecords returns the merged map of one floor (see Grid) as sealed
// spatial.MapTile records of side cells a side, in the shared frame. A tile's
// version vector counts the keyframes each robot contributed to the floor,
// and its ancestry is each robot's first and last scan there.
func (s *Solution) MapRecords(floor spatial.FloorID, cfg grid.Config, side int) ([]Record, error) {
	g, err := s.Grid(floor, cfg)
	if err != nil {
		return nil, err
	}
	versions := map[string]uint64{}
	var inputs []string
	var latest time.Time
	elevation := 0.0
	for _, r := range s.Robots {
		first, last, n := 0, 0, 0
		for i, kf := range r.Keyframes {
			if r.Shared && kf.Floor == floor {
				if n == 0 {
					first = i
				}
				last, n = i, n+1
			}
		}
		if n == 0 {
			continue
		}
		versions[string(r.Device)] = uint64(n)
		inputs = append(inputs, r.inputs[first], r.inputs[last])
		if t := r.Keyframes[last].Time; t.After(latest) {
			latest, elevation = t, r.elevations[last]
		}
	}
	tiles, err := g.Tiles(floor, side, elevation, versions)
	if err != nil {
		return nil, err
	}
	out := make([]Record, len(tiles))
	for i, t := range tiles {
		out[i] = Record{Payload: t, FrameID: s.FrameID, Time: latest, Inputs: inputs}
	}
	return out, nil
}
