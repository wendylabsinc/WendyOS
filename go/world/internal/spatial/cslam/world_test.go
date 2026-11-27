package cslam

import (
	"math"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// A hand-built world for unit tests: a 12 x 8 m hall with a partition, a
// pillar and an alcove, so that no two places look alike. Coordinates are
// (x, z); the "site" frame of these tests is the frame the walls are given in.
var hall = func() []geom.Segment {
	chain := func(pts ...geom.Vec2) []geom.Segment {
		var out []geom.Segment
		for i := 1; i < len(pts); i++ {
			out = append(out, geom.Segment{A: pts[i-1], B: pts[i]})
		}
		return out
	}
	v := func(x, z float64) geom.Vec2 { return geom.Vec2{X: x, Z: z} }
	walls := chain(v(0, 0), v(12, 0), v(12, 8), v(7, 8), v(7, 9), v(5.5, 9), v(5.5, 8), v(0, 8), v(0, 0)) // outline with an alcove
	walls = append(walls, chain(v(4, 0), v(4, 3))...)                                                     // partition
	walls = append(walls, chain(v(8, 5), v(9, 5), v(9, 6), v(8, 6), v(8, 5))...)                          // pillar
	return walls
}()

var simEpoch = time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

// testFloor is the floor unit-test robots are on: the mapper's default.
const testFloor spatial.FloorID = "ground"

const (
	simStep     = 50 * time.Millisecond // odometry period; a scan every 4th step
	simSpeed    = 1.0                   // m/s
	simTurnRate = 1.5                   // rad/s
	simBeams    = 180
	simRangeSD  = 0.01
)

// simRobot is one simulated robot: where it really goes and how its sensors lie.
type simRobot struct {
	dev   spatial.DeviceID
	route []geom.Vec2 // waypoints, driven in order
	mount geom.Pose2  // true lidar mount
	// odomOrigin is the arbitrary frame odometry starts in; odomScale and
	// odomYawPerMetre are its systematic errors.
	odomOrigin      geom.Pose2
	odomScale       float64
	odomYawPerMetre float64
	clockOffset     time.Duration // lidar clock minus site time
	start           time.Duration
}

// truePoses drives the route: straight legs at simSpeed, turns on the spot
// at simTurnRate. Pose i is at start + i*simStep.
func (r simRobot) truePoses() []geom.Pose2 {
	p := geom.Pose2{X: r.route[0].X, Z: r.route[0].Z, Yaw: r.route[1].Sub(r.route[0]).Angle()}
	out := []geom.Pose2{p}
	dt := simStep.Seconds()
	for _, w := range r.route[1:] {
		for want := w.Sub(p.Pos()).Angle(); ; {
			d := geom.AngleDiff(want, p.Yaw)
			if math.Abs(d) < 1e-9 {
				break
			}
			p.Yaw = geom.WrapAngle(p.Yaw + math.Copysign(math.Min(math.Abs(d), simTurnRate*dt), d))
			out = append(out, p)
		}
		for {
			d := w.Sub(p.Pos())
			if d.Norm() < 1e-9 {
				break
			}
			step := d.Unit().Scale(math.Min(d.Norm(), simSpeed*dt))
			p.X, p.Z = p.X+step.X, p.Z+step.Z
			out = append(out, p)
		}
	}
	return out
}

// castScan is the lidar: beam i leaves the scanner at yaw AngleMin + i*inc in
// the scan frame and returns the nearest wall.
func castScan(h spatial.Header, sensor geom.Pose2, walls []geom.Segment, rng *rand.Rand) spatial.Scan {
	s := spatial.Scan{Header: h, AngleMin: -math.Pi, AngleIncrement: 2 * math.Pi / simBeams, RangeMin: 0.05, RangeMax: 20, RangeSigma: simRangeSD, Ranges: make([]float64, simBeams)}
	for i := range s.Ranges {
		dir, best := geom.Heading(sensor.Yaw+s.Angle(i)), math.Inf(1)
		for _, w := range walls {
			if d, ok := geom.RaySegment(sensor.Pos(), dir, w); ok && d < best {
				best = d
			}
		}
		s.Ranges[i] = spatial.RangeNoReturn
		if best < s.RangeMax {
			noise := 0.0
			if rng != nil {
				noise = simRangeSD * rng.NormFloat64()
			}
			s.Ranges[i] = math.Max(best+noise, s.RangeMin)
		}
	}
	return s
}

// simRun is everything one robot produced, with the truth to score against.
type simRun struct {
	robot simRobot
	meas  []spatial.Measurement // in time order
	truth []geom.Pose2          // base pose at start + i*simStep
}

// truthAt is the true base pose at a site time (nearest odometry instant).
func (r simRun) truthAt(at time.Time) geom.Pose2 {
	i := int(math.Round(float64(at.Sub(simEpoch)-r.robot.start) / float64(simStep)))
	return r.truth[min(max(i, 0), len(r.truth)-1)]
}

// calibration is the lidar calibration the mapper is given: the true mount
// and clock offset.
func (r simRun) calibration() spatial.Calibration {
	return spatial.Calibration{
		Device: r.robot.dev, Sensor: "lidar", Kind: spatial.KindLidar2D, SensorToParent: geom.Pose3FromPose2(r.robot.mount, 0),
		Parent: spatial.ParentDeviceBase, ParentDevice: r.robot.dev, TranslationSigmaM: 0.005, RotationSigmaRad: 0.002,
		ResidualUnit: "m", Method: "test", Revision: "test-1", State: spatial.CalibrationValid, TimeOffset: r.robot.clockOffset,
	}
}

func simulate(r simRobot, walls []geom.Segment, seed uint64) simRun {
	rng := rand.New(rand.NewPCG(seed, 0x63736c616d))
	run := simRun{robot: r, truth: r.truePoses()}
	odom := r.odomOrigin
	noise := spatial.OdometryNoise{TransPerMetre: 0.03, YawPerMetre: 0.02, YawPerRadian: 0.01}
	for i, p := range run.truth {
		at := simEpoch.Add(r.start + time.Duration(i)*simStep)
		if i > 0 {
			// Odometry integrates the true step with a scale error and a
			// heading drift per metre.
			d := run.truth[i-1].Between(p)
			d.X, d.Z = d.X*r.odomScale, d.Z*r.odomScale
			d.Yaw += r.odomYawPerMetre * d.Pos().Norm()
			odom = odom.Compose(d).Normalized()
		}
		run.meas = append(run.meas, spatial.Odometry{Header: spatial.Header{Device: r.dev, Sensor: "odom", Time: at}, Pose: odom, Noise: noise})
		if i%4 == 0 {
			h := spatial.Header{Device: r.dev, Sensor: "lidar", Time: at.Add(r.clockOffset)}
			run.meas = append(run.meas, castScan(h, p.Compose(r.mount), walls, rng))
		}
	}
	return run
}

// The two standard test robots circle the hall in opposite directions from
// different corners, in unrelated odometry frames.
var (
	robotA = simRobot{
		dev:   "robot-a",
		route: loop(geom.Vec2{X: 1.5, Z: 4.2}, geom.Vec2{X: 10.5, Z: 4.2}, geom.Vec2{X: 10.5, Z: 7}, geom.Vec2{X: 1.5, Z: 7}),
		mount: geom.Pose2{X: 0.15, Z: 0.04, Yaw: 0.1}, odomOrigin: geom.Pose2{X: 5, Z: -3, Yaw: 1},
		odomScale: 1.03, odomYawPerMetre: 0.02, clockOffset: 30 * time.Millisecond,
	}
	robotB = simRobot{
		dev:   "robot-b",
		route: loop(geom.Vec2{X: 10.5, Z: 7}, geom.Vec2{X: 10.5, Z: 4.2}, geom.Vec2{X: 1.5, Z: 4.2}, geom.Vec2{X: 1.5, Z: 7}),
		mount: geom.Pose2{X: 0.1, Z: -0.05, Yaw: -0.15}, odomOrigin: geom.Pose2{X: -2, Z: 7, Yaw: -2},
		odomScale: 0.97, odomYawPerMetre: -0.015, clockOffset: -20 * time.Millisecond,
	}
)

// loop closes a route and runs it once and a bit, so the start is revisited.
func loop(pts ...geom.Vec2) []geom.Vec2 {
	return append(append([]geom.Vec2{}, pts...), pts[0], pts[1])
}

// testConfig shrinks submaps to the size of the hall.
func testConfig() Config {
	return Config{SubmapKeyframes: 16, LoopMinKeyframes: 16}
}

// merge interleaves runs by measurement time, as a live stream would.
func merge(runs ...simRun) []spatial.Measurement {
	var out []spatial.Measurement
	next := make([]int, len(runs))
	for {
		best := -1
		for k, r := range runs {
			if next[k] < len(r.meas) && (best < 0 || r.meas[next[k]].When().Before(runs[best].meas[next[best]].When())) {
				best = k
			}
		}
		if best < 0 {
			return out
		}
		out = append(out, runs[best].meas[next[best]])
		next[best]++
	}
}

// mapperFor returns a mapper that knows the runs' calibrations.
func mapperFor(t *testing.T, cfg Config, runs ...simRun) *Mapper {
	t.Helper()
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range runs {
		if err := m.SetCalibration(r.calibration()); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

func ingest(t *testing.T, m *Mapper, meas []spatial.Measurement) {
	t.Helper()
	for _, v := range meas {
		if err := m.Ingest(v); err != nil {
			t.Fatal(err)
		}
	}
}

func solve(t *testing.T, m *Mapper) *Solution {
	t.Helper()
	sol, err := m.Solve()
	if err != nil {
		t.Fatal(err)
	}
	return sol
}
