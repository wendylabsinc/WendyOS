package spatialsim

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/geography"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

const (
	// Noise is accepted within this many sigmas of the model's prediction.
	sigmas = 5.0
	// A noiseless simulation must reproduce analytic geometry to this.
	exact = 1e-9
	// 60 s of one robot's 10 Hz, 360-beam lidar must generate within this.
	lidarMinuteBudget = time.Second
)

func sec(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }

// encode renders a stream as NDJSON lines, validating every measurement.
func encode(t testing.TB, s *Site, from, to time.Duration, f Filter) []string {
	t.Helper()
	var out []string
	for m := range s.Stream(s.At(from), s.At(to), f) {
		line, err := spatial.EncodeMeasurement(m)
		if err != nil {
			t.Fatalf("%T from %v: %v", m, m.When(), err)
		}
		out = append(out, string(line))
	}
	return out
}

func sensorPose(t testing.TB, s *Site, dev spatial.DeviceID, at time.Time) geom.Pose2 {
	t.Helper()
	st, ok := s.Truth().Device(dev, at)
	mount, ok2 := s.Truth().LidarMount(dev, at)
	if !ok || !ok2 {
		t.Fatalf("%s has no lidar pose at %v", dev, at)
	}
	return st.Pose.Compose(mount)
}

// probe returns the reading of the beam that points closest to target and
// the distance from the lidar to target.
func probe(t testing.TB, s *Site, dev spatial.DeviceID, at time.Duration, target geom.Vec2) (r, dist float64, state spatial.BeamState) {
	t.Helper()
	scan, ok := s.Scan(dev, s.At(at))
	if !ok {
		t.Fatalf("%s has no scan at %v", dev, at)
	}
	v := sensorPose(t, s, dev, s.At(at)).InverseApply(target)
	n := len(scan.Ranges)
	i := (int(math.Round((v.Angle()-scan.AngleMin)/scan.AngleIncrement))%n + n) % n
	_, r, state = scan.Beam(i)
	return r, v.Norm(), state
}

// whenNear finds the instant in [from, to) at which a device is closest to p.
func whenNear(t testing.TB, s *Site, dev spatial.DeviceID, p geom.Vec2, from, to time.Duration) time.Duration {
	t.Helper()
	best, bestDist := from, math.Inf(1)
	for at := from; at < to; at += 100 * time.Millisecond {
		if st, ok := s.Truth().Device(dev, s.At(at)); ok && st.Pose.Pos().Dist(p) < bestDist {
			best, bestDist = at, st.Pose.Pos().Dist(p)
		}
	}
	if bestDist > 0.2 {
		t.Fatalf("%s never comes within 0.2 m of %v (closest %.2f m)", dev, p, bestDist)
	}
	return best
}

// rectRange is the distance from pose along a sensor-frame angle to the
// boundary of the rectangle [0, x1] x [0, z1].
func rectRange(pose geom.Pose2, angle, x1, z1 float64) float64 {
	dir := geom.Heading(pose.Yaw + angle)
	best := math.Inf(1)
	if dir.X > 0 {
		best = math.Min(best, (x1-pose.X)/dir.X)
	} else if dir.X < 0 {
		best = math.Min(best, -pose.X/dir.X)
	}
	if dir.Z > 0 {
		best = math.Min(best, (z1-pose.Z)/dir.Z)
	} else if dir.Z < 0 {
		best = math.Min(best, -pose.Z/dir.Z)
	}
	return best
}

func TestSameSeedSameMeasurements(t *testing.T) {
	// The window starts at the epoch, so manifests are included.
	const from, to = 0, 2 * time.Second
	a := encode(t, mustSite(t, Office, 7, Options{}), from, to, Filter{})
	b := encode(t, mustSite(t, Office, 7, Options{}), from, to, Filter{})
	c := encode(t, mustSite(t, Office, 8, Options{}), from, to, Filter{})
	if len(a) < 1000 {
		t.Fatalf("only %d measurements in %v", len(a), to-from)
	}
	if !slices.Equal(a, b) {
		t.Fatal("two sites with the same seed produced different measurements")
	}
	if slices.Equal(a, c) {
		t.Fatal("two sites with different seeds produced identical measurements")
	}
	for _, line := range a {
		m, err := spatial.DecodeMeasurement([]byte(line))
		if err != nil {
			t.Fatalf("decode %s: %v", line, err)
		}
		if again, err := spatial.EncodeMeasurement(m); err != nil || string(again) != line {
			t.Fatalf("measurement does not survive the NDJSON round trip: %s", line)
		}
	}
}

func TestGenerationOrderIndependence(t *testing.T) {
	fresh, used := mustSite(t, Office, 7, Options{}), mustSite(t, Office, 7, Options{})
	// Generate unrelated data on one site first: other devices, later
	// instants, and odometry far ahead, which grows the integration cache.
	for range used.Stream(used.At(0), used.At(4*time.Second), Filter{}) {
	}
	used.Odometry(Rover1, used.At(500*time.Second))
	used.Scan(Rover2, used.At(333*time.Second))

	at := 200300 * time.Millisecond
	line := func(m spatial.Measurement, ok bool) string {
		if !ok {
			return "none"
		}
		b, err := spatial.EncodeMeasurement(m)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	sample := func(s *Site) []string {
		var out []string
		add := func(m spatial.Measurement, ok bool) { out = append(out, line(m, ok)) }
		scan, ok := s.Scan(Rover1, s.At(at))
		add(scan, ok)
		odom, ok := s.Odometry(Rover1, s.At(at))
		add(odom, ok)
		imu, ok := s.Imu(Rover1, s.At(at))
		add(imu, ok)
		baro, ok := s.Baro(Climber, s.At(at))
		add(baro, ok)
		gnss, ok := s.Gnss(Ranger, s.At(20*time.Second))
		add(gnss, ok)
		frame, ok := s.CameraFrame(CamLobby, s.At(at))
		add(frame, ok)
		gas, ok := s.Gas(Rover1, s.At(at))
		add(gas, ok)
		for _, r := range s.Ranges(Rover1, s.At(at)) {
			add(r, true)
		}
		for _, b := range s.Bearings("mic-2", s.At(40*time.Second)) {
			add(b, true)
		}
		return out
	}
	want, got := sample(fresh), sample(used)
	if len(want) < 10 || slices.Contains(want, "none") {
		t.Fatalf("the sample misses a sensor: %d measurements", len(want))
	}
	if !slices.Equal(want, got) {
		t.Fatal("a measurement depends on what was generated before it")
	}

	// One device's stream is the same whether or not others are merged in,
	// and equals the per-device generator on the sample grid.
	one := Filter{Devices: []spatial.DeviceID{Rover1}, Kinds: []spatial.SensorKind{spatial.KindLidar2D}}
	alone := encode(t, fresh, 10*time.Second, 12*time.Second, one)
	var merged []string
	for m := range used.Stream(used.At(10*time.Second), used.At(12*time.Second), Filter{}) {
		if dev, _ := m.From(); dev == Rover1 && m.Kind() == spatial.KindLidar2D {
			merged = append(merged, line(m, true))
		}
	}
	if len(alone) != 20 || !slices.Equal(alone, merged) {
		t.Fatalf("filtered stream (%d scans) differs from the merged stream (%d scans)", len(alone), len(merged))
	}
	scan, ok := fresh.Scan(Rover1, fresh.At(10*time.Second))
	if direct := line(scan, ok); direct != alone[0] {
		t.Fatal("Scan at a grid instant differs from the stream")
	}
}

func TestStreamOrderAndContent(t *testing.T) {
	s := officeSite(t)
	tr := s.Truth()
	kinds := map[string]int{}
	last := time.Time{}
	for m := range s.Stream(s.At(28*time.Second), s.At(34*time.Second), Filter{}) {
		dev, sensor := m.From()
		off, ok := tr.ClockOffset(dev, sensor)
		if !ok {
			t.Fatalf("%s/%s has no clock offset", dev, sensor)
		}
		sampled := m.When().Add(-off)
		if sampled.Before(last) {
			t.Fatalf("stream is not ordered by sampling instant: %v after %v", sampled, last)
		}
		last = sampled
		if err := m.Validate(); err != nil {
			t.Fatal(err)
		}
		kinds[fmt.Sprintf("%T", m)]++
	}
	for _, want := range []string{"Scan", "Odometry", "Imu", "Baro", "GnssFix", "CameraFrame", "Range", "Bearing", "Scalar"} {
		if kinds["spatial."+want] == 0 {
			t.Errorf("no %s in the merged stream; saw %v", want, kinds)
		}
	}

	// Manifests come first, one per sensor, when the interval covers a start.
	manifests, samples := 0, 0
	for m := range s.Stream(s.At(0), s.At(200*time.Millisecond), Filter{Devices: []spatial.DeviceID{Rover1}}) {
		if _, ok := m.(spatial.SensorManifest); ok {
			if samples > 0 {
				t.Fatal("a manifest followed a sample")
			}
			manifests++
		} else {
			samples++
		}
	}
	if manifests != 8 || samples == 0 {
		t.Fatalf("rover-1 announced %d sensors and produced %d samples; want 8 and some", manifests, samples)
	}

	// Laziness: stopping early and filtering must not touch other devices.
	lazy := mustSite(t, Office, 1, Options{})
	n := 0
	for range lazy.Stream(lazy.At(0), lazy.At(time.Hour), Filter{Devices: []spatial.DeviceID{Rover1}, Kinds: []spatial.SensorKind{spatial.KindOdometry}}) {
		if n++; n == 50 {
			break
		}
	}
	if got := len(lazy.byID[Rover1].odom); got == 0 || got > 200 {
		t.Errorf("50 odometry samples integrated %d steps; want a few hundred at most", got)
	}
	if len(lazy.byID[Rover2].odom) != 0 {
		t.Error("streaming rover-1 integrated rover-2's odometry")
	}
}

func TestLidarMatchesAnalyticRoom(t *testing.T) {
	// An empty 8 x 6 m room and a robot whose lidar mount slips at 30 s.
	build := func(cfg Config) *Site {
		b, f, err := small("test", 3, Options{Config: &cfg})
		if err != nil {
			t.Fatal(err)
		}
		f.room("room", "office", 0, 0, 8, 6)
		d := b.robot(Rover1, 1, 0, 1, wp{geom.Vec2{X: 2, Z: 2}, 0}, wp{geom.Vec2{X: 6, Z: 2.5}, 0}, wp{geom.Vec2{X: 5, Z: 4.5}, 0})
		b.shiftLidar(d, 30*time.Second, geom.Pose2{X: 0.2, Z: 0.05, Yaw: -0.3})
		s, err := b.build()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	quiet := build(DefaultConfig().Noiseless())
	for _, at := range []time.Duration{sec(3.3), sec(11.7), sec(41.2), sec(55.5)} {
		scan, _ := quiet.Scan(Rover1, quiet.At(at))
		pose := sensorPose(t, quiet, Rover1, quiet.At(at))
		for i := range scan.Ranges {
			angle, r, state := scan.Beam(i)
			if want := rectRange(pose, angle, 8, 6); state != spatial.BeamHit || math.Abs(r-want) > exact {
				t.Fatalf("noiseless beam %d at %v reads %v (state %d); the wall is at %v", i, at, r, state, want)
			}
		}
	}
	before, _ := quiet.Truth().LidarMount(Rover1, quiet.At(29*time.Second))
	after, _ := quiet.Truth().LidarMount(Rover1, quiet.At(31*time.Second))
	if before == after || before.Pos().Norm() < 0.05 || math.Abs(before.Yaw) < 0.05 {
		t.Fatalf("lidar mount must be non-trivial and shift at 30 s: %+v then %+v", before, after)
	}

	noisy := build(DefaultConfig())
	cfg := noisy.Config()
	var sum float64
	hits, invalid, beams := 0, 0, 0
	for at := time.Duration(0); at < 20*time.Second; at += 100 * time.Millisecond {
		scan, _ := noisy.Scan(Rover1, noisy.At(at))
		if scan.RangeSigma != cfg.LidarSigma || len(scan.Ranges) != cfg.LidarBeams {
			t.Fatalf("scan declares sigma %v and %d beams", scan.RangeSigma, len(scan.Ranges))
		}
		pose := sensorPose(t, noisy, Rover1, noisy.At(at))
		for i := range scan.Ranges {
			angle, r, state := scan.Beam(i)
			beams++
			switch state {
			case spatial.BeamHit:
				e := r - rectRange(pose, angle, 8, 6)
				if math.Abs(e) > sigmas*cfg.LidarSigma+exact {
					t.Fatalf("beam %d at %v is %.3f m off the wall", i, at, e)
				}
				sum += e * e
				hits++
			case spatial.BeamInvalid:
				invalid++
			default:
				t.Fatalf("beam %d at %v has no return inside a closed room", i, at)
			}
		}
	}
	if rms := math.Sqrt(sum / float64(hits)); rms < 0.9*cfg.LidarSigma || rms > 1.1*cfg.LidarSigma {
		t.Errorf("range noise RMS %.4f m; configured %.4f m", rms, cfg.LidarSigma)
	}
	if rate := float64(invalid) / float64(beams); rate < 0.5*cfg.LidarDropout || rate > 1.5*cfg.LidarDropout {
		t.Errorf("dropout rate %.4f; configured %.4f", rate, cfg.LidarDropout)
	}

	// With furniture a beam may stop short of the wall but never go past it.
	room := mustSite(t, SingleRoom, 3, Options{})
	short, total := 0, 0
	for at := time.Duration(0); at < 20*time.Second; at += time.Second {
		scan, _ := room.Scan(Rover1, room.At(at))
		pose := sensorPose(t, room, Rover1, room.At(at))
		for i := range scan.Ranges {
			if angle, r, state := scan.Beam(i); state == spatial.BeamHit {
				e := r - rectRange(pose, angle, 8, 6)
				if e > sigmas*cfg.LidarSigma {
					t.Fatalf("beam %d at %v goes %.3f m through the wall", i, at, e)
				}
				if total++; e < -sigmas*cfg.LidarSigma {
					short++
				}
			}
		}
	}
	if short == 0 || short > total/4 {
		t.Errorf("%d of %d beams hit furniture; want some and at most a quarter", short, total)
	}
}

func TestCameraDetectionsFollowGeomModel(t *testing.T) {
	s := officeSite(t)
	tr := s.Truth()
	shapes := map[string]spatial.FiducialGeometry{}
	for _, g := range s.Fiducials() {
		shapes[g.ID] = g
	}
	tags := map[string]Landmark{}
	for _, l := range tr.Landmarks() {
		tags[l.ID] = l
	}
	tol := sigmas * s.Config().PixelSigma * math.Sqrt2
	markers, wallTags := 0, 0
	for _, cam := range []spatial.DeviceID{CamCorridorWest, CamKitchen, Rover1, Handheld} {
		off, _ := tr.ClockOffset(cam, SensorCamera)
		for at := time.Duration(0); at < 40*time.Second; at += 200 * time.Millisecond {
			frame, ok := s.CameraFrame(cam, s.At(at))
			if !ok || !frame.When().Equal(s.At(at+off)) {
				t.Fatalf("%s frame at %v: ok=%v, stamped %v, clock offset %v", cam, at, ok, frame.When(), off)
			}
			in, pose, _ := tr.Camera(cam, s.At(at))
			for _, det := range frame.Detections {
				if det.Kind != spatial.DetectFiducial {
					continue
				}
				shape, known := shapes[det.ID]
				if !known || det.Index >= len(shape.Points) {
					t.Fatalf("%s reports fiducial %q point %d, which Fiducials() does not declare", cam, det.ID, det.Index)
				}
				var p geom.Vec3
				if shape.Carrier != "" {
					st, _ := tr.Device(shape.Carrier, s.At(at))
					p = geom.Pose3FromPose2(st.Pose, st.Elevation).Apply(shape.Points[det.Index])
					markers++
				} else {
					p = tags[det.ID].Pose.Apply(shape.Points[det.Index])
					wallTags++
				}
				want, ok := in.ProjectWorld(pose, p)
				if !ok || want.Dist(det.Pixel) > tol {
					t.Fatalf("%s sees %s[%d] at %v; geom projects the true point to %v", cam, det.ID, det.Index, det.Pixel, want)
				}
				origin, dir, err := in.PixelRay(pose, det.Pixel)
				if err != nil {
					t.Fatal(err)
				}
				if miss, limit := p.Sub(origin).Cross(dir).Norm(), 1.5*p.Dist(origin)*tol/in.FX+1e-6; miss > limit {
					t.Fatalf("%s: the ray through %s[%d] passes %.4f m from the true point (limit %.4f)", cam, det.ID, det.Index, miss, limit)
				}
			}
		}
	}
	if markers < 100 || wallTags < 400 {
		t.Errorf("checked only %d marker points and %d wall-tag corners", markers, wallTags)
	}
}

func TestWallsAndBodiesOcclude(t *testing.T) {
	// Closed rooms: neither lidar nor camera reaches the other room.
	two := mustSite(t, TwoRooms, 5, Options{})
	diagonal := math.Hypot(6, 5)
	own, foreign := 0, 0
	for at := time.Duration(0); at < 30*time.Second; at += 500 * time.Millisecond {
		scan, _ := two.Scan(Rover1, two.At(at))
		for i := range scan.Ranges {
			if _, r, state := scan.Beam(i); state != spatial.BeamInvalid && (state != spatial.BeamHit || r > diagonal+sigmas*two.Config().LidarSigma) {
				t.Fatalf("beam %d at %v leaves the closed room: %v", i, at, r)
			}
		}
		frame, _ := two.CameraFrame(Rover1, two.At(at))
		for _, det := range frame.Detections {
			switch {
			case det.Kind != spatial.DetectFiducial:
			case strings.HasPrefix(det.ID, "tag-room-a-"):
				own++
			default:
				foreign++
			}
		}
	}
	if own == 0 || foreign != 0 {
		t.Errorf("rover-1 saw %d fiducial points in its own room and %d through walls", own, foreign)
	}

	// The sight-line test itself, on the standard site.
	s := officeSite(t)
	lv := s.levels[0]
	eye := geom.Vec3{X: 4, Y: 2.5, Z: 4}
	for _, c := range []struct {
		name    string
		at      time.Duration
		to      geom.Vec3
		blocked bool
	}{
		{"within the lobby", 0, geom.Vec3{X: 7, Y: 1, Z: 3}, false},
		{"through the lobby opening", 0, geom.Vec3{X: 4, Y: 1, Z: 10}, false},
		{"through the kitchen wall", 0, geom.Vec3{X: 10, Y: 1, Z: 4}, true},
		{"through the closed entrance", 279 * time.Second, geom.Vec3{X: -2, Y: 1, Z: 4.2}, true},
		{"through the open entrance", 281 * time.Second, geom.Vec3{X: -2, Y: 1, Z: 4.2}, false},
		{"over the reception desk", 0, geom.Vec3{X: 4, Y: 2, Z: 0.1}, false},
		{"under the top of the reception desk", 0, geom.Vec3{X: 4, Y: 0.2, Z: 0.1}, true},
	} {
		sc := s.scene(lv, c.at)
		for i := range sc.solids { // keep the moving people out of this check
			if sc.solids[i].class == "person" {
				sc.solids[i].shape.Height = 0
			}
		}
		if got := sc.blocked(eye, c.to, "", ""); got != c.blocked {
			t.Errorf("sight line %s: blocked=%v, want %v", c.name, got, c.blocked)
		}
	}

	// One person hides the other from the lobby camera.
	cfg := DefaultConfig().Noiseless()
	clean := mustSite(t, Office, 1, Options{Config: &cfg})
	tr := clean.Truth()
	hidden := 0
	for at := time.Duration(0); at < 60*time.Second; at += 200 * time.Millisecond {
		in, pose, _ := tr.Camera(CamLobby, clean.At(at))
		sc := clean.scene(clean.levels[0], at)
		for _, a := range tr.Actors(clean.At(at)) {
			if a.Floor != FloorGround || !sc.blocked(pose.T, a.Shape.Centre.Lift(a.Shape.Height/2), "", a.ID) {
				continue
			}
			foot, _ := in.ProjectWorld(pose, a.Shape.Centre.Lift(0))
			frame, _ := clean.CameraFrame(CamLobby, clean.At(at))
			for _, det := range frame.Detections {
				if det.Kind == spatial.DetectObject && det.Pixel.Dist(foot) < 1 {
					t.Fatalf("%s is hidden at %v but was detected", a.ID, at)
				}
			}
			hidden++
		}
	}
	if hidden == 0 {
		t.Error("the two people in the lobby never occlude each other for the lobby camera")
	}
}

func TestScheduledChangesShowInLidar(t *testing.T) {
	s := officeSite(t)
	margin := 0.3
	// Each case looks from a point on a loop at something that changes
	// between OfficeChangesFrom and OfficeChangesUntil, once before and once
	// the same number of whole loops later.
	cases := []struct {
		name          string
		dev           spatial.DeviceID
		from, target  geom.Vec2
		solidBefore   bool // whether the beam stops at the target before the change
		solidAfter    bool
		approachLimit float64 // a solid target stops the beam within this of its centre
	}{
		{"lab east door closes", Rover2, geom.Vec2{X: 13, Z: 15.5}, geom.Vec2{X: 13, Z: 11.5}, false, true, 0.1},
		{"lab crate appears", Rover2, geom.Vec2{X: 13, Z: 15.5}, geom.Vec2{X: 10.5, Z: 15.6}, false, true, 0.6},
		{"storage crate disappears", Rover2, geom.Vec2{X: 21, Z: 17}, geom.Vec2{X: 19, Z: 19}, true, false, 0.75},
		{"kitchen table moves away", Rover1, geom.Vec2{X: 12, Z: 6}, geom.Vec2{X: 14.3, Z: 6.8}, true, false, 0.65},
		{"kitchen table arrives", Rover1, geom.Vec2{X: 12, Z: 6}, geom.Vec2{X: 9.5, Z: 7}, false, true, 0.65},
		// Upstairs, where the climber's is the only lidar.
		{"lounge door to the meeting room closes", Climber, geom.Vec2{X: 6, Z: 15}, geom.Vec2{X: 5.5, Z: 13}, false, true, 0.1},
		{"upper storage crate appears", Climber, geom.Vec2{X: 22, Z: 15}, geom.Vec2{X: 24.5, Z: 17.5}, false, true, 0.6},
	}
	for _, c := range cases {
		loop := sec(s.byID[c.dev].route.period)
		t1 := whenNear(t, s, c.dev, c.from, 0, loop)
		t2 := t1 + loop*time.Duration(OfficeChangesUntil/loop+1)
		if t1 >= OfficeChangesFrom || t2 <= OfficeChangesUntil {
			t.Fatalf("%s: instants %v and %v do not straddle the change window", c.name, t1, t2)
		}
		for i, at := range []time.Duration{t1, t2} {
			solid := []bool{c.solidBefore, c.solidAfter}[i]
			r, dist, state := probe(t, s, c.dev, at, c.target)
			if state == spatial.BeamInvalid { // a dropout: take the next sweep's neighbour in time
				r, dist, state = probe(t, s, c.dev, at+time.Millisecond, c.target)
			}
			switch {
			case solid && (state != spatial.BeamHit || r > dist+0.05 || r < dist-c.approachLimit):
				t.Errorf("%s: at %v the beam reads %.2f m; the target is %.2f m away", c.name, at, r, dist)
			case !solid && state == spatial.BeamHit && r < dist+margin:
				t.Errorf("%s: at %v the beam stops at %.2f m; nothing should be within %.2f m", c.name, at, r, dist+margin)
			}
		}
	}

	// NoChanges freezes the environment at its initial state.
	frozen := mustSite(t, Office, 1, Options{NoChanges: true})
	if n := len(frozen.Truth().Changes()); n != 0 {
		t.Errorf("NoChanges site lists %d changes", n)
	}
	if open, _ := frozen.Truth().DoorOpen("g-lab-east-door", frozen.At(OfficeChangesUntil)); !open {
		t.Error("NoChanges site closed a door")
	}
}

func TestCameraBumpAndMountShift(t *testing.T) {
	s := officeSite(t)
	tr := s.Truth()
	cfg := s.Config()
	// Mean displacement of the wall-tag corners seen in both frames.
	shift := func(a, b time.Duration) (float64, int) {
		fa, _ := s.CameraFrame(CamCorridorWest, s.At(a))
		fb, _ := s.CameraFrame(CamCorridorWest, s.At(b))
		type key struct {
			id    string
			index int
		}
		seen := map[key]geom.Pixel{}
		for _, d := range fa.Detections {
			if d.Kind == spatial.DetectFiducial && strings.HasPrefix(d.ID, "tag-") {
				seen[key{d.ID, d.Index}] = d.Pixel
			}
		}
		sum, n := 0.0, 0
		for _, d := range fb.Detections {
			if p, ok := seen[key{d.ID, d.Index}]; ok && d.Kind == spatial.DetectFiducial {
				sum += p.Dist(d.Pixel)
				n++
			}
		}
		return sum / float64(n), n
	}
	const minBumpPixels = 15.0
	if quiet, n := shift(OfficeCameraBump-3*time.Second, OfficeCameraBump-time.Second); n < 8 || quiet > sigmas*cfg.PixelSigma {
		t.Errorf("static tags moved %.2f px between two quiet frames (%d corners)", quiet, n)
	}
	if moved, n := shift(OfficeCameraBump-time.Second, OfficeCameraBump+time.Second); n < 8 || moved < minBumpPixels {
		t.Errorf("static tags moved only %.2f px across the bump (%d corners); want over %v", moved, n, minBumpPixels)
	}
	_, before, _ := tr.Camera(CamCorridorWest, s.At(OfficeCameraBump-time.Second))
	_, after, _ := tr.Camera(CamCorridorWest, s.At(OfficeCameraBump))
	if angle, move := geom.RotAngle(before.R, after.R)*180/math.Pi, before.T.Dist(after.T); angle < 1 || angle > 6 || move < 0.01 || move > 0.06 {
		t.Errorf("bump turned the camera %.2f° and moved it %.3f m; want a few degrees and centimetres", angle, move)
	}

	// The camera's IMU rings for half a second after the bump and is quiet before.
	const quietLimit, joltFloor = 0.5, 2.0
	peak := func(from, to time.Duration) float64 {
		worst := 0.0
		for at := from; at < to; at += 20 * time.Millisecond {
			m, ok := s.Imu(CamCorridorWest, s.At(at))
			if !ok {
				t.Fatal("the bumped camera has no IMU")
			}
			worst = math.Max(worst, m.Accel.Sub(geom.Vec3{Y: gravity}).Norm())
		}
		return worst
	}
	if q := peak(OfficeCameraBump-20*time.Second, OfficeCameraBump); q > quietLimit {
		t.Errorf("IMU deviates %.2f m/s² from rest before the bump", q)
	}
	if j := peak(OfficeCameraBump, OfficeCameraBump+500*time.Millisecond); j < joltFloor {
		t.Errorf("IMU peaks at only %.2f m/s² during the bump", j)
	}
	if q := peak(OfficeCameraBump+2*time.Second, OfficeCameraBump+10*time.Second); q > quietLimit {
		t.Errorf("IMU still deviates %.2f m/s² two seconds after the bump", q)
	}

	m0, _ := tr.LidarMount(Rover2, s.At(OfficeMountShift-time.Second))
	m1, _ := tr.LidarMount(Rover2, s.At(OfficeMountShift))
	if d := m0.Between(m1); d.Pos().Norm() < 0.02 || math.Abs(d.Yaw) < 0.03 {
		t.Errorf("rover-2 lidar mount shift is too small: %+v", d)
	}
	kinds := map[DisturbanceKind]int{}
	for _, d := range tr.Disturbances() {
		kinds[d.Kind]++
	}
	if kinds[DisturbCameraBump] != 1 || kinds[DisturbMountShift] != 1 || kinds[DisturbLateStart] != 2 {
		t.Errorf("disturbances %v; want one bump, one mount shift, two late starts", kinds)
	}
	calm := mustSite(t, Office, 1, Options{NoDisturbances: true})
	_, c0, _ := calm.Truth().Camera(CamCorridorWest, calm.At(0))
	_, c1, _ := calm.Truth().Camera(CamCorridorWest, calm.At(OfficeDuration))
	if c0 != c1 {
		t.Error("NoDisturbances site still bumps the camera")
	}
}

func TestLateDevicesAreSilentUntilTheyStart(t *testing.T) {
	s := officeSite(t)
	for _, c := range []struct {
		dev   spatial.DeviceID
		start time.Duration
		kinds int
	}{{Latecomer, OfficeLateStart, 8}, {CamLate, OfficeLateCamera, 1}} {
		only := Filter{Devices: []spatial.DeviceID{c.dev}}
		for m := range s.Stream(s.At(0), s.At(c.start), only) {
			t.Fatalf("%s produced %T at %v before it started", c.dev, m, m.When())
		}
		before := s.At(c.start - time.Millisecond)
		if _, ok := s.CameraFrame(c.dev, before); ok {
			t.Errorf("%s has a camera frame before it starts", c.dev)
		}
		if _, ok := s.Truth().Device(c.dev, before); ok {
			t.Errorf("truth places %s before it starts", c.dev)
		}
		manifests, samples := 0, 0
		for m := range s.Stream(s.At(c.start), s.At(c.start+2*time.Second), only) {
			if mf, ok := m.(spatial.SensorManifest); ok {
				if samples > 0 || mf.Validate() != nil {
					t.Fatalf("%s: bad or late manifest %+v", c.dev, mf)
				}
				manifests++
			} else {
				samples++
			}
		}
		if manifests != c.kinds || samples == 0 {
			t.Errorf("%s announced %d sensors and produced %d samples after starting; want %d and some", c.dev, manifests, samples, c.kinds)
		}
	}
	if _, ok := s.Scan(Latecomer, s.At(OfficeLateStart-time.Millisecond)); ok {
		t.Error("latecomer scans before it starts")
	}
	first, ok := s.Odometry(Latecomer, s.At(OfficeLateStart))
	if !ok || first.Pose != (geom.Pose2{}) {
		t.Errorf("latecomer odometry must start at the identity, got %+v (ok=%v)", first.Pose, ok)
	}
	if st, _ := s.Truth().Device(Latecomer, s.At(OfficeLateStart)); st.Pose.Pos().Norm() < 1 {
		t.Errorf("latecomer's true start %+v is too close to the origin to test anything", st.Pose)
	}
}

func TestOdometryDriftMatchesConfig(t *testing.T) {
	// Windows of one metre of straight travel in the corridor scenario,
	// across seeds so that the per-device bias is sampled too.
	const (
		seeds  = 24
		window = time.Second
		// Bias and white noise each contribute the configured sigma per
		// metre on the forward axis and white noise alone on the lateral
		// one, so the planar RMS is about √3 sigma and the yaw RMS √2 sigma.
		transLow, transHigh = 1.2, 2.4
		yawLow, yawHigh     = 1.0, 2.0
	)
	cfg := DefaultConfig()
	var trans, yaw float64
	n := 0
	for seed := uint64(1); seed <= seeds; seed++ {
		s := mustSite(t, Corridor, seed, Options{})
		for at := time.Second; at < 100*time.Second; at += window {
			a, _ := s.Truth().Device(Rover1, s.At(at))
			b, _ := s.Truth().Device(Rover1, s.At(at+window))
			truth := a.Pose.Between(b.Pose)
			if math.Abs(truth.Pos().Norm()-1) > 1e-6 || truth.Yaw != 0 {
				continue // a turn falls in this window
			}
			oa, _ := s.Odometry(Rover1, s.At(at))
			ob, _ := s.Odometry(Rover1, s.At(at+window))
			e := truth.Between(oa.Pose.Between(ob.Pose))
			trans += e.Pos().Norm2()
			yaw += e.Yaw * e.Yaw
			n++
		}
		if o, _ := s.Odometry(Rover1, s.At(0)); o.Pose != (geom.Pose2{}) || o.Noise.TransPerMetre != cfg.OdomTransPerMetre {
			t.Fatalf("odometry at start %+v with noise %+v", o.Pose, o.Noise)
		}
	}
	if n < 1000 {
		t.Fatalf("only %d straight windows", n)
	}
	if r := math.Sqrt(trans/float64(n)) / cfg.OdomTransPerMetre; r < transLow || r > transHigh {
		t.Errorf("translation error over 1 m is %.2f x the configured 2 %%; want %.1f to %.1f", r, transLow, transHigh)
	}
	if r := math.Sqrt(yaw/float64(n)) / cfg.OdomYawPerMetre; r < yawLow || r > yawHigh {
		t.Errorf("yaw error over 1 m is %.2f x the configured sigma; want %.1f to %.1f", r, yawLow, yawHigh)
	}

	// Drift accumulates: after a few loops the odometry frame no longer
	// matches the truth, and without noise it matches exactly.
	s := officeSite(t)
	start, _ := s.Truth().Device(Rover1, s.At(0))
	end, _ := s.Truth().Device(Rover1, s.At(300*time.Second))
	o, _ := s.Odometry(Rover1, s.At(300*time.Second))
	if drift := start.Pose.Compose(o.Pose).Pos().Dist(end.Pose.Pos()); drift < 0.2 || drift > 30 {
		t.Errorf("rover-1 odometry is %.2f m off after 300 s; want visible but bounded drift", drift)
	}
	quiet := DefaultConfig().Noiseless()
	exactSite := mustSite(t, Office, 1, Options{Config: &quiet})
	o, _ = exactSite.Odometry(Rover1, exactSite.At(300*time.Second))
	if got := start.Pose.Compose(o.Pose); got.Pos().Dist(end.Pose.Pos()) > 1e-6 || math.Abs(geom.AngleDiff(got.Yaw, end.Pose.Yaw)) > 1e-6 {
		t.Errorf("noiseless odometry %+v does not reproduce the true pose %+v", got, end.Pose)
	}
}

func TestGnssOnlyOutdoors(t *testing.T) {
	s := officeSite(t)
	tr := s.Truth()
	ref := tr.GeoReference()
	if err := ref.Validate(); err != nil {
		t.Fatal(err)
	}
	tangent, err := geography.NewTangent(ref.Origin)
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.Config()
	var sum float64
	fixes, indoors := 0, 0
	for at := time.Duration(0); at < 300*time.Second; at += time.Second {
		st, _ := tr.Device(Ranger, s.At(at))
		fix, ok := s.Gnss(Ranger, s.At(at))
		if ok != (st.Floor == FloorYard) {
			t.Fatalf("at %v the ranger is on %q but GNSS available=%v", at, st.Floor, ok)
		}
		if !ok {
			indoors++
			continue
		}
		local, err := tangent.ToLocal(geography.Coordinate{LatitudeDegrees: fix.LatitudeDegrees, LongitudeDegrees: fix.LongitudeDegrees, AltitudeMetres: fix.AltitudeMetres, AltitudeDatum: geography.WGS84Ellipsoid})
		if err != nil {
			t.Fatal(err)
		}
		e := ref.SiteToTangent().InverseApply(geom.Vec2{X: local[0], Z: local[2]}).Dist(st.Pose.Pos())
		if e > sigmas*fix.HorizontalAccuracy*math.Sqrt2 {
			t.Fatalf("fix at %v is %.1f m from the truth", at, e)
		}
		sum += e * e
		fixes++
	}
	// Planar RMS of white noise with the configured per-axis sigma is √2 sigma.
	if rms := math.Sqrt(sum / float64(fixes)); fixes < 100 || indoors < 20 || rms < 0.8*math.Sqrt2*cfg.GnssSigma || rms > 1.3*math.Sqrt2*cfg.GnssSigma {
		t.Errorf("%d fixes outdoors, %d seconds indoors, planar RMS %.2f m; want about %.2f m", fixes, indoors, rms, math.Sqrt2*cfg.GnssSigma)
	}
	for at := time.Duration(0); at < 100*time.Second; at += time.Second {
		if _, ok := s.Gnss(Rover1, s.At(at)); ok {
			t.Fatalf("rover-1 never leaves the building but has a fix at %v", at)
		}
	}
}

func TestBaroFollowsFloors(t *testing.T) {
	s := officeSite(t)
	tr := s.Truth()
	var alt, pressure [2]float64
	var n [2]int
	changes, last := 0, FloorGround
	for at := time.Duration(0); at < 200*time.Second; at += 200 * time.Millisecond {
		st, _ := tr.Device(Climber, s.At(at))
		b, ok := s.Baro(Climber, s.At(at))
		if !ok {
			t.Fatal("climber has no barometer")
		}
		if st.Floor != last {
			changes, last = changes+1, st.Floor
		}
		level := -1
		switch st.Elevation {
		case 0:
			level = 0
		case StoreyHeight:
			level = 1
		}
		if level >= 0 {
			alt[level] += b.Altitude
			pressure[level] += b.PressurePa
			n[level]++
		}
	}
	if changes < 3 || n[0] < 20 || n[1] < 200 {
		t.Fatalf("climber changed floor %d times with %d ground and %d upper samples", changes, n[0], n[1])
	}
	// The slow drift moves the mean by well under a metre over this window.
	const tolerance = 0.8
	if rise := alt[1]/float64(n[1]) - alt[0]/float64(n[0]); math.Abs(rise-StoreyHeight) > tolerance {
		t.Errorf("barometric altitude rises %.2f m between floors; the storey is %.1f m", rise, StoreyHeight)
	}
	// About 12 Pa per metre near sea level.
	if drop := pressure[0]/float64(n[0]) - pressure[1]/float64(n[1]); drop < 25 || drop > 60 {
		t.Errorf("pressure drops %.1f Pa between floors; want about 42", drop)
	}
}

func TestRangesBearingsAndGas(t *testing.T) {
	s := officeSite(t)
	tr := s.Truth()
	cfg := s.Config()
	ends := map[spatial.DeviceID]geom.Vec3{}
	for _, b := range tr.Beacons() {
		ends[b.ID] = b.Position
	}
	// The antenna of each device, from the surveyed mount and the true base.
	mounts := map[spatial.DeviceID]geom.Vec3{}
	for _, tag := range s.Survey().Tags {
		mounts[tag.Device] = tag.Mount
	}
	antenna := func(dev spatial.DeviceID, at time.Duration) (geom.Vec3, bool) {
		st, ok := tr.Device(dev, s.At(at))
		mount, declared := mounts[dev]
		return geom.Pose3FromPose2(st.Pose, st.Elevation).Apply(mount), ok && declared
	}
	los, nlos, peers := 0, 0, 0
	for at := time.Duration(0); at < 80*time.Second; at += time.Second {
		tag, _ := antenna(Rover1, at)
		for _, r := range s.Ranges(Rover1, s.At(at)) {
			if err := r.Validate(); err != nil {
				t.Fatal(err)
			}
			end, beacon := ends[r.Peer]
			if !beacon {
				var ok bool
				if end, ok = antenna(r.Peer, at); !ok {
					t.Fatalf("range to unknown peer %q", r.Peer)
				}
				peers++
			}
			e := r.Distance - tag.Dist(end)
			clear, ok := tr.RangeLineOfSight(Rover1, r.Peer, s.At(at))
			switch {
			case !ok:
				t.Fatalf("truth has no line-of-sight answer for %q", r.Peer)
			case r.LineOfSight == nil || *r.LineOfSight != clear:
				t.Fatalf("range to %s at %v does not flag its line of sight (%v)", r.Peer, at, clear)
			case clear && (math.Abs(e) > sigmas*cfg.UwbSigma || r.Sigma != cfg.UwbSigma):
				t.Fatalf("clear range to %s is off by %.2f m with sigma %.2f", r.Peer, e, r.Sigma)
			case !clear && (e < -sigmas*cfg.UwbSigma || e > sigmas*(cfg.UwbSigma+cfg.UwbNlosSigma) || r.Sigma <= cfg.UwbSigma):
				t.Fatalf("blocked range to %s is off by %.2f m with sigma %.2f", r.Peer, e, r.Sigma)
			}
			if clear {
				los++
			} else {
				nlos++
			}
		}
	}
	if los < 100 || nlos < 100 || peers < 100 {
		t.Errorf("%d clear, %d blocked and %d device-to-device ranges; want plenty of each", los, nlos, peers)
	}

	var alarm SoundSource
	for _, src := range tr.SoundSources() {
		if src.ID == "alarm" {
			alarm = src
		}
	}
	heard := 0
	for _, mic := range []spatial.DeviceID{"mic-1", "mic-2", "mic-3"} {
		st, _ := tr.Device(mic, s.At(0))
		want := alarm.Position.XZ().Sub(st.Pose.Pos()).Angle()
		for at := 30 * time.Second; at < 40*time.Second; at += 200 * time.Millisecond {
			for _, b := range s.Bearings(mic, s.At(at)) {
				if b.Source != "alarm" || math.Abs(geom.AngleDiff(st.Pose.Yaw+b.Azimuth, want)) > sigmas*cfg.BearingSigma {
					t.Fatalf("%s hears %q at site bearing %.3f; the alarm is at %.3f", mic, b.Source, st.Pose.Yaw+b.Azimuth, want)
				}
				heard++
			}
		}
		if got := s.Bearings(mic, s.At(10*time.Second)); len(got) != 0 {
			t.Errorf("%s reports %d bearings while every source is silent", mic, len(got))
		}
	}
	if heard != 3*50 {
		t.Errorf("the three corridor arrays heard the alarm %d times in 10 s; want 150", heard)
	}
	if got := s.Bearings("mic-4", s.At(35*time.Second)); len(got) != 0 {
		t.Error("the lab array hears the corridor alarm through the wall")
	}
	for _, mic := range []spatial.DeviceID{"mic-4", "mic-5"} {
		if got := s.Bearings(mic, s.At(150*time.Second)); len(got) != 1 || got[0].Source != "compressor" || got[0].Validate() != nil {
			t.Errorf("%s should hear only the compressor in the lab, got %+v", mic, got)
		}
	}

	// Rover-1 passes the stove in the kitchen and is far from it in office A.
	near := whenNear(t, s, Rover1, geom.Vec2{X: 10, Z: 3}, 0, 80*time.Second)
	far := whenNear(t, s, Rover1, geom.Vec2{X: 22.5, Z: 3}, 0, 80*time.Second)
	gNear, _ := s.Gas(Rover1, s.At(near))
	gFar, _ := s.Gas(Rover1, s.At(far))
	if gNear.Value-gFar.Value < 300 || math.Abs(gFar.Value-GasBackgroundPPM) > 50 {
		t.Errorf("gas reads %.0f ppm by the stove and %.0f ppm far away", gNear.Value, gFar.Value)
	}
	if want := tr.GasConcentration(FloorGround, geom.Vec2{X: 10, Z: 3}); math.Abs(gNear.Value-want) > sigmas*cfg.GasSigma+5 {
		t.Errorf("gas by the stove reads %.0f ppm; the field is %.0f ppm", gNear.Value, want)
	}
	if up := tr.GasConcentration(FloorUpper, geom.Vec2{X: 9, Z: 0.6}); up != GasBackgroundPPM {
		t.Errorf("the gas field leaks to the upper floor: %.0f ppm", up)
	}
}

func TestAmbiguousRegions(t *testing.T) {
	// Two closed identical rooms: the scans differ only by noise.
	quiet := DefaultConfig().Noiseless()
	two := mustSite(t, TwoRooms, 1, Options{Config: &quiet})
	amb, ok := two.Truth().Ambiguity()
	if !ok || amb.Continuous || len(amb.Rooms) != 2 {
		t.Fatalf("two-room ambiguity: %+v, %v", amb, ok)
	}
	for at := time.Duration(0); at < 20*time.Second; at += time.Second {
		a, _ := two.Scan("rover-1", two.At(at))
		b, _ := two.Scan("rover-2", two.At(at))
		pa, _ := two.Truth().Device("rover-1", two.At(at))
		pb, _ := two.Truth().Device("rover-2", two.At(at))
		if pb.Pose.Pos().Sub(pa.Pose.Pos()).Dist(amb.Shift) > exact {
			t.Fatalf("the robots are not related by the ambiguity shift at %v", at)
		}
		for i := range a.Ranges {
			if math.Abs(a.Ranges[i]-b.Ranges[i]) > exact {
				t.Fatalf("beam %d differs between the identical rooms at %v: %v and %v", i, at, a.Ranges[i], b.Ranges[i])
			}
		}
	}

	// Featureless corridor: every hit is on a side wall, and the beams
	// along the corridor return nothing.
	corridor := mustSite(t, Corridor, 1, Options{})
	amb, ok = corridor.Truth().Ambiguity()
	if !ok || !amb.Continuous {
		t.Fatalf("corridor ambiguity: %+v, %v", amb, ok)
	}
	cfg := corridor.Config()
	for at := time.Duration(0); at < 40*time.Second; at += time.Second {
		scan, _ := corridor.Scan(Rover1, corridor.At(at))
		pose := sensorPose(t, corridor, Rover1, corridor.At(at))
		open := 0
		for i := range scan.Ranges {
			switch angle, r, state := scan.Beam(i); state {
			case spatial.BeamNoReturn:
				open++
			case spatial.BeamHit:
				z := pose.Apply(geom.Heading(angle).Scale(r)).Z
				if math.Min(math.Abs(z), math.Abs(z-2.4)) > sigmas*cfg.LidarSigma {
					t.Fatalf("beam %d at %v hits something that is not a side wall (z=%.2f)", i, at, z)
				}
			}
		}
		if open < 4 {
			t.Fatalf("only %d beams see past the end of the corridor at %v", open, at)
		}
	}

	// The standard site's two offices are the same room twice.
	s := officeSite(t)
	amb, ok = s.Truth().Ambiguity()
	if !ok || len(amb.Rooms) != 2 {
		t.Fatalf("office ambiguity: %+v, %v", amb, ok)
	}
	var polys [2][]geom.Vec2
	for _, r := range s.Truth().Rooms() {
		for i, id := range amb.Rooms {
			if r.ID == id {
				polys[i] = r.Polygon
			}
		}
	}
	inside := func(i int, p geom.Vec2) bool { return geom.PointInPolygon(p, polys[i]) }
	var content [2][]Shape
	for _, o := range s.Truth().Objects(s.At(0)) {
		for i := range polys {
			if o.Floor == amb.Floor && inside(i, o.Shape.Centre) {
				content[i] = append(content[i], o.Shape)
			}
		}
	}
	if len(polys[0]) == 0 || len(content[0]) < 2 || len(content[0]) != len(content[1]) {
		t.Fatalf("offices hold %d and %d objects", len(content[0]), len(content[1]))
	}
	for i, v := range polys[0] {
		if v.Add(amb.Shift).Dist(polys[1][i]) > exact {
			t.Errorf("office outlines differ at vertex %d", i)
		}
	}
	for i, sh := range content[0] {
		sh.Centre = sh.Centre.Add(amb.Shift)
		if sh != content[1][i] {
			t.Errorf("office furniture differs: %+v and %+v", sh, content[1][i])
		}
	}
}

func TestLidarSpeedBudget(t *testing.T) {
	s := mustSite(t, Office, 1, Options{})
	start := time.Now()
	n := 0
	for m := range s.Stream(s.At(0), s.At(time.Minute), Filter{Devices: []spatial.DeviceID{Rover1}, Kinds: []spatial.SensorKind{spatial.KindLidar2D}}) {
		if scan, ok := m.(spatial.Scan); ok && len(scan.Ranges) == 360 {
			n++
		}
	}
	elapsed := time.Since(start)
	if n != 600 {
		t.Fatalf("%d sweeps of 360 beams in a minute; want 600", n)
	}
	if budget := lidarMinuteBudget * raceSlowdown; elapsed > budget {
		t.Errorf("60 s of lidar took %v; budget %v", elapsed, budget)
	}
	t.Logf("60 s of one robot's lidar: %v", elapsed)
}

func TestConcurrentGenerationIsDeterministic(t *testing.T) {
	const workers, samples = 8, 41 // samples is prime, so every stride below visits all of them
	at := func(s *Site, i int) time.Time { return s.At(time.Duration(i) * 7 * time.Second) }
	serial := mustSite(t, Office, 4, Options{})
	want := make([]geom.Pose2, samples)
	for i := range want {
		o, _ := serial.Odometry(Rover3, at(serial, i))
		want[i] = o.Pose
	}
	shared := mustSite(t, Office, 4, Options{})
	got := make([][]geom.Pose2, workers)
	var wg sync.WaitGroup
	for w := range got {
		got[w] = make([]geom.Pose2, samples)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < samples; k++ {
				i := (k*(w+1) + w) % samples // every worker visits the instants in its own order
				o, _ := shared.Odometry(Rover3, at(shared, i))
				got[w][i] = o.Pose
				shared.Scan(Rover3, at(shared, i))
			}
		}()
	}
	wg.Wait()
	for w := range got {
		if !slices.Equal(got[w], want) {
			t.Fatalf("worker %d read odometry that differs from the serial run", w)
		}
	}
}

func TestSurveyMatchesTruth(t *testing.T) {
	s := officeSite(t)
	survey := s.Survey()
	if !slices.Equal(survey.Beacons, s.Truth().Beacons()) || len(survey.MicArrays) != 5 {
		t.Fatalf("survey lists %d beacons and %d microphone arrays", len(survey.Beacons), len(survey.MicArrays))
	}
	for _, m := range survey.MicArrays {
		st, ok := s.Truth().Device(m.Device, s.At(0))
		if !ok || st.Pose != m.Pose || st.Elevation != m.Elevation || st.Floor != m.Floor {
			t.Errorf("surveyed %+v, truth %+v", m, st)
		}
	}
	if len(survey.Tags) != 6 {
		t.Errorf("survey lists %d UWB tags; want one per robot", len(survey.Tags))
	}
	for _, tag := range survey.Tags {
		if mount, ok := s.Truth().UwbMount(tag.Device); !ok || mount != tag.Mount || mount.Y <= 0 || mount.XZ().Norm() == 0 {
			t.Errorf("%s: surveyed antenna %v, truth %v; want a mount off the base origin", tag.Device, tag.Mount, mount)
		}
	}
	if _, ok := s.Truth().UwbMount(Handheld); ok {
		t.Error("the handheld has no UWB antenna")
	}
}
