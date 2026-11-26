package spatialsim

import (
	"encoding/binary"
	"errors"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/geography"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// ClockDomain is the World clock domain of every simulated site: site time,
// as Site.At returns it. Components built from simulated measurements carry it
// in Timing.ClockDomain, and a replica that receives them is configured with it.
const ClockDomain = "wendy.spatial.sim"

// ScenarioVersion names the revision of the scenarios in this package. It
// changes when a layout, route or schedule does.
const ScenarioVersion = "2"

// SiteFrame is the World frame every simulated site-frame record is published
// in. Reference.DevicePose names it as its FrameID.
var SiteFrame = world.FrameRef{ID: "site", Revision: "sim-" + ScenarioVersion}

// Sensor ids. A simulated device carries at most one sensor of each kind.
const (
	SensorLidar    spatial.SensorID = "lidar"
	SensorOdometry spatial.SensorID = "odom"
	SensorImu      spatial.SensorID = "imu"
	SensorBaro     spatial.SensorID = "baro"
	SensorGnss     spatial.SensorID = "gnss"
	SensorCamera   spatial.SensorID = "camera"
	SensorUwb      spatial.SensorID = "uwb"
	SensorMic      spatial.SensorID = "mic"
	SensorGas      spatial.SensorID = "gas"
)

// SensorOf is the sensor id every simulated device uses for a kind.
func SensorOf(kind spatial.SensorKind) spatial.SensorID {
	switch kind {
	case spatial.KindLidar2D:
		return SensorLidar
	case spatial.KindOdometry:
		return SensorOdometry
	case spatial.KindImu:
		return SensorImu
	case spatial.KindBaro:
		return SensorBaro
	case spatial.KindGnss:
		return SensorGnss
	case spatial.KindCamera:
		return SensorCamera
	case spatial.KindUwb:
		return SensorUwb
	case spatial.KindMic:
		return SensorMic
	case spatial.KindGas:
		return SensorGas
	}
	return ""
}

// MarkerID is the fiducial id of the marker a robot carries.
func MarkerID(dev spatial.DeviceID) string { return "marker-" + string(dev) }

// GasBackgroundPPM is the concentration far from every gas source.
const GasBackgroundPPM = 420.0

const (
	gravity     = 9.80665
	tagSide     = 0.2  // metres, edge of a square wall tag
	tagStandoff = 0.02 // metres between a wall and the tag plane
	tagSpacing  = 3.5  // metres of wall per tag
	turnRate    = 1.5  // rad/s, turning on the spot between legs
	liftSpeed   = 0.5  // m/s, vertical legs
	odomStep    = 20 * time.Millisecond
	never       = time.Duration(math.MaxInt64)
)

var epoch = time.Date(2026, time.January, 1, 9, 0, 0, 0, time.UTC)

// Site is one simulated world and its schedule. It is immutable after
// construction and safe for concurrent use.
type Site struct {
	scenario     string // "office", "single-room", ...
	seed         uint64
	cfg          Config
	duration     time.Duration
	levels       []*level // ascending elevation
	floors       []*floor
	devices      []*device // ascending id
	byID         map[spatial.DeviceID]*device
	actors       []*actor
	beacons      []Beacon
	sounds       []SoundSource
	gas          []GasSource
	vertical     []spatial.Portal // stairs and lifts
	target       spatial.TargetGeometry
	shows        []TargetShow
	geo          spatial.GeoReference
	tangent      *geography.Tangent
	ambiguity    *Ambiguity
	changes      []spatial.ChangeEvent
	disturbances []Disturbance
	classes      []string // every object class, sorted
}

// level is one walking surface. Floors that share an elevation (a ground
// floor and its yard) share a level, because sensors see across them.
type level struct {
	elevation float64
	floors    []*floor
	walls     []geom.Segment
	doors     []*Door
	furniture []*furniture
	tags      []Landmark
}

type floor struct {
	id      spatial.FloorID
	name    string
	level   *level
	outdoor bool
	rooms   []Room
}

type furniture struct {
	id, class   string
	floor       *floor
	shape       Shape
	from, until time.Duration // present in [from, until); until 0 means for good
	moveAt      time.Duration // 0 means never
	moved       Shape
}

func (f *furniture) at(t time.Duration) (Shape, bool) {
	if t < f.from || (f.until > 0 && t >= f.until) {
		return Shape{}, false
	}
	if f.moveAt > 0 && t >= f.moveAt {
		return f.moved, true
	}
	return f.shape, true
}

type actor struct {
	id, class string
	level     *level
	route     *route
	phase     float64 // seconds into the loop at the epoch
	shape     Shape   // Centre and Yaw are filled per instant
}

type sensor struct {
	id     spatial.SensorID
	kind   spatial.SensorKind
	model  string
	period time.Duration
	clock  time.Duration // sensor clock minus site time
}

// device is a robot, a handheld, a fixed camera or a microphone array.
type device struct {
	id      spatial.DeviceID
	start   time.Duration // nothing is produced before
	kinds   []spatial.SensorKind
	sensors []sensor

	// A mobile device follows route; a fixed one sits on level.
	route          *route
	class          string // object class a camera reports for the body
	radius, height float64
	level          *level
	pose           geom.Pose2 // fixed devices without a camera
	elevation      float64    // World Y of a fixed device

	uwbMount   geom.Vec3     // antenna in the base frame
	lidarMount [2]geom.Pose2 // sensor to base, before and after lidarShift
	lidarShift time.Duration // 0 means never
	marker     []geom.Vec3   // base frame
	camPose    [2]geom.Pose3 // optical frame to base (mobile) or site (fixed), before and after camBump
	camBump    time.Duration // 0 means never
	intrinsics geom.Intrinsics
	declared   geom.Intrinsics // what the manifest announces

	odomBias [3]float64 // scale, yaw per metre, yaw per radian
	gyroBias float64
	baroRef  float64
	phase    [3]float64 // baro drift, GNSS bias x, GNSS bias z

	odomMu sync.Mutex
	odom   []geom.Pose2 // integrated odometry at multiples of odomStep
}

func (d *device) lidarAt(t time.Duration) geom.Pose2 {
	if d.lidarShift > 0 && t >= d.lidarShift {
		return d.lidarMount[1]
	}
	return d.lidarMount[0]
}

func (d *device) cameraAt(t time.Duration) geom.Pose3 {
	if d.camBump > 0 && t >= d.camBump {
		return d.camPose[1]
	}
	return d.camPose[0]
}

// sensorOf is the device's sensor of a kind, or nil.
func (d *device) sensorOf(kind spatial.SensorKind) *sensor {
	for i := range d.sensors {
		if d.sensors[i].kind == kind {
			return &d.sensors[i]
		}
	}
	return nil
}

func (d *device) has(kind spatial.SensorKind) bool { return d.sensorOf(kind) != nil }

// Epoch is site time zero.
func (s *Site) Epoch() time.Time { return epoch }

// At converts an offset from the epoch to site time.
func (s *Site) At(d time.Duration) time.Time { return spatial.Stamp(epoch.UnixNano() + int64(d)) }

// Duration is the length of the scripted scenario. Routes loop, so
// measurements exist beyond it, but nothing new is scheduled.
func (s *Site) Duration() time.Duration { return s.duration }

func (s *Site) Seed() uint64   { return s.seed }
func (s *Site) Config() Config { return s.cfg }

// Scenario is the name of the scenario the site was built from.
func (s *Site) Scenario() string { return s.scenario }

// SimulationContext is the provenance context shared by every measurement of
// this site, so that records derived by different features apply to one
// world.Replica: the run is the (scenario, seed) pair, the clock is ClockDomain.
func (s *Site) SimulationContext() world.SimulationContext {
	return world.SimulationContext{RunID: s.scenario + "-" + strconv.FormatUint(s.seed, 10), ScenarioVersion: s.scenario + "/" + ScenarioVersion, Seed: s.seed, ClockDomain: ClockDomain}
}

// Input is the ancestry entry of one simulated measurement.
func (s *Site) Input(m spatial.Measurement) world.Input {
	sim := s.SimulationContext()
	return world.Input{ID: spatial.InputID(m), Origin: world.Simulated, Simulation: &sim}
}

// Devices lists every device that produces measurements, by id.
func (s *Site) Devices() []spatial.DeviceID {
	out := make([]spatial.DeviceID, len(s.devices))
	for i, d := range s.devices {
		out[i] = d.id
	}
	return out
}

// Truth returns the ground-truth view of the site. It is for scoring only.
func (s *Site) Truth() Truth { return Truth{s} }

// rng returns the random stream of one (device, stream, n) triple. Streams
// are independent of each other and of the order in which they are opened.
func (s *Site) rng(dev spatial.DeviceID, stream string, n int64) *rand.Rand {
	h := fnv.New64a()
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], s.seed)
	h.Write(b[:])
	h.Write([]byte(dev))
	h.Write([]byte{0})
	h.Write([]byte(stream))
	h.Write([]byte{0})
	binary.LittleEndian.PutUint64(b[:], uint64(n))
	h.Write(b[:])
	a := mix(h.Sum64())
	return rand.New(rand.NewPCG(a, mix(a^0x9e3779b97f4a7c15)))
}

// mix is the splitmix64 finaliser.
func mix(x uint64) uint64 {
	x ^= x >> 30
	x *= 0xbf58476d1ce4e5b9
	x ^= x >> 27
	x *= 0x94d049bb133111eb
	return x ^ x>>31
}

func (s *Site) floor(id spatial.FloorID) *floor {
	for _, f := range s.floors {
		if f.id == id {
			return f
		}
	}
	return nil
}

// levelAt returns the level a body at the given World elevation is on:
// the nearest one, switching halfway up a staircase or lift shaft.
func (s *Site) levelAt(elevation float64) *level {
	for i, lv := range s.levels {
		if i+1 == len(s.levels) || elevation < (lv.elevation+s.levels[i+1].elevation)/2 {
			return lv
		}
	}
	return nil
}

// roomAt returns the room containing p and its floor, or nils.
func (lv *level) roomAt(p geom.Vec2) (*Room, *floor) {
	for _, f := range lv.floors {
		for i := range f.rooms {
			if geom.PointInPolygon(p, f.rooms[i].Polygon) {
				return &f.rooms[i], f
			}
		}
	}
	return nil, nil
}

// floorAt returns the floor of a point on the level; a point in no room
// (a doorway on a boundary) belongs to the level's first floor.
func (lv *level) floorAt(p geom.Vec2) *floor {
	if _, f := lv.roomAt(p); f != nil {
		return f
	}
	return lv.floors[0]
}

// wp is a route waypoint: a planar position and the World elevation there.
type wp struct {
	p    geom.Vec2
	elev float64
}

// leg is a stretch of constant velocity: a straight move, a turn on the
// spot, or a vertical ride.
type leg struct {
	t0, t1 float64 // seconds from the start of the loop
	a, b   geom.Pose2
	ea, eb float64
}

// route is a closed loop of legs that repeats with the given period.
type route struct {
	legs   []leg
	period float64
}

// newRoute joins the waypoints, and the last back to the first, with straight
// moves at speed and turns on the spot. Two successive waypoints at the same
// position and different elevations are a lift ride.
func newRoute(wps []wp, speed float64) (*route, error) {
	n := len(wps)
	heading := func(i int) (float64, bool) {
		a, b := wps[i], wps[(i+1)%n]
		return b.p.Sub(a.p).Angle(), a.p != b.p
	}
	yaw0, found := 0.0, false
	for i := 0; i < n && !found; i++ {
		yaw0, found = heading(i)
	}
	if !found || !(speed > 0) {
		return nil, errors.New("spatialsim: a route needs two distinct waypoints and a positive speed")
	}
	r := &route{}
	t, yaw := 0.0, yaw0
	add := func(dur float64, a, b geom.Pose2, ea, eb float64) {
		if dur > 0 {
			r.legs = append(r.legs, leg{t, t + dur, a, b, ea, eb})
			t += dur
		}
	}
	turn := func(at wp, to float64) {
		add(math.Abs(geom.AngleDiff(to, yaw))/turnRate, geom.Pose2{X: at.p.X, Z: at.p.Z, Yaw: yaw}, geom.Pose2{X: at.p.X, Z: at.p.Z, Yaw: to}, at.elev, at.elev)
		yaw = to
	}
	for i := range wps {
		a, b := wps[i], wps[(i+1)%n]
		h, moves := heading(i)
		if !moves {
			p := geom.Pose2{X: a.p.X, Z: a.p.Z, Yaw: yaw}
			add(math.Abs(b.elev-a.elev)/liftSpeed, p, p, a.elev, b.elev)
			continue
		}
		turn(a, h)
		add(a.p.Dist(b.p)/speed, geom.Pose2{X: a.p.X, Z: a.p.Z, Yaw: h}, geom.Pose2{X: b.p.X, Z: b.p.Z, Yaw: h}, a.elev, b.elev)
	}
	turn(wps[0], yaw0)
	r.period = t
	return r, nil
}

// retime rescales the moving legs so that the loop takes exactly period
// seconds; turns and lift rides keep their duration.
func (r *route) retime(period float64) error {
	moving := 0.0
	for _, l := range r.legs {
		if l.a.Pos() != l.b.Pos() {
			moving += l.t1 - l.t0
		}
	}
	k := (period - (r.period - moving)) / moving
	if !(k > 0) {
		return errors.New("spatialsim: route period is shorter than its turns")
	}
	t := 0.0
	for i := range r.legs {
		l := &r.legs[i]
		dur := l.t1 - l.t0
		if l.a.Pos() != l.b.Pos() {
			dur *= k
		}
		l.t0, l.t1 = t, t+dur
		t += dur
	}
	r.period = t
	return nil
}

// at returns the pose, elevation, planar velocity and yaw rate sec seconds
// after the mover started.
func (r *route) at(sec float64) (geom.Pose2, float64, geom.Vec2, float64) {
	sec = math.Mod(sec, r.period)
	i := sort.Search(len(r.legs)-1, func(i int) bool { return r.legs[i].t1 > sec })
	l := r.legs[i]
	dur := l.t1 - l.t0
	u := (sec - l.t0) / dur
	return l.a.Interpolate(l.b, u), l.ea + (l.eb-l.ea)*u, l.b.Pos().Sub(l.a.Pos()).Scale(1 / dur), geom.AngleDiff(l.b.Yaw, l.a.Yaw) / dur
}

// place returns a device at t: the planar pose and World elevation of its
// base, and the level it is on. t must not precede d.start.
func (s *Site) place(d *device, t time.Duration) (geom.Pose2, float64, *level) {
	if d.route == nil {
		return d.pose, d.elevation, d.level
	}
	p, e, _, _ := d.route.at((t - d.start).Seconds())
	return p, e, s.levelAt(e)
}

// tagAt is the site position of a device's UWB antenna and its level.
func (s *Site) tagAt(d *device, t time.Duration) (geom.Vec3, *level) {
	base, e, lv := s.place(d, t)
	return geom.Pose3FromPose2(base, e).Apply(d.uwbMount), lv
}

func (a *actor) at(t time.Duration) (Shape, geom.Vec2) {
	p, _, v, _ := a.route.at(t.Seconds() + a.phase)
	sh := a.shape
	sh.Centre, sh.Yaw = p.Pos(), p.Yaw
	return sh, v
}

// solid is anything that returns a lidar beam and can hide what is behind it.
type solid struct {
	id, class string
	shape     Shape
	corners   [4]geom.Vec2
	base      float64 // World elevation it stands on
}

// scene is everything movable on one level at one instant.
type scene struct {
	lv     *level
	doors  []geom.Segment // closed doors
	solids []solid
}

// barriers is the scene without its solids: enough for radio and sound.
func (s *Site) barriers(lv *level, t time.Duration) *scene {
	sc := &scene{lv: lv}
	for _, d := range lv.doors {
		if !d.OpenAt(t) {
			sc.doors = append(sc.doors, d.Segment)
		}
	}
	return sc
}

func (s *Site) scene(lv *level, t time.Duration) *scene {
	sc := s.barriers(lv, t)
	add := func(id, class string, sh Shape, base float64) {
		sc.solids = append(sc.solids, solid{id, class, sh, sh.Corners(), base})
	}
	for _, f := range lv.furniture {
		if sh, ok := f.at(t); ok {
			add(f.id, f.class, sh, lv.elevation)
		}
	}
	for _, a := range s.actors {
		if a.level == lv {
			sh, _ := a.at(t)
			add(a.id, a.class, sh, lv.elevation)
		}
	}
	for _, d := range s.devices {
		if d.route == nil || t < d.start {
			continue
		}
		if p, e, l := s.place(d, t); l == lv {
			add(string(d.id), d.class, Shape{Centre: p.Pos(), Yaw: p.Yaw, Radius: d.radius, Height: d.height}, e)
		}
	}
	return sc
}

// cut reports whether the segment a-b crosses sg.
func cut(a, b geom.Vec2, sg geom.Segment) bool {
	if math.Max(a.X, b.X) < math.Min(sg.A.X, sg.B.X) || math.Min(a.X, b.X) > math.Max(sg.A.X, sg.B.X) ||
		math.Max(a.Z, b.Z) < math.Min(sg.A.Z, sg.B.Z) || math.Min(a.Z, b.Z) > math.Max(sg.A.Z, sg.B.Z) {
		return false
	}
	_, ok := geom.Segment{A: a, B: b}.Intersect(sg)
	return ok
}

// walled reports whether a wall or a closed door separates a from b.
func (sc *scene) walled(a, b geom.Vec2) bool {
	for _, w := range sc.lv.walls {
		if cut(a, b, w) {
			return true
		}
	}
	for _, w := range sc.doors {
		if cut(a, b, w) {
			return true
		}
	}
	return false
}

// blocked reports whether the sight line from a to b is cut: by a wall or a
// closed door at any height, or by a solid where the line passes below its
// top. The solids named skip1 and skip2 are ignored.
func (sc *scene) blocked(a, b geom.Vec3, skip1, skip2 string) bool {
	p, q := a.XZ(), b.XZ()
	if sc.walled(p, q) {
		return true
	}
	line := geom.Segment{A: p, B: q}
	length := line.Length()
	if length == 0 {
		return false
	}
	for i := range sc.solids {
		so := &sc.solids[i]
		if so.id == skip1 || so.id == skip2 || line.DistanceTo(so.shape.Centre) > so.shape.Bound() {
			continue
		}
		top := so.base + so.shape.Height
		below := func(at geom.Vec2) bool { return a.Y+(b.Y-a.Y)*p.Dist(at)/length < top }
		if so.shape.Radius > 0 {
			if below(line.ClosestPoint(so.shape.Centre)) {
				return true
			}
			continue
		}
		for k := 0; k < 4; k++ {
			if at, ok := line.Intersect(geom.Segment{A: so.corners[k], B: so.corners[(k+1)%4]}); ok && below(at) {
				return true
			}
		}
	}
	return false
}
