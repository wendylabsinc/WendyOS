package spatialsim

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
)

// TestCalibrationTargetShows checks what intrinsic calibration needs: every
// fixed camera sees the whole target many times, at varied depth and tilt,
// and the corners are where the true camera puts them.
func TestCalibrationTargetShows(t *testing.T) {
	const (
		minViews      = 40                 // whole-target frames per camera
		minTiltSpread = 30 * math.Pi / 180 // between the least and most tilted view
		minDepthRange = 1.0                // metres between the nearest and farthest view
	)
	s := officeSite(t)
	tr := s.Truth()
	target := s.Target()
	tol := sigmas * s.Config().PixelSigma * math.Sqrt2
	shown := map[spatial.DeviceID]bool{}
	for _, sh := range s.TargetShows() {
		shown[sh.Camera] = true
		views := 0
		tiltLo, tiltHi, depthLo, depthHi := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
		for at := sh.Start - time.Second; at < sh.Start+sh.Duration+time.Second; at += 200 * time.Millisecond {
			frame, _ := s.CameraFrame(sh.Camera, s.At(at))
			board, showing := tr.TargetPose(s.At(at))
			in, cam, _ := tr.Camera(sh.Camera, s.At(at))
			corners := 0
			for _, det := range frame.Detections {
				if det.Kind != spatial.DetectTargetCorner {
					continue
				}
				corners++
				p, ok := target.Corner(det.Index)
				if !showing || !ok || det.ID != target.ID {
					t.Fatalf("%s reports target corner %d of %q at %v, outside a show", sh.Camera, det.Index, det.ID, at)
				}
				if want, _ := in.ProjectWorld(cam, board.Apply(p)); want.Dist(det.Pixel) > tol {
					t.Fatalf("%s: corner %d at %v, the true camera projects it to %v", sh.Camera, det.Index, det.Pixel, want)
				}
			}
			if corners == 0 {
				continue
			}
			if corners != target.Count() {
				t.Fatalf("%s sees %d of %d corners at %v; the target is reported whole or not at all", sh.Camera, corners, target.Count(), at)
			}
			views++
			rel := cam.Between(board)
			tilt := math.Acos(math.Min(1, rel.Rotate(geom.Vec3{Z: 1}).Z))
			tiltLo, tiltHi = math.Min(tiltLo, tilt), math.Max(tiltHi, tilt)
			depthLo, depthHi = math.Min(depthLo, rel.T.Z), math.Max(depthHi, rel.T.Z)
		}
		if views < minViews || tiltHi-tiltLo < minTiltSpread || depthHi-depthLo < minDepthRange {
			t.Errorf("%s: %d whole views, tilt %.0f° to %.0f°, depth %.1f to %.1f m", sh.Camera, views, tiltLo*180/math.Pi, tiltHi*180/math.Pi, depthLo, depthHi)
		}
	}
	for _, m := range s.Manifests() {
		if m.Kind() == spatial.KindCamera && m.Attachment.Mode == spatial.AttachFixed && !shown[m.Device] {
			t.Errorf("fixed camera %s is never shown the target", m.Device)
		}
	}
	none := mustSite(t, Office, 1, Options{TargetShows: []TargetShow{}})
	if _, ok := none.Truth().TargetPose(none.At(20 * time.Second)); ok || len(none.TargetShows()) != 0 {
		t.Error("an empty show list must show the target to nobody")
	}
	if _, err := Office(1, Options{TargetShows: []TargetShow{{Camera: "mic-1", Start: time.Second, Duration: time.Second}}}); err == nil {
		t.Error("a show for a device without a camera was accepted")
	}
}

// TestCamerasObserveWhatFeaturesNeed walks the fixed and mobile cameras
// through the pre-change patrol and checks the observations that
// calibration, localization, registration, tracking and semantics rely on.
func TestCamerasObserveWhatFeaturesNeed(t *testing.T) {
	const (
		minMarkerPoints = 150 // robot-marker points per fixed camera
		minMarkerSpread = 0.4 // of the image width and of its height that those points span
		minTagFraction  = 0.9 // of a fixed camera's frames that show a wall tag
		minHandheld     = 0.8 // of the handheld's frames that show a wall tag
		minMutual       = 100 // marker points each ground rover's camera sees on other robots
	)
	s := officeSite(t)
	type tally struct {
		frames, tagFrames, markers int
		carriers                   map[string]bool
		classes                    map[string]int
		lo, hi                     geom.Pixel // bounding box of the marker points
	}
	tallies := map[spatial.DeviceID]*tally{}
	for _, m := range s.Manifests() {
		if m.Kind() != spatial.KindCamera {
			continue
		}
		tl := &tally{carriers: map[string]bool{}, classes: map[string]int{}, lo: geom.Pixel{U: math.Inf(1), V: math.Inf(1)}, hi: geom.Pixel{U: math.Inf(-1), V: math.Inf(-1)}}
		tallies[m.Device] = tl
		start := max(s.byID[m.Device].start, 0)
		for at := start; at < start+150*time.Second; at += 400 * time.Millisecond {
			frame, ok := s.CameraFrame(m.Device, s.At(at))
			if !ok || frame.Validate() != nil || frame.PixelSigma != s.Config().PixelSigma {
				t.Fatalf("%s frame at %v is missing or invalid", m.Device, at)
			}
			tl.frames++
			tag := false
			for _, det := range frame.Detections {
				switch {
				case det.Kind == spatial.DetectObject:
					tl.classes[det.Class]++
					if det.ID != "" || det.BBox == nil {
						t.Fatalf("%s: object detections carry a box and no identity: %+v", m.Device, det)
					}
				case strings.HasPrefix(det.ID, "marker-"):
					tl.markers++
					tl.carriers[det.ID] = true
					tl.lo = geom.Pixel{U: math.Min(tl.lo.U, det.Pixel.U), V: math.Min(tl.lo.V, det.Pixel.V)}
					tl.hi = geom.Pixel{U: math.Max(tl.hi.U, det.Pixel.U), V: math.Max(tl.hi.V, det.Pixel.V)}
				case strings.HasPrefix(det.ID, "tag-"):
					tag = true
				}
			}
			if tag {
				tl.tagFrames++
			}
		}
	}
	for dev, tl := range tallies {
		fixed := s.byID[dev].route == nil
		tagged := float64(tl.tagFrames) / float64(tl.frames)
		// A marker that crosses only a corner of the image leaves the camera
		// pose weakly constrained however many points it yields.
		wide, high := (tl.hi.U-tl.lo.U)/float64(mobileLens.Width), (tl.hi.V-tl.lo.V)/float64(mobileLens.Height)
		switch {
		case fixed && (tl.markers < minMarkerPoints || tagged < minTagFraction):
			t.Errorf("fixed camera %s: %d marker points, wall tags in %.2f of frames", dev, tl.markers, tagged)
		case fixed && (wide < minMarkerSpread || high < minMarkerSpread):
			t.Errorf("fixed camera %s: marker points span %.2f of the image width and %.2f of its height; want %.1f of each", dev, wide, high, minMarkerSpread)
		case dev == Handheld && tagged < minHandheld:
			t.Errorf("the handheld sees wall tags in only %.2f of its frames", tagged)
		case (dev == Rover1 || dev == Rover2 || dev == Rover3) && (tl.markers < minMutual || len(tl.carriers) < 2):
			t.Errorf("%s's camera sees %d marker points on %d other robots", dev, tl.markers, len(tl.carriers))
		}
		if fixed {
			t.Logf("%-15s %5d marker points from %d robots over %.2f x %.2f of the image", dev, tl.markers, len(tl.carriers), wide, high)
		}
	}
	// The lobby and the corridors are crossed by more than one robot.
	for _, dev := range []spatial.DeviceID{CamLobby, CamCorridorWest, CamCorridorEast} {
		if n := len(tallies[dev].carriers); n < 2 {
			t.Errorf("%s sees the markers of %d robots; want at least 2", dev, n)
		}
	}
	for _, c := range []struct {
		dev   spatial.DeviceID
		class string
		min   int
	}{
		{CamLobby, "person", 300}, {CamYard, "cart", 150}, {CamYard, "person", 150},
		{CamKitchen, "stove", 150}, {CamKitchen, "fridge", 150}, {CamKitchen, "table", 150},
		{CamCorridorWest, "robot", 150}, {Rover1, "person", 10}, {Ranger, "cart", 10},
		// The labs are told from the offices by their benches.
		{CamLab, "bench", 150}, {CamUpper, "bench", 150}, {CamUpper, "shelf", 150}, {CamUpper, "robot", 50},
	} {
		if got := tallies[c.dev].classes[c.class]; got < c.min {
			t.Errorf("%s reports class %q %d times; want at least %d", c.dev, c.class, got, c.min)
		}
	}

	// The ranger's lidar returns on the actors its camera sees in the yard.
	const bodyTolerance = 0.1
	onActor := 0
	for at := time.Duration(0); at < 50*time.Second; at += 500 * time.Millisecond {
		scan, _ := s.Scan(Ranger, s.At(at))
		pose := sensorPose(t, s, Ranger, s.At(at))
		actors := s.Truth().Actors(s.At(at))
		for i := range scan.Ranges {
			if angle, r, state := scan.Beam(i); state == spatial.BeamHit {
				hit := pose.Apply(geom.Heading(angle).Scale(r))
				for _, a := range actors {
					if a.Floor == FloorYard && hit.Dist(a.Shape.Centre) < a.Shape.Bound()+bodyTolerance {
						onActor++
					}
				}
			}
		}
	}
	if onActor < 200 {
		t.Errorf("only %d lidar returns from the yard's person and cart", onActor)
	}
}

// TestObjectDetectionRates checks that the miss, confusion and
// false-positive knobs do what they say, and that a noiseless foot pixel
// lands on the object's true floor position.
func TestObjectDetectionRates(t *testing.T) {
	const frames = 400
	objects := func(cfg Config) ([][]spatial.Detection, *Site) {
		s := mustSite(t, SingleRoom, 9, Options{Config: &cfg})
		var out [][]spatial.Detection
		for i := 0; i < frames; i++ {
			frame, _ := s.CameraFrame("cam-1", s.At(time.Duration(i)*200*time.Millisecond))
			var dets []spatial.Detection
			for _, det := range frame.Detections {
				if det.Kind == spatial.DetectObject {
					dets = append(dets, det)
				}
			}
			out = append(out, dets)
		}
		return out, s
	}
	total := func(fs [][]spatial.Detection) (n int) {
		for _, f := range fs {
			n += len(f)
		}
		return n
	}
	clean := DefaultConfig().Noiseless()
	base, site := objects(clean)
	if total(base) < 2*frames {
		t.Fatalf("only %d object detections in %d frames", total(base), frames)
	}
	// Foot pixel back to the floor: the desk, the barrel and the robot.
	truth := map[string][]geom.Vec2{}
	for i, f := range base {
		at := site.At(time.Duration(i) * 200 * time.Millisecond)
		in, cam, _ := site.Truth().Camera("cam-1", at)
		clear(truth)
		for _, o := range site.Truth().Objects(at) {
			truth[o.Class] = append(truth[o.Class], o.Shape.Centre)
		}
		st, _ := site.Truth().Device(Rover1, at)
		truth["robot"] = append(truth["robot"], st.Pose.Pos())
		for _, det := range f {
			origin, dir, err := in.PixelRay(cam, det.Pixel)
			if err != nil {
				t.Fatal(err)
			}
			foot, ok := geom.RayPlaneY(origin, dir, 0)
			best := math.Inf(1)
			for _, p := range truth[det.Class] {
				best = math.Min(best, foot.XZ().Dist(p))
			}
			if !ok || best > 1e-6 {
				t.Fatalf("frame %d: %s foot pixel lands %.4f m from the object", i, det.Class, best)
			}
			if det.Pixel.U < det.BBox.Min.U || det.Pixel.U > det.BBox.Max.U || det.Confidence < 0.5 {
				t.Fatalf("frame %d: foot pixel outside the box or low confidence: %+v", i, det)
			}
		}
	}

	miss := clean
	miss.ObjectMissRate = 0.3
	missed, _ := objects(miss)
	if got := 1 - float64(total(missed))/float64(total(base)); math.Abs(got-0.3) > 0.06 {
		t.Errorf("miss rate %.3f; configured 0.3", got)
	}

	confuse := clean
	confuse.ClassConfusionRate = 0.3
	confused, _ := objects(confuse)
	wrong := 0
	for i := range base {
		if len(confused[i]) != len(base[i]) {
			t.Fatal("class confusion changed the number of detections")
		}
		for k := range base[i] {
			if confused[i][k].Class != base[i][k].Class {
				wrong++
			}
		}
	}
	if got := float64(wrong) / float64(total(base)); math.Abs(got-0.3) > 0.06 {
		t.Errorf("confusion rate %.3f; configured 0.3", got)
	}

	spurious := clean
	spurious.FalsePositiveRate = 1
	extra, _ := objects(spurious)
	for i := range base {
		if len(extra[i]) != len(base[i])+1 {
			t.Fatalf("frame %d has %d detections with a certain false positive, %d without", i, len(extra[i]), len(base[i]))
		}
	}
}

// TestActorsPassClose checks what makes identity switches possible: in each
// pair of actors whose paths cross, the bodies come within a few decimetres
// of touching, yet never touch.
func TestActorsPassClose(t *testing.T) {
	const (
		maxGap = 0.35 // metres between the closest surfaces at the closest approach
		step   = 20 * time.Millisecond
	)
	s := officeSite(t)
	tr := s.Truth()
	// gap is the distance between two footprints: circle to circle, or circle
	// to box.
	gap := func(a, b Shape) float64 {
		if a.Radius == 0 {
			a, b = b, a
		}
		if b.Radius > 0 {
			return a.Centre.Dist(b.Centre) - a.Radius - b.Radius
		}
		l := geom.Pose2{X: b.Centre.X, Z: b.Centre.Z, Yaw: b.Yaw}.InverseApply(a.Centre)
		return math.Hypot(math.Max(math.Abs(l.X)-b.Half.X, 0), math.Max(math.Abs(l.Z)-b.Half.Z, 0)) - a.Radius
	}
	for _, pair := range [][2]string{{"person-1", "person-2"}, {"person-3", "cart-1"}} {
		closest := math.Inf(1)
		for at := time.Duration(0); at < 2*time.Minute; at += step {
			shapes := map[string]Shape{}
			for _, a := range tr.Actors(s.At(at)) {
				shapes[a.ID] = a.Shape
			}
			closest = math.Min(closest, gap(shapes[pair[0]], shapes[pair[1]]))
		}
		t.Logf("%s and %s come within %.2f m", pair[0], pair[1], closest)
		if closest <= 0 || closest > maxGap {
			t.Errorf("%s and %s come within %.2f m of each other; want a near miss under %.2f m", pair[0], pair[1], closest, maxGap)
		}
	}
}

// TestUwbLineOfSightCoverage checks where UWB positioning has what it needs:
// three beacons in true line of sight for most rounds of the robots in the
// corridor and the lab, and a radio verdict on every range.
func TestUwbLineOfSightCoverage(t *testing.T) {
	const (
		minRounds = 0.6 // of a device's rounds with three clear beacons
		needed    = 3
	)
	s := officeSite(t)
	tr := s.Truth()
	beacons := map[spatial.DeviceID]bool{}
	for _, b := range s.Survey().Beacons {
		beacons[b.ID] = true
	}
	for dev, want := range map[spatial.DeviceID]float64{Rover2: minRounds, Rover3: minRounds, Latecomer: 0.95} {
		rounds, good := 0, 0
		for at := OfficeLateStart; at < OfficeLateStart+3*time.Minute; at += time.Second {
			clear := 0
			for _, r := range s.Ranges(dev, s.At(at)) {
				truth, ok := tr.RangeLineOfSight(dev, r.Peer, s.At(at))
				if r.LineOfSight == nil || !ok || *r.LineOfSight != truth {
					t.Fatalf("%s to %s at %v: line-of-sight flag %v, truth %v", dev, r.Peer, at, r.LineOfSight, truth)
				}
				if beacons[r.Peer] && truth {
					clear++
				}
			}
			rounds++
			if clear >= needed {
				good++
			}
		}
		if frac := float64(good) / float64(rounds); frac < want {
			t.Errorf("%s has %d clear beacons in %.2f of its rounds; want %.2f", dev, needed, frac, want)
		}
	}
}
