package anchors

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// Hand-built worlds for the unit tests, as wall segments in (x, z).
func chain(pts ...geom.Vec2) []geom.Segment {
	var out []geom.Segment
	for i := 1; i < len(pts); i++ {
		out = append(out, geom.Segment{A: pts[i-1], B: pts[i]})
	}
	return out
}

func v(x, z float64) geom.Vec2 { return geom.Vec2{X: x, Z: z} }

// hall is an L, 12 x 5 m with a 7 x 4 m leg, and a pillar: no two places in
// it look alike. (A rectangle with small features does not do: half a turn
// maps its outline onto itself, and from most places that outline is nearly
// all a scan holds.) square is 6 x 6 m and empty: it looks the same after a
// quarter turn.
var (
	hall = append(
		chain(v(0, 0), v(12, 0), v(12, 5), v(7, 5), v(7, 9), v(0, 9), v(0, 0)),
		chain(v(9.5, 1), v(10.5, 1), v(10.5, 2), v(9.5, 2), v(9.5, 1))...)
	square = chain(v(0, 0), v(6, 0), v(6, 6), v(0, 6), v(0, 0))
)

var epoch = time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)

const (
	mapDevice  spatial.DeviceID = "mapper"
	lateDevice spatial.DeviceID = "late"
	lidar      spatial.SensorID = "lidar"
	ground     spatial.FloorID  = "ground"
)

// cast is the noise-free sweep of a scanner at the pose: 360 beams, beam i at
// yaw AngleMin + i·AngleIncrement in the scanner's frame.
func cast(walls []geom.Segment, device spatial.DeviceID, sensor geom.Pose2, at time.Time) spatial.Scan {
	s := spatial.Scan{
		Header:   spatial.Header{Device: device, Sensor: lidar, Time: at},
		AngleMin: -math.Pi, AngleIncrement: 2 * math.Pi / 360, RangeMin: 0.1, RangeMax: 20, RangeSigma: 0.01,
		Ranges: make([]float64, 360),
	}
	for i := range s.Ranges {
		best := math.Inf(1)
		for _, w := range walls {
			if d, ok := geom.RaySegment(sensor.Pos(), geom.Heading(sensor.Yaw+s.Angle(i)), w); ok && d < best {
				best = d
			}
		}
		s.Ranges[i] = spatial.RangeNoReturn
		if best >= s.RangeMin && best <= s.RangeMax {
			s.Ranges[i] = best
		}
	}
	return s
}

// keyframe is a keyframe of a device whose base is truly at base in the
// walls' frame and which believes itself at local.
func keyframe(t *testing.T, walls []geom.Segment, device spatial.DeviceID, sequence uint64, base, local, mount geom.Pose2) spatial.Keyframe {
	t.Helper()
	scan, err := spatial.Compact(cast(walls, device, base.Compose(mount), epoch))
	if err != nil {
		t.Fatal(err)
	}
	c := 1e-4
	return spatial.Keyframe{
		Device: device, Sequence: sequence, Floor: ground, Time: epoch.Add(time.Duration(sequence) * time.Second),
		Local: local, Covariance: geom.Mat3{c, 0, 0, 0, c, 0, 0, 0, c}, SensorToBase: mount, Scan: scan,
	}
}

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func near(t *testing.T, what string, got, want geom.Pose2, metres, radians float64) {
	t.Helper()
	if d, turn := got.Pos().Dist(want.Pos()), math.Abs(geom.AngleDiff(got.Yaw, want.Yaw)); d > metres || turn > radians {
		t.Errorf("%s: got %+v, want %+v (%.3f m, %.4f rad off)", what, got, want, d, turn)
	}
}

// A device heading +Z (yaw +π/2) has its +X ahead and its +Z, its right, at
// site −X: seen from above positive yaw turns clockwise.
func TestAnchorFollowsItsKeyframe(t *testing.T) {
	s := open(t, t.TempDir())
	mount := geom.Pose2{X: 0.15}
	at := geom.Pose2{X: 1, Z: 2, Yaw: math.Pi / 2}
	if _, err := s.ApplyKeyframes(geom.Pose2{}, []spatial.Keyframe{keyframe(t, hall, mapDevice, 5, at, at, mount)}); err != nil {
		t.Fatal(err)
	}
	ahead, err := s.Place("ahead", spatial.KeyframeRef{Device: mapDevice, Sequence: 5, Offset: geom.Pose2{X: 1}}, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	right, err := s.Place("right", spatial.KeyframeRef{Device: mapDevice, Sequence: 5, Offset: geom.Pose2{Z: 1, Yaw: 0.25}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "ahead", ahead.Pose2(), geom.Pose2{X: 1, Z: 3, Yaw: math.Pi / 2}, 1e-9, 1e-9)
	near(t, "right", right.Pose2(), geom.Pose2{X: 0, Z: 2, Yaw: math.Pi/2 + 0.25}, 1e-9, 1e-9)
	if ahead.ID != "anchor-1" || right.ID != "anchor-2" || ahead.Revision != "1" || ahead.Floor != ground || ahead.PositionM[1] != 0.5 {
		t.Errorf("placed %s revision %s and %s", ahead.ID, ahead.Revision, right.ID)
	}

	// The pose graph moves the keyframe: both anchors follow, one revision on.
	moved := keyframe(t, hall, mapDevice, 5, at, geom.Pose2{X: 2, Z: 2}, mount)
	c, err := s.ApplyKeyframes(geom.Pose2{}, []spatial.Keyframe{moved})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Moved) != 2 || len(c.Unbound) != 0 || c.Moved[0].Revision != "2" || c.Moved[0].PositionM[1] != 0.5 {
		t.Fatalf("correction moved %d, unbound %d", len(c.Moved), len(c.Unbound))
	}
	near(t, "ahead after the correction", c.Moved[0].Pose2(), geom.Pose2{X: 3, Z: 2}, 1e-9, 1e-9)
	near(t, "right after the correction", c.Moved[1].Pose2(), geom.Pose2{X: 2, Z: 3, Yaw: 0.25}, 1e-9, 1e-9)
	if c.Moved[0].UncertaintyM < 0.01 || c.Moved[0].UncertaintyM > 0.05 {
		t.Errorf("uncertainty %.4f m from a keyframe known to 1 cm and 0.01 rad, 1 m away", c.Moved[0].UncertaintyM)
	}

	// The same keyframe again changes nothing.
	if c, err = s.ApplyKeyframes(geom.Pose2{}, []spatial.Keyframe{moved}); err != nil || len(c.Moved) != 0 {
		t.Fatalf("repeated correction moved %d anchors, %v", len(c.Moved), err)
	}
	// The same keyframe in a frame that sits elsewhere in the site.
	if c, err = s.ApplyKeyframes(geom.Pose2{X: 10, Yaw: math.Pi}, []spatial.Keyframe{moved}); err != nil || len(c.Moved) != 2 {
		t.Fatalf("correction through a frame transform moved %d, %v", len(c.Moved), err)
	}
	near(t, "ahead through a frame transform", c.Moved[0].Pose2(), geom.Pose2{X: 7, Z: -2, Yaw: math.Pi}, 1e-9, 1e-9)
	if got, _ := s.Get(ahead.ID); got.Revision != "3" {
		t.Errorf("revision %s after two corrections, want 3", got.Revision)
	}
}

func TestStoreSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	mount := geom.Pose2{X: 0.1, Yaw: 0.1}
	var frames []spatial.Keyframe
	for i := range 4 {
		p := geom.Pose2{X: 1.5 + 0.5*float64(i), Z: 5}
		frames = append(frames, keyframe(t, hall, mapDevice, uint64(i), p, p, mount))
	}
	if _, err := s.ApplyKeyframes(geom.Pose2{}, frames); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b", "c"} {
		if _, err := s.Place(name, spatial.KeyframeRef{Device: mapDevice, Sequence: 1, Offset: geom.Pose2{X: 0.5, Z: 1}}, 0); err != nil {
			t.Fatal(err)
		}
	}
	updated, err := s.Update("anchor-2", "b moved", spatial.KeyframeRef{Device: mapDevice, Sequence: 3, Offset: geom.Pose2{Z: -1}}, 1)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != "2" || updated.Name != "b moved" || updated.Keyframes[0].Sequence != 3 {
		t.Errorf("updated to revision %s, name %q, keyframes %+v", updated.Revision, updated.Name, updated.Keyframes)
	}
	if err := s.Delete("anchor-3"); err != nil {
		t.Fatal(err)
	}
	before := s.Anchors()
	if len(before) != 2 || before[0].ID != "anchor-1" || before[1].ID != "anchor-2" {
		t.Fatalf("%d anchors before the restart", len(before))
	}
	if n := len(before[0].Keyframes); n != 3 || before[0].Keyframes[0].Sequence != 1 {
		t.Errorf("anchor-1 is bound to %d keyframes, first %+v; want 3 with the placement first", n, before[0].Keyframes[0])
	}
	s.Close()

	s = open(t, dir)
	if after := s.Anchors(); !reflect.DeepEqual(before, after) {
		t.Fatalf("restart changed the anchors: %d before, %d after", len(before), len(after))
	}
	// The keyframes are the map's to keep: none after a restart.
	if _, err := s.Place("d", spatial.KeyframeRef{Device: mapDevice, Sequence: 1}, 0); !errors.Is(err, ErrNoKeyframe) {
		t.Errorf("placing without keyframes: %v", err)
	}
	c, err := s.ApplyKeyframes(geom.Pose2{}, nil)
	if err != nil || len(c.Unbound) != 2 || len(c.Moved) != 0 {
		t.Errorf("correction without keyframes: moved %d, unbound %v, %v", len(c.Moved), c.Unbound, err)
	}
	// Fed the same keyframes, nothing moves and no revision is spent; a
	// deleted identifier is not handed out again.
	if c, err = s.ApplyKeyframes(geom.Pose2{}, frames); err != nil || len(c.Moved) != 0 || len(c.Unbound) != 0 {
		t.Errorf("the same keyframes after a restart: moved %d, unbound %v, %v", len(c.Moved), c.Unbound, err)
	}
	d, err := s.Place("d", spatial.KeyframeRef{Device: mapDevice, Sequence: 1}, 0)
	if err != nil || d.ID != "anchor-4" {
		t.Errorf("placed %q after the restart, %v; want anchor-4", d.ID, err)
	}

	// With the keyframe it was placed against gone from the map, an anchor
	// follows another it is bound to: all three are bound to keyframe 2.
	s.Close()
	s = open(t, dir)
	shifted := frames[2]
	shifted.Local.Z += 2
	if c, err = s.ApplyKeyframes(geom.Pose2{}, []spatial.Keyframe{shifted}); err != nil || len(c.Moved) != 3 || c.Moved[0].ID != "anchor-1" {
		t.Fatalf("correction of a neighbour keyframe moved %d, %v", len(c.Moved), err)
	}
	near(t, "anchor-1 on its neighbour", c.Moved[0].Pose2(), geom.Pose2{X: 2.5, Z: 8}, 1e-9, 1e-9)

	for _, err := range []error{s.Delete("anchor-9"), func() error { _, err := s.Update("anchor-9", "", spatial.KeyframeRef{}, 0); return err }()} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("unknown anchor: %v", err)
		}
	}
	s.Close()
	if err := s.Delete("anchor-1"); !errors.Is(err, os.ErrClosed) {
		t.Errorf("delete on a closed store: %v", err)
	}
}

// Keyframes that disagree about an anchor show in its uncertainty.
func TestDisagreeingKeyframesWidenTheUncertainty(t *testing.T) {
	s := open(t, t.TempDir())
	a, b := geom.Pose2{X: 2, Z: 5}, geom.Pose2{X: 2.5, Z: 5}
	frames := []spatial.Keyframe{keyframe(t, hall, mapDevice, 0, a, a, geom.Pose2{}), keyframe(t, hall, mapDevice, 1, b, b, geom.Pose2{})}
	if _, err := s.ApplyKeyframes(geom.Pose2{}, frames); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Place("p", spatial.KeyframeRef{Device: mapDevice, Sequence: 0}, 0); err != nil {
		t.Fatal(err)
	}
	frames[1].Local.X += 0.4
	c, err := s.ApplyKeyframes(geom.Pose2{}, frames[1:])
	if err != nil {
		t.Fatal(err)
	}
	// The pose stays with the first binding, so nothing moved; ask the store.
	got, _ := s.Get("anchor-1")
	if len(c.Moved) != 0 || got.UncertaintyM > 0.05 {
		t.Fatalf("correction moved %d, uncertainty %.3f m", len(c.Moved), got.UncertaintyM)
	}
	frames[0].Local.Z += 0.01
	if c, err = s.ApplyKeyframes(geom.Pose2{}, frames[:1]); err != nil || len(c.Moved) != 1 || c.Moved[0].UncertaintyM < 0.39 {
		t.Errorf("anchor between keyframes 0.4 m apart: moved %d, %v", len(c.Moved), err)
	}
}

func TestBind(t *testing.T) {
	s := open(t, t.TempDir())
	a, b := geom.Pose2{X: 2, Z: 5, Yaw: 1}, geom.Pose2{X: 9, Z: 2}
	upstairs := keyframe(t, hall, mapDevice, 2, a, geom.Pose2{X: 6, Z: 4}, geom.Pose2{})
	upstairs.Floor = "upper"
	if _, err := s.ApplyKeyframes(geom.Pose2{}, []spatial.Keyframe{keyframe(t, hall, mapDevice, 0, a, a, geom.Pose2{}), keyframe(t, hall, mapDevice, 1, b, b, geom.Pose2{}), upstairs}); err != nil {
		t.Fatal(err)
	}
	want := geom.Pose2{X: 7.5, Z: 3, Yaw: 0.3}
	ref, err := s.Bind(ground, want)
	if err != nil || ref.Sequence != 1 {
		t.Fatalf("bound to %+v, %v; want keyframe 1, the nearest on the floor", ref, err)
	}
	placed, err := s.Place("p", ref, 0)
	if err != nil {
		t.Fatal(err)
	}
	near(t, "placed at a site pose", placed.Pose2(), want, 1e-9, 1e-9)
	if _, err := s.Bind(ground, geom.Pose2{X: 30, Z: 30}); !errors.Is(err, ErrNoKeyframe) {
		t.Errorf("binding far from every keyframe: %v", err)
	}
	if _, err := s.Bind("cellar", want); !errors.Is(err, ErrNoKeyframe) {
		t.Errorf("binding on a floor without keyframes: %v", err)
	}
	floorless := upstairs
	floorless.Sequence, floorless.Floor = 3, ""
	if _, err := s.ApplyKeyframes(geom.Pose2{}, []spatial.Keyframe{floorless}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Place("q", spatial.KeyframeRef{Device: mapDevice, Sequence: 3}, 0); !errors.Is(err, ErrNoKeyframe) {
		t.Errorf("placing against a keyframe without a floor: %v", err)
	}
}

func TestJournalToleratesATornTail(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	at := geom.Pose2{X: 2, Z: 5}
	frames := []spatial.Keyframe{keyframe(t, hall, mapDevice, 0, at, at, geom.Pose2{})}
	if _, err := s.ApplyKeyframes(geom.Pose2{}, frames); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if _, err := s.Place(name, spatial.KeyframeRef{Device: mapDevice, Sequence: 0}, 0); err != nil {
			t.Fatal(err)
		}
	}
	both := s.Anchors()
	s.Close()
	path := filepath.Join(dir, journalFile)
	whole, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reopened := func(data []byte) (*Store, error) {
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		s, err := Open(dir, Config{})
		if err == nil {
			t.Cleanup(func() { s.Close() })
		}
		return s, err
	}
	half := len(whole) / 2 // each record is half the file
	for name, tail := range map[string][]byte{
		"record cut short":      whole[half : len(whole)-7],
		"header cut short":      whole[half : half+5],
		"zero-filled tail":      make([]byte, 64),
		"last record corrupted": append(append([]byte{}, whole[half:len(whole)-1]...), whole[len(whole)-1]^0xff),
	} {
		s, err := reopened(append(append([]byte{}, whole[:half]...), tail...))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if got := s.Anchors(); len(got) != 1 || !reflect.DeepEqual(got[0], both[0]) {
			t.Errorf("%s: %d anchors survive, want the first", name, len(got))
		}
		// The tail is gone: the next record lands behind an intact one.
		if _, err := s.ApplyKeyframes(geom.Pose2{}, frames); err != nil {
			t.Fatal(err)
		}
		if a, err := s.Place("c", spatial.KeyframeRef{Device: mapDevice, Sequence: 0}, 0); err != nil || a.ID != "anchor-2" {
			t.Errorf("%s: placed %q after recovery, %v", name, a.ID, err)
		}
		s.Close()
		if s, err = Open(dir, Config{}); err != nil || len(s.Anchors()) != 2 {
			t.Errorf("%s: reopening after recovery: %v", name, err)
		}
		if err == nil {
			s.Close()
		}
	}
	if s, err := reopened(whole); err != nil || !reflect.DeepEqual(s.Anchors(), both) {
		t.Errorf("intact journal: %v", err)
	}
	// Damage with records behind it is not a torn tail.
	damaged := append([]byte{}, whole...)
	damaged[half/2] ^= 0xff
	if _, err := reopened(damaged); !errors.Is(err, ErrCorrupt) {
		t.Errorf("damaged first record: %v, want ErrCorrupt", err)
	}
}

func TestSignatureRoundTrip(t *testing.T) {
	scan, err := spatial.Compact(cast(hall, mapDevice, geom.Pose2{X: 2, Z: 5}, epoch))
	if err != nil {
		t.Fatal(err)
	}
	three := []patch{{geom.Pose2{X: 1, Z: -2, Yaw: 0.5}, scan}, {geom.Pose2{X: 0.5}, scan}, {geom.Pose2{Yaw: -3}, scan}}
	b, err := encodeSignature(three)
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeSignature(b)
	if err != nil || !reflect.DeepEqual(got, three) {
		t.Fatalf("three scans do not come back whole: %v", err)
	}
	// More scans than fit whole are thinned, every scan to the same share.
	many := make([]patch, 8)
	for i := range many {
		many[i] = three[0]
	}
	if b, err = encodeSignature(many); err != nil || len(b) > spatial.MaxSignatureBytes {
		t.Fatalf("eight scans: %d bytes, %v", len(b), err)
	}
	if got, err = decodeSignature(b); err != nil || len(got) != 8 || len(got[7].scan.RangesMM) != 180 ||
		got[7].scan.AngleIncrement != 2*scan.AngleIncrement || got[7].scan.RangesMM[10] != scan.RangesMM[20] {
		t.Fatalf("eight scans decode to %d, %v", len(got), err)
	}
	for name, bad := range map[string][]byte{"empty": nil, "version": append([]byte{9}, b[1:]...), "truncated": b[:len(b)-3], "trailing": append(append([]byte{}, b...), 0)} {
		if _, err := decodeSignature(bad); !errors.Is(err, ErrSignature) {
			t.Errorf("%s signature: %v", name, err)
		}
	}
}

// placed returns a store's anchor at the offset from one keyframe of a
// mapper standing at the poses given, in the walls' own frame.
func placed(t *testing.T, walls []geom.Segment, offset geom.Pose2, at ...geom.Pose2) spatial.Anchor {
	t.Helper()
	s := open(t, t.TempDir())
	mount := geom.Pose2{X: 0.15, Z: 0.05, Yaw: 0.1}
	var frames []spatial.Keyframe
	for i, p := range at {
		frames = append(frames, keyframe(t, walls, mapDevice, uint64(i), p, p, mount))
	}
	if _, err := s.ApplyKeyframes(geom.Pose2{}, frames); err != nil {
		t.Fatal(err)
	}
	a, err := s.Place("place", spatial.KeyframeRef{Device: mapDevice, Sequence: uint64(len(at) / 2), Offset: offset}, 0.2)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// A device that has never seen the map, in a frame of its own, with another
// scanner mount, standing elsewhere in the hall.
func TestResolveInTheDevicesFrame(t *testing.T) {
	anchor := placed(t, hall, geom.Pose2{X: 1, Z: -0.5, Yaw: 0.3}, geom.Pose2{X: 1.5, Z: 5, Yaw: 0.2}, geom.Pose2{X: 2, Z: 5.1, Yaw: 0.2}, geom.Pose2{X: 2.5, Z: 5.2, Yaw: 0.2})
	ownToWalls := geom.Pose2{X: 3, Z: -2, Yaw: 0.7}
	mount := geom.Pose2{X: -0.1, Yaw: -0.2}
	var frames []spatial.Keyframe
	for i, p := range []geom.Pose2{{X: 6, Z: 3, Yaw: 2}, {X: 6.4, Z: 3.3, Yaw: 2.5}, {X: 6.6, Z: 3.8, Yaw: 2.5}} {
		frames = append(frames, keyframe(t, hall, lateDevice, uint64(i), p, ownToWalls.Between(p), mount))
	}
	found, err := Resolve(Config{}, frames, []spatial.Anchor{anchor, {ID: "torn", Signature: []byte{1}}})
	if err != nil || len(found) != 2 {
		t.Fatal(err)
	}
	r := found[0]
	if r.Err != nil {
		t.Fatal(r.Err)
	}
	near(t, "anchor in the device frame", r.Anchor.Pose2(), ownToWalls.Between(anchor.Pose2()), 0.03, 0.01)
	if r.ID != anchor.ID || r.Matched != 3 || r.Score < 0.8 || r.Anchor.PositionM[1] != 0.2 || r.Anchor.Name != "place" ||
		r.Anchor.UncertaintyM <= 0 || r.Anchor.UncertaintyM > 0.05 || r.Covariance[8] <= 0 {
		t.Errorf("resolution: %d scans, score %.2f, 1σ %.3f m", r.Matched, r.Score, r.Anchor.UncertaintyM)
	}
	if !errors.Is(found[1].Err, ErrSignature) || found[1].ID != "torn" {
		t.Errorf("torn signature: %v", found[1].Err)
	}

	if _, err := Resolve(Config{}, nil, []spatial.Anchor{anchor}); !errors.Is(err, ErrInsufficient) {
		t.Errorf("no keyframes: %v", err)
	}
	if _, err := Resolve(Config{MaxContradiction: -1}, frames, nil); !errors.Is(err, ErrConfig) {
		t.Errorf("negative threshold: %v", err)
	}
}

func TestResolveRefuses(t *testing.T) {
	p := geom.Pose2{X: 2.2, Z: 2.6, Yaw: 0.4}
	inSquare := []spatial.Keyframe{keyframe(t, square, lateDevice, 0, p, p, geom.Pose2{})}
	q := geom.Pose2{X: 6, Z: 3, Yaw: 2}
	inHall := []spatial.Keyframe{keyframe(t, hall, lateDevice, 0, q, q, geom.Pose2{})}

	// A square room fits itself four ways.
	found, err := Resolve(Config{}, inSquare, []spatial.Anchor{placed(t, square, geom.Pose2{X: 1}, geom.Pose2{X: 3.5, Z: 1.8})})
	if err != nil || !errors.Is(found[0].Err, ErrAmbiguous) || found[0].Score != 0 || found[0].Matched != 0 {
		t.Errorf("anchor in a square room: %v, %v; want ErrAmbiguous", found[0].Err, err)
	}
	// A place the device has not seen is not found, in either direction.
	if found, err = Resolve(Config{}, inHall, []spatial.Anchor{placed(t, square, geom.Pose2{X: 1}, geom.Pose2{X: 3.5, Z: 1.8})}); err != nil || found[0].Err == nil {
		t.Errorf("the square's anchor resolved in the hall with score %.2f, %v", found[0].Score, err)
	}
	if found, err = Resolve(Config{}, inSquare, []spatial.Anchor{placed(t, hall, geom.Pose2{X: 1}, geom.Pose2{X: 2, Z: 5})}); err != nil || found[0].Err == nil {
		t.Errorf("the hall's anchor resolved in the square with score %.2f, %v", found[0].Score, err)
	}
}

// A scan with a wall across floor the device saw empty contradicts the map;
// the same scan where it was taken, or out in space the device never saw,
// does not.
func TestContradiction(t *testing.T) {
	g, err := grid.New(grid.Config{})
	if err != nil {
		t.Fatal(err)
	}
	at := geom.Pose2{X: 6, Z: 3}
	scan := cast(hall, lateDevice, at, epoch)
	if err := g.InsertScan(at, scan); err != nil {
		t.Fatal(err)
	}
	field, err := g.Field()
	if err != nil {
		t.Fatal(err)
	}
	seen := deviceMap{g, field}
	if c := seen.contradiction(scan, at, 0.2); c > 0.01 {
		t.Errorf("a scan contradicts the map made from it: %.3f", c)
	}
	if c := seen.contradiction(scan, geom.Pose2{X: 8.5, Z: 2}, 0.2); c < 0.2 {
		t.Errorf("a scan shifted 2.7 m contradicts %.3f of itself, want a clear share", c)
	}
	if c := seen.contradiction(scan, geom.Pose2{X: 40, Z: 40}, 0.2); c != 0 {
		t.Errorf("a scan in unseen space contradicts %.3f", c)
	}
}

// calibration is a lidar calibration as registration publishes it.
func calibration(device spatial.DeviceID, mount geom.Pose2, offset time.Duration) spatial.Calibration {
	return spatial.Calibration{
		Device: device, Sensor: lidar, Kind: spatial.KindLidar2D, SensorToParent: geom.Pose3FromPose2(mount, 0.3),
		Parent: spatial.ParentDeviceBase, ParentDevice: device, TranslationSigmaM: 0.01, RotationSigmaRad: 0.01,
		Method: "test", Revision: "1", State: spatial.CalibrationValid, TimeOffset: offset,
	}
}

// drive feeds a Resolver a device that goes 3 m along +X through the hall,
// turns a quarter on the spot toward +Z and goes on 2 m: odometry at 20 Hz in a frame
// of its own, with a heading error of turnBias per radian turned; scans at
// 10 Hz, stamped on a clock that runs offset ahead. It returns the true base
// pose at each instant fed.
func drive(t *testing.T, r *Resolver, mount geom.Pose2, offset time.Duration, turnBias float64) map[time.Time]geom.Pose2 {
	t.Helper()
	const step = 50 * time.Millisecond
	pose := geom.Pose2{X: 3, Z: 2.5}
	odom := geom.Pose2{X: -7, Z: 1, Yaw: 2} // where the device believes it starts
	truth := map[time.Time]geom.Pose2{}
	for i := range 160 {
		at := epoch.Add(time.Duration(i) * step)
		var move geom.Pose2
		switch {
		case i == 0:
		case i <= 60 || i > 81:
			move.X = 0.05
		default:
			move.Yaw = math.Pi / 2 / 21
		}
		pose = pose.Compose(move)
		move.Yaw *= 1 + turnBias
		odom = odom.Compose(move)
		truth[at] = pose
		feed := []spatial.Measurement{
			spatial.Odometry{Header: spatial.Header{Device: lateDevice, Sensor: "odom", Time: at}, Pose: odom, Noise: spatial.OdometryNoise{TransPerMetre: 0.02, YawPerMetre: 0.002, YawPerRadian: 0.01}},
			spatial.Odometry{Header: spatial.Header{Device: "other", Sensor: "odom", Time: at}, Pose: geom.Pose2{X: 99}},
			spatial.Imu{Header: spatial.Header{Device: lateDevice, Sensor: "imu", Time: at}},
		}
		if i%2 == 0 {
			feed = append(feed, cast(hall, lateDevice, pose.Compose(mount), at.Add(offset)))
		}
		for _, m := range feed {
			if err := r.Ingest(m); err != nil {
				t.Fatal(err)
			}
		}
	}
	return truth
}

func TestResolverKeyframes(t *testing.T) {
	mount := geom.Pose2{X: 0.12, Z: -0.04, Yaw: 0.15}
	for _, offset := range []time.Duration{40 * time.Millisecond, -40 * time.Millisecond} {
		r, err := NewResolver(Config{Window: 1 << 10}, calibration(lateDevice, mount, offset))
		if err != nil {
			t.Fatal(err)
		}
		// Odometry that turns 5 % too far: 0.08 rad over the quarter turn.
		truth := drive(t, r, mount, offset, 0.05)
		frames := r.Keyframes()
		if len(frames) < 12 || len(frames) > 16 {
			t.Fatalf("%d keyframes over 5 m and a quarter turn, want one per 0.5 m or 0.5 rad", len(frames))
		}
		first, ok := truth[frames[0].Time]
		if !ok {
			t.Fatalf("keyframe time %v is not a site instant: the clock offset was not removed", frames[0].Time)
		}
		for i, kf := range frames {
			if err := kf.Validate(); err != nil {
				t.Fatal(err)
			}
			if kf.Sequence != uint64(i) || kf.Device != lateDevice || kf.Floor != "" {
				t.Errorf("keyframe %d is %s/%d on floor %q", i, kf.Device, kf.Sequence, kf.Floor)
			}
			near(t, "mount", kf.SensorToBase, mount, 1e-9, 1e-9)
			// Relative to the first keyframe, each is where the device
			// truly went: the scans removed the odometry's turn error, which
			// would put the last keyframe 0.16 m off.
			near(t, "keyframe "+kf.Time.Sub(epoch).String(), frames[0].Local.Between(kf.Local), first.Between(truth[kf.Time]), 0.04, 0.01)
		}
		last := frames[len(frames)-1]
		if last.Covariance[0] <= 0 || last.Covariance[8] <= 0 {
			t.Errorf("last keyframe carries no uncertainty: %v", last.Covariance)
		}

		// The anchor of a mapper that was here before, found from the window.
		anchor := placed(t, hall, geom.Pose2{X: 1, Z: -0.5, Yaw: 0.3}, geom.Pose2{X: 1.5, Z: 5, Yaw: 0.2}, geom.Pose2{X: 2, Z: 5.1, Yaw: 0.2}, geom.Pose2{X: 2.5, Z: 5.2, Yaw: 0.2})
		found, err := r.Resolve([]spatial.Anchor{anchor})
		if err != nil || found[0].Err != nil {
			t.Fatal(err, found[0].Err)
		}
		got := last.Local.Between(found[0].Anchor.Pose2())
		want := truth[last.Time].Between(anchor.Pose2())
		near(t, "anchor seen from the device", got, want, 0.05, 0.02)
		// 0.01 rad of mount yaw at this distance, on top of the match.
		if sigma, arm := found[0].Anchor.UncertaintyM, 0.01*got.Pos().Norm(); sigma < arm || sigma > arm+0.03 {
			t.Errorf("uncertainty %.3f m at %.1f m with a mount known to 0.01 rad", sigma, got.Pos().Norm())
		}
	}
}

func TestResolverWindowAndInput(t *testing.T) {
	mount := geom.Pose2{X: 0.1}
	r, err := NewResolver(Config{Window: 3}, calibration(lateDevice, mount, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Resolve(nil); !errors.Is(err, ErrInsufficient) {
		t.Errorf("resolve before any keyframe: %v", err)
	}
	// A scan before any odometry places nothing.
	if err := r.Ingest(cast(hall, lateDevice, geom.Pose2{X: 3, Z: 2.5}, epoch.Add(-time.Second))); err != nil || len(r.Keyframes()) != 0 {
		t.Errorf("scan without odometry: %v, %d keyframes", err, len(r.Keyframes()))
	}
	drive(t, r, mount, 0, 0)
	frames := r.Keyframes()
	if len(frames) != 3 || frames[2].Sequence < 11 || frames[0].Sequence != frames[2].Sequence-2 {
		t.Errorf("window of 3 holds %d keyframes ending at %d", len(frames), frames[len(frames)-1].Sequence)
	}
	stale := spatial.Odometry{Header: spatial.Header{Device: lateDevice, Sensor: "odom", Time: epoch}}
	if err := r.Ingest(stale); err == nil {
		t.Error("odometry from the past was accepted")
	}
	if err := r.Ingest(spatial.Scan{Header: spatial.Header{Device: lateDevice, Sensor: lidar, Time: epoch}}); err == nil {
		t.Error("an invalid scan was accepted")
	}

	fixed := calibration(lateDevice, mount, 0)
	fixed.Parent, fixed.ParentDevice, fixed.Location = spatial.ParentSite, "", &spatial.Location{PositionM: [3]float64{0.1, 0.3, 0}}
	camera := calibration(lateDevice, mount, 0)
	camera.Kind = spatial.KindUwb
	for name, c := range map[string]spatial.Calibration{"site-fixed": fixed, "not a lidar": camera} {
		if _, err := NewResolver(Config{}, c); !errors.Is(err, ErrConfig) {
			t.Errorf("%s calibration: %v", name, err)
		}
	}
	if _, err := NewResolver(Config{}, spatial.Calibration{}); err == nil {
		t.Error("an invalid calibration was accepted")
	}
}

func TestConfig(t *testing.T) {
	for name, c := range map[string]Config{
		"negative radius":   {BindRadius: -1},
		"infinite shift":    {MinShiftM: math.Inf(1)},
		"negative window":   {Window: -1},
		"too many bindings": {MaxBindings: spatial.MaxAnchorBindings + 1},
		"bad match":         {Match: DefaultConfig().Match, MaxContradiction: math.NaN()},
	} {
		if _, err := Open(t.TempDir(), c); !errors.Is(err, ErrConfig) {
			t.Errorf("%s: %v", name, err)
		}
	}
	got, err := Config{}.withDefaults()
	if want := DefaultConfig(); err != nil || got.Window != want.Window || got.MaxBindings != want.MaxBindings || got.Match.MinScore != want.Match.MinScore {
		t.Errorf("zero config: %+v, %v", got, err)
	}
}

func TestComponent(t *testing.T) {
	anchor := placed(t, hall, geom.Pose2{X: 1}, geom.Pose2{X: 2, Z: 5})
	inputs := make([]world.Input, spatial.MaxInputs+5)
	for i := range inputs {
		inputs[i] = world.Input{ID: spatial.InputID(cast(hall, mapDevice, geom.Pose2{X: 2, Z: 5}, epoch.Add(time.Duration(i)*time.Second))), Origin: world.Measured}
	}
	c, err := Component(spatial.Scope{Tenant: "t", Site: "s", Source: world.Source{Device: "gateway", App: "wendy", Service: "spatial", Stream: "anchors"}}, world.Version{Epoch: 1, Sequence: 2},
		world.FrameRef{ID: "site", Revision: "1"}, world.Timing{SampleTime: epoch, ClockDomain: "test"}, time.Hour, "test", anchor, inputs)
	if err != nil {
		t.Fatal(err)
	}
	// One stream of scans reduces to its earliest and its latest.
	if p := c.Provenance; p.Origin != world.Inferred || p.ModelID != "wendy.spatial.anchors" || p.ModelVersion != ModelVersion || len(p.Inputs) != 2 {
		t.Errorf("provenance: origin %s, model %s %s, %d inputs", p.Origin, p.ModelID, p.ModelVersion, len(p.Inputs))
	}
	back, err := spatial.FromComponent[spatial.Anchor](c)
	if err != nil || !reflect.DeepEqual(back, anchor) {
		t.Errorf("anchor does not come back from its component: %v", err)
	}
}
