package objtrack

import (
	"cmp"
	"math"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/spatialsim"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// Design targets of feature 7 (spatial intelligence design, features table).
const (
	targetMOTA       = 0.9 // MOTA must exceed this
	targetIDSwitches = 2   // at most this many identity switches
	targetRMSE       = 0.2 // position RMSE of matched tracks, metres, must be below this
	targetStatic     = 0   // confirmed tracks on static structure
)

// Scoring protocol (CLEAR MOT). It is the test's, not the design's.
const (
	// matchRadiusM is how close a confirmed track must be to a mover to
	// count as tracking it.
	matchRadiusM = 0.5
	// matchLevelM bounds the elevation difference of a match: half a storey,
	// since Truth switches a climber's floor at half height.
	matchLevelM = 2.0
	// scoreEvery and scorePhase set the scoring instants. Every sensor
	// samples on a 100 ms grid, so the instants fall between samples and the
	// tracker is scored on its prediction 50 ms past its latest data.
	scoreEvery = 200 * time.Millisecond
	scorePhase = 50 * time.Millisecond
	// staticNearM is how close to furniture, a wall or a closed door an
	// unmatched confirmed track must be to count as tracking structure.
	staticNearM = 0.3
)

// The acceptance window covers every scheduled change of the Office
// scenario: furniture appears at 250, 265 and 285 s, is removed at 260 s and
// moved at 270 s, doors close at 255 and 275 s and open at 280 s. The static
// map is what a mapper had built before the window.
const (
	windowFrom = 225 * time.Second
	windowTo   = 300 * time.Second
)

func seeds(t *testing.T) []uint64 {
	t.Helper()
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

// rig is a tracker wired to a simulated site through the Reference face:
// device poses, sensor calibrations and the static map are what the features
// upstream of tracking would publish, at DefaultReferenceConfig accuracy.
// Nothing in it reads Truth.
type rig struct {
	t       *testing.T
	s       *spatialsim.Site
	ref     spatialsim.Reference
	tk      *Tracker
	rev     map[spatial.SensorRef]string
	refused int
}

func newRig(t *testing.T, s *spatialsim.Site) *rig {
	t.Helper()
	ref, err := s.Reference(spatialsim.DefaultReferenceConfig())
	if err != nil {
		t.Fatal(err)
	}
	tk, err := New(Config{})
	if err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, s: s, ref: ref, tk: tk, rev: map[spatial.SensorRef]string{}}
}

// floors declares to the tracker every floor a localized device stood on
// during [from, to), at the elevation its pose reported, with the reference
// map of that span when mapped is set.
func (r *rig) floors(from, to time.Duration, mapped bool) {
	r.t.Helper()
	reported := map[spatial.FloorID][]float64{}
	for _, dev := range r.s.Devices() {
		for at := from; at < to; at += time.Second {
			if p, ok := r.ref.DevicePose(dev, r.s.At(at)); ok {
				reported[p.Floor] = append(reported[p.Floor], p.PositionM[1])
			}
		}
	}
	// A pose on the stairs is between planes; a floor's plane is the median
	// elevation reported on it.
	elevation := map[spatial.FloorID]float64{}
	for id, ys := range reported {
		slices.Sort(ys)
		elevation[id] = ys[len(ys)/2]
	}
	ids := make([]spatial.FloorID, 0, len(elevation))
	for id := range elevation {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		if !mapped {
			if err := r.tk.SetFloor(id, elevation[id], nil); err != nil {
				r.t.Fatal(err)
			}
			continue
		}
		g, err := r.ref.Grid(id, r.s.At(from), r.s.At(to))
		if err != nil {
			r.t.Fatal(err)
		}
		if err := r.tk.SetFloor(id, elevation[id], g); err != nil {
			r.t.Fatal(err)
		}
	}
}

// feed streams [from, to) of the given devices' lidars and cameras into the
// tracker. tick, when not nil, is called at each scoring instant after
// everything sampled before it has been ingested.
func (r *rig) feed(from, to time.Duration, devices []spatial.DeviceID, tick func(at time.Time)) {
	r.t.Helper()
	next := r.s.At(from + scorePhase)
	flush := func(until time.Time) {
		for ; tick != nil && !next.After(until); next = next.Add(scoreEvery) {
			tick(next)
		}
	}
	filter := spatialsim.Filter{Devices: devices, Kinds: []spatial.SensorKind{spatial.KindLidar2D, spatial.KindCamera}}
	for m := range r.s.Stream(r.s.At(from), r.s.At(to), filter) {
		if _, manifest := m.(spatial.SensorManifest); manifest {
			continue
		}
		dev, sensor := m.From()
		sref := spatial.SensorRef{Device: dev, Sensor: sensor}
		// A sample is stamped on its sensor's clock, up to ClockOffsetMax
		// from site time, and the reference holds no calibration before a
		// device starts: look it up that much later. (That finds a new
		// revision early within ClockOffsetMax of a disturbance; the windows
		// used here contain none.)
		calib, ok := r.ref.Calibration(dev, sensor, m.When().Add(r.s.Config().ClockOffsetMax))
		if !ok {
			r.t.Fatalf("no reference calibration for %s/%s", dev, sensor)
		}
		if r.rev[sref] != calib.Revision {
			if err := r.tk.SetCalibration(calib); err != nil {
				r.t.Fatal(err)
			}
			r.rev[sref] = calib.Revision
		}
		at := m.When().Add(-calib.TimeOffset)
		// 20 ms covers the clock-offset error of the reference calibration.
		flush(at.Add(-20 * time.Millisecond))
		if calib.Parent == spatial.ParentDeviceBase {
			pose, ok := r.ref.DevicePose(dev, at)
			if !ok {
				r.t.Fatalf("no reference pose for %s", dev)
			}
			if err := r.tk.ObservePose(at, pose); err != nil {
				r.t.Fatal(err)
			}
		}
		if err := r.tk.Ingest(m); err != nil {
			r.refused++
			r.t.Errorf("ingest %s: %v", spatial.InputID(m), err)
		}
	}
	flush(r.s.At(to))
}

// score accumulates the CLEAR MOT counts.
type score struct {
	last    map[string]string // mover → the track that last matched it
	current map[string]string // mover → its match at the previous instant

	truths, misses, falsePositives, switches int
	actorTruths, actorMisses, actorSwitches  int
	matched                                  int
	sumSq, actorSumSq                        float64
	actorMatched                             int
	static                                   int
	floorRight, floorWrong                   int
	classRight, classWrong                   int
	perMover                                 map[string]*[4]int // truths, misses, switches, matched
	ghosts                                   map[string]int     // unmatched confirmed track → instants
	onStructure                              map[string]int     // of which on static structure
	notes                                    []string
}

func newScore() *score {
	return &score{last: map[string]string{}, current: map[string]string{}, perMover: map[string]*[4]int{}, ghosts: map[string]int{}, onStructure: map[string]int{}}
}

// step scores the tracker's confirmed tracks against Truth at one instant:
// existing matches are kept while they stay within matchRadiusM, the rest
// are assigned by least total distance, a mover whose track differs from the
// last one that matched it is an identity switch.
func (sc *score) step(s *spatialsim.Site, at time.Time, all []spatial.Track) {
	tr := s.Truth()
	elevation := map[spatial.FloorID]float64{}
	for _, f := range tr.Floors(at) {
		elevation[f.Floor] = f.ElevationM
	}
	actors := map[string]bool{}
	for _, a := range tr.Actors(at) {
		actors[a.ID] = true
	}
	movers := tr.Movers(at)
	var tracks []spatial.Track
	for _, k := range all {
		if k.State == spatial.TrackConfirmed {
			tracks = append(tracks, k)
		}
	}
	dist := func(i, j int) (float64, bool) {
		m, k := movers[i], tracks[j]
		d := m.Shape.Centre.Dist(k.Point().XZ())
		return d, d <= matchRadiusM && math.Abs(k.PositionM[1]-elevation[m.Floor]) <= matchLevelM
	}
	match1 := make([]int, len(movers)) // track index per mover, -1 none
	taken := make([]bool, len(tracks))
	for i, m := range movers {
		match1[i] = -1
		if id, ok := sc.current[m.ID]; ok {
			j := slices.IndexFunc(tracks, func(k spatial.Track) bool { return k.ID == id })
			if j >= 0 {
				if _, ok := dist(i, j); ok {
					match1[i], taken[j] = j, true
				}
			}
		}
	}
	rest := match(len(movers), len(tracks), matchRadiusM, func(i, j int) (float64, bool) {
		if match1[i] >= 0 || taken[j] {
			return 0, false
		}
		return dist(i, j)
	})
	clear(sc.current)
	for i, m := range movers {
		j := match1[i]
		if j < 0 {
			j = rest[i]
		}
		pm := sc.perMover[m.ID]
		if pm == nil {
			pm = new([4]int)
			sc.perMover[m.ID] = pm
		}
		sc.truths++
		pm[0]++
		if actors[m.ID] {
			sc.actorTruths++
		}
		if j < 0 {
			sc.misses++
			pm[1]++
			if actors[m.ID] {
				sc.actorMisses++
			}
			continue
		}
		taken[j] = true
		k := tracks[j]
		if prev, ok := sc.last[m.ID]; ok && prev != k.ID {
			sc.switches++
			pm[2]++
			if actors[m.ID] {
				sc.actorSwitches++
			}
			sc.notes = append(sc.notes, at.Sub(s.Epoch()).String()+" "+m.ID+" "+prev+"->"+k.ID)
		}
		sc.last[m.ID], sc.current[m.ID] = k.ID, k.ID
		d, _ := dist(i, j)
		sc.matched++
		pm[3]++
		sc.sumSq += d * d
		if actors[m.ID] {
			sc.actorMatched++
			sc.actorSumSq += d * d
		}
		if k.Floor == m.Floor {
			sc.floorRight++
		} else {
			sc.floorWrong++
		}
		if k.Class != "" {
			if k.Class == m.Class {
				sc.classRight++
			} else {
				sc.classWrong++
			}
		}
	}
	// Whatever is confirmed and matches no mover is a false positive, and it
	// is static structure tracked when it sits on furniture (as it stands at
	// this instant, after any scheduled change), a wall or a closed door.
	objects := tr.Objects(at)
	doors := tr.Doors()
	for j, k := range tracks {
		if taken[j] {
			continue
		}
		sc.falsePositives++
		sc.ghosts[k.ID]++
		p := k.Point().XZ()
		onStructure := false
		for _, o := range objects {
			if math.Abs(k.PositionM[1]-elevation[o.Floor]) <= matchLevelM && (o.Shape.Contains(p) || o.Shape.Centre.Dist(p) <= o.Shape.Bound()+staticNearM) {
				onStructure = true
			}
		}
		for floor, e := range elevation {
			if math.Abs(k.PositionM[1]-e) > matchLevelM {
				continue
			}
			for _, w := range tr.Walls(floor) {
				onStructure = onStructure || w.DistanceTo(p) <= staticNearM
			}
		}
		for _, d := range doors {
			if open, _ := tr.DoorOpen(d.ID, at); !open && math.Abs(k.PositionM[1]-elevation[d.Floor]) <= matchLevelM {
				onStructure = onStructure || d.Segment.DistanceTo(p) <= staticNearM
			}
		}
		if onStructure {
			sc.static++
			if sc.onStructure[k.ID]++; sc.onStructure[k.ID] == 1 {
				sc.notes = append(sc.notes, at.Sub(s.Epoch()).String()+" on static structure "+k.ID)
			}
		}
	}
}

func (sc *score) mota() float64 {
	return 1 - float64(sc.misses+sc.falsePositives+sc.switches)/float64(sc.truths)
}

// actorMOTA scores the scripted people and carts alone, the movers that do
// not report their own pose. Every false positive is charged to them.
func (sc *score) actorMOTA() float64 {
	return 1 - float64(sc.actorMisses+sc.falsePositives+sc.actorSwitches)/float64(sc.actorTruths)
}

func (sc *score) rmse() float64 { return math.Sqrt(sc.sumSq / float64(max(sc.matched, 1))) }
func (sc *score) actorRMSE() float64 {
	return math.Sqrt(sc.actorSumSq / float64(max(sc.actorMatched, 1)))
}

func (sc *score) log(t *testing.T) {
	ids := make([]string, 0, len(sc.perMover))
	for id := range sc.perMover {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		pm := sc.perMover[id]
		t.Logf("  %-10s instants=%d missed=%d switches=%d matched=%d", id, pm[0], pm[1], pm[2], pm[3])
	}
	ghosts := make([]string, 0, len(sc.ghosts))
	for id := range sc.ghosts {
		ghosts = append(ghosts, id)
	}
	slices.SortFunc(ghosts, func(a, b string) int { return cmp.Compare(a, b) })
	for _, id := range ghosts {
		t.Logf("  unmatched confirmed track %s for %d instants, %d of them on static structure", id, sc.ghosts[id], sc.onStructure[id])
	}
	for _, n := range sc.notes {
		t.Logf("  %s", n)
	}
}

// TestAcceptanceOffice is the feature's acceptance test: the standard Office
// scenario with default noise, every lidar and camera on the site, eleven
// movers on three floors (four scripted actors that only sensors can see,
// seven localized devices), the two lobby walkers passing within decimetres
// of each other each loop, robots driving between them and the cameras, the
// yard actors leaving and re-entering the camera's and the ranger's view,
// and every scheduled furniture and door change. Only confirmed tracks are
// scored, from the first instant of the window with a cold tracker.
//
// Every target is asserted twice: over all movers, the set Truth.Movers
// names for scoring, and over the four actors alone with every false
// positive of the site charged to them. The second is the harder one: a
// localized device is tracked from its own pose, an actor only by sensors.
//
// What decides the actors' score is how well the fixed cameras are
// calibrated, because a foot pixel 10 m from a camera 3 m up moves 0.4 m on
// the floor per 0.3 degrees of camera rotation, the reference calibration's
// 1σ per axis. While the yard camera is the only sensor on the walker at the
// far end of the yard, its track is as biased as that camera, and beyond
// matchRadiusM it scores as a miss and a false positive at once. A seed
// whose yard or lobby camera drew an unlucky calibration misses the actors'
// MOTA target for that reason (seeds 10, 13 and 32 of 1 to 40 when this was
// written; in seed 10 the lobby camera is 1.3 degrees off, past the 1 degree
// target of the calibration feature, and the walkers it alone sees also
// change identity). The tracker does not refine calibrations.
func TestAcceptanceOffice(t *testing.T) {
	for _, seed := range seeds(t) {
		s, err := spatialsim.Office(seed, spatialsim.Options{})
		if err != nil {
			t.Fatal(err)
		}
		r := newRig(t, s)
		r.floors(mapFrom, windowFrom, true)
		sc := newScore()
		r.feed(windowFrom, windowTo, nil, func(at time.Time) {
			tracks := r.tk.Tracks(at)
			for _, k := range tracks {
				if err := k.Validate(); err != nil {
					t.Fatalf("track %s at %s: %v", k.ID, at.Sub(s.Epoch()), err)
				}
			}
			sc.step(s, at, tracks)
		})
		t.Logf("seed %d: %d truth instants, %d missed, %d false positives, %d switches, %d matched", seed, sc.truths, sc.misses, sc.falsePositives, sc.switches, sc.matched)
		sc.log(t)
		t.Logf("METRIC mota=%.4f ratio (target > %.1f)", sc.mota(), targetMOTA)
		t.Logf("METRIC id_switches=%d count (target <= %d)", sc.switches, targetIDSwitches)
		t.Logf("METRIC position_rmse=%.4f m (target < %.1f)", sc.rmse(), targetRMSE)
		t.Logf("METRIC static_structure_tracked=%d count (target <= %d)", sc.static, targetStatic)
		t.Logf("METRIC actor_mota=%.4f ratio (target > %.1f)", sc.actorMOTA(), targetMOTA)
		t.Logf("METRIC actor_id_switches=%d count (target <= %d)", sc.actorSwitches, targetIDSwitches)
		t.Logf("METRIC actor_position_rmse=%.4f m (target < %.1f)", sc.actorRMSE(), targetRMSE)
		t.Logf("METRIC floor_label_accuracy=%.4f ratio (target >= 0, reported)", float64(sc.floorRight)/float64(max(sc.floorRight+sc.floorWrong, 1)))
		t.Logf("METRIC class_accuracy=%.4f ratio (target >= 0, reported)", float64(sc.classRight)/float64(max(sc.classRight+sc.classWrong, 1)))
		if sc.mota() <= targetMOTA || sc.actorMOTA() <= targetMOTA {
			t.Errorf("seed %d: MOTA %.4f (actors alone %.4f), target > %.1f", seed, sc.mota(), sc.actorMOTA(), targetMOTA)
		}
		if sc.switches > targetIDSwitches {
			t.Errorf("seed %d: %d identity switches, target <= %d", seed, sc.switches, targetIDSwitches)
		}
		if sc.rmse() >= targetRMSE || sc.actorRMSE() >= targetRMSE {
			t.Errorf("seed %d: position RMSE %.4f m (actors alone %.4f), target < %.1f", seed, sc.rmse(), sc.actorRMSE(), targetRMSE)
		}
		if sc.static > targetStatic {
			t.Errorf("seed %d: %d confirmed track instants on static structure, target %d", seed, sc.static, targetStatic)
		}
	}
}

// TestComponentFromSimulation publishes what the tracker holds after a few
// seconds of the Office scenario, with the ancestry of the measurements
// behind each track.
func TestComponentFromSimulation(t *testing.T) {
	s, err := spatialsim.Office(seeds(t)[0], spatialsim.Options{})
	if err != nil {
		t.Fatal(err)
	}
	r := newRig(t, s)
	r.floors(0, 2*time.Second, false)
	cameras := []spatial.DeviceID{spatialsim.CamLobby, spatialsim.CamCorridorWest}
	r.feed(0, 5*time.Second, cameras, nil)
	at := s.At(5 * time.Second)
	sim := s.SimulationContext()
	scope := spatial.Scope{Tenant: "t", Site: "office", Source: world.Source{Device: "gateway", App: "spatial", Service: "objtrack", Stream: "tracks"}}
	published := 0
	for i, k := range r.tk.Tracks(at) {
		if k.State != spatial.TrackConfirmed {
			continue
		}
		var inputs []world.Input
		for _, id := range r.tk.Evidence(k.ID) {
			inputs = append(inputs, world.Input{ID: id, Origin: world.Simulated, Simulation: &sim})
		}
		if len(inputs) == 0 {
			t.Fatalf("confirmed track %s has no evidence", k.ID)
		}
		c, err := Component(scope, world.Version{Epoch: 1, Sequence: uint64(i + 1)}, spatialsim.SiteFrame, world.Timing{SampleTime: at, ClockDomain: spatialsim.ClockDomain}, 2*time.Second, "test", k, inputs)
		if err != nil {
			t.Fatalf("track %s: %v", k.ID, err)
		}
		if c.Provenance.ModelID != "wendy.spatial.objtrack" || c.Provenance.Origin != world.Inferred {
			t.Fatalf("provenance %+v", c.Provenance)
		}
		back, err := spatial.FromComponent[spatial.Track](c)
		if err != nil || !reflect.DeepEqual(back, k) {
			t.Fatalf("round trip of %s: %v", k.ID, err)
		}
		published++
	}
	if published == 0 {
		t.Fatal("no confirmed track to publish after 5 s of two cameras")
	}
}
