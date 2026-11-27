package objtrack

import (
	"errors"
	"math"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
)

// Everything in this file is built by hand: no simulator, no noise that is
// not drawn here.

var t0 = spatial.Stamp(1_700_000_000_000_000_000)

func at(seconds float64) time.Time { return t0.Add(time.Duration(seconds * float64(time.Second))) }

// det is a detection at (x, z) with an isotropic 1σ by the camera of a device.
func det(x, z, sigma float64, class string, device spatial.DeviceID) Detection {
	return Detection{
		Position: geom.Vec2{X: x, Z: z}, Cov: geom.Cov2{sigma * sigma, 0, 0, sigma * sigma}, RadiusM: 0.25,
		Class: class, Confidence: 0.9, Floor: "ground", Sensor: spatial.SensorRef{Device: device, Sensor: "camera"},
	}
}

func newTracker(t *testing.T) *Tracker {
	t.Helper()
	tk, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	return tk
}

func observe(t *testing.T, tk *Tracker, when time.Time, dets ...Detection) {
	t.Helper()
	if err := tk.Observe(when, dets); err != nil {
		t.Fatal(err)
	}
}

// reported are the tracks an application acts on: confirmed and coasting.
func reported(tk *Tracker, when time.Time) []spatial.Track {
	var out []spatial.Track
	for _, k := range tk.Tracks(when) {
		if k.State == spatial.TrackConfirmed || k.State == spatial.TrackCoasting {
			out = append(out, k)
		}
	}
	return out
}

func nearest(tracks []spatial.Track, p geom.Vec2) spatial.Track {
	best := spatial.Track{}
	for i, k := range tracks {
		if i == 0 || k.Point().XZ().Dist(p) < best.Point().XZ().Dist(p) {
			best = k
		}
	}
	return best
}

func TestFilterFollowsConstantVelocity(t *testing.T) {
	r := geom.Cov2{1e-4, 0, 0, 1e-4}
	f := newFilter(geom.Vec2{X: 1, Z: 2}, r, 1.5)
	for i := 1; i <= 30; i++ {
		f.predict(0.1, 4)
		z := geom.Vec2{X: 1 + 0.1*float64(i), Z: 2 - 0.05*float64(i)} // 1 m/s in x, -0.5 m/s in z
		if d2, _, ok := f.distance(z, r); !ok || (i > 5 && d2 > 1) {
			t.Fatalf("step %d: a measurement on the path is %.2f away", i, d2)
		}
		if !f.update(z, r) {
			t.Fatal("update refused")
		}
	}
	if f.pos().Dist(geom.Vec2{X: 4, Z: 0.5}) > 0.01 || f.vel().Dist(geom.Vec2{X: 1, Z: -0.5}) > 0.05 {
		t.Fatalf("state %+v", f.x)
	}
	for i := 0; i < 4; i++ {
		if f.p[5*i] <= 0 {
			t.Fatalf("variance %d = %g", i, f.p[5*i])
		}
		for j := 0; j < 4; j++ {
			if f.p[4*i+j] != f.p[4*j+i] {
				t.Fatal("covariance not symmetric")
			}
		}
	}
	before := f.posCov().MaxSigma()
	f.predict(1, 4)
	if f.pos().Dist(geom.Vec2{X: 5, Z: 0}) > 0.06 || f.posCov().MaxSigma() <= before {
		t.Fatalf("prediction %v, sigma %.3f after %.3f", f.pos(), f.posCov().MaxSigma(), before)
	}
	// A singular innovation is refused, not inverted.
	g := filter{}
	if g.update(geom.Vec2{}, geom.Cov2{}) {
		t.Fatal("singular update accepted")
	}
	// A stopped filter stays put and keeps no velocity.
	f.stop()
	p := f.pos()
	f.predict(10, 0)
	f.update(p.Add(geom.Vec2{X: 0.1}), r)
	if f.vel() != (geom.Vec2{}) || f.pos().Dist(p) > 0.11 {
		t.Fatalf("stopped filter moved: %+v", f.x)
	}
}

func TestAssignIsOptimal(t *testing.T) {
	rng := rand.New(rand.NewPCG(3, 9))
	for trial := 0; trial < 200; trial++ {
		n := 1 + rng.IntN(4)
		m := n + rng.IntN(3)
		cost := make([][]float64, n)
		for i := range cost {
			cost[i] = make([]float64, m)
			for j := range cost[i] {
				cost[i][j] = math.Round(rng.Float64()*200-100) / 10 // negative costs too
			}
		}
		got := assign(cost)
		total, used := 0.0, map[int]bool{}
		for i, j := range got {
			if used[j] {
				t.Fatalf("column %d used twice", j)
			}
			used[j] = true
			total += cost[i][j]
		}
		best := math.Inf(1)
		var search func(i int, taken uint, sum float64)
		search = func(i int, taken uint, sum float64) {
			if i == n {
				best = math.Min(best, sum)
				return
			}
			for j := 0; j < m; j++ {
				if taken&(1<<j) == 0 {
					search(i+1, taken|1<<j, sum+cost[i][j])
				}
			}
		}
		search(0, 0, 0)
		if math.Abs(total-best) > 1e-9 {
			t.Fatalf("trial %d: assignment costs %g, the optimum is %g", trial, total, best)
		}
	}
}

func TestMatchLeavesUnexplainedUnassigned(t *testing.T) {
	cost := [][]float64{{1, 8}, {2, 30}, {50, 50}}
	ok := [][]bool{{true, true}, {true, true}, {false, true}}
	got := match(3, 2, 10, func(i, j int) (float64, bool) { return cost[i][j], ok[i][j] })
	// Row 0 gives column 0 to row 1 (1+10 > 8+2); row 2 has nothing under the
	// cost of staying unassigned.
	if !reflect.DeepEqual(got, []int{1, 0, -1}) {
		t.Fatalf("match = %v", got)
	}
	if got := match(0, 3, 10, nil); got != nil {
		t.Fatalf("no rows: %v", got)
	}
	if got := match(2, 0, 10, func(i, j int) (float64, bool) { return 0, true }); !reflect.DeepEqual(got, []int{-1, -1}) {
		t.Fatalf("no columns: %v", got)
	}
}

const roomSide = 8.0

// roomScan is what a 360 beam lidar at pose sees in an empty square room
// [0, roomSide]² in the site frame, plus bodies given as circles (forward,
// right, radius) in the SENSOR frame.
func roomScan(pose geom.Pose2, when time.Time, bodies ...[3]float64) spatial.Scan {
	s := spatial.Scan{
		Header:   spatial.Header{Device: "rover", Sensor: "lidar", Time: when},
		AngleMin: -math.Pi, AngleIncrement: 2 * math.Pi / 360, RangeMin: 0.1, RangeMax: 20, RangeSigma: 0.015,
		Ranges: make([]float64, 360),
	}
	o := pose.Pos()
	for i := range s.Ranges {
		dir := pose.Rotate(geom.Heading(s.Angle(i)))
		r := math.Inf(1)
		for _, c := range [][2]float64{{o.X, dir.X}, {o.Z, dir.Z}} {
			if c[1] > 0 {
				r = math.Min(r, (roomSide-c[0])/c[1])
			} else if c[1] < 0 {
				r = math.Min(r, -c[0]/c[1])
			}
		}
		for _, b := range bodies {
			if d, ok := geom.RayCircle(geom.Vec2{}, geom.Heading(s.Angle(i)), geom.Vec2{X: b[0], Z: b[1]}, b[2]); ok {
				r = math.Min(r, d)
			}
		}
		s.Ranges[i] = r
	}
	return s
}

// roomMap is the occupancy map of the empty room, as a mapper would build it.
func roomMap(t *testing.T) *grid.Grid {
	t.Helper()
	g, err := grid.New(grid.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	for rep := 0; rep < 6; rep++ {
		for _, p := range []geom.Pose2{{X: 2, Z: 2}, {X: 6, Z: 6, Yaw: 1}, {X: 4, Z: 4, Yaw: 2.5}, {X: 2, Z: 6, Yaw: -1}, {X: 6, Z: 2, Yaw: 0.3}} {
			if err := g.InsertScan(p, roomScan(p, t0)); err != nil {
				t.Fatal(err)
			}
		}
	}
	return g
}

func TestLidarDetectionsSubtractMapAndPlaceBodies(t *testing.T) {
	bg, err := NewBackground(roomMap(t))
	if err != nil {
		t.Fatal(err)
	}
	// The sensor at (3, 4) faces +Z (yaw +π/2). Positive yaw is clockwise seen
	// from above and sensor +Z is right, so its right is site -X: a body 2 m
	// ahead and 2 m to the right stands at site (1, 6), one 2 m ahead and 2 m
	// to the left at (5, 6).
	pose := geom.Pose2{X: 3, Z: 4, Yaw: math.Pi / 2}
	sigma := PoseSigma{PositionM: 0.02, RotationRad: 0.005}
	for _, c := range []struct {
		name string
		body [3]float64
		want geom.Vec2
	}{
		{"ahead right", [3]float64{2, 2, 0.25}, geom.Vec2{X: 1, Z: 6}},
		{"ahead left", [3]float64{2, -2, 0.25}, geom.Vec2{X: 5, Z: 6}},
		{"behind", [3]float64{-2.5, 0, 0.3}, geom.Vec2{X: 3, Z: 1.5}},
	} {
		dets, err := LidarDetections(roomScan(pose, t0, c.body), pose, sigma, bg, Config{})
		if err != nil {
			t.Fatal(err)
		}
		if len(dets) != 1 {
			t.Fatalf("%s: %d detections, want the body alone: %+v", c.name, len(dets), dets)
		}
		d := dets[0]
		if e := d.Position.Dist(c.want); e > 0.05 {
			t.Errorf("%s: detected at %v, %.3f m from %v", c.name, d.Position, e, c.want)
		}
		if math.Abs(d.RadiusM-c.body[2]) > 0.08 {
			t.Errorf("%s: radius %.3f, want %.2f", c.name, d.RadiusM, c.body[2])
		}
		if _, err := d.Cov.Inverse(); err != nil || d.Cov.MaxSigma() > 0.3 {
			t.Errorf("%s: covariance %v", c.name, d.Cov)
		}
	}
	// The empty room is all map: nothing to report.
	if dets, err := LidarDetections(roomScan(pose, t0), pose, sigma, bg, Config{}); err != nil || len(dets) != 0 {
		t.Fatalf("empty room: %v, %+v", err, dets)
	}
	// Two bodies, the nearer one hiding half of the other: the hidden one is
	// refused (its visible part does not say where its centre is), the near
	// one is reported.
	dets, err := LidarDetections(roomScan(pose, t0, [3]float64{1.5, 0, 0.25}, [3]float64{3, 0.3, 0.25}), pose, sigma, bg, Config{})
	if err != nil || len(dets) != 1 || dets[0].Position.Dist(geom.Vec2{X: 3, Z: 5.5}) > 0.05 {
		t.Fatalf("occlusion: %v, %+v", err, dets)
	}
	// Without a map there is nothing to subtract.
	if _, err := LidarDetections(roomScan(pose, t0), pose, sigma, nil, Config{}); !errors.Is(err, ErrNoMap) {
		t.Fatalf("no background: %v", err)
	}
	if _, err := NewBackground(); !errors.Is(err, ErrNoMap) {
		t.Fatalf("empty background: %v", err)
	}
	if _, err := LidarDetections(spatial.Scan{}, pose, sigma, bg, Config{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid scan: %v", err)
	}
	if _, err := LidarDetections(roomScan(pose, t0), pose, sigma, bg, Config{StaticSigmas: -1}); !errors.Is(err, ErrConfig) {
		t.Fatalf("invalid config: %v", err)
	}
}

func object(px geom.Pixel, class string, confidence float64) spatial.Detection {
	return spatial.Detection{
		Kind: spatial.DetectObject, Pixel: px, Class: class, Confidence: confidence,
		BBox: &spatial.BBox{Min: geom.Pixel{U: px.U - 40, V: px.V - 200}, Max: geom.Pixel{U: px.U + 40, V: px.V}},
	}
}

func testCamera(t *testing.T) Camera {
	t.Helper()
	pose, err := geom.LookAt(geom.Vec3{X: 0.3, Y: 2.7, Z: 0.3}, geom.Vec3{X: 4, Y: 0.3, Z: 4}, geom.Vec3{Y: 1})
	if err != nil {
		t.Fatal(err)
	}
	return Camera{
		Intrinsics: geom.Intrinsics{FX: 800, FY: 800, CX: 640, CY: 360, K1: -0.1, K2: 0.01, Width: 1280, Height: 720},
		ToSite:     pose, Sigma: PoseSigma{PositionM: 0.015, RotationRad: 0.005},
	}
}

func TestCameraDetectionsProjectFootPixels(t *testing.T) {
	cam := testCamera(t)
	foot := geom.Vec3{X: 3, Z: 5}
	px, ok := cam.Intrinsics.ProjectWorld(cam.ToSite, foot)
	if !ok || !cam.Intrinsics.InImage(px) {
		t.Fatalf("test point not in view: %v", px)
	}
	sky, _ := cam.Intrinsics.ProjectWorld(cam.ToSite, geom.Vec3{X: 6, Y: 4, Z: 6}) // above the camera: its ray never meets the floor
	frame := spatial.CameraFrame{
		Header: spatial.Header{Device: "cam-a", Sensor: "camera", Time: t0}, Width: 1280, Height: 720, PixelSigma: 0.5,
		Detections: []spatial.Detection{
			{Kind: spatial.DetectFiducial, ID: "tag-1", Pixel: px},
			object(px, "person", 0.9),
			object(geom.Pixel{U: px.U + 60, V: px.V}, "person", 0.3), // under MinConfidence
			object(sky, "person", 0.9),
		},
	}
	dets, err := CameraDetections(frame, cam, nil, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if len(dets) != 1 {
		t.Fatalf("%d detections, want 1: %+v", len(dets), dets)
	}
	d := dets[0]
	if e := d.Position.Dist(foot.XZ()); e > 1e-3 {
		t.Errorf("foot placed at %v, %.4f m off", d.Position, e)
	}
	if d.Class != "person" || d.Confidence != 0.9 {
		t.Errorf("class %q %.2f", d.Class, d.Confidence)
	}
	// Half the 80 px box at the foot's range.
	if want := 40 * foot.Dist(cam.ToSite.T) / 800; math.Abs(d.RadiusM-want) > 1e-9 {
		t.Errorf("radius %.3f, want %.3f", d.RadiusM, want)
	}
	// The floor point is least certain along the view direction, and at 6.5 m
	// from a camera 2.7 m up it is known to decimetres, not centimetres.
	major, minor, yaw := d.Cov.Eigen()
	view := foot.XZ().Sub(cam.ToSite.T.XZ()).Angle()
	if off := math.Abs(math.Sin(yaw - view)); major <= minor || off > 0.1 {
		t.Errorf("covariance axes %.4f, %.4f at yaw %.2f; view direction %.2f", major, minor, yaw, view)
	}
	if s := d.Cov.MaxSigma(); s < 0.05 || s > 0.3 {
		t.Errorf("sigma %.3f m", s)
	}
	// A foot pixel on mapped structure is structure.
	bg, err := NewBackground(roomMap(t))
	if err != nil {
		t.Fatal(err)
	}
	wall, _ := cam.Intrinsics.ProjectWorld(cam.ToSite, geom.Vec3{X: 4, Z: 7.97})
	frame.Detections = []spatial.Detection{object(wall, "shelf", 0.9), object(px, "person", 0.9)}
	if dets, err := CameraDetections(frame, cam, bg, Config{}); err != nil || len(dets) != 1 || dets[0].Class != "person" {
		t.Fatalf("with background: %v, %+v", err, dets)
	}
	bad := cam
	bad.Intrinsics.FX = 0
	if _, err := CameraDetections(frame, bad, nil, Config{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid intrinsics: %v", err)
	}
}

func TestTrackLifecycleAndClassVotes(t *testing.T) {
	tk := newTracker(t)
	confirmedAt, id := -1, ""
	for i := 0; i <= 30; i++ { // 1 m/s along +X, 10 Hz
		class := "person"
		if i == 7 {
			class = "cart" // one confused frame
		}
		when := at(float64(i) / 10)
		observe(t, tk, when, det(float64(i)/10, 2, 0.05, class, "cam-a"))
		tracks := tk.Tracks(when)
		if len(tracks) != 1 {
			t.Fatalf("step %d: %d tracks", i, len(tracks))
		}
		k := tracks[0]
		if err := k.Validate(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if id == "" {
			id = k.ID
		}
		if k.ID != id {
			t.Fatalf("step %d: identity changed from %s to %s", i, id, k.ID)
		}
		if k.State == spatial.TrackConfirmed && confirmedAt < 0 {
			confirmedAt = i
		}
		if (k.State != spatial.TrackTentative) != (confirmedAt >= 0) {
			t.Fatalf("step %d: state %s", i, k.State)
		}
	}
	// It must travel MinTravelM plus its diameter, 1 m, to be confirmed.
	if confirmedAt < 10 || confirmedAt > 12 {
		t.Fatalf("confirmed at step %d, want after 1 m of travel", confirmedAt)
	}
	k := tk.Tracks(at(3))[0]
	if k.Point().XZ().Dist(geom.Vec2{X: 3, Z: 2}) > 0.05 || k.VelocityMPS.Dist(geom.Vec2{X: 1}) > 0.25 || k.Floor != "ground" {
		t.Fatalf("estimate %+v", k)
	}
	if k.Class != "person" || k.ClassConfidence < 0.9 || k.ClassConfidence >= 1 {
		t.Fatalf("class %q %.3f", k.Class, k.ClassConfidence)
	}
	if !k.FirstSeen.Equal(at(0)) || !k.LastSeen.Equal(at(3)) || len(k.Sensors) != 1 || k.Sensors[0].Device != "cam-a" || k.RadiusM != 0.25 {
		t.Fatalf("bookkeeping %+v", k)
	}
	// Unobserved it is predicted, with growing uncertainty, then dropped.
	if got := tk.Tracks(at(3.3))[0]; got.State != spatial.TrackConfirmed {
		t.Fatalf("0.3 s unobserved: %s", got.State)
	}
	coast := tk.Tracks(at(4))[0]
	if coast.State != spatial.TrackCoasting || coast.ID != id || coast.Point().XZ().Dist(geom.Vec2{X: 4, Z: 2}) > 0.3 || coast.UncertaintyM <= k.UncertaintyM || !coast.LastSeen.Equal(at(3)) {
		t.Fatalf("coasting %+v", coast)
	}
	if got := tk.Tracks(at(6.5)); len(got) != 0 {
		t.Fatalf("3.5 s unobserved: %+v", got)
	}
	observe(t, tk, at(7))
	if got := tk.Tracks(at(7)); len(got) != 0 {
		t.Fatalf("after deletion: %+v", got)
	}
}

func TestStaticStructureIsNeverConfirmed(t *testing.T) {
	tk := newTracker(t)
	rng := rand.New(rand.NewPCG(7, 7))
	noisy := func(p geom.Vec2, sigma float64) (float64, float64) {
		return p.X + sigma*rng.NormFloat64(), p.Z + sigma*rng.NormFloat64()
	}
	seen := 0
	for i := 0; i < 600; i++ { // 60 s at 10 Hz
		// A crate that was not in the map, moved 3 m by a scheduled event
		// half way, seen by a lidar and by a camera whose calibration puts
		// it 0.6 m to the side. A second piece of furniture stands 3 m off.
		crate, shelf := geom.Vec2{X: 4, Z: 4}, geom.Vec2{X: 4, Z: 7}
		if i >= 300 {
			crate = geom.Vec2{X: 7, Z: 4}
		}
		when := at(float64(i) / 10)
		if i%13 != 0 { // dropouts
			x, z := noisy(crate, 0.05)
			observe(t, tk, when, det(x, z, 0.08, "", "lidar-a"))
		}
		if i%2 == 0 && i%7 != 0 { // the camera at 5 Hz, with gaps of its own
			x, z := noisy(crate.Add(geom.Vec2{X: 0.6}), 0.1)
			sx, sz := noisy(shelf, 0.1)
			observe(t, tk, when.Add(40*time.Millisecond), det(x, z, 0.3, "crate", "cam-b"), det(sx, sz, 0.3, "shelf", "cam-b"))
		}
		for _, k := range tk.Tracks(when) {
			if k.State != spatial.TrackTentative {
				t.Fatalf("step %d: track %s on static structure is %s at %v", i, k.ID, k.State, k.Point())
			}
			seen++
		}
	}
	if seen < 600 {
		t.Fatalf("the structure was detected but tracked for only %d track instants", seen)
	}
}

func TestJumpBetweenStaticBodiesIsNotTravel(t *testing.T) {
	tk := newTracker(t)
	a, b, c := geom.Vec2{X: 2, Z: 2}, geom.Vec2{X: 4.2, Z: 2}, geom.Vec2{X: 6.4, Z: 2}
	see := func(s float64, p geom.Vec2, device spatial.DeviceID) {
		t.Helper()
		observe(t, tk, at(s), det(p.X, p.Z, 0.05, "", device))
		if rep := reported(tk, at(s)); len(rep) != 0 {
			t.Fatalf("t=%.1f: a track that only jumped between static bodies is %s", s, rep[0].State)
		}
	}
	// Something is seen three times at a. Its track is young and may be
	// moving, so when 0.8 s later a body shows 2.2 m away at b, the track is
	// the likeliest explanation and takes it; the same again at c. It has
	// then been in three places, but no sensor has seen it travel: one saw
	// it at a and at c, the other only at b.
	for i := 0; i < 3; i++ {
		see(float64(i)/10, a, "lidar-a")
	}
	id := tk.Tracks(at(0.2))[0].ID
	for i := 0; i < 5; i++ {
		see(1+float64(i)/10, b, "lidar-b")
	}
	if ks := tk.Tracks(at(1.4)); len(ks) != 1 || ks[0].ID != id {
		t.Fatalf("premise: the track at a did not take the detection at b: %+v", ks)
	}
	for i := 0; i < 20; i++ {
		see(2.2+float64(i)/10, c, "lidar-a")
	}
	if ks := tk.Tracks(at(4.1)); len(ks) != 1 || ks[0].ID != id || ks[0].Point().XZ().Dist(c) > 0.1 {
		t.Fatalf("premise: the track did not take the detection at c: %+v", ks)
	}
}

func TestWatchedStructureStaysPut(t *testing.T) {
	tk := newTracker(t)
	a, b := geom.Vec2{X: 2, Z: 2}, geom.Vec2{X: 4.2, Z: 2}
	for i := 0; i <= 60; i++ { // 6 s, longer than StaticAfter
		observe(t, tk, at(float64(i)/10), det(a.X, a.Z, 0.05, "", "lidar-a"))
	}
	id := tk.Tracks(at(6))[0].ID
	// The same jump as in the test above, but the track has been watched
	// standing still for long enough to be structure: it no longer grows
	// uncertain while unobserved, and what appears at b is something else.
	for i := 0; i < 2; i++ {
		observe(t, tk, at(6.8+float64(i)/10), det(b.X, b.Z, 0.05, "", "lidar-a"))
	}
	ks := tk.Tracks(at(6.9))
	if len(ks) != 2 {
		t.Fatalf("%d tracks, want the structure at a and a new one at b: %+v", len(ks), ks)
	}
	for _, k := range ks {
		want := b
		if k.ID == id {
			want = a
			if k.VelocityMPS != (geom.Vec2{}) || k.UncertaintyM > 0.05 {
				t.Fatalf("structure track %+v", k)
			}
		}
		if k.State != spatial.TrackTentative || k.Point().XZ().Dist(want) > 0.05 {
			t.Fatalf("track %s is %s at %v, want tentative at %v", k.ID, k.State, k.Point(), want)
		}
		if err := k.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	// A localized device that turns out to be what was standing there is a
	// device track again, moving or not.
	if err := tk.ObservePose(at(6.9), devicePose("rover", a.X, a.Z, 0)); err != nil {
		t.Fatal(err)
	}
	if got, ok := tk.DeviceTrack("rover"); !ok || got != id {
		t.Fatalf("device track %q, want %s", got, id)
	}
}

func TestCrossSensorHandoffKeepsIdentity(t *testing.T) {
	tk := newTracker(t)
	// A walker at 1 m/s along +X. cam-a sees it for the first 5 m, nobody for
	// the next 1.4 m, cam-b (which places it 0.2 m to the side) from there on.
	var id string
	ids := map[string]bool{}
	for i := 0; i <= 50; i++ {
		s := float64(i) / 5
		when := at(s)
		switch {
		case s <= 5:
			observe(t, tk, when, det(s, 3, 0.15, "person", "cam-a"))
		case s >= 6.4:
			observe(t, tk, when, det(s, 3.2, 0.15, "person", "cam-b"))
		default:
			observe(t, tk, when) // time passes
		}
		rep := reported(tk, when)
		if s >= 3 && len(rep) != 1 {
			t.Fatalf("t=%.1f: %d reported tracks", s, len(rep))
		}
		for _, k := range rep {
			ids[k.ID] = true
			id = k.ID
			if want := spatial.TrackCoasting; s > 5.6 && s < 6.4 && k.State != want {
				t.Fatalf("t=%.1f: state %s between the views", s, k.State)
			}
			if s >= 6.4 && k.State != spatial.TrackConfirmed {
				t.Fatalf("t=%.1f: state %s in the second view", s, k.State)
			}
		}
	}
	if len(ids) != 1 {
		t.Fatalf("the walker had %d identities: %v", len(ids), ids)
	}
	k := reported(tk, at(10))[0]
	want := []spatial.SensorRef{{Device: "cam-a", Sensor: "camera"}, {Device: "cam-b", Sensor: "camera"}}
	if k.ID != id || !reflect.DeepEqual(k.Sensors, want) || k.Point().XZ().Dist(geom.Vec2{X: 10, Z: 3.2}) > 0.3 {
		t.Fatalf("after handoff: %+v", k)
	}
	// One unassigned detection is all that has to remain of a second track.
	for _, k := range tk.Tracks(at(10)) {
		if k.ID != id {
			t.Fatalf("second track %+v", k)
		}
	}
}

// crossing runs two walkers whose paths cross at right angles 0.8 s apart,
// through a 5% miss rate, and returns the tracker and where they end.
func crossing(t *testing.T) (*Tracker, geom.Vec2, geom.Vec2) {
	tk := newTracker(t)
	rng := rand.New(rand.NewPCG(11, 4))
	var a, b geom.Vec2
	for i := 0; i <= 100; i++ {
		s := float64(i) / 10
		a, b = geom.Vec2{X: s, Z: 5}, geom.Vec2{X: 5, Z: s - 0.8}
		var dets []Detection
		for _, p := range []geom.Vec2{a, b} {
			if rng.Float64() < 0.05 {
				continue
			}
			dets = append(dets, det(p.X+0.05*rng.NormFloat64(), p.Z+0.05*rng.NormFloat64(), 0.07, "person", "cam-a"))
		}
		if i%2 == 1 && len(dets) == 2 {
			dets[0], dets[1] = dets[1], dets[0] // the order within a sample must not matter
		}
		observe(t, tk, at(s), dets...)
	}
	return tk, a, b
}

func TestCrossingWalkersKeepIdentities(t *testing.T) {
	tk := newTracker(t)
	rng := rand.New(rand.NewPCG(11, 4))
	var idA, idB string
	for i := 0; i <= 100; i++ {
		s := float64(i) / 10
		a, b := geom.Vec2{X: s, Z: 5}, geom.Vec2{X: 5, Z: s - 0.8}
		var dets []Detection
		for _, p := range []geom.Vec2{a, b} {
			if rng.Float64() < 0.05 {
				continue
			}
			dets = append(dets, det(p.X+0.05*rng.NormFloat64(), p.Z+0.05*rng.NormFloat64(), 0.07, "person", "cam-a"))
		}
		observe(t, tk, at(s), dets...)
		rep := reported(tk, at(s))
		if s < 3 {
			continue
		}
		if len(rep) != 2 {
			t.Fatalf("t=%.1f: %d reported tracks", s, len(rep))
		}
		ka, kb := nearest(rep, a), nearest(rep, b)
		if idA == "" {
			idA, idB = ka.ID, kb.ID
		}
		if ka.ID != idA || kb.ID != idB || idA == idB {
			t.Fatalf("t=%.1f: identities %s, %s; were %s, %s", s, ka.ID, kb.ID, idA, idB)
		}
		if ka.Point().XZ().Dist(a) > 0.2 || kb.Point().XZ().Dist(b) > 0.2 {
			t.Fatalf("t=%.1f: tracks %.2f and %.2f m from their walkers", s, ka.Point().XZ().Dist(a), kb.Point().XZ().Dist(b))
		}
	}
}

func TestTrackerIsDeterministic(t *testing.T) {
	tk1, a, b := crossing(t)
	tk2, _, _ := crossing(t)
	got, again := tk1.Tracks(at(10)), tk2.Tracks(at(10))
	if !reflect.DeepEqual(got, again) {
		t.Fatal("two runs over the same input differ")
	}
	rep := reported(tk1, at(10))
	if len(rep) != 2 || nearest(rep, a).ID == nearest(rep, b).ID {
		t.Fatalf("reported %+v", rep)
	}
}

func devicePose(dev spatial.DeviceID, x, z, yaw float64) spatial.DevicePose {
	return spatial.DevicePose{
		Device: dev, Floor: "ground", FrameID: "site", Location: spatial.At(geom.Vec3{X: x, Z: z}, 0.03), Yaw: yaw,
		Covariance: geom.Mat3{4e-4, 0, 0, 0, 4e-4, 0, 0, 0, 1e-4}, Method: "test", Score: 1,
	}
}

func TestDeviceOwnsOneTrack(t *testing.T) {
	tk := newTracker(t)
	// A camera sees the robot before it is localized.
	for i := 0; i < 3; i++ {
		observe(t, tk, at(float64(i)/10), det(2.3, 2, 0.2, "robot", "cam-a"))
	}
	before := tk.Tracks(at(0.2))
	if len(before) != 1 || before[0].State != spatial.TrackTentative {
		t.Fatalf("before localization: %+v", before)
	}
	if _, ok := tk.DeviceTrack("rover"); ok {
		t.Fatal("device track before any pose")
	}
	for i := 3; i <= 40; i++ {
		s := float64(i) / 10
		x := 2 + 0.5*(s-0.3) // 0.5 m/s along +X
		if err := tk.ObservePose(at(s), devicePose("rover", x, 2, 0)); err != nil {
			t.Fatal(err)
		}
		// The camera keeps seeing it 0.3 m off. Once the robot's own camera
		// reports something 0.1 m from the robot: a sensor does not see its
		// carrier, so that is something else.
		observe(t, tk, at(s), det(x+0.3, 2, 0.2, "robot", "cam-a"))
		if i == 10 {
			observe(t, tk, at(s), det(x+0.1, 2, 0.1, "robot", "rover"))
			if n := len(tk.Tracks(at(s))); n != 2 {
				t.Fatalf("the robot's own detection: %d tracks, want a second one", n)
			}
		}
		id, ok := tk.DeviceTrack("rover")
		if !ok || id != before[0].ID {
			t.Fatalf("t=%.1f: device track %q, the camera's track was %s", s, id, before[0].ID)
		}
		rep := reported(tk, at(s))
		if len(rep) != 1 || rep[0].ID != id || rep[0].State != spatial.TrackConfirmed {
			t.Fatalf("t=%.1f: reported %+v", s, rep)
		}
		if e := rep[0].Point().XZ().Dist(geom.Vec2{X: x, Z: 2}); e > 0.05 {
			t.Fatalf("t=%.1f: device track %.3f m from its pose", s, e)
		}
	}
	k := reported(tk, at(4))[0]
	if k.Class != "robot" || len(k.Sensors) != 1 || k.Sensors[0].Device != "cam-a" || math.Abs(k.VelocityMPS.X-0.5) > 0.15 {
		t.Fatalf("device track %+v", k)
	}
	if err := tk.ObservePose(at(4), spatial.DevicePose{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid pose: %v", err)
	}
}

func lidarCalibration(mount geom.Pose2, offset time.Duration) spatial.Calibration {
	return spatial.Calibration{
		Device: "rover", Sensor: "lidar", Kind: spatial.KindLidar2D, SensorToParent: geom.Pose3FromPose2(mount, 0),
		Parent: spatial.ParentDeviceBase, ParentDevice: "rover", TranslationSigmaM: 0.01, RotationSigmaRad: 0.005,
		ResidualUnit: "m", Method: "test", Revision: "r1", State: spatial.CalibrationValid, TimeOffset: offset,
	}
}

func cameraCalibration(cam Camera) spatial.Calibration {
	loc := spatial.At(cam.ToSite.T, 0.03)
	return spatial.Calibration{
		Device: "cam-a", Sensor: "camera", Kind: spatial.KindCamera, Intrinsics: &cam.Intrinsics, SensorToParent: cam.ToSite,
		Parent: spatial.ParentSite, TranslationSigmaM: cam.Sigma.PositionM, RotationSigmaRad: cam.Sigma.RotationRad,
		ResidualUnit: "px", Method: "test", Revision: "r1", State: spatial.CalibrationValid, Location: &loc,
	}
}

func TestIngestPlacesMeasurementsThroughCalibrationAndPose(t *testing.T) {
	tk := newTracker(t)
	if err := tk.SetFloor("ground", 0, roomMap(t)); err != nil {
		t.Fatal(err)
	}
	mount := geom.Pose2{X: 0.2, Z: 0.05, Yaw: 0.1}
	const offset = 40 * time.Millisecond // the lidar's clock runs 40 ms ahead of site time
	if err := tk.SetCalibration(lidarCalibration(mount, offset)); err != nil {
		t.Fatal(err)
	}
	cam := testCamera(t)
	if err := tk.SetCalibration(cameraCalibration(cam)); err != nil {
		t.Fatal(err)
	}

	// The robot drives +Z from (3, 3); a body stands at site (1.5, 6).
	body := geom.Vec2{X: 1.5, Z: 6}
	scanAt := func(s float64) spatial.Scan {
		sensor := geom.Pose2{X: 3, Z: 3 + 0.5*s, Yaw: math.Pi / 2}.Compose(mount)
		local := sensor.InverseApply(body)
		return roomScan(sensor, at(s).Add(offset), [3]float64{local.X, local.Z, 0.25})
	}
	pose := func(s float64) spatial.DevicePose { return devicePose("rover", 3, 3+0.5*s, math.Pi/2) }
	if err := tk.ObservePose(at(0), pose(0)); err != nil {
		t.Fatal(err)
	}
	if err := tk.Ingest(scanAt(0)); err != nil {
		t.Fatal(err)
	}
	// Between two poses the carrier's pose is interpolated.
	if err := tk.ObservePose(at(0.2), pose(0.2)); err != nil {
		t.Fatal(err)
	}
	if err := tk.Ingest(scanAt(0.1)); err != nil {
		t.Fatal(err)
	}
	var seen []spatial.Track
	for _, k := range tk.Tracks(at(0.2)) {
		if k.State == spatial.TrackTentative {
			seen = append(seen, k)
		}
	}
	if len(seen) != 1 || seen[0].Point().XZ().Dist(body) > 0.06 || seen[0].Floor != "ground" || seen[0].Sensors[0] != (spatial.SensorRef{Device: "rover", Sensor: "lidar"}) {
		t.Fatalf("lidar track %+v", seen)
	}
	want := []string{spatial.InputID(scanAt(0)), spatial.InputID(scanAt(0.1))}
	if got := tk.Evidence(seen[0].ID); !reflect.DeepEqual(got, want) {
		t.Fatalf("evidence %v, want %v", got, want)
	}
	// Too long after the last pose the scan cannot be placed.
	if err := tk.Ingest(scanAt(0.8)); !errors.Is(err, ErrNoPose) {
		t.Fatalf("scan 0.6 s after the last pose: %v", err)
	}

	// The fixed camera sees the same body: one track, two sensors.
	px, _ := cam.Intrinsics.ProjectWorld(cam.ToSite, body.Lift(0))
	frame := spatial.CameraFrame{
		Header: spatial.Header{Device: "cam-a", Sensor: "camera", Time: at(0.3)}, Width: 1280, Height: 720, PixelSigma: 0.5,
		Detections: []spatial.Detection{object(px, "person", 0.9)},
	}
	if err := tk.Ingest(frame); err != nil {
		t.Fatal(err)
	}
	k := nearest(tk.Tracks(at(0.3)), body)
	if k.ID != seen[0].ID || len(k.Sensors) != 2 || k.Class != "person" || k.Point().XZ().Dist(body) > 0.06 {
		t.Fatalf("fused track %+v", k)
	}
	// Other measurement kinds are not this feature's.
	if err := tk.Ingest(spatial.Imu{Header: spatial.Header{Device: "rover", Sensor: "imu", Time: at(0.3)}}); err != nil {
		t.Fatal(err)
	}
	if tk.Evidence("nope") != nil {
		t.Fatal("evidence for an unknown track")
	}
}

func TestRefusals(t *testing.T) {
	if _, err := New(Config{GateProbability: 1.5}); !errors.Is(err, ErrConfig) {
		t.Fatalf("gate probability 1.5: %v", err)
	}
	if _, err := New(Config{DeleteAfter: time.Millisecond}); !errors.Is(err, ErrConfig) {
		t.Fatalf("DeleteAfter under CoastAfter: %v", err)
	}
	if c := newTracker(t).Config(); c != DefaultConfig() {
		t.Fatalf("Config{} is not DefaultConfig(): %+v", c)
	}

	tk := newTracker(t)
	pose := geom.Pose2{X: 3, Z: 4}
	scan := roomScan(pose, at(0))
	if err := tk.Ingest(scan); !errors.Is(err, ErrNoCalibration) {
		t.Fatalf("no calibration: %v", err)
	}
	stale := lidarCalibration(geom.Pose2{}, 0)
	stale.State = spatial.CalibrationQuarantined
	if err := tk.SetCalibration(stale); err != nil {
		t.Fatal(err)
	}
	if err := tk.Ingest(scan); !errors.Is(err, ErrNoCalibration) {
		t.Fatalf("quarantined calibration: %v", err)
	}
	if err := tk.SetCalibration(lidarCalibration(geom.Pose2{}, 0)); err != nil {
		t.Fatal(err)
	}
	if err := tk.Ingest(scan); !errors.Is(err, ErrNoPose) {
		t.Fatalf("no pose: %v", err)
	}
	if err := tk.ObservePose(at(0), devicePose("rover", 3, 4, 0)); err != nil {
		t.Fatal(err)
	}
	if err := tk.Ingest(scan); !errors.Is(err, ErrNoMap) {
		t.Fatalf("no floor: %v", err)
	}
	if err := tk.SetFloor("ground", 0, nil); err != nil {
		t.Fatal(err)
	}
	if err := tk.Ingest(scan); !errors.Is(err, ErrNoMap) {
		t.Fatalf("floor without a map: %v", err)
	}
	if err := tk.SetFloor("", 0, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unnamed floor: %v", err)
	}
	if err := tk.SetCalibration(spatial.Calibration{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid calibration: %v", err)
	}
	if got := tk.Tracks(at(0)); len(got) != 1 || got[0].State != spatial.TrackConfirmed {
		t.Fatalf("refused measurements changed the tracker: %+v", got) // the device's own track only
	}

	ok := det(1, 1, 0.1, "person", "cam-a")
	for name, d := range map[string]Detection{
		"singular covariance": func() Detection { d := ok; d.Cov = geom.Cov2{}; return d }(),
		"no floor":            func() Detection { d := ok; d.Floor = ""; return d }(),
		"no sensor":           func() Detection { d := ok; d.Sensor = spatial.SensorRef{}; return d }(),
		"NaN position":        func() Detection { d := ok; d.Position.X = math.NaN(); return d }(),
		"negative radius":     func() Detection { d := ok; d.RadiusM = -1; return d }(),
	} {
		if err := tk.Observe(at(1), []Detection{ok, d}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if err := tk.Observe(time.Time{}, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero time: %v", err)
	}
	if len(tk.Tracks(at(1))) != 1 {
		t.Fatal("a refused batch started a track")
	}
	observe(t, tk, at(5), ok)
	if err := tk.Observe(at(3), []Detection{ok}); !errors.Is(err, ErrStale) {
		t.Fatalf("2 s late: %v", err)
	}
	observe(t, tk, at(4.5), ok) // within MaxLateness
}

func TestLevelsKeepFloorsApart(t *testing.T) {
	tk := newTracker(t)
	for id, y := range map[spatial.FloorID]float64{"ground": 0, "yard": 0.1, "upper": 3.5} {
		if err := tk.SetFloor(id, y, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Two walkers one above the other, both along +X.
	for i := 0; i <= 30; i++ {
		s := float64(i) / 10
		down, up := det(s, 2, 0.05, "person", "cam-a"), det(s, 2, 0.05, "person", "cam-u")
		up.Floor, up.ElevationM = "upper", 3.5
		observe(t, tk, at(s), down)
		observe(t, tk, at(s), up)
	}
	rep := reported(tk, at(3))
	if len(rep) != 2 || rep[0].Floor == rep[1].Floor || math.Abs(rep[0].PositionM[1]-rep[1].PositionM[1]) != 3.5 {
		t.Fatalf("two floors: %+v", rep)
	}
	for _, k := range rep {
		if len(k.Sensors) != 1 {
			t.Fatalf("track %s mixes floors: %+v", k.ID, k.Sensors)
		}
	}
	// A fixed camera 2.7 m above the ground plane looks at the ground level,
	// not at the storey above it.
	cam := testCamera(t)
	if err := tk.SetCalibration(cameraCalibration(cam)); err != nil {
		t.Fatal(err)
	}
	pl, err := tk.place(spatial.Header{Device: "cam-a", Sensor: "camera", Time: at(3)})
	if err != nil || pl.lv.elevation != 0 || len(pl.lv.floors) != 2 || pl.floor != "ground" {
		t.Fatalf("camera placed on %+v, %v", pl.lv, err)
	}
}
