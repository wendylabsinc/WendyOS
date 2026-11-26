package anchors

import (
	"fmt"
	"math"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/spatialsim"
)

// Design targets (spatial intelligence design, feature 6).
const (
	// targetCorrectedM bounds every anchor's site position error after the
	// map correction.
	targetCorrectedM = 0.10
	// targetResolvedM bounds the position error of every anchor a newly
	// arrived device resolves.
	targetResolvedM = 0.15
)

// Scenario constants: what the test does, not what it must reach.
const (
	// placeReachM is how close a keyframe must pass to a named point for the
	// point to be anchored: an operator marks places the robot drove past.
	placeReachM = 1.5
	// hereEvery places an anchor at the robot itself on every n-th keyframe.
	hereEvery = 20
	// minResolved is the fewest anchors the late device must find each time
	// it looks: there are more than that many around the lab it drives in.
	minResolved = 3
)

func seeds(t *testing.T) []uint64 {
	env := os.Getenv("SPATIAL_SEEDS")
	if env == "" {
		return []uint64{1}
	}
	var out []uint64
	for _, f := range strings.Split(env, ",") {
		v, err := strconv.ParseUint(strings.TrimSpace(f), 10, 64)
		if err != nil {
			t.Fatalf("SPATIAL_SEEDS: %v", err)
		}
		out = append(out, v)
	}
	return out
}

func reopen(t *testing.T, s *Store, dir string) *Store {
	t.Helper()
	before := s.Anchors()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if after := s.Anchors(); !reflect.DeepEqual(before, after) {
		t.Fatalf("restart changed the anchors: %d before, %d after", len(before), len(after))
	}
	return s
}

// TestAcceptanceOffice is the design scenario: anchors are placed against a
// drifted map, the store restarts, the map is corrected, the store restarts
// again, and a device that arrives later finds the anchors in its own frame.
func TestAcceptanceOffice(t *testing.T) {
	for _, seed := range seeds(t) {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) { acceptOffice(t, seed) })
	}
}

func acceptOffice(t *testing.T, seed uint64) {
	site, err := spatialsim.Office(seed, spatialsim.Options{})
	if err != nil {
		t.Fatal(err)
	}
	// Upstream stand-ins, all the sanctioned Reference at its default errors:
	// lidar calibrations (registration), corrected device poses and floors
	// (collaborative SLAM, multifloor).
	ref, err := site.Reference(spatialsim.DefaultReferenceConfig())
	if err != nil {
		t.Fatal(err)
	}
	// A second, independent realisation of the same errors stands in for the
	// operator who marks a place as seen from the robot: the place's offset
	// from a keyframe is read against these poses (2 cm, 0.5°), so that it
	// shares no error with the corrected map and nothing cancels.
	marks := spatialsim.DefaultReferenceConfig()
	marks.Seed = seed + 7919
	marker, err := site.Reference(marks)
	if err != nil {
		t.Fatal(err)
	}
	truth := site.Truth() // scoring only

	const mapper = spatialsim.Rover2
	mapperLidar, ok := ref.Calibration(mapper, spatialsim.SensorLidar, site.At(0))
	if !ok {
		t.Fatal("no lidar calibration for the mapper")
	}
	// The uncorrected map: the mapper's keyframes in its own frame, each
	// step from odometry and scan matching with no loop closed (a Resolver
	// with an unbounded window stands in for the SLAM front end), hooked to
	// the site at the first keyframe and drifting from there.
	front, err := NewResolver(Config{Window: 1 << 20}, mapperLidar)
	if err != nil {
		t.Fatal(err)
	}
	for m := range site.Stream(site.At(0), site.At(mapWindow), spatialsim.Filter{Devices: []spatial.DeviceID{mapper}, Kinds: []spatial.SensorKind{spatial.KindLidar2D, spatial.KindOdometry}}) {
		if err := front.Ingest(m); err != nil {
			t.Fatal(err)
		}
	}
	drifted := front.Keyframes()
	corrected := make([]spatial.Keyframe, len(drifted))
	for i := range drifted {
		pose, ok := ref.DevicePose(mapper, drifted[i].Time)
		if !ok {
			t.Fatal("no reference pose for the mapper")
		}
		drifted[i].Floor = pose.Floor
		corrected[i] = drifted[i]
		corrected[i].Local, corrected[i].Covariance = pose.Pose2(), pose.Covariance
	}
	first, _ := ref.DevicePose(mapper, drifted[0].Time)
	odometryToSite := first.Pose2().Compose(drifted[0].Local.Inverse())

	dir := t.TempDir()
	store, err := Open(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyKeyframes(odometryToSite, drifted); err != nil {
		t.Fatal(err)
	}

	// Place the anchors against the drifted map. want holds the true site
	// position of each, for scoring.
	want := map[string]geom.Vec2{}
	for _, p := range truth.Points() {
		if p.Floor != spatialsim.FloorGround {
			continue
		}
		target := geom.Pose2{X: p.Position.X, Z: p.Position.Z}
		at, reach := spatial.KeyframeRef{}, math.Inf(1)
		for _, kf := range drifted {
			seen, _ := marker.DevicePose(mapper, kf.Time)
			if d := seen.Pose2().Pos().Dist(target.Pos()); d < reach {
				at, reach = spatial.KeyframeRef{Device: mapper, Sequence: kf.Sequence, Offset: seen.Pose2().Between(target)}, d
			}
		}
		if reach > placeReachM {
			continue
		}
		a, err := store.Place(p.Name, at, p.Position.Y)
		if err != nil {
			t.Fatal(err)
		}
		want[a.ID] = target.Pos()
	}
	for i := hereEvery / 2; i < len(drifted); i += hereEvery {
		kf := drifted[i]
		a, err := store.Place(fmt.Sprintf("here-%d", kf.Sequence), spatial.KeyframeRef{Device: mapper, Sequence: kf.Sequence}, 0)
		if err != nil {
			t.Fatal(err)
		}
		state, _ := truth.Device(mapper, kf.Time)
		want[a.ID] = state.Pose.Pos()
	}
	score := func(anchors []spatial.Anchor) (worst, rmse float64) {
		for _, a := range anchors {
			e := a.Pose2().Pos().Dist(want[a.ID])
			worst, rmse = math.Max(worst, e), rmse+e*e
		}
		return worst, math.Sqrt(rmse / float64(len(anchors)))
	}
	driftWorst, _ := score(store.Anchors())

	// Restart, then the map correction arrives.
	store = reopen(t, store, dir)
	moved, err := store.ApplyKeyframes(geom.Pose2{}, corrected)
	if err != nil {
		t.Fatal(err)
	}
	if len(moved.Moved) != len(want) || len(moved.Unbound) != 0 {
		t.Fatalf("correction moved %d of %d anchors, %d unbound", len(moved.Moved), len(want), len(moved.Unbound))
	}
	for _, a := range moved.Moved {
		if a.Revision != "2" {
			t.Errorf("%s has revision %s after one correction, want 2", a.ID, a.Revision)
		}
	}
	store = reopen(t, store, dir)
	defer store.Close()
	worst, rmse := score(store.Anchors())
	for _, a := range store.Anchors() {
		t.Logf("anchor %s %-18s at (%.2f, %.2f) error %.3f m, 1σ %.3f m", a.ID, a.Name, a.PositionM[0], a.PositionM[2], a.Pose2().Pos().Dist(want[a.ID]), a.UncertaintyM)
	}
	t.Logf("%d anchors on %d keyframes; worst error before the correction %.2f m", len(want), len(drifted), driftWorst)
	t.Logf("METRIC anchor_error_after_correction_max=%.3f m (target < %.2f)", worst, targetCorrectedM)
	t.Logf("METRIC anchor_error_after_correction_rmse=%.3f m (target < %.2f)", rmse, targetCorrectedM)
	if worst >= targetCorrectedM {
		t.Errorf("worst anchor error after the correction %.3f m, target < %.2f", worst, targetCorrectedM)
	}

	// A device that arrives later, in its own frame. Its lidar calibration is
	// the Reference stand-in for registration (mount 1 cm and 0.5°, clock
	// offset 3 ms). It looks for the anchors from several places along its
	// route. An anchor it finds is scored where the device has it relative to
	// its scanner at the last keyframe, against where it is: the frame of the
	// device's own scans. The same error in the base frame is reported too;
	// it adds the device's mount yaw error times the distance to the anchor
	// (0.5° is 7 cm at 8 m), which is registration's to answer for and goes
	// to zero as the device drives up to the anchor.
	const late = spatialsim.Latecomer
	lateLidar, ok := ref.Calibration(late, spatialsim.SensorLidar, site.At(spatialsim.OfficeLateStart))
	if !ok {
		t.Fatal("no lidar calibration for the late device")
	}
	resolver, err := NewResolver(Config{}, lateLidar)
	if err != nil {
		t.Fatal(err)
	}
	filter := spatialsim.Filter{Devices: []spatial.DeviceID{late}, Kinds: []spatial.SensorKind{spatial.KindLidar2D, spatial.KindOdometry}}
	resolved, worst, rmse, worstBase := 0, 0.0, 0.0, 0.0
	for i, look := range lateLooks {
		from := spatialsim.OfficeLateStart
		if i > 0 {
			from += lateLooks[i-1]
		}
		for m := range site.Stream(site.At(from), site.At(spatialsim.OfficeLateStart+look), filter) {
			if err := resolver.Ingest(m); err != nil {
				t.Fatal(err)
			}
		}
		found, err := resolver.Resolve(store.Anchors())
		if err != nil {
			t.Fatal(err)
		}
		frames := resolver.Keyframes()
		now := frames[len(frames)-1]
		state, _ := truth.Device(late, now.Time)
		mount, _ := truth.LidarMount(late, now.Time)
		here := 0
		for _, r := range found {
			if r.Err != nil {
				t.Logf("after %v: refused %v", look, r.Err)
				continue
			}
			at := r.Anchor.Pose2().Pos()
			got := now.Local.Compose(now.SensorToBase).InverseApply(at)
			e := got.Dist(state.Pose.Compose(mount).InverseApply(want[r.ID]))
			base := now.Local.InverseApply(at).Dist(state.Pose.InverseApply(want[r.ID]))
			t.Logf("after %v: resolved %s %-16s %.1f m away, error %.3f m (base frame %.3f m, 1σ %.3f m), score %.2f, %d scans",
				look, r.ID, r.Anchor.Name, got.Norm(), e, base, r.Anchor.UncertaintyM, r.Score, r.Matched)
			here, worst, rmse, worstBase = here+1, math.Max(worst, e), rmse+e*e, math.Max(worstBase, base)
		}
		if here < minResolved {
			t.Errorf("after %v the late device resolved %d of %d anchors, want at least %d", look, here, len(found), minResolved)
		}
		resolved += here
	}
	if resolved == 0 {
		t.Fatal("the late device resolved no anchor")
	}
	rmse = math.Sqrt(rmse / float64(resolved))
	t.Logf("METRIC anchors_resolved_by_new_device=%d resolutions in %d looks (target >= %d per look)", resolved, len(lateLooks), minResolved)
	t.Logf("METRIC anchor_error_new_device_max=%.3f m (target < %.2f)", worst, targetResolvedM)
	t.Logf("METRIC anchor_error_new_device_rmse=%.3f m (target < %.2f)", rmse, targetResolvedM)
	t.Logf("METRIC anchor_error_new_device_base_frame_max=%.3f m (reported, not a target: includes the device's mount yaw error)", worstBase)
	if worst >= targetResolvedM {
		t.Errorf("worst error of an anchor resolved by the late device %.3f m, target < %.2f", worst, targetResolvedM)
	}
}
