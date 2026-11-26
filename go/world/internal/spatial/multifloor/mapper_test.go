package multifloor

import (
	"errors"
	"math"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// knot is where a scripted device is at an instant: site x, z and elevation.
type knot struct{ t, x, z, y float64 }

// script is a hand-built device: it walks straight between knots, reports
// odometry at 20 Hz in a frame of its own, a barometer at 5 Hz, a GNSS fix
// every second while outdoors, and gets a localizer fix at 2 Hz.
type script struct {
	dev       spatial.DeviceID
	knots     []knot
	odomFrame geom.Pose2                            // the odometry frame in the site frame
	yawBias   float64                               // odometry heading drift, rad per metre
	outdoors  func(p geom.Vec2) bool                // nil: never
	localized func(t float64, p geom.Vec2) bool     // nil: always
	gnssError func(t float64) (east, south float64) // nil: 1.5 m white noise
	seed      uint64

	odom geom.Pose2
	prev geom.Pose2
	rng  *rand.Rand
}

// pose is the true pose and elevation at t. The heading is along the current
// leg; a leg without planar motion (a lift ride, a wait) keeps the last one.
func (s *script) pose(t float64) (geom.Pose2, float64) {
	k := s.knots
	yaw, p, y := 0.0, geom.Vec2{X: k[0].x, Z: k[0].z}, k[0].y
	for i := 1; i < len(k); i++ {
		a, b := geom.Vec2{X: k[i-1].x, Z: k[i-1].z}, geom.Vec2{X: k[i].x, Z: k[i].z}
		if t < k[i-1].t {
			break
		}
		if b.Dist(a) > 1e-9 {
			yaw = b.Sub(a).Angle()
		}
		u := math.Min((t-k[i-1].t)/(k[i].t-k[i-1].t), 1)
		p, y = a.Lerp(b, u), k[i-1].y+u*(k[i].y-k[i-1].y)
	}
	return geom.Pose2{X: p.X, Z: p.Z, Yaw: yaw}, y
}

// pressureOf inverts PressureAltitude.
func pressureOf(altitude float64) float64 {
	return 101325 * math.Pow(1-altitude/44330.77, 1/0.190263)
}

// play runs the scripts against a mapper for the given seconds, calling probe
// after every 50 ms step, and returns the floor changes.
func play(t *testing.T, m *Mapper, seconds float64, probe func(now float64), scripts ...*script) []Transition {
	t.Helper()
	var out []Transition
	take := func(tr []Transition, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, tr...)
	}
	for k := 0; float64(k)*0.05 < seconds; k++ {
		now := float64(k) * 0.05
		for _, s := range scripts {
			if now < s.knots[0].t {
				continue
			}
			pose, elevation := s.pose(now)
			if s.rng == nil {
				s.rng = rand.New(rand.NewPCG(s.seed, 30))
				s.odom = s.odomFrame.Inverse().Compose(pose)
			} else {
				delta := s.prev.Between(pose)
				delta.Yaw += s.yawBias * delta.Pos().Norm()
				s.odom = s.odom.Compose(delta)
			}
			s.prev = pose
			take(m.Ingest(spatial.Odometry{Header: spatial.Header{Device: s.dev, Sensor: "odom", Time: at(now)}, Pose: s.odom}))
			if k%4 == 0 {
				h := 120 + elevation + 0.1*s.rng.NormFloat64()
				take(m.Ingest(spatial.Baro{Header: spatial.Header{Device: s.dev, Sensor: "baro", Time: at(now)}, PressurePa: pressureOf(h), Altitude: h, AltitudeSigma: 0.1}))
			}
			if k%20 == 0 && s.outdoors != nil && s.outdoors(pose.Pos()) {
				e, so := 1.5*s.rng.NormFloat64(), 1.5*s.rng.NormFloat64()
				if s.gnssError != nil {
					e, so = s.gnssError(now)
				}
				take(m.Ingest(fixAt(t, s.dev, at(now), pose.Pos().Lift(elevation), e, so)))
			}
			if k%10 == 0 && (s.localized == nil || s.localized(now, pose.Pos())) {
				// The floor and the elevation are left out on purpose.
				take(m.FixPose(spatial.DevicePose{
					Device: s.dev, FrameID: "site", Location: spatial.At(pose.Pos().Lift(0), 0.03),
					Yaw: pose.Yaw, Covariance: geom.Mat3{4e-4, 0, 0, 0, 4e-4, 0, 0, 0, 1e-4}, Score: 0.9,
				}, at(now)))
			}
		}
		if probe != nil {
			probe(now)
		}
	}
	return out
}

func newMapper(t *testing.T, cfg Config) *Mapper {
	t.Helper()
	m, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// Up the stairs from (10, 2) to (10, 7.5), across the upper floor, down the
// lift at (4, 4).
func climberScript(dev spatial.DeviceID, start float64) *script {
	k := []knot{{0, 2, 2, 0}, {8, 10, 2, 0}, {13.5, 10, 7.5, 3.5}, {19.5, 4, 7.5, 3.5}, {24.5, 4, 7.5, 3.5}, {28, 4, 4, 3.5}, {32, 4, 4, 3.5}, {39, 4, 4, 0}, {50, 4, 4, 0}}
	for i := range k {
		k[i].t += start
	}
	return &script{dev: dev, knots: k, odomFrame: geom.Pose2{X: 30, Z: -12, Yaw: 1}, seed: uint64(len(dev))}
}

func TestMapperFloorsAndPortals(t *testing.T) {
	m := newMapper(t, Config{})
	a, b := climberScript("a", 0), climberScript("bb", 60)
	var worst, stairElevation float64
	sawMoving := false
	trs := play(t, m, 115, func(now float64) {
		for _, s := range []*script{a, b} {
			p, ok := m.Pose(s.dev)
			if !ok {
				continue
			}
			truth, elevation := s.pose(now)
			worst = math.Max(worst, math.Max(p.Pose2().Pos().Dist(truth.Pos()), math.Abs(geom.AngleDiff(p.Yaw, truth.Yaw))))
			if err := p.Validate(); err != nil {
				t.Fatal(err)
			}
			if _, moving, _ := m.Floor(s.dev); s == a && now > 10 && now < 12 {
				sawMoving = sawMoving || moving
				stairElevation = math.Max(stairElevation, p.PositionM[1])
				_ = elevation
			}
		}
	}, a, b)
	// The odometry frame is turned a radian from the site frame and the walk
	// turns left and right: a wrong composition order or handedness would
	// show here as metres.
	if worst > 0.05 {
		t.Errorf("pose strayed %.3f (m or rad) from the scripted walk", worst)
	}
	if !sawMoving || stairElevation < 0.5 || stairElevation > 3 {
		t.Errorf("on the stairs: moving %v, elevation %.2f", sawMoving, stairElevation)
	}

	want := []struct {
		dev      spatial.DeviceID
		from, to spatial.FloorID
		kind     spatial.PortalKind
		portal   string
		at       float64
	}{
		{"a", "ground", "level-1", spatial.PortalStairs, "stairs-1", 10.75},
		{"a", "level-1", "ground", spatial.PortalLift, "lift-1", 35.5},
		{"bb", "ground", "level-1", spatial.PortalStairs, "stairs-1", 70.75},
		{"bb", "level-1", "ground", spatial.PortalLift, "lift-1", 95.5},
	}
	if len(trs) != len(want) {
		t.Fatalf("%d floor changes, want %d: %+v", len(trs), len(want), trs)
	}
	for i, w := range want {
		tr := trs[i]
		if tr.Device != w.dev || tr.From != w.from || tr.To != w.to || tr.Kind != w.kind || tr.Portal != w.portal {
			t.Errorf("change %d = %s %s->%s by %s (%s)", i, tr.Device, tr.From, tr.To, tr.Kind, tr.Portal)
		}
		if math.Abs(sec(tr.At)-w.at) > 0.5 || math.Abs(math.Abs(tr.DeltaM)-3.5) > 0.3 || tr.SigmaM <= 0 || len(tr.Inputs) < 3 {
			t.Errorf("change %d at %.2f s (want %.2f), %.2f m ± %.2f, inputs %v", i, sec(tr.At), w.at, tr.DeltaM, tr.SigmaM, tr.Inputs)
		}
		if !tr.Start.Before(tr.At) || !tr.At.Before(tr.End) || tr.End.After(tr.Confirmed) {
			t.Errorf("change %d: start %v at %v end %v confirmed %v", i, sec(tr.Start), sec(tr.At), sec(tr.End), sec(tr.Confirmed))
		}
	}

	floors := m.Floors()
	if len(floors) != 2 || floors[0].Floor != "ground" || floors[0].ElevationM != 0 || floors[1].Floor != "level-1" || math.Abs(floors[1].ElevationM-3.5) > 0.2 {
		t.Fatalf("floors = %+v", floors)
	}
	for _, f := range floors {
		if err := f.Validate(); err != nil || len(f.Portals) != 2 || f.Outdoor || f.Location == nil {
			t.Errorf("floor %s: %v, %d portals", f.Floor, err, len(f.Portals))
		}
	}
	portals := m.Portals()
	if len(portals) != 2 {
		t.Fatalf("portals = %+v", portals)
	}
	ends := map[string][2]geom.Vec2{"stairs-1": {{X: 10, Z: 2}, {X: 10, Z: 7.5}}, "lift-1": {{X: 4, Z: 4}, {X: 4, Z: 4}}}
	for _, p := range portals {
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
		e := ends[p.ID]
		da, db := geom.Vec3FromArray(p.A.PositionM).XZ().Dist(e[0]), geom.Vec3FromArray(p.B.PositionM).XZ().Dist(e[1])
		if p.A.Floor != "ground" || p.B.Floor != "level-1" || da > 0.6 || db > 0.6 || math.Abs(p.B.PositionM[1]-3.5) > 0.2 || math.Abs(p.Confidence-2.0/3) > 1e-9 {
			t.Errorf("portal %s: %s (%.2f m off) to %s (%.2f m off), confidence %.2f", p.ID, p.A.Floor, da, p.B.Floor, db, p.Confidence)
		}
	}
}

func inYard(p geom.Vec2) bool { return p.X > 20 }

// Out of the door at (20, 5), twice round a yard 40 m from the site origin,
// and back in.
func rangerScript() *script {
	k := []knot{{0, 8, 5, 0}}
	add := func(x, z float64) {
		last := k[len(k)-1]
		k = append(k, knot{last.t + math.Hypot(x-last.x, z-last.z), x, z, 0})
	}
	add(22, 5)
	for lap := 0; lap < 2; lap++ {
		add(22, 1)
		add(40, 1)
		add(40, 14)
		add(22, 14)
		add(22, 5)
	}
	add(8, 5)
	add(8, 6)
	return &script{dev: "ranger", knots: k, odomFrame: geom.Pose2{X: -3, Z: 4, Yaw: -2}, outdoors: inYard, seed: 5}
}

func TestMapperOutdoorsAndGeoReference(t *testing.T) {
	m := newMapper(t, Config{})
	s := rangerScript()
	end := s.knots[len(s.knots)-1].t + 5
	var before geom.Vec2
	var step, refusedUntil float64
	refusals := map[bool]int{}
	trs := play(t, m, end, func(now float64) {
		// The largest move of the pose that is not the walk itself.
		if p, ok := m.Pose(s.dev); ok {
			truth, _ := s.pose(now)
			here := p.Pose2().Pos().Sub(truth.Pos())
			step, before = math.Max(step, here.Dist(before)), here
		}
		if _, err := m.GeoReference(); err != nil {
			refusals[errors.Is(err, geom.ErrInsufficient) || errors.Is(err, ErrBaseline)]++
			refusedUntil = now
		}
	}, s)
	if refusals[false] != 0 || refusedUntil < 14+10 || refusedUntil > 60 {
		t.Errorf("geo reference refused %v, until %.0f s; the first fix is at 14 s", refusals, refusedUntil)
	}
	if step > 0.02 {
		t.Errorf("the pose stepped %.3f m when GNSS came or went", step)
	}
	lap := s.knots[len(s.knots)-3].t // back through the door
	want := []struct {
		from, to spatial.FloorID
		at       float64
	}{{"ground", "ground-outdoor", 12}, {"ground-outdoor", "ground", lap + 2}}
	if len(trs) != len(want) {
		t.Fatalf("floor changes = %+v", trs)
	}
	for i, w := range want {
		if tr := trs[i]; tr.From != w.from || tr.To != w.to || tr.Kind != spatial.PortalDoor || tr.Portal != "door-1" || math.Abs(sec(tr.At)-w.at) > 1 || tr.DeltaM != 0 {
			t.Errorf("change %d = %s->%s by %s (%s) at %.1f s, want %.1f", i, tr.From, tr.To, tr.Kind, tr.Portal, sec(tr.At), w.at)
		}
	}
	floors := m.Floors()
	if len(floors) != 2 || floors[1].Floor != "ground-outdoor" || !floors[1].Outdoor || floors[1].ElevationM != 0 || len(floors[1].Portals) != 1 {
		t.Fatalf("floors = %+v", floors)
	}
	door := m.Portals()[0]
	if d := geom.Vec3FromArray(door.A.PositionM).XZ().Dist(geom.Vec2{X: 20, Z: 5}); d > 1 || door.A != (spatial.PortalEnd{Floor: "ground", PositionM: door.B.PositionM}) || door.B.Floor != "ground-outdoor" {
		t.Errorf("door %.2f m from the doorway: %+v", d, door)
	}
	fit, err := m.GeoReference()
	if err != nil {
		t.Fatal(err)
	}
	if e := geoError(t, fit.Reference, handGeo, geom.Vec2{X: 31, Z: 7.5}); e > 1 || fit.RMSE < 1 || fit.RMSE > 3 {
		t.Errorf("yard centre %.2f m off, residual %.2f m", e, fit.RMSE)
	}
}

// Outdoors the localizer has no map. With a geo reference from an earlier
// visit, GNSS must hold a pose that odometry alone lets drift.
func TestMapperGnssBoundsDriftWithoutPoseFixes(t *testing.T) {
	run := func(seed uint64, cfg Config) (worst float64) {
		m := newMapper(t, cfg)
		s := rangerScript()
		s.seed, s.yawBias = seed, 0.006 // three times the default noise model
		firstLap := s.knots[6].t
		s.localized = func(now float64, p geom.Vec2) bool { return now < firstLap || !inYard(p) }
		play(t, m, s.knots[len(s.knots)-3].t, func(now float64) {
			if p, ok := m.Pose(s.dev); ok && now > firstLap {
				truth, _ := s.pose(now)
				worst = math.Max(worst, p.Pose2().Pos().Dist(truth.Pos()))
			}
		}, s)
		return worst
	}
	for seed := uint64(1); seed <= 8; seed++ {
		fused := run(seed, Config{})
		// The same walk with every fix discarded as too poor.
		blind := run(seed, Config{GnssMaxAccuracyM: 0.1})
		t.Logf("seed %d: worst pose error on a 62 m lap without pose fixes: %.2f m with GNSS, %.2f m on odometry alone", seed, fused, blind)
		if blind < 3 {
			t.Fatalf("odometry alone drifted only %.2f m; the case proves nothing", blind)
		}
		// 2.1 m is the horizontal sigma of one fix: fused, the pose must
		// never be worse than a single raw fix would be.
		if fused > 2.1 || fused > blind/2 {
			t.Errorf("seed %d: GNSS held the pose to %.2f m, odometry alone to %.2f m", seed, fused, blind)
		}
	}
}

func TestMapperRejectsGnssOutlier(t *testing.T) {
	m := newMapper(t, Config{})
	s := rangerScript()
	// Exact fixes, except one that is 60 m off once the reference exists.
	s.gnssError = func(now float64) (float64, float64) {
		if now == 100 {
			return 60, -20
		}
		return 0, 0
	}
	var before geom.Vec2
	var step float64
	play(t, m, 110, func(now float64) {
		if p, ok := m.Pose(s.dev); ok {
			truth, _ := s.pose(now)
			here := p.Pose2().Pos().Sub(truth.Pos())
			if now > 99 && now < 102 {
				step = math.Max(step, here.Dist(before))
			}
			before = here
		}
	}, s)
	if _, err := m.GeoReference(); err != nil {
		t.Fatal(err)
	}
	if step > 0.01 {
		t.Errorf("a 63 m outlier moved the pose %.3f m", step)
	}
}

func TestMapperCarriesLateFixForward(t *testing.T) {
	m := newMapper(t, Config{})
	fix := func(x, z float64, when float64) {
		t.Helper()
		if _, err := m.FixPose(spatial.DevicePose{Device: "d", FrameID: "site", Location: spatial.At(geom.Vec3{X: x, Z: z}, 0), Covariance: geom.Mat3{1e-4, 0, 0, 0, 1e-4, 0, 0, 0, 1e-4}}, at(when)); err != nil {
			t.Fatal(err)
		}
	}
	for k := 0; k <= 40; k++ { // 1 m/s along +X in a frame that starts at the site origin
		now := float64(k) * 0.05
		if _, err := m.Ingest(spatial.Odometry{Header: spatial.Header{Device: "d", Sensor: "odom", Time: at(now)}, Pose: geom.Pose2{X: now}}); err != nil {
			t.Fatal(err)
		}
		if k == 0 {
			fix(5, 5, 0)
		}
	}
	// A fix for where the device was half a second ago, 0.3 m to the side.
	fix(6.5, 5.3, 1.5)
	p, _ := m.Pose("d")
	if got := p.Pose2().Pos(); got.Dist(geom.Vec2{X: 7, Z: 5.3}) > 0.05 {
		t.Fatalf("pose %v after a fix that was 0.5 s old, want about (7, 5.3)", got)
	}
}

func TestMapperWithoutOdometryOrPose(t *testing.T) {
	m := newMapper(t, Config{})
	var trs []Transition
	profile := ramp{{20, 0}, {27, 3.5}}
	for k := 0; k < 200; k++ {
		now := float64(k) * 0.2
		tr, err := m.Ingest(spatial.Baro{Header: spatial.Header{Device: "tag", Sensor: "baro", Time: at(now)}, PressurePa: pressureOf(profile.at(now)), AltitudeSigma: 0.1})
		if err != nil {
			t.Fatal(err)
		}
		trs = append(trs, tr...)
	}
	// The floor change is known; how and where it happened is not.
	if len(trs) != 1 || trs[0].To != "level-1" || trs[0].Kind != "" || trs[0].Portal != "" || len(m.Portals()) != 0 {
		t.Fatalf("changes %+v, portals %v", trs, m.Portals())
	}
	if _, ok := m.Pose("tag"); ok {
		t.Fatal("a pose without a single fix")
	}
	if f, moving, ok := m.Floor("tag"); f != "level-1" || moving || !ok {
		t.Fatalf("floor %q moving %v ok %v", f, moving, ok)
	}
	if _, _, ok := m.Floor("nobody"); ok {
		t.Fatal("unknown device has a floor")
	}
}

func TestMapperInputChecks(t *testing.T) {
	m := newMapper(t, Config{})
	odom := spatial.Odometry{Header: spatial.Header{Device: "d", Sensor: "odom", Time: at(1)}}
	if _, err := m.Ingest(odom); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ingest(odom); !errors.Is(err, ErrOutOfOrder) {
		t.Errorf("repeated sample: %v", err)
	}
	if _, err := m.Ingest(spatial.Baro{Header: spatial.Header{Device: "d", Sensor: "baro", Time: at(1)}}); err == nil {
		t.Error("a barometer sample without pressure was accepted")
	}
	if tr, err := m.Ingest(spatial.Imu{}); tr != nil || err != nil {
		t.Errorf("an IMU sample is not this package's business: %v", err)
	}
	if _, err := m.FixPose(spatial.DevicePose{Device: "d", FrameID: "site", Yaw: math.NaN()}, at(1)); err == nil {
		t.Error("a NaN pose fix was accepted")
	}
	if _, err := m.FixPose(spatial.DevicePose{Device: "d"}, at(1)); err == nil {
		t.Error("a pose fix without a frame was accepted")
	}
	for name, cfg := range map[string]Config{
		"floor match above a storey": {FloorMatchM: 3},
		"start floor id":             {StartFloor: "first floor"},
		"gate":                       {GnssGate: 1.5},
		"timeout":                    {GnssTimeout: time.Millisecond},
	} {
		if _, err := New(cfg); err == nil {
			t.Errorf("config %q was accepted", name)
		}
	}
}

func TestMapperRecordsPublish(t *testing.T) {
	m := newMapper(t, Config{StartFloor: "lobby", StartElevationM: 12})
	a, r := climberScript("a", 0), rangerScript()
	play(t, m, 110, nil, a, r)
	records := m.Records()
	scope := spatial.Scope{Tenant: "t", Site: "s", Source: world.Source{Device: "gateway", App: "spatial", Service: "multifloor", Stream: "site"}}
	frame := world.FrameRef{ID: "site", Revision: "1"}
	seen := map[string]int{}
	for i, rec := range records {
		c, err := rec.Component(scope, world.Version{Epoch: 1, Sequence: uint64(i + 1)}, frame, "test-clock", time.Minute, "test", func(id string) world.Input {
			return world.Input{ID: id, Origin: world.Measured}
		})
		if err != nil {
			t.Fatalf("%s: %v", rec.Payload.EntityKey(), err)
		}
		if c.Provenance.ModelID != "wendy.spatial.multifloor" || c.Provenance.Origin != world.Inferred || len(c.Provenance.Inputs) == 0 || c.Timing.SampleTime.IsZero() {
			t.Errorf("%s: provenance %+v", c.Key.Entity, c.Provenance)
		}
		for _, in := range c.Provenance.Inputs {
			if !strings.HasPrefix(in.ID, "a/") && !strings.HasPrefix(in.ID, "ranger/") {
				t.Errorf("%s: input %q is not a measurement of this run", c.Key.Entity, in.ID)
			}
		}
		if _, err := spatial.DecodePayload(c); err != nil {
			t.Errorf("%s: %v", c.Key.Entity, err)
		}
		seen[c.Key.Type]++
	}
	want := map[string]int{spatial.TypeFloorInfo: 3, spatial.TypePortal: 3, spatial.TypeGeoReference: 1, spatial.TypeDevicePose: 2}
	for typ, n := range want {
		if seen[typ] != n {
			t.Errorf("%d records of %s, want %d", seen[typ], typ, n)
		}
	}
	floors := m.Floors()
	if floors[0].Floor != "lobby" || floors[0].ElevationM != 12 || floors[1].Floor != "lobby-outdoor" || math.Abs(floors[2].ElevationM-15.5) > 0.2 {
		t.Errorf("floors = %+v", floors)
	}
	// Records are a snapshot: the same state gives the same records.
	again := m.Records()
	for i := range records {
		if records[i].Payload.EntityKey() != again[i].Payload.EntityKey() || !records[i].At.Equal(again[i].At) {
			t.Fatalf("record %d differs between two calls", i)
		}
	}
}
