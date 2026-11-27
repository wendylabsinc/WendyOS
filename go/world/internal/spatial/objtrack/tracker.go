package objtrack

import (
	"cmp"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const (
	// Feature names this package in provenance: ModelID is
	// spatial.ModelPrefix + Feature.
	Feature = "objtrack"
	// ModelVersion changes when the estimator's output for the same input does.
	ModelVersion = "1"
)

// sensorUse is one sensor that has observed a track: the first and the
// latest measurement of it that did (the span of evidence), and how far that
// sensor has seen the track travel.
type sensorUse struct {
	ref         spatial.SensorRef
	first, last string
	anchor      waypoint // the sensor's first detection of the track
	mid         waypoint // its first detection half the required travel from anchor
	legs        int      // 1 once mid is set
}

type vote struct {
	class  string
	weight float64
}

type track struct {
	id     string
	device spatial.DeviceID // the localized device this track is, or empty
	f      filter
	at     time.Time // instant of the filter state

	first, last time.Time // first and latest observation
	lastPos     geom.Vec2 // position at the latest observation
	floor       spatial.FloorID
	elevation   float64
	radius      float64

	hits      int
	confirmed bool
	static    bool // seen for StaticAfter without travelling: structure, never confirmed

	votes []vote
	uses  []sensorUse
}

// bias is what the tracker has learned of one sensor's fixed error: the
// estimate of the u of Detection.Bias and its covariance, which start at the
// prior (zero, identity).
//
// ponytail: learned only from localized devices, whose positions are known
// independently, and never forgotten until the calibration is replaced. For
// a camera it is the rotation error, which is most of a calibration error
// but not its focal length or height. Consider states per calibration
// parameter, learned from every track two sensors share, are the upgrade.
type bias struct {
	u geom.Vec3
	p geom.Mat3
}

// split returns what a detection's fixed error is expected to be, and the
// covariance of the detection once that is taken out: the noise plus what is
// still unknown of the fixed error.
func (b *bias) split(d *Detection) (shift geom.Vec2, cov geom.Cov2) {
	if b == nil {
		return geom.Vec2{}, d.Cov
	}
	m, u := d.Bias, b.u
	shift = geom.Vec2{X: m[0]*u.X + m[1]*u.Y + m[2]*u.Z, Z: m[3]*u.X + m[4]*u.Y + m[5]*u.Z}
	return shift, d.Cov.Add(project(m, b.p.Add(geom.Identity3().Scale(-1))))
}

// learn updates a sensor's fixed error from its detection d (as the sensor
// made it) of a body whose position is known independently: a Kalman update
// of u with the measurement d.Position - at = B·u + noise.
func (t *Tracker) learn(d *Detection, at geom.Vec2, atCov geom.Cov2) {
	m := d.Bias
	if m == ([6]float64{}) {
		return
	}
	b := t.biases[d.Sensor]
	if b == nil {
		b = &bias{p: geom.Identity3()}
		t.biases[d.Sensor] = b
	}
	noise := d.Cov.Add(project(m, geom.Identity3().Scale(-1))).Add(atCov)
	si, err := project(m, b.p).Add(noise).Inverse()
	if err != nil {
		return
	}
	shift, _ := b.split(d)
	y := d.Position.Sub(at).Sub(shift)
	var pb, k [3][2]float64 // P·Bᵀ and K = P·Bᵀ·S⁻¹
	for i := 0; i < 3; i++ {
		for j := 0; j < 2; j++ {
			for l := 0; l < 3; l++ {
				pb[i][j] += b.p[3*i+l] * m[3*j+l]
			}
		}
		k[i] = [2]float64{pb[i][0]*si[0] + pb[i][1]*si[2], pb[i][0]*si[1] + pb[i][1]*si[3]}
	}
	b.u = b.u.Add(geom.Vec3{X: k[0][0]*y.X + k[0][1]*y.Z, Y: k[1][0]*y.X + k[1][1]*y.Z, Z: k[2][0]*y.X + k[2][1]*y.Z})
	// P ← P - K·(P·Bᵀ)ᵀ, symmetrized against round-off.
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			b.p[3*i+j] -= k[i][0]*pb[j][0] + k[i][1]*pb[j][1]
		}
	}
	for i := 0; i < 3; i++ {
		for j := i + 1; j < 3; j++ {
			v := (b.p[3*i+j] + b.p[3*j+i]) / 2
			b.p[3*i+j], b.p[3*j+i] = v, v
		}
	}
}

// waypoint is a detection a tentative track's travel is measured against.
type waypoint struct {
	p   geom.Vec2
	cov geom.Cov2
}

type floorMap struct {
	id        spatial.FloorID
	elevation float64
	grid      *grid.Grid // may be nil
}

// level is one floor plane: the floors that share an elevation.
type level struct {
	elevation float64
	floors    []floorMap
	bg        *Background // nil when no floor of the level has a map
}

type timedPose struct {
	at   time.Time
	pose spatial.DevicePose
}

// Tracker is the streaming tracker. Feed it in time order from one
// goroutine; it reads no clock. Its time is the newest site instant it has
// been given.
type Tracker struct {
	cfg    Config
	gate   float64 // chi-square quantile, 2 dof, GateProbability: significance of travel
	now    time.Time
	next   uint64
	tracks []*track // creation order
	floors []floorMap
	levels []level
	calibs map[spatial.SensorRef]spatial.Calibration
	biases map[spatial.SensorRef]*bias
	poses  map[spatial.DeviceID][2]timedPose // older, newer
}

// New returns an empty tracker. Zero Config fields take their defaults.
func New(cfg Config) (*Tracker, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	gate, err := geom.ChiSquareQuantile(2, cfg.GateProbability)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	return &Tracker{cfg: cfg, gate: gate, calibs: map[spatial.SensorRef]spatial.Calibration{}, biases: map[spatial.SensorRef]*bias{}, poses: map[spatial.DeviceID][2]timedPose{}}, nil
}

// Config returns the configuration in use, defaults filled in.
func (t *Tracker) Config() Config { return t.cfg }

// SetFloor declares a floor: its plane's World elevation and, when there is
// one, its static occupancy map, which is copied. Call it again when the map
// changes. Floors within LevelToleranceM of each other share a level and
// each other's static structure. A floor without a map still gives cameras a
// plane to place foot pixels on; scans taken on it are refused.
func (t *Tracker) SetFloor(floor spatial.FloorID, elevationM float64, static *grid.Grid) error {
	if floor == "" || math.IsNaN(elevationM) || math.IsInf(elevationM, 0) {
		return fmt.Errorf("%w: floor", ErrInvalid)
	}
	if static != nil {
		static = static.Clone()
	}
	floors := slices.DeleteFunc(slices.Clone(t.floors), func(f floorMap) bool { return f.id == floor })
	floors = append(floors, floorMap{floor, elevationM, static})
	slices.SortFunc(floors, func(a, b floorMap) int {
		return cmp.Or(cmp.Compare(a.elevation, b.elevation), cmp.Compare(a.id, b.id))
	})
	// ponytail: every level's background is rebuilt on each call. Rebuild
	// only the level that changed if maps are replaced often.
	var levels []level
	for _, f := range floors {
		if n := len(levels); n == 0 || f.elevation-levels[n-1].elevation > t.cfg.LevelToleranceM {
			levels = append(levels, level{elevation: f.elevation})
		}
		l := &levels[len(levels)-1]
		l.floors = append(l.floors, f)
	}
	for i := range levels {
		var grids []*grid.Grid
		for _, f := range levels[i].floors {
			if f.grid != nil {
				grids = append(grids, f.grid)
			}
		}
		if len(grids) == 0 {
			continue
		}
		bg, err := NewBackground(grids...)
		if err != nil {
			return err
		}
		levels[i].bg = bg
	}
	t.floors, t.levels = floors, levels
	return nil
}

// SetCalibration installs or replaces a sensor's calibration: the mount of a
// robot's lidar or camera, the site pose and intrinsics of a fixed camera,
// and the sensor's clock offset. Measurements of a sensor whose calibration
// is missing or not CalibrationValid are refused.
func (t *Tracker) SetCalibration(c spatial.Calibration) error {
	if err := c.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	ref := spatial.SensorRef{Device: c.Device, Sensor: c.Sensor}
	t.calibs[ref] = c
	delete(t.biases, ref) // a new calibration has a new error
	return nil
}

// ObservePose reports where a localized device was at a site instant. The
// pose places that device's sensors, and updates the device's own track,
// which is confirmed from the first pose. A tentative or confirmed track
// already within MinSeparationM of the device becomes that track.
func (t *Tracker) ObservePose(at time.Time, p spatial.DevicePose) error {
	if err := p.Validate(); err != nil || at.IsZero() {
		return fmt.Errorf("%w: device pose", ErrInvalid)
	}
	if err := t.advance(at); err != nil {
		return err
	}
	switch b := t.poses[p.Device]; {
	case !at.Before(b[1].at):
		t.poses[p.Device] = [2]timedPose{b[1], {at, p}}
	case at.After(b[0].at):
		t.poses[p.Device] = [2]timedPose{{at, p}, b[1]}
	}

	z := p.Pose2().Pos()
	r := geom.PositionCov(p.Covariance).Add(geom.Cov2{1e-6, 0, 0, 1e-6}) // an exact pose must still invert
	elevation := p.PositionM[1]
	var own *track
	for _, tr := range t.tracks {
		if tr.device == p.Device {
			own = tr
			break
		}
	}
	if own == nil {
		// The device may have been tracked by others before it was localized.
		best := t.cfg.MinSeparationM
		for _, tr := range t.tracks {
			if tr.device != "" || math.Abs(tr.elevation-elevation) > t.cfg.LevelToleranceM {
				continue
			}
			t.predict(tr, at)
			if d := tr.f.pos().Dist(z); d <= best {
				own, best = tr, d
			}
		}
	}
	if own == nil {
		own = t.spawn(at, z, r)
	} else {
		t.predict(own, at)
		if !own.f.update(z, r) {
			return fmt.Errorf("%w: device pose covariance", ErrInvalid)
		}
		own.hits++
	}
	own.device, own.confirmed, own.static = p.Device, true, false
	own.floor, own.elevation = p.Floor, elevation
	own.lastPos = own.f.pos()
	if at.After(own.last) {
		own.last = at
	}
	return nil
}

// poseAt is the pose of a device at a site instant: an observed pose within
// a millisecond, or the two newest poses interpolated, or extrapolated by at
// most MaxPoseAge. Covariance, floor and elevation come from the nearer pose.
func (t *Tracker) poseAt(dev spatial.DeviceID, at time.Time) (spatial.DevicePose, error) {
	b, ok := t.poses[dev]
	if !ok {
		return spatial.DevicePose{}, fmt.Errorf("%w: %s", ErrNoPose, dev)
	}
	for _, p := range b {
		if d := at.Sub(p.at); !p.at.IsZero() && d > -time.Millisecond && d < time.Millisecond {
			return p.pose, nil
		}
	}
	if b[0].at.IsZero() || at.Before(b[0].at) || at.Sub(b[1].at) > t.cfg.MaxPoseAge || b[1].at.Sub(b[0].at) > 2*t.cfg.MaxPoseAge+time.Millisecond || !b[1].at.After(b[0].at) {
		return spatial.DevicePose{}, fmt.Errorf("%w: %s", ErrNoPose, dev)
	}
	s := float64(at.Sub(b[0].at)) / float64(b[1].at.Sub(b[0].at))
	out := b[1].pose
	if s < 0.5 {
		out = b[0].pose
	}
	pose := b[0].pose.Pose2().Interpolate(b[1].pose.Pose2(), s)
	out.PositionM[0], out.PositionM[2], out.Yaw = pose.X, pose.Z, pose.Yaw
	return out, nil
}

// placed is a sensor at the instant of one of its measurements.
type placed struct {
	ref    spatial.SensorRef
	calib  spatial.Calibration
	at     time.Time  // site time
	toSite geom.Pose3 // sensor frame to site
	sigma  PoseSigma
	lv     *level
	floor  spatial.FloorID // floor to label detections with when no map tells
}

func (t *Tracker) place(h spatial.Header) (placed, error) {
	ref := spatial.SensorRef{Device: h.Device, Sensor: h.Sensor}
	c, ok := t.calibs[ref]
	if !ok || c.State != spatial.CalibrationValid {
		return placed{}, fmt.Errorf("%w: %s/%s", ErrNoCalibration, h.Device, h.Sensor)
	}
	pl := placed{
		ref: ref, calib: c, at: siteTime(h.Time, c.TimeOffset), toSite: c.SensorToParent,
		sigma: PoseSigma{PositionM: c.TranslationSigmaM, RotationRad: c.RotationSigmaRad},
	}
	if c.Parent == spatial.ParentDeviceBase {
		p, err := t.poseAt(c.ParentDevice, pl.at)
		if err != nil {
			return placed{}, err
		}
		pl.toSite = geom.Pose3FromPose2(p.Pose2(), p.PositionM[1]).Compose(c.SensorToParent)
		pl.sigma.PositionM = math.Hypot(pl.sigma.PositionM, geom.PositionCov(p.Covariance).MaxSigma())
		pl.sigma.RotationRad = math.Hypot(pl.sigma.RotationRad, math.Sqrt(math.Max(p.Covariance[8], 0)))
		pl.floor = p.Floor
		for i := range t.levels {
			if slices.ContainsFunc(t.levels[i].floors, func(f floorMap) bool { return f.id == p.Floor }) {
				pl.lv = &t.levels[i]
			}
		}
	}
	if pl.lv == nil {
		// The floor under the sensor: the highest plane not above it.
		for i := range t.levels {
			if t.levels[i].elevation <= pl.toSite.T.Y+1e-6 {
				pl.lv = &t.levels[i]
			}
		}
	}
	if pl.lv == nil {
		return placed{}, fmt.Errorf("%w: %s/%s", ErrNoMap, h.Device, h.Sensor)
	}
	if pl.floor == "" {
		// A fixed sensor stands on the floor that knows the ground under it.
		pl.floor = pl.lv.floorAt(pl.toSite.T.XZ(), pl.lv.floors[0].id)
	}
	return pl, nil
}

// floorAt is the floor of the level whose map knows the cell at p best.
// prefer wins a tie and stands where no map knows the cell.
//
// ponytail: floors that share a level (a ground floor and the yard outside
// it) are told apart only by their maps, and both know the strip seen
// through a doorway. Region polygons from the semantic layer are the upgrade.
func (l *level) floorAt(p geom.Vec2, prefer spatial.FloorID) spatial.FloorID {
	know := func(f floorMap) float64 {
		if f.grid == nil {
			return 0
		}
		return math.Abs(f.grid.LogOddsAt(p))
	}
	out, best := prefer, 0.0
	for _, f := range l.floors {
		if f.id == prefer {
			best = know(f)
		}
	}
	for _, f := range l.floors {
		if w := know(f); w > best {
			out, best = f.id, w
		}
	}
	return out
}

// label completes detections of one measurement. A detection stands on the
// floor of its level whose map knows its cell best (a body seen through a
// doorway is on the floor beyond it), and on the sensor's floor in a tie.
func (pl placed) label(dets []Detection, input string) {
	for i := range dets {
		d := &dets[i]
		d.Floor, d.ElevationM, d.Sensor, d.Input = pl.lv.floorAt(d.Position, pl.floor), pl.lv.elevation, pl.ref, input
	}
}

// Ingest takes one measurement. A Scan or a CameraFrame is turned into
// detections and tracked; any other kind is ignored. The sensor needs a
// valid calibration, a mounted sensor a pose of its carrier at the
// measurement's site time, and a scan a static map of its floor: without
// them the measurement is refused with ErrNoCalibration, ErrNoPose or
// ErrNoMap and the tracker is unchanged.
func (t *Tracker) Ingest(m spatial.Measurement) error {
	var dets []Detection
	var pl placed
	var err error
	switch v := m.(type) {
	case spatial.Scan:
		if pl, err = t.place(v.Header); err != nil {
			return err
		}
		sensor, _, ok := pl.toSite.Planar()
		if !ok {
			return fmt.Errorf("%w: %s/%s is not a planar lidar", ErrNoCalibration, v.Device, v.Sensor)
		}
		if dets, err = LidarDetections(v, sensor, pl.sigma, pl.lv.bg, t.cfg); err != nil {
			return err
		}
	case spatial.CameraFrame:
		if pl, err = t.place(v.Header); err != nil {
			return err
		}
		if pl.calib.Intrinsics == nil {
			return fmt.Errorf("%w: %s/%s has no intrinsics", ErrNoCalibration, v.Device, v.Sensor)
		}
		cam := Camera{Intrinsics: *pl.calib.Intrinsics, ToSite: pl.toSite, Sigma: pl.sigma, FloorElevationM: pl.lv.elevation}
		if dets, err = CameraDetections(v, cam, pl.lv.bg, t.cfg); err != nil {
			return err
		}
	default:
		return nil
	}
	pl.label(dets, spatial.InputID(m))
	if pl.calib.Parent != spatial.ParentSite {
		// A mounted sensor's error moves with its carrier's pose error: it is
		// not the same in every sample.
		for i := range dets {
			dets[i].Bias = [6]float64{}
		}
	}
	return t.Observe(pl.at, dets)
}

// advance moves the tracker's time forward to at and deletes expired tracks.
func (t *Tracker) advance(at time.Time) error {
	if !t.now.IsZero() && t.now.Sub(at) > t.cfg.MaxLateness {
		return fmt.Errorf("%w: %s behind", ErrStale, t.now.Sub(at))
	}
	if at.After(t.now) {
		t.now = at
	}
	t.tracks = slices.DeleteFunc(t.tracks, func(tr *track) bool { return t.state(tr, t.now) == spatial.TrackLost })
	return nil
}

// state is the lifecycle state of a track at an instant; TrackLost means
// deleted.
func (t *Tracker) state(tr *track, at time.Time) spatial.TrackState {
	age := at.Sub(tr.last)
	switch {
	case !tr.confirmed && age > t.cfg.TentativeTimeout, age > t.cfg.DeleteAfter:
		return spatial.TrackLost
	case !tr.confirmed:
		return spatial.TrackTentative
	case age > t.cfg.CoastAfter:
		return spatial.TrackCoasting
	}
	return spatial.TrackConfirmed
}

func (t *Tracker) predict(tr *track, at time.Time) {
	if dt := at.Sub(tr.at); dt > 0 {
		tr.f.predict(dt.Seconds(), t.processNoise(tr))
		tr.at = at
	}
}

// processNoise is the acceleration density a track is predicted with: none
// for structure.
func (t *Tracker) processNoise(tr *track) float64 {
	if tr.static {
		return 0
	}
	return t.cfg.ProcessNoise
}

func (t *Tracker) spawn(at time.Time, z geom.Vec2, r geom.Cov2) *track {
	t.next++
	tr := &track{id: fmt.Sprintf("t%06d", t.next), f: newFilter(z, r, t.cfg.InitialSpeedSigma), at: at, first: at, last: at, lastPos: z, hits: 1}
	t.tracks = append(t.tracks, tr)
	return tr
}

// Observe takes the detections of one sensor sample taken at a site instant:
// the entry under Ingest, for detections made elsewhere. Each track is
// predicted to the instant; detections are assigned to tracks of their level
// so that the sample is as likely as possible; an assigned detection updates
// its track, an unassigned one starts a tentative track. An empty batch still
// advances time. An invalid detection refuses the batch.
//
// ponytail: a measurement older than the tracker's time (within MaxLateness)
// is applied to the tracks as they are now, without retrodiction, an error of
// up to the body's speed times the lateness. Sensors whose clocks agree to
// tens of milliseconds do not need more.
func (t *Tracker) Observe(at time.Time, dets []Detection) error {
	if at.IsZero() {
		return fmt.Errorf("%w: zero time", ErrInvalid)
	}
	for _, d := range dets {
		// The noise left when the fixed error is taken out of the covariance
		// must still be a covariance.
		noise := d.Cov.Add(project(d.Bias, geom.Identity3().Scale(-1)))
		if _, err := noise.Inverse(); err != nil || d.Floor == "" || d.Sensor.Device == "" || d.Sensor.Sensor == "" ||
			!finite(d.Position.X, d.Position.Z, d.RadiusM, d.Confidence, d.ElevationM) || !finite(d.Bias[:]...) || d.RadiusM < 0 || !(noise[0] > 0) || !(noise.Det() > 0) {
			return fmt.Errorf("%w: detection", ErrInvalid)
		}
	}
	if err := t.advance(at); err != nil {
		return err
	}
	// From here on a detection is what its sensor reported less what the
	// sensor is by now known to get wrong.
	raw := dets
	dets = slices.Clone(dets)
	for i := range dets {
		shift, cov := t.biases[dets[i].Sensor].split(&dets[i])
		dets[i].Position, dets[i].Cov = dets[i].Position.Sub(shift), cov
	}
	for _, tr := range t.tracks {
		t.predict(tr, at)
	}
	cfg := &t.cfg
	sameLevel := func(d *Detection, tr *track) bool { return math.Abs(d.ElevationM-tr.elevation) <= cfg.LevelToleranceM }
	tracks := t.tracks // spawned tracks are appended to t.tracks, not matched in this batch
	// A detection goes to the track under which it is most likely, and to
	// none when it is more likely a false or new one. The cost of a pair is
	// minus twice its log likelihood: d² + ln|S| for where it is (not d²
	// alone, by which a track that knows little about where it is, a new or a
	// coasting one, is close to everything and takes detections from the
	// well-known track they belong to), plus what its class costs on that
	// track. Leaving it unassigned costs the same likelihood at density
	// ClutterDensity. A precise track therefore keeps a detection that a
	// fixed chi-square gate would refuse: one a few decimetres from a body
	// known to centimetres is that body seen by a sensor whose calibration
	// is off, not a second body.
	null := -2 * math.Log(2*math.Pi*cfg.ClutterDensity)
	sol := match(len(dets), len(tracks), null, func(i, j int) (float64, bool) {
		d, tr := &dets[i], tracks[j]
		// Nothing outruns MaxSpeedMPS, and a sensor does not see its carrier.
		reach := cfg.MaxSpeedMPS*math.Max(at.Sub(tr.last).Seconds(), 0) + cfg.MinSeparationM + 3*d.Cov.MaxSigma()
		if !sameLevel(d, tr) || (tr.device != "" && tr.device == d.Sensor.Device) || d.Position.Dist(tr.lastPos) > reach {
			return 0, false
		}
		d2, logDet, ok := tr.f.distance(d.Position, d.Cov)
		return d2 + logDet + t.classCost(tr, d), ok
	})
	for i := range dets {
		d := &dets[i]
		if j := sol[i]; j >= 0 {
			if tr := tracks[j]; tr.device != "" {
				t.learn(&raw[i], tr.f.pos(), tr.f.posCov())
			}
			t.update(tracks[j], d, at)
			continue
		}
		if len(t.tracks) >= cfg.MaxTracks {
			continue
		}
		tr := t.spawn(at, d.Position, d.Cov)
		tr.floor, tr.elevation, tr.radius = d.Floor, d.ElevationM, d.RadiusM
		t.note(tr, d)
	}
	return nil
}

// classCost is minus twice the log of how much less likely a detection's
// class is on this track than on a track all of whose votes are for that
// class: 0 when they agree or either has no class, -2·ln(ε/(1-ε)) when every
// vote is for another class, ε being ClassConfusion.
func (t *Tracker) classCost(tr *track, d *Detection) float64 {
	total, same := 0.0, 0.0
	for _, v := range tr.votes {
		total += v.weight
		if v.class == d.Class {
			same = v.weight
		}
	}
	if d.Class == "" || total == 0 {
		return 0
	}
	e, p := t.cfg.ClassConfusion, same/total
	return -2 * math.Log(((1-e)*p+e*(1-p))/(1-e))
}

// update folds an assigned detection into its track and decides whether the
// track has now shown that it moves.
func (t *Tracker) update(tr *track, d *Detection, at time.Time) {
	if !tr.f.update(d.Position, d.Cov) {
		return
	}
	tr.hits++
	tr.lastPos = tr.f.pos()
	if at.After(tr.last) {
		tr.last = at
	}
	if d.RadiusM > 0 {
		if tr.radius == 0 {
			tr.radius = d.RadiusM
		}
		tr.radius += 0.2 * (d.RadiusM - tr.radius)
	}
	if tr.device == "" { // a device's floor is the one it reports
		tr.floor, tr.elevation = d.Floor, d.ElevationM
	}
	u := t.note(tr, d)
	if tr.confirmed || tr.static || u == nil {
		return
	}
	// Static structure the map does not know (new furniture, a closed door)
	// is detected too, and its centre wanders by up to its own size as the
	// view changes. A mover is something that has gone further than that,
	// and significantly further than the two detections compared are
	// uncertain.
	//
	// It must also have been somewhere in between. A tentative track that
	// lost its object and was then given a detection of another one has
	// "travelled" too, in one jump; a mover leaves a third position, half the
	// distance out, that it has since left as well.
	//
	// And one sensor must have seen all three. Sensors disagree about where
	// a body is by their calibration errors, which cancel between two
	// detections by the same sensor and do not between two sensors.
	//
	// ponytail: a body that stands still from the moment it is first seen is
	// never confirmed, because nothing tells it from new furniture. Class
	// evidence (a "person" vote) could confirm it if that case matters.
	need := t.cfg.MinTravelM + 2*tr.radius
	beyond := func(w waypoint, dist float64) bool {
		v := d.Position.Sub(w.p)
		m, err := geom.MahalanobisSq2(v, w.cov.Add(d.Cov))
		return v.Norm() >= dist && err == nil && m >= t.gate
	}
	switch {
	case u.legs == 0 && beyond(u.anchor, need/2):
		u.mid, u.legs = waypoint{d.Position, d.Cov}, 1
	case u.legs == 1 && beyond(u.anchor, need) && beyond(u.mid, need/2) && tr.hits >= t.cfg.ConfirmHits:
		tr.confirmed = true
	}
	// What has been watched for StaticAfter without showing that is
	// structure. Its track stops assuming it might move: with no velocity
	// and no process noise it stays where it is through a gap in its
	// detections, instead of growing uncertain enough to be given the next
	// unexplained detection metres away.
	if !tr.confirmed && at.Sub(tr.first) >= t.cfg.StaticAfter {
		tr.static = true
		tr.f.stop()
	}
}

// note records the class vote and the sensor of a detection on its track,
// and returns the sensor's record, nil when the track has no room for it.
func (t *Tracker) note(tr *track, d *Detection) *sensorUse {
	if d.Class != "" && d.Confidence > 0 {
		i := slices.IndexFunc(tr.votes, func(v vote) bool { return v.class == d.Class })
		if i < 0 {
			tr.votes = append(tr.votes, vote{class: d.Class})
			i = len(tr.votes) - 1
		}
		tr.votes[i].weight += d.Confidence
	}
	i := slices.IndexFunc(tr.uses, func(u sensorUse) bool { return u.ref == d.Sensor })
	if i < 0 {
		if len(tr.uses) >= spatial.MaxTrackSensors {
			return nil
		}
		tr.uses = append(tr.uses, sensorUse{ref: d.Sensor, first: d.Input, anchor: waypoint{d.Position, d.Cov}})
		i = len(tr.uses) - 1
	}
	tr.uses[i].last = d.Input
	return &tr.uses[i]
}

// Tracks returns every live track as it is estimated to be at a site
// instant, ordered by ID: the state predicted from the track's latest
// update, its covariance grown accordingly. An instant before the tracker's
// time gives the tracks as they are now. Tentative tracks are included;
// publish and act on TrackConfirmed (observed within CoastAfter) and
// TrackCoasting (predicted only) as the application requires.
func (t *Tracker) Tracks(at time.Time) []spatial.Track {
	if at.Before(t.now) {
		at = t.now
	}
	out := []spatial.Track{}
	for _, tr := range t.tracks {
		state := t.state(tr, at)
		if state == spatial.TrackLost {
			continue
		}
		f := tr.f
		f.predict(at.Sub(tr.at).Seconds(), t.processNoise(tr))
		p := spatial.Track{
			ID: tr.id, Floor: tr.floor, Location: spatial.At(f.pos().Lift(tr.elevation), f.posCov().MaxSigma()),
			VelocityMPS: f.vel(), Covariance: f.p, State: state, FirstSeen: tr.first, LastSeen: tr.last, RadiusM: tr.radius,
		}
		total := 0.0
		best := vote{}
		for _, v := range tr.votes {
			total += v.weight
			if v.weight > best.weight || (v.weight == best.weight && v.class < best.class) {
				best = v
			}
		}
		if total > 0 {
			p.Class, p.ClassConfidence = best.class, best.weight/total
		}
		for _, u := range tr.uses {
			p.Sensors = append(p.Sensors, u.ref)
		}
		slices.SortFunc(p.Sensors, func(a, b spatial.SensorRef) int {
			return cmp.Or(cmp.Compare(a.Device, b.Device), cmp.Compare(a.Sensor, b.Sensor))
		})
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b spatial.Track) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// DeviceTrack returns the ID of the track that is a localized device.
func (t *Tracker) DeviceTrack(dev spatial.DeviceID) (id string, ok bool) {
	for _, tr := range t.tracks {
		if tr.device == dev {
			return tr.id, true
		}
	}
	return "", false
}

// Evidence returns the spatial.InputID of the first and the latest
// measurement from each sensor that observed a track, sorted: the ancestry
// to publish it with. It is empty for an unknown track and for a device
// track no sensor has seen, whose ancestry is that of the device's pose.
func (t *Tracker) Evidence(trackID string) []string {
	var out []string
	for _, tr := range t.tracks {
		if tr.id != trackID {
			continue
		}
		for _, u := range tr.uses {
			out = append(out, u.first, u.last)
		}
	}
	slices.Sort(out)
	return slices.DeleteFunc(slices.Compact(out), func(s string) bool { return s == "" })
}

// Component wraps a track as a World component with Inferred provenance,
// ModelID "wendy.spatial.objtrack". inputs are the ancestry leaves, built by
// the caller from Evidence (it knows whether they were measured, simulated
// or replayed); they are cut to spatial.MaxInputs with spatial.ReduceInputs.
func Component(scope spatial.Scope, version world.Version, frame world.FrameRef, timing world.Timing, ttl time.Duration, appVersion string, tr spatial.Track, inputs []world.Input) (world.Component, error) {
	prov, err := spatial.InferredProvenance(Feature, ModelVersion, appVersion, spatial.ReduceInputs(inputs))
	if err != nil {
		return world.Component{}, err
	}
	return spatial.ToComponent(scope, version, frame, timing, ttl, prov, tr)
}

func finite(v ...float64) bool {
	for _, x := range v {
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return false
		}
	}
	return true
}
