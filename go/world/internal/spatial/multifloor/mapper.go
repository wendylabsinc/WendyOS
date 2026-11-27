package multifloor

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/geography"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

const (
	// Feature names this package in provenance: ModelID "wendy.spatial.multifloor".
	Feature = "multifloor"
	// ModelVersion is the provenance model version of the records.
	ModelVersion = "1"
	// PoseMethod is the Method of the device poses Mapper reports.
	PoseMethod = "odometry-gnss-fusion"
)

// ErrOutOfOrder reports a measurement that is not newer than the last one of
// its kind from the same device. It was not used.
var ErrOutOfOrder = errors.New("multifloor: measurement out of time order")

// Config tunes a Mapper. A zero field takes its default.
type Config struct {
	// StartFloor is the id of the floor devices power on at, and
	// StartElevationM its elevation: the vertical datum of the floor model.
	// Defaults "ground" and 0.
	//
	// ponytail: every device is taken to start on this floor. A device that
	// powers on elsewhere is labelled one storey off until the engine seeds
	// its floor from a localizer's floor match; add that call when one exists.
	StartFloor      spatial.FloorID
	StartElevationM float64

	// Floor tunes the barometric detector of each device.
	Floor FloorConfig
	// FloorMatchM is how close a height reached must be to a known floor's
	// elevation to be that floor rather than a new one. It must be below
	// Floor.MinFloorHeightM. Default 1 m.
	FloorMatchM float64
	// LiftMaxTravelM is the most a device travels over the ground during a
	// lift ride; a floor change with more is stairs or a ramp. Default 1 m.
	LiftMaxTravelM float64
	// RampMaxSlope is the steepest rise over run of a ramp; steeper is
	// stairs. Default 0.2.
	RampMaxSlope float64
	// PortalMergeM is how close both ends of two floor changes must be to be
	// the same portal. Default 2 m.
	PortalMergeM float64

	// GnssInterval is the fix period of the receivers. It dates an
	// indoor-outdoor crossing half a period before the first fix and after
	// the last. Default 1 s.
	GnssInterval time.Duration
	// GnssTimeout is the silence after which a receiver is indoors again.
	// Default 2.5 s: one missed fix is not a doorway.
	GnssTimeout time.Duration
	// OutdoorFixes is the number of consecutive fixes that puts a device
	// outdoors. Default 2.
	OutdoorFixes int
	// GnssMaxAccuracyM discards fixes whose reported horizontal accuracy is
	// worse: the weak fixes a receiver still produces just inside a door.
	// Default 10 m.
	GnssMaxAccuracyM float64
	// GnssGate is the chi-square confidence beyond which a fix that
	// disagrees with the pose is not fused. Default 0.999.
	GnssGate float64

	// Geo tunes the geo reference.
	Geo GeoConfig
	// GeoMaxPoseSigmaM keeps a fix out of the geo reference while the pose it
	// would be paired with is less certain than this, so that a pose leaning
	// on GNSS does not feed the reference it is corrected by. Default 0.3 m.
	GeoMaxPoseSigmaM float64
	// GeoRefit is the number of new fixes after which the geo reference used
	// for fusion is estimated again. Default 10.
	GeoRefit int
	// GeoMaxSamples bounds the fixes kept; beyond it every other one is
	// dropped and new ones are thinned to match. Default 4096.
	GeoMaxSamples int

	// Odometry is the noise model for odometry that reports none. Default
	// 2 % of distance, 0.002 rad per metre, 1 % of rotation.
	Odometry spatial.OdometryNoise
	// FixSigmaFloorM and FixYawFloorRad are added to the covariance of every
	// pose fix, so that an over-confident localizer cannot freeze the filter.
	// Defaults 5 mm and 1 mrad.
	FixSigmaFloorM, FixYawFloorRad float64
}

func (c Config) withDefaults() (Config, error) {
	c.Floor, c.Geo = c.Floor.withDefaults(), c.Geo.withDefaults()
	def := func(v *float64, d float64) {
		if *v == 0 {
			*v = d
		}
	}
	if c.StartFloor == "" {
		c.StartFloor = "ground"
	}
	def(&c.FloorMatchM, 1)
	def(&c.LiftMaxTravelM, 1)
	def(&c.RampMaxSlope, 0.2)
	def(&c.PortalMergeM, 2)
	def(&c.GnssMaxAccuracyM, 10)
	def(&c.GnssGate, 0.999)
	def(&c.GeoMaxPoseSigmaM, 0.3)
	def(&c.FixSigmaFloorM, 0.005)
	def(&c.FixYawFloorRad, 0.001)
	if c.GnssInterval == 0 {
		c.GnssInterval = time.Second
	}
	if c.GnssTimeout == 0 {
		c.GnssTimeout = 2500 * time.Millisecond
	}
	if c.OutdoorFixes == 0 {
		c.OutdoorFixes = 2
	}
	if c.GeoRefit == 0 {
		c.GeoRefit = 10
	}
	if c.GeoMaxSamples == 0 {
		c.GeoMaxSamples = 4096
	}
	if c.Odometry == (spatial.OdometryNoise{}) {
		c.Odometry = spatial.OdometryNoise{TransPerMetre: 0.02, YawPerMetre: 0.002, YawPerRadian: 0.01}
	}
	probe := spatial.FloorInfo{Floor: c.StartFloor + "-outdoor", ElevationM: c.StartElevationM}
	if err := probe.Validate(); err != nil {
		return c, fmt.Errorf("multifloor: start floor: %w", err)
	}
	if !(c.FloorMatchM < c.Floor.MinFloorHeightM) || c.GeoMaxSamples < 2*c.Geo.MinFixes || c.GnssTimeout < c.GnssInterval {
		return c, errors.New("multifloor: FloorMatchM must be below MinFloorHeightM, GeoMaxSamples at least twice MinFixes, GnssTimeout at least GnssInterval")
	}
	return c, nil
}

// Transition is one change of floor by one device.
type Transition struct {
	Device   spatial.DeviceID
	From, To spatial.FloorID
	// Kind is how the device changed floor: stairs, lift or ramp for a change
	// of storey, door for a step between a storey and the open ground beside
	// it. It is empty for a change of storey by a device without odometry,
	// which cannot be told apart.
	Kind spatial.PortalKind
	// Portal is the id of the portal the change created or confirmed. It is
	// empty when the device had no site pose to place one with.
	Portal string
	// At is when the device changed floor: half way up for a change of
	// storey, between the last missing and the first present GNSS fix for a
	// door. Start and End bound the vertical motion (both equal At for a
	// door). Confirmed is when the change was accepted, the instant the
	// device's floor label switched.
	Start, At, End, Confirmed time.Time
	// DeltaM is the height gained with its conservative error bound SigmaM;
	// both are zero for a door.
	DeltaM, SigmaM float64
	// Inputs are the InputIDs of the measurements the change rests on.
	Inputs []string
}

// Record is something Mapper would publish: a payload, its sample time and
// the InputIDs of the measurements it was derived from.
type Record struct {
	Payload spatial.Payload
	At      time.Time
	Inputs  []string
}

// Component wraps the record as a World component: Inferred provenance with
// ModelID "wendy.spatial.multifloor" and the record's ancestry. input turns an
// InputID into the ancestry entry of that measurement (measured, simulated or
// replayed), which only the caller knows.
func (r Record) Component(scope spatial.Scope, version world.Version, frame world.FrameRef, clockDomain string, ttl time.Duration, appVersion string, input func(id string) world.Input) (world.Component, error) {
	inputs := make([]world.Input, len(r.Inputs))
	for i, id := range r.Inputs {
		inputs[i] = input(id)
	}
	prov, err := spatial.InferredProvenance(Feature, ModelVersion, appVersion, spatial.ReduceInputs(inputs))
	if err != nil {
		return world.Component{}, err
	}
	return spatial.ToComponent(scope, version, frame, world.Timing{SampleTime: r.At, ClockDomain: clockDomain}, ttl, prov, r.Payload)
}

// Mapper is the streaming floor model. It is not safe for concurrent use, and
// its state depends only on the order of the calls.
type Mapper struct {
	cfg     Config
	gate    float64
	devices map[spatial.DeviceID]*device
	levels  []*level
	portals []*portal

	samples       []GeoSample
	stride, skip  int
	pending       int
	fit           spatial.GeoReference
	tangent       *geography.Tangent
	lastGeoSample time.Time
}

// level is one storey: the indoor floor and the outdoor floor beside it.
type level struct {
	id        spatial.FloorID
	elevation float64
	arrivals  int
	side      [2]side // indoors, outdoors
}

type side struct {
	seen, extent bool
	lo, hi       geom.Vec2
	at           time.Time
	inputs       []string
}

type end struct {
	level   int
	outdoor bool
	pos     geom.Vec2
}

type portal struct {
	id     string
	kind   spatial.PortalKind
	a, b   end // running means
	n      int
	at     time.Time
	inputs []string
}

type stamped struct {
	t       time.Time
	dist    float64 // odometer
	pose    geom.Pose2
	located bool
}

type stampedID struct {
	t  time.Time
	id string
}

type device struct {
	id                  spatial.DeviceID
	now                 time.Time
	odomT, baroT, gnssT time.Time
	odom                geom.Pose2
	hasOdom             bool
	dist                float64 // odometer
	blind               float64 // distance run since the last pose fix

	pose    geom.Pose2
	cov     geom.Mat3
	located bool
	frame   string
	score   float64
	trail   []stamped // the last Floor.History of poses

	det     *Detector
	level   int
	outdoor bool
	baro    []stampedID // the last Floor.History of barometer sample ids

	lastFix   time.Time
	streak    int
	entered   time.Time
	enteredID string

	odomID, baroID, gnssID string
}

// unobserved is the yaw variance given to a GNSS fix, which has no heading.
const unobserved = 1e9

// New returns an empty Mapper.
func New(cfg Config) (*Mapper, error) {
	cfg, err := cfg.withDefaults()
	if err != nil {
		return nil, err
	}
	gate, err := geom.ChiSquareQuantile(2, cfg.GnssGate)
	if err != nil {
		return nil, fmt.Errorf("multifloor: GnssGate: %w", err)
	}
	return &Mapper{
		cfg: cfg, gate: gate, devices: map[spatial.DeviceID]*device{}, stride: 1,
		levels: []*level{{id: cfg.StartFloor, elevation: cfg.StartElevationM}},
	}, nil
}

// Ingest takes the next measurement. Odometry moves the device's pose, a
// barometer sample may complete a change of storey, a GNSS fix may move the
// device outdoors, feeds the geo reference and, once there is one, corrects
// the pose. Other measurements are ignored. Measurements of one kind from one
// device must be in time order (ErrOutOfOrder otherwise). It returns the floor
// changes this measurement completed, usually none.
func (m *Mapper) Ingest(meas spatial.Measurement) ([]Transition, error) {
	switch meas.(type) {
	case spatial.Odometry, spatial.Baro, spatial.GnssFix:
	default:
		return nil, nil
	}
	if err := meas.Validate(); err != nil {
		return nil, err
	}
	dev, _ := meas.From()
	t, id := meas.When(), spatial.InputID(meas)
	d := m.device(dev, t, id)
	late := func(last *time.Time) bool {
		if !t.After(*last) {
			return true
		}
		*last = t
		return false
	}
	switch v := meas.(type) {
	case spatial.Odometry:
		if late(&d.odomT) {
			return nil, ErrOutOfOrder
		}
		out := m.tick(d, t)
		if d.hasOdom {
			delta := d.odom.Between(v.Pose)
			d.dist += delta.Pos().Norm()
			if d.located {
				noise := v.Noise
				if noise == (spatial.OdometryNoise{}) {
					noise = m.cfg.Odometry
				}
				d.cov = symmetric(geom.ComposeCov(d.pose, d.cov, delta, d.drift(noise, delta)))
				d.pose = d.pose.Compose(delta).Normalized()
			}
		}
		d.odom, d.hasOdom, d.odomID = v.Pose, true, id
		d.trail = append(d.trail, stamped{t, d.dist, d.pose, d.located})
		for len(d.trail) > 1 && t.Sub(d.trail[0].t) > m.cfg.Floor.History {
			d.trail = d.trail[1:]
		}
		m.cover(d)
		return out, nil
	case spatial.Baro:
		if late(&d.baroT) {
			return nil, ErrOutOfOrder
		}
		out := m.tick(d, t)
		d.baroID = id
		d.baro = append(d.baro, stampedID{t, id})
		for t.Sub(d.baro[0].t) > m.cfg.Floor.History {
			d.baro = d.baro[1:]
		}
		if mv, ok := d.det.Add(t, PressureAltitude(v.PressurePa), v.AltitudeSigma); ok {
			out = append(out, m.climb(d, mv))
		}
		return out, nil
	case spatial.GnssFix:
		if late(&d.gnssT) {
			return nil, ErrOutOfOrder
		}
		out := m.tick(d, t)
		if v.HorizontalAccuracy > m.cfg.GnssMaxAccuracyM {
			return out, nil
		}
		d.gnssID = id
		if d.lastFix.IsZero() || t.Sub(d.lastFix) > m.cfg.GnssTimeout {
			d.streak, d.entered, d.enteredID = 0, t.Add(-m.cfg.GnssInterval/2), id
		}
		d.streak, d.lastFix = d.streak+1, t
		if !d.outdoor && d.streak >= m.cfg.OutdoorFixes {
			out = append(out, m.cross(d, true, d.entered, d.enteredID, id))
		}
		if d.located && d.hasOdom {
			m.locate(d, v)
		}
		return out, nil
	}
	return nil, nil
}

// FixPose takes a site-frame pose of a device from a localizer, valid at the
// given instant on the device's odometry clock. Only the planar pose, its
// covariance, the frame id and the score are used: the floor and the
// elevation are this package's to estimate. Between fixes the pose follows
// odometry; a fix older than the newest odometry is carried forward by the
// motion since. A device has no pose until its first fix.
func (m *Mapper) FixPose(p spatial.DevicePose, at time.Time) ([]Transition, error) {
	z := p.Pose2()
	for _, v := range append([]float64{z.X, z.Z, z.Yaw, p.Score}, p.Covariance[:]...) {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, errors.New("multifloor: pose fix is not finite")
		}
	}
	if p.Device == "" || p.FrameID == "" || at.IsZero() {
		return nil, errors.New("multifloor: pose fix needs a device, a frame and an instant")
	}
	d := m.device(p.Device, at, "")
	out := m.tick(d, at)
	f2, y2 := m.cfg.FixSigmaFloorM*m.cfg.FixSigmaFloorM, m.cfg.FixYawFloorRad*m.cfg.FixYawFloorRad
	r := symmetric(p.Covariance).Add(geom.Mat3{f2, 0, 0, 0, f2, 0, 0, 0, y2})
	switch {
	case !d.located || !d.hasOdom:
		// Without odometry there is nothing to blend a fix with.
		d.pose, d.cov, d.located = z.Normalized(), r, true
	default:
		if then := d.at(at); then.located {
			z = z.Compose(then.pose.Between(d.pose))
		}
		if err := d.fuse(z, r); err != nil {
			return out, err
		}
	}
	d.frame, d.score, d.blind = p.FrameID, p.Score, 0
	if n := len(d.trail); n > 0 && d.hasOdom {
		d.trail[n-1].pose, d.trail[n-1].located = d.pose, true
	} else {
		d.trail = append(d.trail[:0], stamped{at, 0, d.pose, true})
	}
	m.cover(d)
	return out, nil
}

// Pose is the current pose of a device in the frame of its pose fixes, with
// the floor it is on. The elevation is that of the floor, plus the height
// climbed so far while the device is between floors. It is false until the
// device has had a pose fix.
func (m *Mapper) Pose(dev spatial.DeviceID) (spatial.DevicePose, bool) {
	d := m.devices[dev]
	if d == nil || !d.located {
		return spatial.DevicePose{}, false
	}
	elevation := m.levels[d.level].elevation
	if off, _ := d.det.Offset(); d.det.Moving() {
		elevation += off
	}
	return spatial.DevicePose{
		Device: d.id, Floor: m.floorID(d.level, d.outdoor), FrameID: d.frame,
		Location: spatial.At(d.pose.Pos().Lift(elevation), geom.PositionCov(d.cov).MaxSigma()),
		Yaw:      d.pose.Yaw, Covariance: d.cov, Method: PoseMethod, Score: d.score,
	}, true
}

// Floor is the floor a device is on, with or without a pose. moving reports
// that its barometer has left the floor's level: it is on the stairs or in a
// lift and its next floor is not yet known. ok is false for a device that has
// sent nothing.
func (m *Mapper) Floor(dev spatial.DeviceID) (floor spatial.FloorID, moving, ok bool) {
	d := m.devices[dev]
	if d == nil {
		return "", false, false
	}
	return m.floorID(d.level, d.outdoor), d.det.Moving(), true
}

// Floors describes every floor a device has been on, storeys in order of
// discovery and the outdoor floor after its storey. Location is the extent of
// the poses seen there.
func (m *Mapper) Floors() []spatial.FloorInfo {
	var out []spatial.FloorInfo
	m.eachFloor(func(f spatial.FloorInfo, _ *side) { out = append(out, f) })
	return out
}

func (m *Mapper) eachFloor(fn func(spatial.FloorInfo, *side)) {
	for i, l := range m.levels {
		for o := range l.side {
			s := &l.side[o]
			if !s.seen {
				continue
			}
			f := spatial.FloorInfo{Floor: m.floorID(i, o == 1), ElevationM: l.elevation, Outdoor: o == 1}
			for _, p := range m.portals {
				if len(f.Portals) < spatial.MaxFloorPortals && ((p.a.level == i && p.a.outdoor == f.Outdoor) || (p.b.level == i && p.b.outdoor == f.Outdoor)) {
					f.Portals = append(f.Portals, m.payload(p))
				}
			}
			if s.extent {
				loc := spatial.Enclose(l.elevation, s.lo, s.hi)
				f.Location = &loc
			}
			fn(f, s)
		}
	}
}

// Portals are the places where devices changed floor, in order of discovery.
// A portal's ends are the mean positions at which the changes began and
// ended, the lower or the indoor floor first, and its confidence grows with
// the number of passages.
func (m *Mapper) Portals() []spatial.Portal {
	out := make([]spatial.Portal, len(m.portals))
	for i, p := range m.portals {
		out[i] = m.payload(p)
	}
	return out
}

func (m *Mapper) payload(p *portal) spatial.Portal {
	ea, eb := m.levels[p.a.level].elevation, m.levels[p.b.level].elevation
	return spatial.Portal{
		ID: p.id, Kind: p.kind, State: spatial.PortalOpen, Confidence: float64(p.n) / float64(p.n+1),
		A:        spatial.PortalEnd{Floor: m.floorID(p.a.level, p.a.outdoor), PositionM: p.a.pos.Lift(ea).Array()},
		B:        spatial.PortalEnd{Floor: m.floorID(p.b.level, p.b.outdoor), PositionM: p.b.pos.Lift(eb).Array()},
		Location: spatial.Enclose((ea+eb)/2, p.a.pos, p.b.pos),
	}
}

// GeoReference fits the geo reference to every fix collected so far. See
// Georeference for the errors with which it refuses.
func (m *Mapper) GeoReference() (GeoFit, error) { return Georeference(m.samples, m.cfg.Geo) }

// Records is everything to publish: the floors, the portals, the geo
// reference when there is one, and the pose of every located device that has
// sent a measurement, devices by id.
func (m *Mapper) Records() []Record {
	var out []Record
	m.eachFloor(func(f spatial.FloorInfo, s *side) { out = append(out, Record{f, s.at, slices.Clone(s.inputs)}) })
	for _, p := range m.portals {
		out = append(out, Record{m.payload(p), p.at, slices.Clone(p.inputs)})
	}
	if fit, err := m.GeoReference(); err == nil {
		out = append(out, Record{fit.Reference, m.lastGeoSample, fit.Inputs})
	}
	ids := make([]spatial.DeviceID, 0, len(m.devices))
	for id := range m.devices {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		d := m.devices[id]
		if inputs := note(nil, d.odomID, d.baroID, d.gnssID); len(inputs) > 0 {
			if p, ok := m.Pose(id); ok {
				out = append(out, Record{p, d.now, inputs})
			}
		}
	}
	return out
}

func (m *Mapper) device(id spatial.DeviceID, t time.Time, input string) *device {
	d := m.devices[id]
	if d == nil {
		d = &device{id: id, det: NewDetector(m.cfg.Floor)}
		m.devices[id] = d
	}
	if t.After(d.now) {
		d.now = t
	}
	if s := &m.levels[d.level].side[index(d.outdoor)]; !s.seen || len(s.inputs) == 0 {
		m.see(d.level, d.outdoor, d.now, input)
	}
	return d
}

func (m *Mapper) floorID(level int, outdoor bool) spatial.FloorID {
	if outdoor {
		return m.levels[level].id + "-outdoor"
	}
	return m.levels[level].id
}

// see records that a floor exists and what showed it.
func (m *Mapper) see(level int, outdoor bool, t time.Time, inputs ...string) {
	s := &m.levels[level].side[index(outdoor)]
	s.seen, s.at, s.inputs = true, t, note(s.inputs, inputs...)
}

// cover grows the extent of the floor a device stands on.
func (m *Mapper) cover(d *device) {
	if !d.located || d.det.Moving() {
		return
	}
	s, p := &m.levels[d.level].side[index(d.outdoor)], d.pose.Pos()
	if !s.extent {
		s.extent, s.lo, s.hi = true, p, p
	}
	s.lo, s.hi = geom.Vec2{X: math.Min(s.lo.X, p.X), Z: math.Min(s.lo.Z, p.Z)}, geom.Vec2{X: math.Max(s.hi.X, p.X), Z: math.Max(s.hi.Z, p.Z)}
}

// tick notices a receiver that has fallen silent: the device is indoors again.
func (m *Mapper) tick(d *device, t time.Time) []Transition {
	if !d.outdoor || t.Sub(d.lastFix) <= m.cfg.GnssTimeout {
		return nil
	}
	return []Transition{m.cross(d, false, d.lastFix.Add(m.cfg.GnssInterval/2), d.gnssID)}
}

// cross moves a device between a storey and the open ground beside it.
func (m *Mapper) cross(d *device, outdoor bool, at time.Time, inputs ...string) Transition {
	inputs = note(inputs, d.odomID)
	tr := Transition{
		Device: d.id, From: m.floorID(d.level, d.outdoor), To: m.floorID(d.level, outdoor), Kind: spatial.PortalDoor,
		Start: at, At: at, End: at, Confirmed: d.now, Inputs: inputs,
	}
	d.outdoor = outdoor
	m.see(d.level, outdoor, d.now, inputs...)
	if e := d.at(at); e.located {
		tr.Portal = m.join(spatial.PortalDoor, end{d.level, false, e.pose.Pos()}, end{d.level, true, e.pose.Pos()}, d.now, inputs)
	}
	return tr
}

// climb moves a device to the storey a vertical move ended on.
func (m *Mapper) climb(d *device, mv Move) Transition {
	a, b := d.at(mv.Start), d.at(mv.End)
	var kind spatial.PortalKind
	if travel := b.dist - a.dist; d.hasOdom {
		switch {
		case travel < m.cfg.LiftMaxTravelM:
			kind = spatial.PortalLift
		case math.Abs(mv.DeltaM)/travel < m.cfg.RampMaxSlope:
			kind = spatial.PortalRamp
		default:
			kind = spatial.PortalStairs
		}
	}
	inputs := note(nil, d.baroAt(mv.Start), d.baroAt(mv.End), d.baroID, d.odomID)
	from, to := d.level, m.match(m.levels[d.level].elevation+mv.DeltaM)
	tr := Transition{
		Device: d.id, From: m.floorID(from, d.outdoor), To: m.floorID(to, d.outdoor), Kind: kind,
		Start: mv.Start, At: mv.Mid, End: mv.End, Confirmed: mv.Confirmed, DeltaM: mv.DeltaM, SigmaM: mv.SigmaM, Inputs: inputs,
	}
	d.level = to
	m.see(to, d.outdoor, mv.Confirmed, inputs...)
	if kind != "" && a.located && b.located {
		tr.Portal = m.join(kind, end{from, d.outdoor, a.pose.Pos()}, end{to, d.outdoor, b.pose.Pos()}, mv.Confirmed, inputs)
	}
	return tr
}

// match is the storey at an elevation: the nearest known one within
// FloorMatchM, whose elevation takes the new height into its mean, or a new
// one. The start floor is the datum and never moves.
func (m *Mapper) match(elevation float64) int {
	best := -1
	for i, l := range m.levels {
		if d := math.Abs(l.elevation - elevation); d <= m.cfg.FloorMatchM && (best < 0 || d < math.Abs(m.levels[best].elevation-elevation)) {
			best = i
		}
	}
	if best < 0 {
		m.levels = append(m.levels, &level{id: spatial.FloorID(fmt.Sprintf("level-%d", len(m.levels))), elevation: elevation, arrivals: 1})
		return len(m.levels) - 1
	}
	if l := m.levels[best]; best > 0 {
		l.elevation = (l.elevation*float64(l.arrivals) + elevation) / float64(l.arrivals+1)
		l.arrivals++
	}
	return best
}

// join records one passage between two floor ends and returns the id of the
// portal it belongs to.
func (m *Mapper) join(kind spatial.PortalKind, a, b end, t time.Time, inputs []string) string {
	if ea, eb := m.levels[a.level].elevation, m.levels[b.level].elevation; ea > eb || (a.level == b.level && a.outdoor) {
		a, b = b, a
	}
	same := func(p, q end) bool {
		return p.level == q.level && p.outdoor == q.outdoor && p.pos.Dist(q.pos) <= m.cfg.PortalMergeM
	}
	count := 1
	for _, p := range m.portals {
		if p.kind != kind {
			continue
		}
		count++
		if same(p.a, a) && same(p.b, b) {
			w := 1 / float64(p.n+1)
			p.a.pos, p.b.pos = p.a.pos.Lerp(a.pos, w), p.b.pos.Lerp(b.pos, w)
			p.n, p.at, p.inputs = p.n+1, t, note(p.inputs, inputs...)
			return p.id
		}
	}
	p := &portal{id: fmt.Sprintf("%s-%d", kind, count), kind: kind, a: a, b: b, n: 1, at: t, inputs: note(nil, inputs...)}
	m.portals = append(m.portals, p)
	return p.id
}

// locate uses a GNSS fix: as evidence for the geo reference, paired with the
// pose before the fix touches it, and then as a correction of that pose.
func (m *Mapper) locate(d *device, fix spatial.GnssFix) {
	if geom.PositionCov(d.cov).MaxSigma() <= m.cfg.GeoMaxPoseSigmaM {
		if m.skip++; m.skip >= m.stride {
			m.skip = 0
			m.samples = append(m.samples, GeoSample{Site: d.pose.Pos().Lift(m.levels[d.level].elevation), Fix: fix})
			m.pending, m.lastGeoSample = m.pending+1, fix.Time
			if len(m.samples) > m.cfg.GeoMaxSamples {
				keep := m.samples[:0]
				for i := 0; i < len(m.samples); i += 2 {
					keep = append(keep, m.samples[i])
				}
				m.samples, m.stride = keep, m.stride*2
			}
		}
	}
	if m.pending >= m.cfg.GeoRefit {
		m.pending = 0
		if fit, err := m.GeoReference(); err == nil {
			if tangent, err := geography.NewTangent(fit.Reference.Origin); err == nil {
				m.fit, m.tangent = fit.Reference, tangent
			}
		}
	}
	if m.tangent == nil {
		return
	}
	z, err := siteOf(m.fit, m.tangent, fix)
	if err != nil {
		return
	}
	// ponytail: the slowly varying GNSS error is fused as if it were white,
	// so outdoors without pose fixes the reported pose sigma settles below
	// Geo.BiasM although the pose cannot be better than that; carry the bias
	// as a filter state if the covariance must stay honest there.
	lever := z.Dist(m.fit.Point().XZ()) * m.fit.YawSigmaRad
	r := fix.HorizontalAccuracy*fix.HorizontalAccuracy + m.fit.UncertaintyM*m.fit.UncertaintyM + lever*lever
	innovation := geom.PositionCov(d.cov).Add(geom.Cov2{r, 0, 0, r})
	if md, err := geom.MahalanobisSq2(z.Sub(d.pose.Pos()), innovation); err != nil || md > m.gate {
		return
	}
	if d.fuse(geom.Pose2{X: z.X, Z: z.Z, Yaw: d.pose.Yaw}, geom.Mat3{r, 0, 0, 0, r, 0, 0, 0, unobserved}) == nil {
		d.trail[len(d.trail)-1].pose = d.pose
	}
}

// drift is the covariance of one odometry increment. On top of the white
// part of the noise model it carries the model's other reading: the per-metre
// figures are also a bias of the device, whose error grows with the distance
// D run since the last pose fix and not with its root. The variance of a
// bias b·D grows by 2·b²·D per metre, which is what is added here, so the
// pose covariance stays honest through a long stretch without fixes.
func (d *device) drift(noise spatial.OdometryNoise, delta geom.Pose2) geom.Mat3 {
	q := noise.Covariance(delta)
	step := delta.Pos().Norm()
	if step == 0 {
		return q
	}
	u, grow := delta.Pos().Unit(), 2*d.blind*step
	t2, y2 := noise.TransPerMetre*noise.TransPerMetre*grow, noise.YawPerMetre*noise.YawPerMetre*grow
	q[0], q[1], q[3], q[4], q[8] = q[0]+t2*u.X*u.X, q[1]+t2*u.X*u.Z, q[3]+t2*u.X*u.Z, q[4]+t2*u.Z*u.Z, q[8]+y2
	d.blind += step
	return q
}

// fuse is the Kalman update of the pose with an observed pose z of
// covariance r.
func (d *device) fuse(z geom.Pose2, r geom.Mat3) error {
	inv, err := d.cov.Add(r).Inverse()
	if err != nil {
		return fmt.Errorf("multifloor: pose update: %w", err)
	}
	k := d.cov.Mul(inv)
	dx := k.MulVec(geom.Vec3{X: z.X - d.pose.X, Y: z.Z - d.pose.Z, Z: geom.AngleDiff(z.Yaw, d.pose.Yaw)})
	d.pose = geom.Pose2{X: d.pose.X + dx.X, Z: d.pose.Z + dx.Y, Yaw: geom.WrapAngle(d.pose.Yaw + dx.Z)}
	d.cov = symmetric(geom.Identity3().Add(k.Scale(-1)).Mul(d.cov))
	return nil
}

// at is the trail entry nearest an instant; the zero entry without a trail.
func (d *device) at(t time.Time) stamped {
	n := len(d.trail)
	if n == 0 {
		return stamped{}
	}
	i := sort.Search(n, func(i int) bool { return !d.trail[i].t.Before(t) })
	if i == n || (i > 0 && t.Sub(d.trail[i-1].t) < d.trail[i].t.Sub(t)) {
		i--
	}
	return d.trail[i]
}

// baroAt is the id of the barometer sample nearest an instant.
func (d *device) baroAt(t time.Time) string {
	n := len(d.baro)
	i := sort.Search(n, func(i int) bool { return !d.baro[i].t.Before(t) })
	if i == n {
		i--
	}
	return d.baro[i].id
}

func symmetric(c geom.Mat3) geom.Mat3 {
	c[1], c[3] = (c[1]+c[3])/2, (c[1]+c[3])/2
	c[2], c[6] = (c[2]+c[6])/2, (c[2]+c[6])/2
	c[5], c[7] = (c[5]+c[7])/2, (c[5]+c[7])/2
	return c
}

func index(outdoor bool) int {
	if outdoor {
		return 1
	}
	return 0
}

// maxNoted bounds an ancestry list: the earliest and the latest half are kept.
const maxNoted = 16

// note appends the ids that are new and not empty.
func note(list []string, ids ...string) []string {
	for _, id := range ids {
		if id != "" && !slices.Contains(list, id) {
			list = append(list, id)
		}
	}
	if n := len(list); n > maxNoted {
		list = append(list[:maxNoted/2], list[n-maxNoted/2:]...)
	}
	return list
}
