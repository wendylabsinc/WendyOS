package spatialsim

import (
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"strconv"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
)

// ReferenceConfig is the error of the Reference face: how far each stand-in
// sits from truth. Sigmas are 1σ. The defaults are about a third of the
// simulation target of the feature that would produce the record (see the
// table in the spatial design), so an upstream error at its target is a rare
// event, and the pose default matches what scan matching against a good map
// reaches on this site (about 3 cm planar RMSE). The zero value is an exact
// reference, which isolates an estimator's own error; acceptance tests use
// DefaultReferenceConfig, and a test of robustness raises the sigmas.
type ReferenceConfig struct {
	// Seed selects the reference errors. It is independent of the site seed,
	// so one stream of measurements can be paired with several references,
	// such as the poses before and after a map correction.
	Seed uint64

	PoseSigmaM   float64 // per-axis position error of DevicePose, metres. Default 0.02.
	PoseYawSigma float64 // yaw error of DevicePose, radians. Default 0.5°.
	// PoseCorrelation is how long a pose error persists: it is redrawn at
	// this interval and blended in between, as localization drift is. Zero
	// makes it white. Default 20 s.
	PoseCorrelation time.Duration

	MountSigmaM   float64 // per-axis position error of a robot-mounted lidar or camera. Default 0.01.
	MountRotSigma float64 // per-axis rotation error of a mounted sensor, radians. Default 0.5°.

	CameraSigmaM     float64 // per-axis position error of a fixed camera. Default 0.015.
	CameraRotSigma   float64 // per-axis rotation error of a fixed camera, radians. Default 0.3°.
	FocalSigma       float64 // relative error of a fixed camera's focal lengths. Default 0.003.
	PrincipalSigmaPx float64 // per-axis error of a fixed camera's principal point. Default 1.
	DistortionSigma  float64 // error of a fixed camera's k1 and k2. Default 0.002.

	TimeOffsetSigma time.Duration // error of a sensor's clock offset. Default 3 ms.

	LandmarkSigmaM   float64 // per-axis position error of a wall tag. Default 0.02.
	LandmarkRotSigma float64 // per-axis rotation error of a wall tag, radians. Default 0.5°.

	MapCellSize float64 // cell side of Grid, metres. Zero takes the grid default, 0.05.
	MapScanHz   float64 // sweeps per second that Grid integrates from each lidar. Default 1.
}

// DefaultReferenceConfig is the reference error acceptance tests are run with.
func DefaultReferenceConfig() ReferenceConfig {
	const deg = math.Pi / 180
	return ReferenceConfig{
		PoseSigmaM: 0.02, PoseYawSigma: 0.5 * deg, PoseCorrelation: 20 * time.Second,
		MountSigmaM: 0.01, MountRotSigma: 0.5 * deg,
		CameraSigmaM: 0.015, CameraRotSigma: 0.3 * deg, FocalSigma: 0.003, PrincipalSigmaPx: 1, DistortionSigma: 0.002,
		TimeOffsetSigma: 3 * time.Millisecond,
		LandmarkSigmaM:  0.02, LandmarkRotSigma: 0.5 * deg,
		MapCellSize: 0.05, MapScanHz: 1,
	}
}

// Reference is the third face of a Site: stand-ins for what OTHER features
// would have estimated, as the payloads they would publish. Each is truth plus
// a seeded error of the size in ReferenceConfig, and says so in its
// covariance or sigma fields.
//
// The rule: a feature test may feed a Reference value to its estimator only
// for a quantity that feature does not itself estimate. A tracker may take
// device poses and mounts from here; a localizer may take the map and the
// landmarks but not the pose; calibration may take robot poses but not the
// camera it calibrates. Reference is the only truth-derived input a test may
// use. Truth stays for scoring.
type Reference struct {
	s   *Site
	cfg ReferenceConfig
}

// Reference returns the reference face with the given error model.
func (s *Site) Reference(cfg ReferenceConfig) (Reference, error) {
	for _, v := range []float64{
		cfg.PoseSigmaM, cfg.PoseYawSigma, cfg.MountSigmaM, cfg.MountRotSigma, cfg.CameraSigmaM, cfg.CameraRotSigma,
		cfg.FocalSigma, cfg.PrincipalSigmaPx, cfg.DistortionSigma, cfg.LandmarkSigmaM, cfg.LandmarkRotSigma, cfg.MapCellSize, cfg.MapScanHz,
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return Reference{}, errors.New("spatialsim: reference config values must be finite and non-negative")
		}
	}
	if cfg.PoseCorrelation < 0 || cfg.TimeOffsetSigma < 0 {
		return Reference{}, errors.New("spatialsim: reference config durations must be non-negative")
	}
	return Reference{s, cfg}, nil
}

func (r Reference) Config() ReferenceConfig { return r.cfg }

func (r Reference) rng(dev spatial.DeviceID, stream string, n int64) *rand.Rand {
	return r.s.rng(dev, "reference/"+strconv.FormatUint(r.cfg.Seed, 10)+"/"+stream, n)
}

func normal3(r *rand.Rand, sigma float64) geom.Vec3 {
	return geom.Vec3{X: sigma * r.NormFloat64(), Y: sigma * r.NormFloat64(), Z: sigma * r.NormFloat64()}
}

// poseError is the (x, z, yaw) error of a device's reference pose at t.
// Between two draws a and b it is a·cos + b·sin over a quarter turn, which
// keeps the variance at sigma² throughout.
func (r Reference) poseError(d *device, t time.Duration) [3]float64 {
	draw := func(n int64) [3]float64 {
		g := r.rng(d.id, "pose", n)
		return [3]float64{r.cfg.PoseSigmaM * g.NormFloat64(), r.cfg.PoseSigmaM * g.NormFloat64(), r.cfg.PoseYawSigma * g.NormFloat64()}
	}
	if r.cfg.PoseCorrelation <= 0 {
		return draw(int64(t))
	}
	k := int64(t / r.cfg.PoseCorrelation)
	a, b := draw(k), draw(k+1)
	sin, cos := math.Sincos(math.Pi / 2 * float64(t-time.Duration(k)*r.cfg.PoseCorrelation) / float64(r.cfg.PoseCorrelation))
	return [3]float64{a[0]*cos + b[0]*sin, a[1]*cos + b[1]*sin, a[2]*cos + b[2]*sin}
}

// DevicePose is the base pose of a mobile device in the site frame at a site
// instant, as localization or collaborative SLAM would publish it: the true
// pose plus a slowly varying error, with the matching covariance. The floor
// and the elevation are the true ones. It is false for a fixed device (see
// Calibration and Survey) and before the device starts.
func (r Reference) DevicePose(dev spatial.DeviceID, at time.Time) (spatial.DevicePose, bool) {
	s := r.s
	d, t := s.byID[dev], at.Sub(epoch)
	if d == nil || d.route == nil || t < d.start {
		return spatial.DevicePose{}, false
	}
	p, elevation, lv := s.place(d, t)
	e := r.poseError(d, t)
	vp, vy := r.cfg.PoseSigmaM*r.cfg.PoseSigmaM, r.cfg.PoseYawSigma*r.cfg.PoseYawSigma
	return spatial.DevicePose{
		Device: dev, Floor: lv.floorAt(p.Pos()).id, FrameID: SiteFrame.ID,
		Location: spatial.At(geom.Vec3{X: p.X + e[0], Y: elevation, Z: p.Z + e[1]}, r.cfg.PoseSigmaM*math.Sqrt2),
		Yaw:      geom.WrapAngle(p.Yaw + e[2]), Covariance: geom.Mat3{vp, 0, 0, 0, vp, 0, 0, 0, vy},
		Method: "reference", Score: 1,
	}, true
}

// Calibration is the calibration of a lidar, a camera or a UWB tag at a site
// instant, as registration and camera calibration would publish it:
//
//   - a robot's lidar or camera: the true mount in the base frame plus a fixed
//     error; a mobile camera keeps its factory intrinsics;
//   - a fixed camera: the true intrinsics and site pose plus a fixed error;
//   - a UWB tag: the surveyed antenna position, exactly.
//
// TimeOffset is the sensor's true clock offset plus a fixed error. After a
// scheduled disturbance (a camera bump, a mount shift) the record follows the
// new truth under a new Revision with a new error, as if recalibration had
// already run: a recalibration test takes its prior from an instant before
// the disturbance and nothing from after it. It is false for other sensors,
// for unknown devices and before the device starts.
func (r Reference) Calibration(dev spatial.DeviceID, id spatial.SensorID, at time.Time) (spatial.Calibration, bool) {
	s := r.s
	d, t := s.byID[dev], at.Sub(epoch)
	if d == nil || t < d.start {
		return spatial.Calibration{}, false
	}
	i := slices.IndexFunc(d.sensors, func(sn sensor) bool { return sn.id == id })
	if i < 0 {
		return spatial.Calibration{}, false
	}
	sn := &d.sensors[i]
	revision := 1
	if (sn.kind == spatial.KindLidar2D && d.lidarShift > 0 && t >= d.lidarShift) || (sn.kind == spatial.KindCamera && d.camBump > 0 && t >= d.camBump) {
		revision = 2
	}
	g := r.rng(dev, "calibration/"+string(id), int64(revision))
	c := spatial.Calibration{
		Device: dev, Sensor: id, Kind: sn.kind, Parent: spatial.ParentDeviceBase, ParentDevice: dev,
		TranslationSigmaM: r.cfg.MountSigmaM, RotationSigmaRad: r.cfg.MountRotSigma, ResidualUnit: "m",
		Method: "reference", Revision: "reference-" + strconv.Itoa(revision), State: spatial.CalibrationValid,
		TimeOffset: sn.clock + time.Duration(float64(r.cfg.TimeOffsetSigma)*g.NormFloat64()), TimeOffsetSigma: r.cfg.TimeOffsetSigma,
	}
	switch sn.kind {
	case spatial.KindLidar2D:
		m := d.lidarAt(t)
		m.X += r.cfg.MountSigmaM * g.NormFloat64()
		m.Z += r.cfg.MountSigmaM * g.NormFloat64()
		m.Yaw += r.cfg.MountRotSigma * g.NormFloat64()
		c.SensorToParent = geom.Pose3FromPose2(m, 0)
	case spatial.KindCamera:
		posSigma, rotSigma := r.cfg.MountSigmaM, r.cfg.MountRotSigma
		in := d.intrinsics
		if d.route == nil {
			posSigma, rotSigma = r.cfg.CameraSigmaM, r.cfg.CameraRotSigma
			in.FX *= 1 + r.cfg.FocalSigma*g.NormFloat64()
			in.FY *= 1 + r.cfg.FocalSigma*g.NormFloat64()
			in.CX += r.cfg.PrincipalSigmaPx * g.NormFloat64()
			in.CY += r.cfg.PrincipalSigmaPx * g.NormFloat64()
			in.K1 += r.cfg.DistortionSigma * g.NormFloat64()
			in.K2 += r.cfg.DistortionSigma * g.NormFloat64()
			c.Parent, c.ParentDevice = spatial.ParentSite, ""
		}
		truth := d.cameraAt(t)
		c.SensorToParent = geom.Pose3{R: truth.R.Mul(geom.RotExp(normal3(g, rotSigma))), T: truth.T.Add(normal3(g, posSigma))}
		c.Intrinsics, c.ResidualUnit, c.ReprojectionRMSE = &in, "px", s.cfg.PixelSigma
		c.TranslationSigmaM, c.RotationSigmaRad = posSigma, rotSigma
		if d.route == nil {
			loc := spatial.At(c.SensorToParent.T, posSigma*math.Sqrt(3))
			c.Location = &loc
		}
	case spatial.KindUwb:
		c.SensorToParent = geom.Pose3{R: geom.Identity3(), T: d.uwbMount}
		c.TranslationSigmaM, c.RotationSigmaRad = 0, 0
	default:
		return spatial.Calibration{}, false
	}
	return c, true
}

// Landmarks is the landmark map: every wall tag with its true site pose plus
// a fixed error, as whichever feature maps fiducials would publish it. A
// camera-only device localizes against these.
func (r Reference) Landmarks() []spatial.Landmark {
	var out []spatial.Landmark
	for _, lv := range r.s.levels {
		for _, tag := range lv.tags {
			g := r.rng("", "landmark/"+tag.ID, 0)
			pose := geom.Pose3{R: tag.Pose.R.Mul(geom.RotExp(normal3(g, r.cfg.LandmarkRotSigma))), T: tag.Pose.T.Add(normal3(g, r.cfg.LandmarkSigmaM))}
			out = append(out, spatial.Landmark{
				ID: tag.ID, Floor: tag.Floor, TagToSite: pose, Location: spatial.At(pose.T, r.cfg.LandmarkSigmaM*math.Sqrt(3)),
				RotationSigmaRad: r.cfg.LandmarkRotSigma, Observations: 1, Revision: "reference-1",
			})
		}
	}
	return out
}

// Grid is the occupancy map of one floor as a mapper would have built it
// from site time [from, to): MapScanHz sweeps per second of every lidar that
// was on the floor, each inserted at the reference device pose composed with
// the reference lidar mount. It therefore carries real lidar noise, the blur
// of the reference pose error, whatever the actors left behind, and nothing
// where no beam went. Sweeps are taken at their true instants: the clock
// offsets are treated as known. The caller owns the grid.
//
// A map of [0, OfficeChangesFrom) is the site before the scheduled changes
// and one from OfficeChangesUntil on is the site after them. With the default
// config four minutes of the Office ground floor take about 0.5 s to build
// and four of the upper floor 0.1 s, twelve times that under the race
// detector: there, map one floor, a shorter window or coarser cells.
func (r Reference) Grid(floor spatial.FloorID, from, to time.Time) (*grid.Grid, error) {
	s := r.s
	if s.floor(floor) == nil {
		return nil, errors.New("spatialsim: unknown floor")
	}
	g, err := grid.New(grid.Config{CellSize: r.cfg.MapCellSize})
	if err != nil {
		return nil, err
	}
	hz := r.cfg.MapScanHz
	if hz <= 0 || hz > s.cfg.LidarHz {
		hz = s.cfg.LidarHz
	}
	for _, d := range s.devices {
		sn := d.sensorOf(spatial.KindLidar2D)
		if sn == nil {
			continue
		}
		step := time.Duration(math.Round(s.cfg.LidarHz/hz)) * sn.period
		first := max(from.Sub(epoch), d.start)
		for t := (first + step - 1) / step * step; t < to.Sub(epoch); t += step {
			pose, ok := r.DevicePose(d.id, s.At(t))
			if !ok || pose.Floor != floor {
				continue
			}
			mount, _ := r.Calibration(d.id, sn.id, s.At(t))
			scan, _ := s.Scan(d.id, s.At(t))
			lidar, _, _ := mount.SensorToParent.Planar()
			if err := g.InsertScan(pose.Pose2().Compose(lidar), scan); err != nil {
				return nil, err
			}
		}
	}
	return g, nil
}
