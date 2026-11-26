package changes

import (
	"math"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/geom"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/spatialsim"
)

// The design targets of feature 11, on the standard Office scenario with
// default noise.
const (
	targetPrecision = 0.9
	targetRecall    = 0.9
	targetLocationM = 0.3
	// matchGateM is how far a reported event may be from a scheduled one of
	// the same kind and floor and still be scored as that event. It is wider
	// than the location target on purpose: a detection 0.5 m off counts as
	// found and then fails targetLocationM, instead of hiding as a miss.
	matchGateM = 1.0
	// doorSigmaM is the error of the door locations fed to the detector.
	doorSigmaM = 0.10
	// actorReachM is how close to a person's or cart's body an unexplained
	// event must be to be blamed on them.
	actorReachM = 0.5
)

// acceptanceSeeds reads SPATIAL_SEEDS (comma-separated unsigned integers);
// the default is a single seed.
func acceptanceSeeds(t *testing.T) []uint64 {
	env := os.Getenv("SPATIAL_SEEDS")
	if env == "" {
		return []uint64{1}
	}
	var seeds []uint64
	for _, f := range strings.Split(env, ",") {
		v, err := strconv.ParseUint(strings.TrimSpace(f), 10, 64)
		if err != nil {
			t.Fatalf("SPATIAL_SEEDS: %v", err)
		}
		seeds = append(seeds, v)
	}
	return seeds
}

// doorStandIn is the door map the detector is given. Door detection belongs
// to the semantic feature and spatialsim.Reference has no stand-in for it, so
// this is a labelled one: every portal of the site as it stood at the epoch,
// each displaced by a seeded error of doorSigmaM per axis (two map cells, the
// localization target; the semantic feature states no door accuracy of its
// own) and with its state withheld, so nothing about which doors toggle, or
// when, reaches the detector. Door events are located from the changed cells,
// not from these positions.
func doorStandIn(s *spatialsim.Site) []spatial.Portal {
	rng := rand.New(rand.NewPCG(s.Seed(), 0xd005))
	portals := s.Truth().Portals(s.At(0))
	for i := range portals {
		p := &portals[i]
		dx, dz := doorSigmaM*rng.NormFloat64(), doorSigmaM*rng.NormFloat64()
		for _, at := range []*[3]float64{&p.PositionM, &p.A.PositionM, &p.B.PositionM} {
			at[0], at[2] = at[0]+dx, at[2]+dz
		}
		p.State, p.Confidence = spatial.PortalStateUnknown, 0.8
	}
	return portals
}

// phase is an independent tally of what the lidars saw before the scheduled
// changes, while they happened and after them, used only to score.
type phase map[spatial.FloorID]*Evidence

func (p phase) add(t *testing.T, floor spatial.FloorID, sw Sweep) {
	if p[floor] == nil {
		e, err := NewEvidence(floor, Config{})
		if err != nil {
			t.Fatal(err)
		}
		p[floor] = e
	}
	if err := p[floor].Add(sw); err != nil {
		t.Fatal(err)
	}
}

// states lists the state of every cell within radius of centre.
func (p phase) states(floor spatial.FloorID, centre geom.Vec2, radius float64) []State {
	e := p[floor]
	var out []State
	const cell = 0.05
	for dz := -radius; dz <= radius; dz += cell {
		for dx := -radius; dx <= radius; dx += cell {
			if at := (geom.Vec2{X: dx, Z: dz}); at.Norm() <= radius {
				s := Unobserved
				if e != nil {
					s = e.State(centre.Add(at))
				}
				out = append(out, s)
			}
		}
	}
	return out
}

type officeRun struct {
	site                 *spatialsim.Site
	reported             []spatial.ChangeEvent // every event ever reported, in its last revision
	before, during, late phase
	compared             int
}

// runOffice streams the lidars of the Office scenario over [from, until)
// through a Detector with the default Config. Device poses and lidar mounts are the sanctioned
// stand-ins of spatialsim.Reference at its default accuracy; door locations
// are doorStandIn. Nothing else reaches the detector.
func runOffice(t *testing.T, seed uint64, opt spatialsim.Options, from, until time.Duration) officeRun {
	t.Helper()
	site, err := spatialsim.Office(seed, opt)
	if err != nil {
		t.Fatal(err)
	}
	cfg := spatialsim.DefaultReferenceConfig()
	cfg.Seed = seed
	ref, err := site.Reference(cfg)
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDetector(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetPortals(doorStandIn(site)); err != nil {
		t.Fatal(err)
	}
	run := officeRun{site: site, before: phase{}, during: phase{}, late: phase{}}
	latest := map[string]spatial.ChangeEvent{}
	record := func(cs []Change) {
		for _, c := range cs {
			if err := c.Event.Validate(); err != nil {
				t.Fatalf("%+v: %v", c.Event, err)
			}
			latest[c.Event.ID] = c.Event
		}
		run.compared = max(run.compared, d.Compared())
	}
	lastScored := map[spatial.DeviceID]time.Time{}
	for m := range site.Stream(site.At(from), site.At(until), spatialsim.Filter{Kinds: []spatial.SensorKind{spatial.KindLidar2D}}) {
		scan, ok := m.(spatial.Scan)
		if !ok {
			continue
		}
		// A sensor clock may run behind site time, so a device's very first
		// sweep can carry a time from before it started: the reference has
		// no calibration or pose for it yet, and it is no evidence.
		calib, ok := ref.Calibration(scan.Device, scan.Sensor, scan.Time)
		if !ok {
			continue
		}
		at := scan.Time.Add(-calib.TimeOffset)
		pose, ok := ref.DevicePose(scan.Device, at)
		if !ok {
			continue
		}
		cs, err := d.AddScan(pose, calib, scan, site.Input(scan))
		if err != nil {
			t.Fatal(err)
		}
		record(cs)
		// The scoring tallies take sweeps as often as the detector does.
		if last, ok := lastScored[scan.Device]; ok && at.Sub(last) < DefaultConfig().ScanInterval {
			continue
		}
		lastScored[scan.Device] = at
		mount, _, _ := calib.SensorToParent.Planar()
		p := run.late
		switch since := at.Sub(site.Epoch()); {
		case since < spatialsim.OfficeChangesFrom:
			p = run.before
		case since < spatialsim.OfficeChangesUntil:
			p = run.during
		}
		p.add(t, pose.Floor, Sweep{Scan: scan, Sensor: pose.Pose2().Compose(mount), Elevation: pose.PositionM[1], Time: at})
	}
	record(d.Flush())
	for _, ev := range latest {
		run.reported = append(run.reported, ev)
	}
	slices.SortFunc(run.reported, func(a, b spatial.ChangeEvent) int { return strings.Compare(a.ID, b.ID) })
	return run
}

// everSeen reports whether any cell of the event's extent was observed in
// any phase of the run.
func (r officeRun) everSeen(ev spatial.ChangeEvent) bool {
	for _, p := range []phase{r.before, r.during, r.late} {
		for _, s := range p.states(ev.Floor, planar(ev.PositionM), ev.Extent.RadiusM+0.05) {
			if s != Unobserved {
				return true
			}
		}
	}
	return false
}

// blamed names the person or cart whose body came within actorReachM of the
// event during the run, or "".
func (r officeRun) blamed(ev spatial.ChangeEvent, until time.Duration) string {
	tr := r.site.Truth()
	for at := time.Duration(0); at < until; at += 500 * time.Millisecond {
		for _, a := range tr.Actors(r.site.At(at)) {
			if a.Floor == ev.Floor && a.Shape.Centre.Dist(planar(ev.PositionM)) <= a.Shape.Bound()+ev.Extent.RadiusM+actorReachM {
				return a.ID
			}
		}
	}
	return ""
}

// The detector's epochs start at its first scan. Starting the stream at 0
// puts every scheduled change at the head of an epoch; starting it at
// officeStarts[1] puts them in the middle of one and splits the patrol
// passes differently, which is how a deployment meets them.
func TestAcceptanceOfficeChanges(t *testing.T) {
	t.Parallel()
	for _, seed := range acceptanceSeeds(t) {
		for _, from := range officeStarts {
			t.Run("seed="+strconv.FormatUint(seed, 10)+"/start="+from.String(), func(t *testing.T) {
				scoreOffice(t, runOffice(t, seed, spatialsim.Options{}, from, from+officeRunFor), from+officeRunFor)
			})
		}
	}
}

// scoreOffice scores a run against the Truth schedule and asserts the design
// targets.
func scoreOffice(t *testing.T, run officeRun, until time.Duration) {
	tolerance := DefaultConfig().Tolerance

	// The scheduled events inside observed space: those with ground
	// around them that the lidars observed confidently both before
	// the changes and after them.
	var scored []spatial.ChangeEvent
	for _, ev := range run.site.Truth().Changes() {
		before := run.before.states(ev.Floor, planar(ev.PositionM), ev.Extent.RadiusM+tolerance)
		after := run.late.states(ev.Floor, planar(ev.PositionM), ev.Extent.RadiusM+tolerance)
		both := 0
		for i := range before {
			if (before[i] == Free || before[i] == Occupied) && (after[i] == Free || after[i] == Occupied) {
				both++
			}
		}
		if both >= DefaultConfig().MinClusterCells {
			scored = append(scored, ev)
		} else {
			t.Logf("outside observed space: %s %s %s (%d cells seen in both)", ev.ID, ev.Kind, ev.Subject, both)
		}
	}
	if len(scored) < officeMinScored {
		t.Fatalf("only %d scheduled events lie in observed space, want at least %d", len(scored), officeMinScored)
	}

	// Greedy nearest matching of reported to scheduled events of the
	// same kind on the same floor.
	type match struct {
		truth, got int
		dist       float64
	}
	var candidates []match
	for i, want := range scored {
		for j, got := range run.reported {
			if d := planar(got.PositionM).Dist(planar(want.PositionM)); got.Kind == want.Kind && got.Floor == want.Floor && d <= matchGateM {
				candidates = append(candidates, match{i, j, d})
			}
		}
	}
	slices.SortStableFunc(candidates, func(a, b match) int {
		if a.dist != b.dist {
			if a.dist < b.dist {
				return -1
			}
			return 1
		}
		return 0
	})
	foundTruth, usedGot := map[int]bool{}, map[int]bool{}
	var worst, sum float64
	worstRatio := 0.0
	for _, m := range candidates {
		if foundTruth[m.truth] || usedGot[m.got] {
			continue
		}
		foundTruth[m.truth], usedGot[m.got] = true, true
		want, got := scored[m.truth], run.reported[m.got]
		errM := m.dist
		if want.Kind == spatial.ChangeMoved {
			errM = math.Max(errM, planar(*got.FromM).Dist(planar(*want.FromM)))
		}
		if want.Kind == spatial.ChangeDoorOpened || want.Kind == spatial.ChangeDoorClosed {
			if got.Subject != want.Subject {
				t.Errorf("%s reported for door %q, want %q", got.Kind, got.Subject, want.Subject)
			}
		}
		worst, sum = math.Max(worst, errM), sum+errM
		worstRatio = math.Max(worstRatio, m.dist/got.UncertaintyM)
		t.Logf("found %-12s %-22s %.3f m off (stated ± %.2f m, confidence %.2f) as %s", want.Kind, want.Subject, errM, got.UncertaintyM, got.Confidence, got.ID)
	}
	unseen, actors := 0, 0
	for j, got := range run.reported {
		if !run.everSeen(got) {
			unseen++
			t.Errorf("event in never-observed cells: %+v", got)
		}
		if usedGot[j] {
			continue
		}
		who := run.blamed(got, until)
		if who != "" {
			actors++
		}
		t.Logf("false positive: %s %s at (%.2f, %.2f) r %.2f, %s, actor %q", got.Kind, got.Floor, got.PositionM[0], got.PositionM[2], got.Extent.RadiusM, got.ID, who)
	}
	for i, want := range scored {
		if !foundTruth[i] {
			t.Logf("missed: %s %s %s at (%.2f, %.2f)", want.ID, want.Kind, want.Subject, want.PositionM[0], want.PositionM[2])
		}
	}
	precision, recall := 1.0, float64(len(foundTruth))/float64(len(scored))
	if len(run.reported) > 0 {
		precision = float64(len(usedGot)) / float64(len(run.reported))
	}
	mean := 0.0
	if len(foundTruth) > 0 {
		mean = sum / float64(len(foundTruth))
	}
	t.Logf("METRIC changes_precision=%.3f ratio (target > %.1f)", precision, targetPrecision)
	t.Logf("METRIC changes_recall=%.3f ratio (target > %.1f)", recall, targetRecall)
	t.Logf("METRIC changes_location_error_max=%.3f m (target < %.1f)", worst, targetLocationM)
	t.Logf("METRIC changes_location_error_mean=%.3f m (target < %.1f)", mean, targetLocationM)
	t.Logf("METRIC changes_events_in_unobserved_cells=%d events (target = 0)", unseen)
	t.Logf("METRIC changes_events_from_people_or_carts=%d events (target = 0)", actors)
	t.Logf("METRIC changes_scored_events=%d of %d scheduled, %d reported, %d cells compared in the widest epoch", len(scored), len(run.site.Truth().Changes()), len(run.reported), run.compared)
	t.Logf("METRIC changes_location_error_over_stated_uncertainty_max=%.2f ratio (target <= 1)", worstRatio)
	if !(precision > targetPrecision) {
		t.Errorf("precision %.3f, want > %.1f", precision, targetPrecision)
	}
	if !(recall > targetRecall) {
		t.Errorf("recall %.3f, want > %.1f", recall, targetRecall)
	}
	if !(worst < targetLocationM) {
		t.Errorf("location error %.3f m, want < %.1f", worst, targetLocationM)
	}
	if actors != 0 {
		t.Errorf("%d events caused by people or carts, want 0", actors)
	}
	if worstRatio > 1 {
		t.Errorf("a location error is %.2f times its stated uncertainty", worstRatio)
	}
}

// With no scheduled change the same detector on the same scenario, people,
// carts, robots, the bumped camera and the shifted lidar mount included, must
// stay silent.
func TestAcceptanceQuietOfficeReportsNothing(t *testing.T) {
	t.Parallel()
	for _, seed := range acceptanceSeeds(t) {
		t.Run("seed="+strconv.FormatUint(seed, 10), func(t *testing.T) {
			run := runOffice(t, seed, spatialsim.Options{NoChanges: true}, 0, quietRunFor)
			for _, ev := range run.reported {
				t.Errorf("false alarm: %s %s at (%.2f, %.2f) r %.2f, actor %q", ev.Kind, ev.Floor, ev.PositionM[0], ev.PositionM[2], ev.Extent.RadiusM, run.blamed(ev, quietRunFor))
			}
			if run.compared == 0 {
				t.Fatal("no cell was ever compared: the run proves nothing")
			}
			t.Logf("METRIC changes_quiet_run_false_alarms=%d events (target = 0)", len(run.reported))
		})
	}
}
