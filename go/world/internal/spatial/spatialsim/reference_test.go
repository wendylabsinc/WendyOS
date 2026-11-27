package spatialsim

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func mustReference(t testing.TB, s *Site, cfg ReferenceConfig) Reference {
	t.Helper()
	ref, err := s.Reference(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

// TestReferencePosesCarryTheConfiguredError scores the reference device poses
// against truth: the error has the configured size, matches the covariance it
// is published with, persists over seconds and is gone after two correlation
// intervals.
func TestReferencePosesCarryTheConfiguredError(t *testing.T) {
	const (
		sizeTolerance  = 0.25 // relative error allowed on each RMS
		maxStepRatio   = 0.2  // RMS change over one second, relative to sigma
		maxCorrelation = 0.2  // |correlation| at twice the correlation interval
		minSamples     = 2000
	)
	s := officeSite(t)
	tr := s.Truth()
	cfg := DefaultReferenceConfig()
	ref := mustReference(t, s, cfg)
	errAt := func(dev spatial.DeviceID, at time.Duration) ([3]float64, bool) {
		p, ok := ref.DevicePose(dev, s.At(at))
		st, _ := tr.Device(dev, s.At(at))
		if !ok {
			return [3]float64{}, false
		}
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
		if p.Floor != st.Floor || p.PositionM[1] != st.Elevation || p.FrameID != SiteFrame.ID {
			t.Fatalf("%s at %v: floor %s elevation %.2f frame %q", dev, at, p.Floor, p.PositionM[1], p.FrameID)
		}
		return [3]float64{p.PositionM[0] - st.Pose.X, p.PositionM[2] - st.Pose.Z, geom.AngleDiff(p.Yaw, st.Pose.Yaw)}, true
	}
	lag := 2 * cfg.PoseCorrelation
	var sq, step, cross [3]float64
	var whitened float64
	n := 0
	for _, dev := range []spatial.DeviceID{Rover1, Rover2, Rover3, Climber, Ranger, Handheld} {
		for at := time.Duration(0); at+lag < OfficeDuration; at += time.Second {
			e, ok := errAt(dev, at)
			next, _ := errAt(dev, at+time.Second)
			far, _ := errAt(dev, at+lag)
			if !ok {
				t.Fatalf("no reference pose for %s at %v", dev, at)
			}
			p, _ := ref.DevicePose(dev, s.At(at))
			m, err := geom.MahalanobisSq3(e, p.Covariance)
			if err != nil {
				t.Fatal(err)
			}
			whitened += m
			for i := range e {
				sq[i] += e[i] * e[i]
				step[i] += (next[i] - e[i]) * (next[i] - e[i])
				cross[i] += e[i] * far[i]
			}
			n++
		}
	}
	if n < minSamples {
		t.Fatalf("only %d samples", n)
	}
	for i, sigma := range []float64{cfg.PoseSigmaM, cfg.PoseSigmaM, cfg.PoseYawSigma} {
		rms := math.Sqrt(sq[i] / float64(n))
		t.Logf("axis %d: rms %.4f (sigma %.4f), one-second step %.4f, correlation at %v %.2f", i, rms, sigma, math.Sqrt(step[i]/float64(n)), lag, cross[i]/sq[i])
		if math.Abs(rms/sigma-1) > sizeTolerance {
			t.Errorf("axis %d: reference error rms %.4f, configured %.4f", i, rms, sigma)
		}
		if r := math.Sqrt(step[i]/float64(n)) / sigma; r > maxStepRatio {
			t.Errorf("axis %d: the error moves %.2f sigma in a second; it should drift, not jitter", i, r)
		}
		if c := cross[i] / sq[i]; math.Abs(c) > maxCorrelation {
			t.Errorf("axis %d: correlation %.2f after %v", i, c, lag)
		}
	}
	if mean := whitened / float64(n); math.Abs(mean/3-1) > sizeTolerance {
		t.Errorf("mean squared Mahalanobis error %.2f; a calibrated 3-dof covariance gives 3", mean)
	}

	// Fixed devices, unknown devices and devices that are still off have none.
	for _, c := range []struct {
		dev spatial.DeviceID
		at  time.Duration
	}{{CamLobby, time.Second}, {"mic-1", time.Second}, {"nobody", time.Second}, {Latecomer, OfficeLateStart - time.Second}} {
		if _, ok := ref.DevicePose(c.dev, s.At(c.at)); ok {
			t.Errorf("reference pose for %s at %v", c.dev, c.at)
		}
	}

	// The zero config is exact, another seed is another error, and white error is available.
	exact := mustReference(t, s, ReferenceConfig{})
	other := cfg
	other.Seed = 7
	white := cfg
	white.PoseCorrelation = 0
	at := s.At(33 * time.Second)
	st, _ := tr.Device(Rover2, at)
	if p, _ := exact.DevicePose(Rover2, at); p.Pose2() != st.Pose || p.Covariance != (geom.Mat3{}) || p.Validate() != nil {
		t.Errorf("the exact reference pose is %+v, truth %+v", p.Pose2(), st.Pose)
	}
	a, _ := ref.DevicePose(Rover2, at)
	b, _ := mustReference(t, s, other).DevicePose(Rover2, at)
	again, _ := mustReference(t, s, cfg).DevicePose(Rover2, at)
	if a.Pose2() == b.Pose2() || a != again {
		t.Error("the reference error must depend on the reference seed and on nothing else")
	}
	w0, _ := mustReference(t, s, white).DevicePose(Rover2, at)
	w1, _ := mustReference(t, s, white).DevicePose(Rover2, at.Add(time.Millisecond))
	if d := w0.Pose2().Pos().Dist(w1.Pose2().Pos()); d < cfg.PoseSigmaM/10 {
		t.Errorf("white pose error moved %.4f m in a millisecond", d)
	}
	if _, err := s.Reference(ReferenceConfig{PoseSigmaM: -1}); err == nil {
		t.Error("a negative sigma was accepted")
	}
}

// TestReferenceCalibrations scores every reference calibration against
// truth: within a few sigma, never exact, valid as a payload, and following
// the scheduled disturbances under a new revision.
func TestReferenceCalibrations(t *testing.T) {
	s := officeSite(t)
	tr := s.Truth()
	cfg := DefaultReferenceConfig()
	ref := mustReference(t, s, cfg)
	at := s.At(200 * time.Second)
	checked := map[spatial.SensorKind]int{}
	for _, m := range s.Manifests() {
		c, ok := ref.Calibration(m.Device, m.Sensor, at)
		switch m.Kind() {
		case spatial.KindLidar2D, spatial.KindCamera, spatial.KindUwb:
		default:
			if ok {
				t.Errorf("reference calibration for %s/%s", m.Device, m.Sensor)
			}
			continue
		}
		if !ok {
			t.Fatalf("no reference calibration for %s/%s", m.Device, m.Sensor)
		}
		if err := c.Validate(); err != nil {
			t.Fatalf("%s/%s: %v", m.Device, m.Sensor, err)
		}
		checked[m.Kind()]++
		clock, _ := tr.ClockOffset(m.Device, m.Sensor)
		if e := c.TimeOffset - clock; e == 0 || e.Abs() > time.Duration(sigmas*float64(cfg.TimeOffsetSigma)) || c.TimeOffsetSigma != cfg.TimeOffsetSigma {
			t.Errorf("%s/%s: time offset off by %v", m.Device, m.Sensor, e)
		}
		switch m.Kind() {
		case spatial.KindLidar2D:
			truth, _ := tr.LidarMount(m.Device, at)
			got, _, planar := c.SensorToParent.Planar()
			dp, dy := got.Pos().Dist(truth.Pos()), math.Abs(geom.AngleDiff(got.Yaw, truth.Yaw))
			if !planar || c.Parent != spatial.ParentDeviceBase || dp == 0 || dp > sigmas*cfg.MountSigmaM || dy > sigmas*cfg.MountRotSigma {
				t.Errorf("%s lidar mount is off by %.3f m and %.4f rad", m.Device, dp, dy)
			}
		case spatial.KindCamera:
			truth, parent, _ := tr.CameraMount(m.Device, at)
			in, _, _ := tr.Camera(m.Device, at)
			posSigma, rotSigma := cfg.MountSigmaM, cfg.MountRotSigma
			if parent == spatial.ParentSite {
				posSigma, rotSigma = cfg.CameraSigmaM, cfg.CameraRotSigma
				if df := math.Abs(c.Intrinsics.FX/in.FX - 1); df == 0 || df > sigmas*cfg.FocalSigma || math.Abs(c.Intrinsics.CX-in.CX) > sigmas*cfg.PrincipalSigmaPx || math.Abs(c.Intrinsics.K1-in.K1) > sigmas*cfg.DistortionSigma {
					t.Errorf("%s: reference intrinsics %+v, truth %+v", m.Device, *c.Intrinsics, in)
				}
			} else if *c.Intrinsics != in {
				t.Errorf("%s: a factory-calibrated camera keeps its intrinsics", m.Device)
			}
			dp, dr := c.SensorToParent.T.Dist(truth.T), geom.RotAngle(c.SensorToParent.R, truth.R)
			if c.Parent != parent || dp == 0 || dp > sigmas*posSigma*math.Sqrt(3) || dr > sigmas*rotSigma*math.Sqrt(3) {
				t.Errorf("%s camera is off by %.3f m and %.4f rad in %s", m.Device, dp, dr, c.Parent)
			}
		case spatial.KindUwb:
			if mount, _ := tr.UwbMount(m.Device); c.SensorToParent.T != mount {
				t.Errorf("%s: UWB antenna %v, surveyed %v", m.Device, c.SensorToParent.T, mount)
			}
		}
	}
	if checked[spatial.KindLidar2D] != 6 || checked[spatial.KindCamera] != 16 || checked[spatial.KindUwb] != 6 {
		t.Errorf("checked %v calibrations", checked)
	}

	// A disturbed sensor: the prior stays near the old truth, the record after
	// the event follows the new truth under a new revision.
	truthAt := func(dev spatial.DeviceID, sensor spatial.SensorID, at time.Time) geom.Vec3 {
		if sensor == SensorLidar {
			m, _ := tr.LidarMount(dev, at)
			return m.Pos().Lift(0)
		}
		m, _, _ := tr.CameraMount(dev, at)
		return m.T
	}
	for _, c := range []struct {
		dev    spatial.DeviceID
		sensor spatial.SensorID
		event  time.Duration
		sigma  float64
	}{{CamCorridorWest, SensorCamera, OfficeCameraBump, cfg.CameraSigmaM}, {Rover2, SensorLidar, OfficeMountShift, cfg.MountSigmaM}} {
		prior, later := s.At(c.event-time.Second), s.At(c.event)
		before, _ := ref.Calibration(c.dev, c.sensor, prior)
		after, ok := ref.Calibration(c.dev, c.sensor, later)
		if !ok || before.Revision == after.Revision || after.Validate() != nil {
			t.Errorf("%s/%s: no new calibration revision after the disturbance at %v", c.dev, c.sensor, c.event)
		}
		limit := sigmas * c.sigma * math.Sqrt(3)
		if d0, d1 := before.SensorToParent.T.Dist(truthAt(c.dev, c.sensor, prior)), after.SensorToParent.T.Dist(truthAt(c.dev, c.sensor, later)); d0 > limit || d1 > limit {
			t.Errorf("%s/%s: %.3f m from truth before the disturbance and %.3f m after; want within %.3f", c.dev, c.sensor, d0, d1, limit)
		}
	}
	if _, ok := ref.Calibration(CamLate, SensorCamera, s.At(OfficeLateCamera-time.Second)); ok {
		t.Error("reference calibration for a camera that has not announced itself")
	}
	if exact, _ := mustReference(t, s, ReferenceConfig{}).Calibration(CamLobby, SensorCamera, at); true {
		in, pose, _ := tr.Camera(CamLobby, at)
		clock, _ := tr.ClockOffset(CamLobby, SensorCamera)
		if *exact.Intrinsics != in || exact.SensorToParent != pose || exact.TimeOffset != clock || exact.Validate() != nil {
			t.Error("the exact reference calibration differs from truth")
		}
	}
}

// TestReferenceLandmarks checks the landmark map covers every wall tag the
// site declares, near its true pose.
func TestReferenceLandmarks(t *testing.T) {
	s := officeSite(t)
	cfg := DefaultReferenceConfig()
	truth := map[string]Landmark{}
	for _, l := range s.Truth().Landmarks() {
		truth[l.ID] = l
	}
	got := mustReference(t, s, cfg).Landmarks()
	if len(got) != len(truth) || len(got) < 100 {
		t.Fatalf("%d reference landmarks, %d wall tags", len(got), len(truth))
	}
	for _, l := range got {
		want := truth[l.ID]
		if err := l.Validate(); err != nil {
			t.Fatal(err)
		}
		dp, dr := l.TagToSite.T.Dist(want.Pose.T), geom.RotAngle(l.TagToSite.R, want.Pose.R)
		if l.Floor != want.Floor || dp == 0 || dp > sigmas*cfg.LandmarkSigmaM*math.Sqrt(3) || dr > sigmas*cfg.LandmarkRotSigma*math.Sqrt(3) {
			t.Errorf("%s is off by %.3f m and %.4f rad", l.ID, dp, dr)
		}
	}
	// What the handheld sees in the rooms no lidar enters is all in the map.
	known := map[string]bool{}
	for _, l := range got {
		known[l.ID] = true
	}
	seen := 0
	for at := time.Duration(0); at < 70*time.Second; at += time.Second {
		frame, _ := s.CameraFrame(Handheld, s.At(at))
		for _, det := range frame.Detections {
			if strings.HasPrefix(det.ID, "tag-") {
				seen++
				if !known[det.ID] {
					t.Fatalf("the handheld sees %q, which the landmark map lacks", det.ID)
				}
			}
		}
	}
	if seen < 200 {
		t.Errorf("the handheld saw only %d tag corners", seen)
	}
}

// TestReferenceGridAgreesWithTruth builds the reference maps and scores them
// against the truth rasters. It is also the one place the simulator, the
// reference poses and mounts and package grid run together.
func TestReferenceGridAgreesWithTruth(t *testing.T) {
	const (
		cell         = 0.1
		patrol       = 150 * time.Second // every lidar route closes within this
		minAgreement = 0.95              // of cells both the map and truth call free or occupied
		minObserved  = 0.9               // of a scanned room's free cells that the map knows
		maxUnscanned = 0.5               // of the free cells of the room no lidar looks into
		minCells     = 5000              // per floor, known to both
	)
	s := officeSite(t)
	tr := s.Truth()
	cfg := DefaultReferenceConfig()
	cfg.MapCellSize = cell
	ref := mustReference(t, s, cfg)
	from, to := s.At(0), s.At(patrol)
	// Rooms a lidar enters on its patrol, and the one none even looks far
	// into. The upper meeting room is entered by no lidar either, but is seen
	// through its two doors.
	scanned := map[string]bool{
		"g-corridor": true, "g-lobby": true, "g-kitchen": true, "g-office-a": true, "g-office-b": true, "g-lab": true, "g-storage": true,
		"yard": true, "u-corridor": true, "u-lounge": true, "u-lab": true, "u-storage": true,
	}
	unscanned := map[string]bool{"u-office": true}
	floors := []spatial.FloorID{FloorGround, FloorYard, FloorUpper}
	if raceSlowdown > 1 { // one robot's worth of ray casting
		floors = floors[2:]
		for room := range scanned {
			if !strings.HasPrefix(room, "u-") {
				delete(scanned, room)
			}
		}
	}
	for _, floor := range floors {
		g, err := ref.Grid(floor, from, to)
		if err != nil {
			t.Fatal(err)
		}
		raster, err := tr.Occupancy(floor, s.At(patrol), cell)
		if err != nil {
			t.Fatal(err)
		}
		agree, both := 0, 0
		known, free := map[string]int{}, map[string]int{}
		for iz := 0; iz < raster.Height; iz++ {
			for ix := 0; ix < raster.Width; ix++ {
				p := raster.Centre(ix, iz)
				want, got := raster.Cells[iz*raster.Width+ix], g.StateAt(p)
				if want == CellFree {
					if room, ok := tr.RoomAt(floor, p); ok {
						free[room.ID]++
						if got != grid.Unknown {
							known[room.ID]++
						}
					}
				}
				if want == CellUnknown || got == grid.Unknown {
					continue
				}
				both++
				if (want == CellOccupied) == (got == grid.Occupied) {
					agree++
				}
			}
		}
		frac := float64(agree) / float64(both)
		t.Logf("%s: agreement %.3f over %d cells", floor, frac, both)
		if both < minCells || frac < minAgreement {
			t.Errorf("%s: the reference map agrees with truth on %.3f of %d cells; want %.2f", floor, frac, both, minAgreement)
		}
		for room, n := range free {
			seen := float64(known[room]) / float64(n)
			switch {
			case scanned[room] && seen < minObserved:
				t.Errorf("%s: the map knows %.2f of the free cells; want %.2f", room, seen, minObserved)
			case unscanned[room] && seen > maxUnscanned:
				t.Errorf("%s: the map knows %.2f of a room no lidar enters", room, seen)
			}
			delete(scanned, room)
			delete(unscanned, room)
		}
	}
	if len(scanned)+len(unscanned) != 0 {
		t.Errorf("rooms never scored: %v %v", scanned, unscanned)
	}
	if _, err := ref.Grid("nowhere", from, to); err == nil {
		t.Error("unknown floor accepted")
	}

	// The map follows the schedule: a crate is there only afterwards.
	{
		before, err := ref.Grid(FloorUpper, from, to)
		after, _ := ref.Grid(FloorUpper, s.At(OfficeChangesUntil), s.At(OfficeChangesUntil+patrol))
		if err != nil {
			t.Fatal(err)
		}
		// The upper storage crate, 0.8 m square: some cell within a cell of
		// its centre line is occupied once it is there, none before.
		crate := geom.Vec2{X: 24.5, Z: 17.5}
		occupied := func(g *grid.Grid) (n int) {
			for dx := -4; dx <= 4; dx++ {
				for dz := -4; dz <= 4; dz++ {
					if g.StateAt(crate.Add(geom.Vec2{X: float64(dx) * cell, Z: float64(dz) * cell})) == grid.Occupied {
						n++
					}
				}
			}
			return n
		}
		if b, a := occupied(before), occupied(after); b != 0 || a < 4 {
			t.Errorf("upper storage crate: %d occupied cells before it appears, %d after; want none, then its faces", b, a)
		}
	}
}

// TestMarkerGeometryConstrainsFixedCameras measures what the scenario gives
// extrinsic calibration: the least-squares pose of each fixed camera from the
// robot markers it sees before the bump, with the robots where the reference
// localization says they are and the reference intrinsics and clock offset.
// Truth scores the result. Over several reference seeds the median error must
// be inside the feature-1 target and the worst inside twice that: the residue
// is the reference pose error, which a robot carries for a whole pass.
//
// The solve starts from the true pose displaced by startShift and startTurn:
// this test measures how well the data pins the camera down, not how a
// calibrator finds a first guess, and a solution half a metre from where it
// started was put there by the data.
func TestMarkerGeometryConstrainsFixedCameras(t *testing.T) {
	const (
		medianPosition = 0.05              // metres, the feature-1 target
		worstPosition  = 0.10              // metres, any seed
		maxRotation    = 1 * math.Pi / 180 // the feature-1 target, any seed
		startShift     = 0.3               // metres on every axis
		startTurn      = 5 * math.Pi / 180 // about every axis
		huberPx        = 3.0               // robust loss scale
		frameStep      = 2 * time.Second   // pose error outlives many frames, so few are needed
		minPoints      = 200
	)
	s := officeSite(t)
	tr := s.Truth()
	markers := map[string]spatial.FiducialGeometry{}
	for _, f := range s.Fiducials() {
		if f.Carrier != "" {
			markers[f.ID] = f
		}
	}
	seeds := []uint64{1, 2, 3, 4, 5}
	if raceSlowdown > 1 {
		seeds = seeds[:3]
	}
	for _, m := range s.Manifests() {
		if m.Kind() != spatial.KindCamera || m.Attachment.Mode != spatial.AttachFixed {
			continue
		}
		start := s.byID[m.Device].start
		var frames []spatial.CameraFrame
		for at := start; at < OfficeCameraBump; at += frameStep {
			frame, _ := s.CameraFrame(m.Device, s.At(at))
			frames = append(frames, frame)
		}
		_, truth, _ := tr.Camera(m.Device, s.At(start))
		guess := geom.Pose3{R: truth.R.Mul(geom.RotExp(geom.Vec3{X: startTurn, Y: startTurn, Z: startTurn})), T: truth.T.Add(geom.Vec3{X: startShift, Y: startShift, Z: startShift})}
		pose := func(x []float64) geom.Pose3 {
			return geom.Pose3{R: guess.R.Mul(geom.RotExp(geom.Vec3{X: x[0], Y: x[1], Z: x[2]})), T: guess.T.Add(geom.Vec3{X: x[3], Y: x[4], Z: x[5]})}
		}
		var errs []float64
		for _, seed := range seeds {
			cfg := DefaultReferenceConfig()
			cfg.Seed = seed
			ref := mustReference(t, s, cfg)
			cal, _ := ref.Calibration(m.Device, m.Sensor, s.At(start))
			var points []geom.Vec3
			var pixels []geom.Pixel
			for _, frame := range frames {
				when := frame.Time.Add(-cal.TimeOffset)
				for _, det := range frame.Detections {
					f, ok := markers[det.ID]
					if !ok {
						continue
					}
					robot, ok := ref.DevicePose(f.Carrier, when)
					if !ok { // the corrected time falls before the carrier started
						continue
					}
					points = append(points, geom.Pose3FromPose2(robot.Pose2(), robot.PositionM[1]).Apply(f.Points[det.Index]))
					pixels = append(pixels, det.Pixel)
				}
			}
			if len(points) < minPoints {
				t.Fatalf("%s: only %d marker points", m.Device, len(points))
			}
			res, err := geom.LevenbergMarquardt(geom.LMProblem{
				NumResiduals: 2 * len(points),
				Residuals: func(x, r []float64) {
					cam := pose(x)
					for i, p := range points {
						px, _ := cal.Intrinsics.ProjectWorld(cam, p)
						r[2*i], r[2*i+1] = px.U-pixels[i].U, px.V-pixels[i].V
					}
				},
			}, make([]float64, 6), geom.LMConfig{HuberDelta: huberPx})
			if err != nil {
				t.Fatalf("seed %d %s: %v", seed, m.Device, err)
			}
			got := pose(res.X)
			dp, dr := got.T.Dist(truth.T), geom.RotAngle(got.R, truth.R)
			errs = append(errs, dp)
			if dp > worstPosition || dr > maxRotation {
				t.Errorf("seed %d %s: resected %.3f m and %.2f° from truth with %d points; want %.2f m and %.0f°", seed, m.Device, dp, dr*180/math.Pi, len(points), worstPosition, maxRotation*180/math.Pi)
			}
		}
		slices.Sort(errs)
		t.Logf("%-15s position error by seed %.3f m", m.Device, errs)
		if median := errs[len(errs)/2]; median > medianPosition {
			t.Errorf("%s: median position error %.3f m over %d reference seeds; want %.2f", m.Device, median, len(errs), medianPosition)
		}
	}
}

// TestSimulationProvenance checks that a record built from simulated
// measurements with the shared context, frame and clock domain is accepted as
// a World component, and that every site names its own run.
func TestSimulationProvenance(t *testing.T) {
	s := officeSite(t)
	ctx := s.SimulationContext()
	if ctx.Seed != s.Seed() || ctx.ClockDomain != ClockDomain || !strings.HasPrefix(ctx.RunID, "office-") || !strings.HasPrefix(ctx.ScenarioVersion, s.Scenario()+"/") {
		t.Fatalf("simulation context %+v", ctx)
	}
	other := mustSite(t, SingleRoom, 2, Options{})
	if o := other.SimulationContext(); o.RunID == ctx.RunID || o.ScenarioVersion == ctx.ScenarioVersion {
		t.Errorf("two scenarios share a run: %+v and %+v", o, ctx)
	}
	var inputs []world.Input
	for m := range s.Stream(s.At(0), s.At(time.Second), Filter{Devices: []spatial.DeviceID{Rover1}, Kinds: []spatial.SensorKind{spatial.KindLidar2D, spatial.KindOdometry}}) {
		in := s.Input(m)
		if in.ID != spatial.InputID(m) || in.Origin != world.Simulated || *in.Simulation != ctx {
			t.Fatalf("input %+v", in)
		}
		inputs = append(inputs, in)
	}
	prov, err := spatial.InferredProvenance("localize", "1", "test", spatial.ReduceInputs(inputs))
	if err != nil {
		t.Fatal(err)
	}
	at := s.At(time.Second)
	pose, _ := mustReference(t, s, DefaultReferenceConfig()).DevicePose(Rover1, at)
	c, err := spatial.ToComponent(
		spatial.Scope{Tenant: "tenant", Site: "site", Source: world.Source{Device: "gateway", App: "spatial", Service: "localize", Stream: "site"}},
		world.Version{Epoch: 1, Sequence: 1}, SiteFrame, world.Timing{SampleTime: at, ClockDomain: ClockDomain, ErrorKnown: true}, time.Minute, prov, pose)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Provenance.Synthetic() {
		t.Error("a record derived from simulated measurements must be synthetic")
	}
	if back, err := spatial.FromComponent[spatial.DevicePose](c); err != nil || back != pose {
		t.Errorf("round trip: %+v, %v", back, err)
	}
}
