package changes

import (
	"errors"
	"math"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

var t0 = time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

// scene is a hand-built planar world: what a lidar returns from.
type scene []geom.Segment

// box is the outline of an axis-aligned rectangle: a room or a crate.
func box(x0, z0, x1, z1 float64) scene {
	a, b, c, d := geom.Vec2{X: x0, Z: z0}, geom.Vec2{X: x1, Z: z0}, geom.Vec2{X: x1, Z: z1}, geom.Vec2{X: x0, Z: z1}
	return scene{{A: a, B: b}, {A: b, B: c}, {A: c, B: d}, {A: d, B: a}}
}

func crate(cx, cz float64) scene { return box(cx-0.4, cz-0.4, cx+0.4, cz+0.4) }

func join(parts ...scene) scene {
	var out scene
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// scan is a 360-beam sweep of the scene from pose, with 1 cm range noise.
func (sc scene) scan(rng *rand.Rand, pose geom.Pose2, at time.Time) spatial.Scan {
	const beams = 360
	s := spatial.Scan{
		Header:   spatial.Header{Device: "robot", Sensor: "lidar", Time: at},
		AngleMin: -math.Pi, AngleIncrement: 2 * math.Pi / beams, RangeMin: 0.1, RangeMax: 20, RangeSigma: 0.01,
		Ranges: make([]float64, beams),
	}
	for i := range s.Ranges {
		dir, best := geom.Heading(pose.Yaw+s.Angle(i)), math.Inf(1)
		for _, seg := range sc {
			if r, ok := geom.RaySegment(pose.Pos(), dir, seg); ok && r < best {
				best = r
			}
		}
		s.Ranges[i] = spatial.RangeNoReturn
		if best < s.RangeMax {
			s.Ranges[i] = best + s.RangeSigma*rng.NormFloat64()
		}
	}
	return s
}

// The test room is 10 x 8 m; the patrol looks at its middle from four sides.
var (
	room   = box(0, 0, 10, 8)
	patrol = []geom.Pose2{{X: 2, Z: 2, Yaw: 0.3}, {X: 8, Z: 2, Yaw: 2}, {X: 8, Z: 6, Yaw: -2.5}, {X: 2, Z: 6, Yaw: -1}}
)

func input(s spatial.Scan) world.Input {
	return world.Input{ID: spatial.InputID(s), Origin: world.Measured}
}

// survey is the evidence of rounds passes of the patrol, one sweep a second.
func survey(t *testing.T, sc scene, from []geom.Pose2, rounds int, start time.Time) *Evidence {
	t.Helper()
	e, err := NewEvidence("f", Config{})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(7, uint64(start.Unix())))
	for i := 0; i < rounds*len(from); i++ {
		at := start.Add(time.Duration(i) * time.Second)
		s := sc.scan(rng, from[i%len(from)], at)
		if err := e.Add(Sweep{Scan: s, Sensor: from[i%len(from)], Time: at, Input: input(s)}); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func compare(t *testing.T, before, after scene, portals []spatial.Portal) Report {
	t.Helper()
	r, err := Compare(survey(t, before, patrol, 8, t0), survey(t, after, patrol, 8, t0.Add(time.Hour)), portals)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range r.Changes {
		if err := c.Event.Validate(); err != nil {
			t.Fatalf("%+v: %v", c.Event, err)
		}
	}
	return r
}

func planar(p [3]float64) geom.Vec2 { return geom.Vec2{X: p[0], Z: p[2]} }

// near fails unless the event is of the kind and within its own stated
// uncertainty, and 0.1 m, of want.
func near(t *testing.T, ev spatial.ChangeEvent, kind spatial.ChangeKind, want geom.Vec2) {
	t.Helper()
	d := planar(ev.PositionM).Dist(want)
	if ev.Kind != kind || d > 0.1 || d > ev.UncertaintyM || ev.Confidence <= 0 || ev.Confidence > 1 {
		t.Fatalf("got %s at %v ± %.2f (confidence %.2f), want %s within 0.1 m of %v", ev.Kind, ev.PositionM, ev.UncertaintyM, ev.Confidence, kind, want)
	}
}

// Positive yaw is clockwise seen from above and a scan frame has +Z to the
// right: a return on the beam at +90° of a sensor facing +X lies toward +Z,
// and the same sweep from a sensor facing +Z puts it toward -X.
func TestSweepFollowsThePlanarConvention(t *testing.T) {
	s := spatial.Scan{Header: spatial.Header{Device: "robot", Sensor: "lidar", Time: t0}, AngleMin: 0, AngleIncrement: math.Pi / 2, RangeMin: 0.1, RangeMax: 20, Ranges: []float64{spatial.RangeNoReturn, 2}}
	sensor := geom.Vec2{X: 0.025, Z: 0.025} // a cell centre, so the return is one too
	for _, tc := range []struct {
		yaw   float64
		right geom.Vec2 // where the beam at +90° points
	}{
		{0, geom.Vec2{Z: 1}},
		{math.Pi / 2, geom.Vec2{X: -1}},
	} {
		e, err := NewEvidence("f", Config{})
		if err != nil {
			t.Fatal(err)
		}
		for i := range 6 {
			if err := e.Add(Sweep{Scan: s, Sensor: geom.Pose2{X: sensor.X, Z: sensor.Z, Yaw: tc.yaw}, Time: t0.Add(time.Duration(i) * time.Second)}); err != nil {
				t.Fatal(err)
			}
		}
		ahead := geom.Heading(tc.yaw)
		for _, p := range []struct {
			at   geom.Vec2
			want State
		}{
			{sensor.Add(tc.right.Scale(2)), Occupied},                           // the return
			{sensor.Add(tc.right.Scale(1)), Free},                               // crossed on the way
			{sensor.Add(tc.right.Scale(3)), Unobserved},                         // behind the return
			{sensor.Add(tc.right.Scale(-2)), Unobserved},                        // the mirror image
			{sensor.Add(ahead.Scale(5)), Free},                                  // the beam without a return clears
			{sensor.Add(ahead.Scale(DefaultConfig().MaxRange + 1)), Unobserved}, // but only to MaxRange
		} {
			if got := e.State(p.at); got != p.want {
				t.Errorf("yaw %.2f: %v is %s, want %s", tc.yaw, p.at, got, p.want)
			}
		}
	}
}

func TestCompareClassifiesClusters(t *testing.T) {
	a, b := geom.Vec2{X: 3.5, Z: 3}, geom.Vec2{X: 6.5, Z: 5}
	t.Run("unchanged", func(t *testing.T) {
		r := compare(t, join(room, crate(a.X, a.Z)), join(room, crate(a.X, a.Z)), nil)
		if len(r.Changes) != 0 || r.Compared < 10000 {
			t.Fatalf("got %d changes over %d compared cells, want none over the whole room", len(r.Changes), r.Compared)
		}
	})
	t.Run("appeared", func(t *testing.T) {
		r := compare(t, room, join(room, crate(a.X, a.Z)), nil)
		if len(r.Changes) != 1 {
			t.Fatalf("got %d changes, want 1: %+v", len(r.Changes), r.Changes)
		}
		ev := r.Changes[0].Event
		near(t, ev, spatial.ChangeAppeared, a)
		if ev.Extent.RadiusM < 0.4 || ev.Extent.RadiusM > 0.7 || !ev.FirstObserved.Equal(t0.Add(time.Hour)) || !ev.LastObserved.After(ev.FirstObserved) {
			t.Fatalf("extent %.2f m over [%v, %v]", ev.Extent.RadiusM, ev.FirstObserved, ev.LastObserved)
		}
	})
	t.Run("disappeared", func(t *testing.T) {
		r := compare(t, join(room, crate(a.X, a.Z)), room, nil)
		if len(r.Changes) != 1 {
			t.Fatalf("got %d changes, want 1: %+v", len(r.Changes), r.Changes)
		}
		near(t, r.Changes[0].Event, spatial.ChangeDisappeared, a)
	})
	t.Run("moved", func(t *testing.T) {
		r := compare(t, join(room, crate(a.X, a.Z)), join(room, crate(b.X, b.Z)), nil)
		if len(r.Changes) != 1 {
			t.Fatalf("got %d changes, want 1: %+v", len(r.Changes), r.Changes)
		}
		ev := r.Changes[0].Event
		near(t, ev, spatial.ChangeMoved, b)
		if planar(*ev.FromM).Dist(a) > 0.1 || *ev.ToM != ev.PositionM {
			t.Fatalf("moved from %v to %v, want from %v to %v", *ev.FromM, *ev.ToM, a, b)
		}
	})
	t.Run("too far to be a move", func(t *testing.T) {
		before, after := survey(t, join(room, crate(a.X, a.Z)), patrol, 8, t0), survey(t, join(room, crate(b.X, b.Z)), patrol, 8, t0.Add(time.Hour))
		for _, e := range []*Evidence{before, after} {
			e.cfg.MoveMaxDistance = 2
		}
		r, err := Compare(before, after, nil)
		if err != nil || len(r.Changes) != 2 {
			t.Fatalf("got %d changes (%v), want an appearance and a disappearance", len(r.Changes), err)
		}
		kinds := map[spatial.ChangeKind]geom.Vec2{}
		for _, c := range r.Changes {
			kinds[c.Event.Kind] = planar(c.Event.PositionM)
		}
		if kinds[spatial.ChangeAppeared].Dist(b) > 0.1 || kinds[spatial.ChangeDisappeared].Dist(a) > 0.1 {
			t.Fatalf("got %v", kinds)
		}
	})
}

// A wall across the room with a 1 m doorway; the leaf comes and goes.
func TestCompareReportsDoorTransitions(t *testing.T) {
	wall := scene{{A: geom.Vec2{X: 5, Z: 0}, B: geom.Vec2{X: 5, Z: 3.5}}, {A: geom.Vec2{X: 5, Z: 4.5}, B: geom.Vec2{X: 5, Z: 8}}}
	leaf := scene{{A: geom.Vec2{X: 5, Z: 3.5}, B: geom.Vec2{X: 5, Z: 4.5}}}
	open, closed := join(room, wall), join(room, wall, leaf)
	mid := geom.Vec2{X: 5, Z: 4}
	// The portal is where an upstream door detector put it: 10 cm off.
	end := [3]float64{5.1, 0, 3.9}
	door := spatial.Portal{
		ID: "door-1", Kind: spatial.PortalDoor, State: spatial.PortalStateUnknown, WidthM: 1, Confidence: 0.8,
		A: spatial.PortalEnd{Floor: "f", PositionM: end}, B: spatial.PortalEnd{Floor: "f", PositionM: end}, Location: spatial.At(geom.Vec3FromArray(end), 0.1),
	}
	opening := door
	opening.ID, opening.Kind = "gap-1", spatial.PortalOpening
	for _, tc := range []struct {
		name          string
		before, after scene
		portals       []spatial.Portal
		kind          spatial.ChangeKind
		subject       string
	}{
		{"closed", open, closed, []spatial.Portal{door}, spatial.ChangeDoorClosed, "door-1"},
		{"opened", closed, open, []spatial.Portal{door}, spatial.ChangeDoorOpened, "door-1"},
		{"no portal", open, closed, nil, spatial.ChangeAppeared, ""},
		{"an opening has no leaf", open, closed, []spatial.Portal{opening}, spatial.ChangeAppeared, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := compare(t, tc.before, tc.after, tc.portals)
			if len(r.Changes) != 1 {
				t.Fatalf("got %d changes, want 1: %+v", len(r.Changes), r.Changes)
			}
			ev := r.Changes[0].Event
			// The location is measured from the changed cells, not copied
			// from the portal.
			if d := planar(ev.PositionM).Dist(mid); ev.Kind != tc.kind || ev.Subject != tc.subject || d > 0.08 {
				t.Fatalf("got %s %q %.2f m from the doorway, want %s %q", ev.Kind, ev.Subject, d, tc.kind, tc.subject)
			}
		})
	}
}

// Two rooms with a solid wall between them. The reference never entered the
// second room, so a crate found there later is new ground, not a change.
func TestUnobservedAreasAreNeverReported(t *testing.T) {
	west, east := box(0, 0, 5, 8), box(5, 0, 10, 8)
	inWest := []geom.Pose2{{X: 1.5, Z: 2}, {X: 3.5, Z: 6, Yaw: 2}}
	inBoth := append([]geom.Pose2{{X: 6.5, Z: 2, Yaw: 1}, {X: 8.5, Z: 6, Yaw: -2}}, inWest...)
	ref := survey(t, join(west, east), inWest, 8, t0)
	cur := survey(t, join(west, east, crate(7.5, 4)), inBoth, 8, t0.Add(time.Hour))
	seen := 0
	for x := 7.0; x < 8; x += 0.05 {
		for z := 3.5; z < 4.5; z += 0.05 {
			if cur.State(geom.Vec2{X: x, Z: z}) == Occupied {
				seen++
			}
		}
	}
	if seen < 4*DefaultConfig().MinClusterCells {
		t.Fatalf("only %d cells of the crate are occupied in the current map: the test proves nothing", seen)
	}
	r, err := Compare(ref, cur, nil)
	if err != nil || len(r.Changes) != 0 || r.Compared == 0 {
		t.Fatalf("got %d changes over %d cells (%v), want none over the west room", len(r.Changes), r.Compared, err)
	}
	// With nothing observed in common there is no verdict at all.
	apart := survey(t, box(6, 0, 10, 8), inBoth[:2], 8, t0.Add(time.Hour))
	if _, err := Compare(survey(t, box(0, 0, 4, 8), inWest, 8, t0), apart, nil); !errors.Is(err, ErrNoOverlap) {
		t.Fatalf("got %v, want ErrNoOverlap", err)
	}
	other, _ := NewEvidence("g", Config{})
	if _, err := Compare(ref, other, nil); !errors.Is(err, ErrIncompatible) {
		t.Fatalf("got %v, want ErrIncompatible", err)
	}
}

// Something that crosses the room during the epoch is seen at a different
// place by every sweep: its cells read both ways and never become evidence.
func TestMoversAreNeverReported(t *testing.T) {
	ref := survey(t, room, patrol, 8, t0)
	cur, _ := NewEvidence("f", Config{})
	rng := rand.New(rand.NewPCG(3, 3))
	for i := range 64 {
		at := t0.Add(time.Hour + time.Duration(i)*time.Second)
		mover := crate(1.5+0.11*float64(i), 4)
		if err := cur.Add(Sweep{Scan: join(room, mover).scan(rng, patrol[i%4], at), Sensor: patrol[i%4], Time: at}); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Compare(ref, cur, nil)
	if err != nil || len(r.Changes) != 0 {
		t.Fatalf("got %+v (%v), want no changes from a mover", r.Changes, err)
	}
	if s := cur.State(geom.Vec2{X: 5, Z: 3.58}); s == Occupied {
		t.Fatalf("a cell on the mover's path is %s", s)
	}
}

// A door leaf on a cell boundary that closes a quarter into the epoch, seen
// from both sides from poses known to 2 cm and half a degree. Its returns
// fall in the cells on either side of the boundary; if each of those counted
// as crossed whenever the return fell in the other, the leaf would read
// Transient. Config.HitMargin is what keeps every stretch of it Occupied.
func TestThinSurfaceOnACellBoundaryIsOccupied(t *testing.T) {
	leaf := scene{{A: geom.Vec2{X: 5, Z: 3.5}, B: geom.Vec2{X: 5, Z: 4.5}}}
	holes := func(cfg Config) (int, *Evidence) {
		e, err := NewEvidence("f", cfg)
		if err != nil {
			t.Fatal(err)
		}
		rng := rand.New(rand.NewPCG(5, 5))
		for i := range 64 {
			at, pose, sc := t0.Add(time.Duration(i)*time.Second), patrol[i%4], room
			if i >= 16 {
				sc = join(room, leaf)
			}
			believed := geom.Pose2{X: pose.X + 0.02*rng.NormFloat64(), Z: pose.Z + 0.02*rng.NormFloat64(), Yaw: pose.Yaw + 0.009*rng.NormFloat64()}
			// The last sweeps come from a device already on its way
			// upstairs: the map's elevation stays that of the floor.
			elev := 0.0
			if i >= 56 {
				elev = 1.2
			}
			if err := e.Add(Sweep{Scan: sc.scan(rng, pose, at), Sensor: believed, Elevation: elev, Time: at}); err != nil {
				t.Fatal(err)
			}
		}
		n := 0
		for z := 3.725; z < 4.3; z += 0.05 {
			occupied := false
			for _, x := range []float64{4.925, 4.975, 5.025, 5.075} {
				occupied = occupied || e.State(geom.Vec2{X: x, Z: z}) == Occupied
			}
			if !occupied {
				n++
			}
		}
		return n, e
	}
	n, e := holes(Config{})
	if n != 0 {
		t.Errorf("the leaf has %d holes", n)
	}
	if e.elev != 0 {
		t.Errorf("elevation %.2f, want that of most sweeps", e.elev)
	}
	// Without the margin the same sweeps leave holes: the test has teeth.
	if n, _ := holes(Config{HitMargin: 1e-9}); n == 0 {
		t.Error("the leaf is whole without a margin: the scene does not exercise it")
	}
}

// A difference that was not reported must stay one: the reference adopts the
// cells of reported changes only.
func TestReferenceKeepsUnreportedDifferences(t *testing.T) {
	a := geom.Vec2{X: 3.5, Z: 3}
	withCrate := survey(t, join(room, crate(a.X, a.Z)), patrol, 8, t0.Add(time.Hour))
	ref := survey(t, room, patrol, 8, t0)
	ref.adopt(withCrate, nil)
	r, err := Compare(ref, withCrate, nil)
	if err != nil || len(r.Changes) != 1 {
		t.Fatalf("got %d changes (%v) after adopting an unreported epoch, want the crate still", len(r.Changes), err)
	}
	near(t, r.Changes[0].Event, spatial.ChangeAppeared, a)
	l := ledger{cfg: ref.cfg}
	_, reported := l.observe(ref, withCrate)
	ref.adopt(withCrate, reported)
	if r, err := Compare(ref, withCrate, nil); err != nil || len(r.Changes) != 0 {
		t.Fatalf("got %d changes (%v) after adopting the reported crate, want none", len(r.Changes), err)
	}
}

func devicePose(p geom.Pose2) spatial.DevicePose {
	return spatial.DevicePose{
		Device: "robot", Floor: "f", FrameID: "site", Location: spatial.At(p.Pos().Lift(0), 0.03), Yaw: p.Yaw,
		Covariance: geom.Mat3{4e-4, 0, 0, 0, 4e-4, 0, 0, 0, 1e-4}, Method: "test", Score: 1,
	}
}

var lidarMount = spatial.Calibration{
	Device: "robot", Sensor: "lidar", Kind: spatial.KindLidar2D, SensorToParent: geom.IdentityPose3(),
	Parent: spatial.ParentDeviceBase, ParentDevice: "robot", Method: "test", Revision: "r1", State: spatial.CalibrationValid,
	TimeOffset: 40 * time.Millisecond,
}

// stream feeds a detector one epoch of the scene and returns what the
// epochs closed on the way reported.
type stream struct {
	t   *testing.T
	d   *Detector
	rng *rand.Rand
	now time.Time
}

func newStream(t *testing.T, cfg Config) *stream {
	t.Helper()
	d, err := NewDetector(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return &stream{t: t, d: d, rng: rand.New(rand.NewPCG(11, 13)), now: t0}
}

// epoch runs the patrol for one Config.Epoch, a sweep a second. The scans
// carry sensor time, 40 ms ahead of site time as the calibration says.
func (s *stream) epoch(sc scene) []Change {
	s.t.Helper()
	var out []Change
	for end := s.now.Add(s.d.cfg.Epoch); s.now.Before(end); s.now = s.now.Add(time.Second) {
		pose := patrol[int(s.now.Sub(t0)/time.Second)%len(patrol)]
		scan := sc.scan(s.rng, pose, s.now.Add(lidarMount.TimeOffset))
		got, err := s.d.AddScan(devicePose(pose), lidarMount, scan, input(scan))
		if err != nil {
			s.t.Fatal(err)
		}
		out = append(out, got...)
	}
	return out
}

func ids(cs []Change) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.Event.ID
	}
	return out
}

func TestDetectorReportsAPersistingChangeOnce(t *testing.T) {
	a, b := geom.Vec2{X: 3.5, Z: 3}, geom.Vec2{X: 6.5, Z: 5}
	s := newStream(t, Config{Epoch: 32 * time.Second})
	withA, withB := join(room, crate(a.X, a.Z)), join(room, crate(b.X, b.Z))

	if got := s.epoch(room); len(got) != 0 {
		t.Fatalf("the first epoch reported %+v", got)
	}
	// Epoch 1 closes when epoch 2 begins: nothing to compare it with.
	if got := s.epoch(withA); len(got) != 0 || s.d.Compared() != 0 || len(s.d.Changes()) != 0 {
		t.Fatalf("the reference epoch reported %+v over %d cells", got, s.d.Compared())
	}
	// Epoch 2 (the crate arrived) closes at the first scan of epoch 3.
	got := s.epoch(withA)
	if len(got) != 1 || s.d.Compared() == 0 {
		t.Fatalf("got %+v over %d cells, want the crate", got, s.d.Compared())
	}
	first := got[0]
	near(t, first.Event, spatial.ChangeAppeared, a)
	if first.Revision != 1 || len(first.Inputs) == 0 || first.Event.FirstObserved.Before(t0.Add(32*time.Second)) || !first.Event.LastObserved.Before(t0.Add(64*time.Second)) {
		t.Fatalf("revision %d, %d inputs, observed [%v, %v]", first.Revision, len(first.Inputs), first.Event.FirstObserved, first.Event.LastObserved)
	}
	// The crate stays: it is the reference now and is not reported again,
	// but the change is still open under its id.
	if got := s.epoch(withA); len(got) != 0 {
		t.Fatalf("a persisting change was reported again: %+v", got)
	}
	if open := s.d.Changes(); len(open) != 1 || !reflect.DeepEqual(open[0], first) {
		t.Fatalf("open changes %+v, want the first report unchanged", open)
	}
	// The crate is taken away (seen at the close of the next epoch): a new
	// event, not a move back onto the spot it came from.
	s.epoch(room)
	gone := s.epoch(room)
	if len(gone) != 1 || gone[0].Event.ID == first.Event.ID {
		t.Fatalf("got %v after %s, want one new event", ids(gone), first.Event.ID)
	}
	near(t, gone[0].Event, spatial.ChangeDisappeared, a)
	// It turns up elsewhere an epoch later: the disappearance becomes a move
	// under the same id, revised.
	s.epoch(withB)
	moved := s.epoch(withB)
	if len(moved) != 1 || moved[0].Event.ID != gone[0].Event.ID || moved[0].Revision != 2 {
		t.Fatalf("got %+v, want %s revised into a move", moved, gone[0].Event.ID)
	}
	near(t, moved[0].Event, spatial.ChangeMoved, b)
	if planar(*moved[0].Event.FromM).Dist(a) > 0.1 || !moved[0].Event.FirstObserved.Equal(gone[0].Event.FirstObserved) {
		t.Fatalf("moved from %v first observed %v", *moved[0].Event.FromM, moved[0].Event.FirstObserved)
	}
	if open := ids(s.d.Changes()); len(open) != 1 || open[0] != moved[0].Event.ID {
		t.Fatalf("open changes %v, want only the move", open)
	}
	// Flush closes the stream; with nothing new there is nothing to report,
	// and a change without evidence for OpenEpochs is closed.
	s.epoch(withB)
	s.epoch(withB)
	if got := s.d.Flush(); len(got) != 0 || len(s.d.Changes()) != 0 || s.d.Flush() != nil {
		t.Fatalf("flush reported %+v, %d still open", got, len(s.d.Changes()))
	}
}

// A mover that stands still for a whole epoch looks like furniture to a
// lidar; its track is what excludes it.
func TestDetectorExcludesTrackedMovers(t *testing.T) {
	at := geom.Vec2{X: 5, Z: 4}
	parked := join(room, crate(at.X, at.Z))
	run := func(tracked bool) []Change {
		s := newStream(t, Config{Epoch: 32 * time.Second})
		s.epoch(room)
		track := spatial.Track{
			ID: "cart-1", Class: "cart", ClassConfidence: 0.9, Floor: "f", Location: spatial.At(at.Lift(0), 0.2),
			State: spatial.TrackConfirmed, FirstSeen: t0, RadiusM: 0.6,
		}
		var out []Change
		for end := s.now.Add(2 * s.d.cfg.Epoch); s.now.Before(end); {
			if track.LastSeen = s.now; tracked {
				if err := s.d.ObserveTrack(track); err != nil {
					t.Fatal(err)
				}
			}
			pose := patrol[int(s.now.Sub(t0)/time.Second)%len(patrol)]
			scan := parked.scan(s.rng, pose, s.now.Add(lidarMount.TimeOffset))
			got, err := s.d.AddScan(devicePose(pose), lidarMount, scan, input(scan))
			if err != nil {
				t.Fatal(err)
			}
			out, s.now = append(out, got...), s.now.Add(time.Second)
		}
		return append(out, s.d.Flush()...)
	}
	if got := run(true); len(got) != 0 {
		t.Fatalf("a tracked mover was reported: %+v", got)
	}
	if got := run(false); len(got) != 1 || got[0].Event.Kind != spatial.ChangeAppeared {
		t.Fatalf("got %+v, want the untracked parked cart reported as appeared", got)
	}
}

func TestDetectorRefusesWhatItCannotUse(t *testing.T) {
	d, err := NewDetector(Config{})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewPCG(1, 1))
	scan := room.scan(rng, patrol[0], t0)
	add := func(p spatial.DevicePose, c spatial.Calibration, s spatial.Scan) error {
		_, err := d.AddScan(p, c, s, input(s))
		return err
	}
	quarantined := lidarMount
	quarantined.State = spatial.CalibrationQuarantined
	lost := devicePose(patrol[0])
	lost.UncertaintyM = 0.5
	stranger := devicePose(patrol[0])
	stranger.Device = "other"
	for name, tc := range map[string]struct {
		err  error
		want error
	}{
		"quarantined calibration": {add(devicePose(patrol[0]), quarantined, scan), ErrUnusable},
		"uncertain pose":          {add(lost, lidarMount, scan), ErrUnusable},
		"another device's pose":   {add(stranger, lidarMount, scan), ErrInvalid},
		"empty scan":              {add(devicePose(patrol[0]), lidarMount, spatial.Scan{Header: scan.Header}), ErrInvalid},
	} {
		if !errors.Is(tc.err, tc.want) {
			t.Errorf("%s: got %v, want %v", name, tc.err, tc.want)
		}
	}
	if len(d.cur) != 0 {
		t.Fatal("a refused scan left evidence behind")
	}
	// Sweeps closer together than ScanInterval are not separate looks.
	for i := range 25 {
		s := room.scan(rng, patrol[0], t0.Add(time.Duration(i)*100*time.Millisecond))
		if err := add(devicePose(patrol[0]), lidarMount, s); err != nil {
			t.Fatal(err)
		}
	}
	if got := d.cur["f"].Sweeps(); got != 5 {
		t.Fatalf("integrated %d of 25 sweeps at 10 Hz, want 5 (one per half second)", got)
	}
	if err := d.ObserveTrack(spatial.Track{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v, want ErrInvalid for an empty track", err)
	}
	if err := d.SetPortals([]spatial.Portal{{}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v, want ErrInvalid for an empty portal", err)
	}
}

func TestDetectorIsDeterministic(t *testing.T) {
	run := func() []Change {
		s := newStream(t, Config{Epoch: 32 * time.Second})
		s.epoch(room)
		s.epoch(join(room, crate(3.5, 3)))
		s.epoch(join(room, crate(3.5, 3), crate(7, 5.5)))
		s.epoch(join(room, crate(7, 5.5)))
		return append(s.epoch(room), s.d.Flush()...)
	}
	if a, b := run(), run(); len(a) == 0 || !reflect.DeepEqual(a, b) {
		t.Fatalf("two runs of one stream differ:\n%+v\n%+v", a, b)
	}
}

func TestChangeComponent(t *testing.T) {
	r := compare(t, room, join(room, crate(3.5, 3)), nil)
	c := r.Changes[0]
	if len(c.Inputs) == 0 || len(c.Inputs) > spatial.MaxInputs {
		t.Fatalf("%d inputs", len(c.Inputs))
	}
	comp, err := c.Component(Publication{
		Scope: spatial.Scope{Tenant: "acme", Site: "hq", Source: world.Source{Device: "gateway", App: "spatial", Service: "changes", Stream: "events"}},
		Epoch: 1, Frame: world.FrameRef{ID: "site", Revision: "r1"}, Timing: world.Timing{ClockDomain: "site"}, TTL: 10 * time.Minute, AppVersion: "test",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := spatial.FromComponent[spatial.ChangeEvent](comp)
	if err != nil || !reflect.DeepEqual(got, c.Event) {
		t.Fatalf("round trip: %+v (%v), want %+v", got, err, c.Event)
	}
	if p := comp.Provenance; p.Origin != world.Inferred || p.ModelID != "wendy.spatial.changes" || len(p.Inputs) != len(c.Inputs) || comp.Version.Sequence != 1 {
		t.Fatalf("provenance %+v version %+v", p, comp.Version)
	}
}

func TestConfig(t *testing.T) {
	d, err := NewDetector(Config{Epoch: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	want := DefaultConfig()
	want.Epoch = time.Minute
	if d.cfg != want {
		t.Fatalf("got %+v, want the defaults with a one-minute epoch", d.cfg)
	}
	for name, cfg := range map[string]Config{
		"negative cell":         {CellSize: -1},
		"ratios out of order":   {FreeRatio: 0.5, OccupiedRatio: 0.4},
		"epoch under one sweep": {Epoch: time.Millisecond},
		"non-finite tolerance":  {Tolerance: math.NaN()},
		"no observations":       {MinObservations: -1},
	} {
		if _, err := NewDetector(cfg); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: got %v, want ErrConfig", name, err)
		}
		if _, err := NewEvidence("f", cfg); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: got %v, want ErrConfig", name, err)
		}
	}
	if _, err := NewEvidence("", Config{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("got %v, want ErrInvalid for an empty floor", err)
	}
}
