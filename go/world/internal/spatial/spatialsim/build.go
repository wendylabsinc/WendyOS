package spatialsim

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

// builder assembles a Site. Scenario code calls its methods in any order and
// build() derives walls, wall tags, sensors and the truth event lists.
type builder struct {
	s   *Site
	opt Options
	err error
}

func newBuilder(scenario string, seed uint64, opt Options) (*builder, error) {
	cfg := DefaultConfig()
	if opt.Config != nil {
		cfg = *opt.Config
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	s := &Site{
		scenario: scenario, seed: seed, cfg: cfg, byID: map[spatial.DeviceID]*device{},
		target: spatial.TargetGeometry{ID: "board", Rows: 6, Cols: 8, Spacing: 0.08},
		// The site origin is anchored in Amsterdam; site +X points 20° south of east.
		geo: spatial.GeoReference{
			Origin:  world.GeographicAnchor{LatitudeDegrees: 52.3702, LongitudeDegrees: 4.8952, AltitudeMetres: 3, AltitudeDatum: geography.WGS84Ellipsoid},
			SiteYaw: 20 * math.Pi / 180, Method: "truth",
		},
	}
	tangent, err := geography.NewTangent(s.geo.Origin)
	if err != nil {
		return nil, err
	}
	s.tangent = tangent
	return &builder{s: s, opt: opt}, nil
}

func (b *builder) fail(err error) {
	if b.err == nil {
		b.err = err
	}
}

func (b *builder) level(elevation float64) *level {
	lv := &level{elevation: elevation}
	b.s.levels = append(b.s.levels, lv)
	return lv
}

func (b *builder) floor(id spatial.FloorID, name string, lv *level, outdoor bool) *floor {
	f := &floor{id: id, name: name, level: lv, outdoor: outdoor}
	b.s.floors = append(b.s.floors, f)
	lv.floors = append(lv.floors, f)
	return f
}

func rect(x0, z0, x1, z1 float64) []geom.Vec2 {
	return []geom.Vec2{{X: x0, Z: z0}, {X: x1, Z: z0}, {X: x1, Z: z1}, {X: x0, Z: z1}}
}

func (f *floor) room(id, label string, x0, z0, x1, z1 float64) {
	f.poly(id, label, rect(x0, z0, x1, z1))
}

func (f *floor) poly(id, label string, pts []geom.Vec2) {
	f.rooms = append(f.rooms, Room{ID: id, Label: label, Floor: f.id, Polygon: pts})
}

// shaft adds the footprint of a staircase or lift.
func (f *floor) shaft(id, label, portal string, x0, z0, x1, z1 float64) {
	f.rooms = append(f.rooms, Room{ID: id, Label: label, Floor: f.id, Polygon: rect(x0, z0, x1, z1), Portal: portal})
}

// door cuts a gap in the wall it lies on. Toggles are ignored under NoChanges.
func (b *builder) door(f *floor, id string, kind spatial.PortalKind, x0, z0, x1, z1 float64, open bool, toggles ...time.Duration) {
	if b.opt.NoChanges || kind != spatial.PortalDoor {
		toggles = nil
	}
	f.level.doors = append(f.level.doors, &Door{
		ID: id, Floor: f.id, Kind: kind, Segment: geom.Segment{A: geom.Vec2{X: x0, Z: z0}, B: geom.Vec2{X: x1, Z: z1}},
		InitiallyOpen: open || kind != spatial.PortalDoor, Toggles: toggles,
	})
}

func (b *builder) furnish(f *floor, id, class string, sh Shape) *furniture {
	fu := &furniture{id: id, class: class, floor: f, shape: sh}
	f.level.furniture = append(f.level.furniture, fu)
	return fu
}

// box places furniture w metres along yaw and d metres across it.
func (b *builder) box(f *floor, id, class string, cx, cz, w, d, yaw, height float64) *furniture {
	return b.furnish(f, id, class, Shape{Centre: geom.Vec2{X: cx, Z: cz}, Yaw: yaw, Half: geom.Vec2{X: w / 2, Z: d / 2}, Height: height})
}

func (b *builder) round(f *floor, id, class string, cx, cz, radius, height float64) *furniture {
	return b.furnish(f, id, class, Shape{Centre: geom.Vec2{X: cx, Z: cz}, Radius: radius, Height: height})
}

// appear makes the furniture absent until at; under NoChanges it never shows.
func (b *builder) appear(fu *furniture, at time.Duration) {
	fu.from = at
	if b.opt.NoChanges {
		fu.from = never
	}
}

func (b *builder) remove(fu *furniture, at time.Duration) {
	if !b.opt.NoChanges {
		fu.until = at
	}
}

func (b *builder) move(fu *furniture, at time.Duration, cx, cz, yaw float64) {
	if !b.opt.NoChanges {
		fu.moveAt, fu.moved = at, fu.shape
		fu.moved.Centre, fu.moved.Yaw = geom.Vec2{X: cx, Z: cz}, yaw
	}
}

func (b *builder) add(d *device) *device {
	if b.s.byID[d.id] != nil {
		b.fail(fmt.Errorf("spatialsim: duplicate device %q", d.id))
	}
	b.s.byID[d.id] = d
	b.s.devices = append(b.s.devices, d)
	return d
}

func (b *builder) mobile(id spatial.DeviceID, class string, start time.Duration, speed float64, wps []wp) *device {
	r, err := newRoute(wps, speed)
	if err != nil {
		b.fail(err)
		r = &route{legs: []leg{{t1: 1}}, period: 1}
	}
	return b.add(&device{id: id, class: class, start: start, route: r})
}

// mountLookAt is a camera mount in a base frame: at eye, looking along the
// base heading turned by yaw and raised by pitch.
func (b *builder) mountLookAt(eye geom.Vec3, yaw, pitch float64) geom.Pose3 {
	dir := geom.Vec3{X: math.Cos(yaw) * math.Cos(pitch), Y: math.Sin(pitch), Z: math.Sin(yaw) * math.Cos(pitch)}
	p, err := geom.LookAt(eye, eye.Add(dir), geom.Vec3{Y: 1})
	if err != nil {
		b.fail(err)
	}
	return p
}

// mobileLens is the factory-calibrated camera of robots and handhelds.
var mobileLens = geom.Intrinsics{FX: 900, FY: 900, CX: 640, CY: 360, K1: -0.08, K2: 0.01, Width: 1280, Height: 720}

// fixedLens is what a fixed camera's manifest declares. The true lens
// differs by a few percent and has distortion; see build.
var fixedLens = geom.Intrinsics{FX: 800, FY: 800, CX: 640, CY: 360, Width: 1280, Height: 720}

// robotMarker is the fiducial every robot carries: a ring of four points
// above the body and a wider ring of four lower down, in the base frame.
var robotMarker = []geom.Vec3{
	{X: 0.10, Y: 0.55, Z: 0.10}, {X: 0.10, Y: 0.55, Z: -0.10}, {X: -0.10, Y: 0.55, Z: -0.10}, {X: -0.10, Y: 0.55, Z: 0.10},
	{X: 0.20, Y: 0.30}, {Y: 0.30, Z: 0.20}, {X: -0.20, Y: 0.30}, {Y: 0.30, Z: -0.20},
}

// robot adds a robot with the full sensor suite. n varies the lidar and
// camera mounts between robots: the lidar sits 10 to 20 cm ahead of the base
// origin, a few centimetres off the centre line and yawed by 5° to 10°.
func (b *builder) robot(id spatial.DeviceID, n int, start time.Duration, speed float64, wps ...wp) *device {
	d := b.mobile(id, "robot", start, speed, wps)
	d.radius, d.height = 0.25, 0.5
	d.kinds = []spatial.SensorKind{spatial.KindLidar2D, spatial.KindOdometry, spatial.KindImu, spatial.KindBaro, spatial.KindGnss, spatial.KindCamera, spatial.KindUwb, spatial.KindGas}
	k, sign := float64(n), 1.0
	if n%2 == 1 {
		sign = -1
	}
	d.lidarMount[0] = geom.Pose2{X: 0.10 + 0.02*k, Z: -sign * (0.03 + 0.01*k), Yaw: sign * (0.08 + 0.02*k)}
	d.camPose[0] = b.mountLookAt(geom.Vec3{X: 0.18, Y: 0.42, Z: sign * 0.02}, -sign*0.03, 0.05)
	d.intrinsics, d.declared = mobileLens, mobileLens
	d.marker = robotMarker
	d.uwbMount = geom.Vec3{X: -0.08, Y: 0.4}
	return d
}

// handheld adds a camera-only device carried by a walking person.
func (b *builder) handheld(id spatial.DeviceID, start time.Duration, speed float64, wps ...wp) *device {
	d := b.mobile(id, "person", start, speed, wps)
	d.radius, d.height = 0.25, 1.7
	d.kinds = []spatial.SensorKind{spatial.KindCamera}
	d.camPose[0] = b.mountLookAt(geom.Vec3{X: 0.3, Y: 1.4}, 0, 0.1)
	d.intrinsics, d.declared = mobileLens, mobileLens
	return d
}

// camera adds a fixed camera. eye and target are (x, height above the
// floor, z).
func (b *builder) camera(id spatial.DeviceID, f *floor, start time.Duration, imu bool, eye, target geom.Vec3) *device {
	up := geom.Vec3{Y: f.level.elevation}
	pose, err := geom.LookAt(eye.Add(up), target.Add(up), geom.Vec3{Y: 1})
	if err != nil {
		b.fail(err)
	}
	d := b.add(&device{id: id, start: start, level: f.level, kinds: []spatial.SensorKind{spatial.KindCamera}})
	if imu {
		d.kinds = append(d.kinds, spatial.KindImu)
	}
	d.camPose[0] = pose
	return d
}

// bump knocks a fixed camera at an instant: it turns by the rotation vector
// rot and moves by shift, both in its own optical frame.
func (b *builder) bump(d *device, at time.Duration, rot, shift geom.Vec3) {
	if !b.opt.NoDisturbances {
		d.camBump, d.camPose[1] = at, d.camPose[0].Compose(geom.Pose3FromRotVec(rot, shift))
	}
}

func (b *builder) shiftLidar(d *device, at time.Duration, to geom.Pose2) {
	if !b.opt.NoDisturbances {
		d.lidarShift, d.lidarMount[1] = at, to
	}
}

// fixedHeight is how far above the floor microphones and beacons hang.
const fixedHeight = 2.5

func (b *builder) mic(id spatial.DeviceID, f *floor, x, z, yaw float64) {
	b.add(&device{id: id, level: f.level, pose: geom.Pose2{X: x, Z: z, Yaw: yaw}, elevation: f.level.elevation + fixedHeight, kinds: []spatial.SensorKind{spatial.KindMic}})
}

func (b *builder) beacon(id spatial.DeviceID, f *floor, x, z float64) {
	b.s.beacons = append(b.s.beacons, Beacon{ID: id, Floor: f.id, Position: geom.Vec3{X: x, Y: f.level.elevation + fixedHeight, Z: z}})
}

func (b *builder) sound(id string, f *floor, x, height, z float64, active ...Interval) {
	b.s.sounds = append(b.s.sounds, SoundSource{ID: id, Floor: f.id, Position: geom.Vec3{X: x, Y: f.level.elevation + height, Z: z}, Active: active})
}

// actor adds a person or cart that walks the closed path once per period
// seconds, starting phase seconds into it. Actors that share a period keep
// their relative timing, which is how crossing paths are kept collision-free.
func (b *builder) actor(id, class string, f *floor, period, phase float64, shape Shape, path ...geom.Vec2) {
	if b.opt.NoActors {
		return
	}
	wps := make([]wp, len(path))
	for i, p := range path {
		wps[i] = wp{p, f.level.elevation}
	}
	r, err := newRoute(wps, 1)
	if err == nil {
		err = r.retime(period)
	}
	if err != nil {
		b.fail(err)
		return
	}
	b.s.actors = append(b.s.actors, &actor{id: id, class: class, level: f.level, route: r, phase: phase, shape: shape})
}

// stairsOrLift records a vertical portal between two shaft footprints.
func (b *builder) stairsOrLift(id string, kind spatial.PortalKind, lower *floor, lowerRoom string, a geom.Vec2, upper *floor, upperRoom string, c geom.Vec2) {
	b.s.vertical = append(b.s.vertical, spatial.Portal{
		ID: id, Kind: kind, State: spatial.PortalOpen, Confidence: 1,
		A:        spatial.PortalEnd{Floor: lower.id, Region: lowerRoom, PositionM: a.Lift(lower.level.elevation).Array()},
		B:        spatial.PortalEnd{Floor: upper.id, Region: upperRoom, PositionM: c.Lift(upper.level.elevation).Array()},
		Location: spatial.Enclose((lower.level.elevation+upper.level.elevation)/2, a, c),
	})
}

// wallLine identifies an axis-aligned line: coordinate in millimetres.
type wallLine struct {
	vertical bool // constant X
	mm       int64
}

func lineOf(sg geom.Segment) (wallLine, [2]float64, bool) {
	switch {
	case sg.A.X == sg.B.X:
		return wallLine{true, int64(math.Round(sg.A.X * 1000))}, [2]float64{math.Min(sg.A.Z, sg.B.Z), math.Max(sg.A.Z, sg.B.Z)}, true
	case sg.A.Z == sg.B.Z:
		return wallLine{false, int64(math.Round(sg.A.Z * 1000))}, [2]float64{math.Min(sg.A.X, sg.B.X), math.Max(sg.A.X, sg.B.X)}, true
	}
	return wallLine{}, [2]float64{}, false
}

// buildWalls turns the room outlines of a level into wall segments: edges on
// the same line are merged and every door and opening is cut out.
func buildWalls(lv *level) error {
	spans := map[wallLine][][2]float64{}
	for _, f := range lv.floors {
		for _, r := range f.rooms {
			for i, a := range r.Polygon {
				line, span, ok := lineOf(geom.Segment{A: a, B: r.Polygon[(i+1)%len(r.Polygon)]})
				if !ok {
					return fmt.Errorf("spatialsim: room %q is not axis-aligned", r.ID)
				}
				spans[line] = append(spans[line], span)
			}
		}
	}
	gaps := map[wallLine][][2]float64{}
	for _, d := range lv.doors {
		line, span, ok := lineOf(d.Segment)
		if !ok || spans[line] == nil {
			return fmt.Errorf("spatialsim: door %q does not lie on a wall", d.ID)
		}
		gaps[line] = append(gaps[line], span)
	}
	lines := make([]wallLine, 0, len(spans))
	for line := range spans {
		lines = append(lines, line)
	}
	sort.Slice(lines, func(i, j int) bool {
		if lines[i].vertical != lines[j].vertical {
			return lines[j].vertical
		}
		return lines[i].mm < lines[j].mm
	})
	for _, line := range lines {
		c := float64(line.mm) / 1000
		emit := func(lo, hi float64) {
			if hi-lo < 1e-9 {
				return
			}
			sg := geom.Segment{A: geom.Vec2{X: lo, Z: c}, B: geom.Vec2{X: hi, Z: c}}
			if line.vertical {
				sg = geom.Segment{A: geom.Vec2{X: c, Z: lo}, B: geom.Vec2{X: c, Z: hi}}
			}
			lv.walls = append(lv.walls, sg)
		}
		ss, gs := spans[line], gaps[line]
		sort.Slice(ss, func(i, j int) bool { return ss[i][0] < ss[j][0] })
		sort.Slice(gs, func(i, j int) bool { return gs[i][0] < gs[j][0] })
		for i := 0; i < len(ss); {
			lo, hi := ss[i][0], ss[i][1]
			for i++; i < len(ss) && ss[i][0] <= hi; i++ {
				hi = math.Max(hi, ss[i][1])
			}
			for _, g := range gs {
				if g[1] <= lo || g[0] >= hi {
					continue
				}
				emit(lo, g[0])
				lo = math.Max(lo, g[1])
			}
			emit(lo, hi)
		}
	}
	return nil
}

// placeTags hangs square fiducial tags on the walls of every room: one per
// tagSpacing metres of wall, clear of doors, at alternating heights.
func placeTags(lv *level) {
	for _, f := range lv.floors {
		for _, r := range f.rooms {
			if r.Portal != "" {
				continue
			}
			count := 0
			for i, a := range r.Polygon {
				edge := geom.Segment{A: a, B: r.Polygon[(i+1)%len(r.Polygon)]}
				normal := edge.Dir().Perp()
				if !geom.PointInPolygon(edge.Midpoint().Add(normal.Scale(0.05)), r.Polygon) {
					normal = normal.Scale(-1)
				}
				n := max(1, int(math.Round(edge.Length()/tagSpacing)))
			spots:
				for k := 0; k < n; k++ {
					p := edge.A.Lerp(edge.B, (float64(k)+0.5)/float64(n))
					for _, d := range lv.doors {
						if d.Segment.DistanceTo(p) < 0.4 {
							continue spots
						}
					}
					count++
					height := 1.6 + 0.4*float64(count%2)
					at := p.Add(normal.Scale(tagStandoff)).Lift(lv.elevation + height)
					lv.tags = append(lv.tags, Landmark{
						ID: fmt.Sprintf("tag-%s-%d", r.ID, count), Floor: f.id,
						Pose: geom.Pose3{R: geom.Mat3{normal.Z, 0, normal.X, 0, 1, 0, -normal.X, 0, normal.Z}, T: at},
					})
				}
			}
		}
	}
}

var sensorModels = map[spatial.SensorKind]string{
	spatial.KindLidar2D: "sim-lidar-360", spatial.KindOdometry: "sim-wheel-odometry", spatial.KindImu: "sim-imu", spatial.KindBaro: "sim-baro",
	spatial.KindGnss: "sim-gnss", spatial.KindCamera: "sim-camera-720p", spatial.KindUwb: "sim-uwb-tag", spatial.KindMic: "sim-mic-array", spatial.KindGas: "sim-co2",
}

// equip draws a device's seed-dependent constants and fits its sensors.
func (s *Site) equip(d *device) {
	c := &s.cfg
	r := s.rng(d.id, "constants", 0)
	d.odomBias = [3]float64{c.OdomTransPerMetre * r.NormFloat64(), c.OdomYawPerMetre * r.NormFloat64(), c.OdomYawPerRadian * r.NormFloat64()}
	d.gyroBias = c.GyroBias * r.NormFloat64()
	d.baroRef = 40*r.Float64() - 20
	for i := range d.phase {
		d.phase[i] = 2 * math.Pi * r.Float64()
	}
	if d.route == nil && slices.Contains(d.kinds, spatial.KindCamera) {
		// The true lens of a fixed camera: within about 2 % of the declared
		// focal length, the principal point up to a dozen pixels off, and
		// barrel distortion the manifest does not mention.
		in := fixedLens
		in.FX *= 1 + 0.02*(2*r.Float64()-1)
		in.FY = in.FX * (1 + 0.004*(2*r.Float64()-1))
		in.CX += 12 * (2*r.Float64() - 1)
		in.CY += 9 * (2*r.Float64() - 1)
		in.K1 = -0.12 + 0.02*(2*r.Float64()-1)
		in.K2 = 0.015 + 0.005*(2*r.Float64()-1)
		d.intrinsics, d.declared = in, fixedLens
	}
	for _, kind := range d.kinds {
		// Every kind draws its offset so that removing one sensor by rate
		// does not shift the others.
		lo := min(10*time.Millisecond, c.ClockOffsetMax)
		clock := lo + time.Duration(r.Float64()*float64(c.ClockOffsetMax-lo))
		if r.Float64() < 0.5 {
			clock = -clock
		}
		if kind == spatial.KindOdometry {
			clock = 0
		}
		if hz := c.rate(kind); hz > 0 {
			d.sensors = append(d.sensors, sensor{id: SensorOf(kind), kind: kind, model: sensorModels[kind], period: time.Duration(math.Round(float64(time.Second) / hz)), clock: clock})
		}
	}
}

// build finishes the site.
func (b *builder) build() (*Site, error) {
	s := b.s
	if b.err != nil {
		return nil, b.err
	}
	if len(s.levels) == 0 || len(s.floors) == 0 {
		return nil, errors.New("spatialsim: a site needs a floor")
	}
	for _, lv := range s.levels {
		if err := buildWalls(lv); err != nil {
			return nil, err
		}
		placeTags(lv)
		for _, d := range lv.doors {
			off := d.Segment.Dir().Perp().Scale(0.1)
			for i, p := range []geom.Vec2{d.Segment.Midpoint().Sub(off), d.Segment.Midpoint().Add(off)} {
				if r, _ := lv.roomAt(p); r != nil {
					d.Rooms[i] = r.ID
				}
			}
		}
	}
	sort.Slice(s.devices, func(i, j int) bool { return s.devices[i].id < s.devices[j].id })
	classes := map[string]bool{}
	for _, d := range s.devices {
		s.equip(d)
		if d.route != nil {
			classes[d.class] = true
		}
	}
	for _, a := range s.actors {
		classes[a.class] = true
	}
	for _, lv := range s.levels {
		for _, f := range lv.furniture {
			classes[f.class] = true
		}
	}
	for c := range classes {
		s.classes = append(s.classes, c)
	}
	sort.Strings(s.classes)
	b.events()
	if s.shows = b.opt.TargetShows; s.shows == nil {
		var cams []*device
		for _, d := range s.devices {
			if d.route == nil && d.has(spatial.KindCamera) {
				cams = append(cams, d)
			}
		}
		sort.SliceStable(cams, func(i, j int) bool { return cams[i].start < cams[j].start })
		next := 10 * time.Second
		for _, d := range cams {
			start := max(next, d.start+10*time.Second)
			s.shows = append(s.shows, TargetShow{Camera: d.id, Start: start, Duration: 20 * time.Second})
			next = start + 22*time.Second
		}
	}
	for _, sh := range s.shows {
		if d := s.byID[sh.Camera]; d == nil || !d.has(spatial.KindCamera) || sh.Start < d.start || sh.Duration <= 0 {
			return nil, fmt.Errorf("spatialsim: target show for %q needs a camera that has started", sh.Camera)
		}
	}
	return s, nil
}

// events derives the truth lists of environment changes and device
// disturbances from the schedules.
func (b *builder) events() {
	s := b.s
	type change struct {
		at time.Duration
		ev spatial.ChangeEvent
	}
	var changes []change
	add := func(at time.Duration, kind spatial.ChangeKind, f spatial.FloorID, subject string, p geom.Vec2, radius float64) *spatial.ChangeEvent {
		changes = append(changes, change{at, spatial.ChangeEvent{
			Kind: kind, Floor: f, Subject: subject, Confidence: 1, Extent: spatial.Extent{RadiusM: radius},
			Location: spatial.At(p.Lift(s.floor(f).level.elevation), 0), FirstObserved: s.At(at), LastObserved: s.At(at),
		}})
		return &changes[len(changes)-1].ev
	}
	for _, lv := range s.levels {
		for _, f := range lv.furniture {
			if f.from > 0 && f.from != never {
				add(f.from, spatial.ChangeAppeared, f.floor.id, f.id, f.shape.Centre, f.shape.Bound())
			}
			if f.moveAt > 0 {
				ev := add(f.moveAt, spatial.ChangeMoved, f.floor.id, f.id, f.moved.Centre, f.moved.Bound())
				from, to := f.shape.Centre.Lift(lv.elevation).Array(), f.moved.Centre.Lift(lv.elevation).Array()
				ev.FromM, ev.ToM = &from, &to
			}
			if f.until > 0 {
				last, _ := f.at(f.until - 1)
				add(f.until, spatial.ChangeDisappeared, f.floor.id, f.id, last.Centre, last.Bound())
			}
		}
		for _, d := range lv.doors {
			for _, at := range d.Toggles {
				kind := spatial.ChangeDoorClosed
				if d.OpenAt(at) {
					kind = spatial.ChangeDoorOpened
				}
				add(at, kind, d.Floor, d.ID, d.Segment.Midpoint(), d.Segment.Length()/2)
			}
		}
	}
	sort.SliceStable(changes, func(i, j int) bool { return changes[i].at < changes[j].at })
	for i, c := range changes {
		c.ev.ID = fmt.Sprintf("change-%d", i+1)
		s.changes = append(s.changes, c.ev)
	}
	for _, d := range s.devices {
		if d.start > 0 {
			s.disturbances = append(s.disturbances, Disturbance{Kind: DisturbLateStart, Device: d.id, At: s.At(d.start)})
		}
		if d.camBump > 0 {
			s.disturbances = append(s.disturbances, Disturbance{Kind: DisturbCameraBump, Device: d.id, Sensor: SensorCamera, At: s.At(d.camBump)})
		}
		if d.lidarShift > 0 {
			s.disturbances = append(s.disturbances, Disturbance{Kind: DisturbMountShift, Device: d.id, Sensor: SensorLidar, At: s.At(d.lidarShift)})
		}
	}
	sort.SliceStable(s.disturbances, func(i, j int) bool { return s.disturbances[i].At.Before(s.disturbances[j].At) })
}
