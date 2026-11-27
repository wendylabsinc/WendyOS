package history

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/spatialsim"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// Design target of feature 13 (specs/superpowers/specs/2026-11-25-spatial-intelligence-design.md):
// reconstruction equals the recorded live state; survives restart and
// compaction.
const targetMismatchedStates = 0

// The acceptance window is the minute of the Office scenario in which the
// environment changes (all eight scheduled changes fall in 250 to 285 s), the
// ranger comes in from the yard and leaves again and the climber rides the
// lift down and takes the stairs back up. It starts at acceptFrom (235 s;
// later under the race detector, see race_on_test.go).
const (
	acceptUntil = 300 * time.Second
	// restartAt is when the journal is closed and reopened under the running
	// stream; compactTo is the retention horizon of the explicit compaction.
	restartAt = 255 * time.Second
	compactTo = 262 * time.Second
	// autoRetention is the Config.Retention of the second journal, which
	// snapshots by itself as its segments fill.
	autoRetention = 15 * time.Second
	segmentBytes  = 256 << 10

	// stateSampling is the chance that the live state is recorded at a clock
	// reading (the clock reads 20 times a simulated second).
	stateSampling = 0.03
)

// Publishing cadence and lifetimes of the upstream stand-ins.
const (
	poseEvery = 5 // every fifth odometry sample: 4 Hz
	poseTTL   = time.Second

	scanEvery = 500 * time.Millisecond // rover-1's scans that reach the map: 2 Hz
	tileSide  = 64
	tileEvery = 5 * time.Second
	tileTTL   = 10 * time.Minute

	trackTTL        = 2 * time.Second
	trackCoastAfter = time.Second
	trackLostAfter  = 3 * time.Second
	// trackSigmaM is the stand-in tracker's error per axis: 0.21 m RMS
	// radially, no better than objtrack's target (position RMSE < 0.2 m).
	trackSigmaM = 0.15

	changeLatency = 2 * time.Second // a change is reported this long after it happens
	changeConfirm = 3 * time.Second // and confirmed this long after that
	changeTTL     = 10 * time.Minute
	// changeSigmaM is the stand-in change detector's error per axis: 0.31 m
	// RMS radially, no better than changes' target (location < 0.3 m).
	changeSigmaM = 0.22
)

// The journal records what a live world.Replica is told while the Office
// scenario runs, and must reproduce the replica's state at every sampled
// instant: as it stands, after a restart, after compaction, and from a second
// journal that compacts by itself.
//
// The journal estimates nothing, so what it is fed is a stream of the
// components the other features publish, built here by stand-ins:
//
//   - device poses: Site.Reference with its default noise, at 4 Hz per robot,
//     stamped with the odometry sample they answer;
//   - map tiles: rover-1's scans integrated at 2 Hz at the reference pose,
//     lidar mount and clock offset, republished every 5 s where the cells
//     changed;
//   - tracks: one per robot whose marker a camera detects (the measurements
//     decide which camera sees whom, and when), at the reference pose plus
//     trackSigmaM, stamped on the camera's own uncorrected clock so that
//     sample times arrive out of order; coasting after 1 s unseen and
//     tombstoned after 3 s;
//   - change events: a LABELLED STAND-IN for the changes feature, since the
//     simulator documents no reference for it: the scheduled changes
//     (Truth.Changes) reported changeLatency late with changeSigmaM of
//     position error and confirmed once. Truth reaches nothing else, and no
//     estimator at all: the journal treats payloads as opaque bytes.
//
// The injected clock is the odometry clock, which is site time.
func TestAcceptanceReconstructsLiveState(t *testing.T) {
	for _, seed := range seeds(t) {
		t.Run(fmt.Sprintf("seed-%d", seed), func(t *testing.T) { reconstructOffice(t, seed) })
	}
}

// liveSite is the live side of the acceptance test: the replica that holds
// the state, the stand-in publishers that feed it, and the journals that
// record what it is fed.
type liveSite struct {
	t       *testing.T
	s       *spatialsim.Site
	ref     spatialsim.Reference
	rng     *rand.Rand
	clock   *fakeClock
	replica *world.Replica
	dir     string
	cfg     Config
	journal *Journal // compacted by hand
	auto    *Journal // compacts by itself (Config.Retention)

	sequence  map[world.Key]uint64
	sampled   map[world.Key]time.Time // newest sample time published per key
	restarted bool
	live      []liveState

	odometry map[spatial.DeviceID]int
	roverKey world.Key
	path     []Fix // rover-1's published poses, in order

	grid       *grid.Grid
	nextScan   time.Time
	scans      uint64 // integrated
	scanAt     time.Time
	scanInputs []world.Input // since the last tile round
	lastScan   []world.Input
	cells      map[world.Key][]byte
	nextTiles  time.Time

	tracks map[spatial.DeviceID]*liveTrack
	born   int

	changes []liveChange

	records, tombstones, disorder int
}

type liveTrack struct {
	payload   spatial.Track
	component world.Component // as last published
	input     world.Input
	sample    time.Time // of the last sighting, on its camera's clock
	seenAt    time.Time // the clock at the last sighting
	hits      int
}

type liveChange struct {
	event    spatial.ChangeEvent // as scheduled, until it is reported
	happened time.Time
	stage    int // 0 unreported, 1 reported, 2 confirmed
}

func reconstructOffice(t *testing.T, seed uint64) {
	s, err := spatialsim.Office(seed, spatialsim.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ref, err := s.Reference(spatialsim.DefaultReferenceConfig())
	if err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{t: s.At(acceptFrom)}
	replica, err := world.NewReplica(world.Config{
		Tenant: testScope.Tenant, Site: testScope.Site, ClockDomain: spatialsim.ClockDomain, Now: clock.now,
		MaxFutureSkew: time.Second, MaxEntries: 1 << 14, AllowSynthetic: true, Frames: []world.FrameRef{spatialsim.SiteFrame},
	})
	if err != nil {
		t.Fatal(err)
	}
	g, err := grid.New(grid.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	l := &liveSite{
		t: t, s: s, ref: ref, rng: rand.New(rand.NewPCG(seed, 13)), clock: clock, replica: replica,
		dir: t.TempDir(), cfg: Config{Now: clock.now, SegmentBytes: segmentBytes},
		sequence: map[world.Key]uint64{}, sampled: map[world.Key]time.Time{},
		odometry: map[spatial.DeviceID]int{}, grid: g, cells: map[world.Key][]byte{},
		nextScan: s.At(acceptFrom), nextTiles: s.At(acceptFrom + tileEvery), tracks: map[spatial.DeviceID]*liveTrack{},
	}
	l.journal = mustOpen(t, l.dir, l.cfg)
	l.auto = mustOpen(t, t.TempDir(), Config{Now: clock.now, SegmentBytes: segmentBytes, Retention: autoRetention})
	for _, c := range s.Truth().Changes() { // the labelled stand-in: see the test's comment
		l.changes = append(l.changes, liveChange{event: c, happened: c.FirstObserved})
	}

	// Rover-1's scans are drawn at scanEvery from the generator instead: the
	// stream would compute every robot's scans at 10 Hz to use one in fifty.
	filter := spatialsim.Filter{Kinds: []spatial.SensorKind{spatial.KindOdometry, spatial.KindCamera}}
	for m := range s.Stream(s.At(acceptFrom), s.At(acceptUntil), filter) {
		switch v := m.(type) {
		case spatial.Odometry:
			if v.When().After(clock.t) {
				l.advance(v.When())
			}
			l.pose(v)
		case spatial.CameraFrame:
			l.frame(v)
		}
	}
	l.record(clock.t)

	// The stream must have exercised what the target is about.
	types := map[string]int{}
	for _, st := range l.live {
		for _, c := range st.state {
			types[c.Key.Type]++
		}
	}
	for _, typ := range []string{spatial.TypeDevicePose, spatial.TypeTrack, spatial.TypeMapTile, spatial.TypeChangeEvent} {
		if types[typ] == 0 {
			t.Errorf("no recorded live state holds a %s", typ)
		}
	}
	horizon := s.At(compactTo)
	before := 0
	for _, st := range l.live {
		if st.at.Before(horizon) {
			before++
		}
	}
	if before < 1 || len(l.live)-before < 5 || l.tombstones == 0 || !l.restarted {
		t.Fatalf("weak run: %d live states (%d before the horizon), %d tombstones, restarted=%v", len(l.live), before, l.tombstones, l.restarted)
	}
	st := l.journal.Stats()
	t.Logf("seed %d: %d deltas (%d tombstones, %d sampled before their predecessor) on %d keys, %d live states, %d segments, %d KiB",
		seed, l.records, l.tombstones, l.disorder, st.Keys, len(l.live), st.Segments, st.Bytes>>10)
	if st.Records != l.records || st.TruncatedBytes != 0 {
		t.Errorf("journal holds %d of %d deltas after its restart, %d bytes truncated", st.Records, l.records, st.TruncatedBytes)
	}

	metric := func(phase string, j *Journal, horizon time.Time) {
		mismatched := l.verify(j, phase, horizon)
		t.Logf("METRIC history_mismatched_states_%s=%d states (target <= %d)", phase, mismatched, targetMismatchedStates)
		if mismatched > targetMismatchedStates {
			t.Errorf("%s: %d live states did not reconstruct", phase, mismatched)
		}
	}
	reopen := func() {
		if err := l.journal.Close(); err != nil {
			t.Fatal(err)
		}
		l.journal = mustOpen(t, l.dir, l.cfg)
	}
	metric("live", l.journal, time.Time{})
	reopen()
	metric("reopened", l.journal, time.Time{})

	res, err := l.journal.Compact(horizon)
	if err != nil {
		t.Fatal(err)
	}
	after := l.journal.Stats()
	t.Logf("seed %d: compaction dropped %d records and %d segments, kept %d in the snapshot; %d KiB left", seed, res.Dropped, res.Segments, res.Kept, after.Bytes>>10)
	if res.Dropped == 0 || res.Segments == 0 || after.Bytes >= st.Bytes || !after.Horizon.Equal(horizon) {
		t.Errorf("compaction collapsed nothing: %+v, stats %+v", res, after)
	}
	metric("compacted", l.journal, horizon)
	reopen()
	metric("compacted_reopened", l.journal, horizon)

	// The second journal took its snapshots by itself while the stream ran.
	auto := l.auto.Stats()
	if auto.Horizon.IsZero() || auto.Horizon.After(clock.t.Add(-autoRetention)) || auto.Records >= l.records {
		t.Errorf("the retention never compacted: %+v", auto)
	}
	metric("retention", l.auto, auto.Horizon)
}

// timing stamps a component sampled at at, with the clock error its publisher
// admits to.
func timing(at time.Time, bound time.Duration) world.Timing {
	return world.Timing{SampleTime: at, ClockDomain: spatialsim.ClockDomain, ErrorKnown: true, ErrorBound: bound}
}

// publish builds the next version of p's component as feature would and
// applies it.
func (l *liveSite) publish(feature string, tm world.Timing, ttl time.Duration, inputs []world.Input, p spatial.Payload) world.Component {
	l.t.Helper()
	key := spatial.KeyFor(testScope, p)
	prov, err := spatial.InferredProvenance(feature, "1", "history-acceptance", spatial.ReduceInputs(inputs))
	if err != nil {
		l.t.Fatal(err)
	}
	c, err := spatial.ToComponent(testScope, world.Version{Epoch: 1, Sequence: l.sequence[key] + 1}, spatialsim.SiteFrame, tm, ttl, prov, p)
	if err != nil {
		l.t.Fatal(err)
	}
	l.apply(c)
	return c
}

// apply hands one delta to the live replica and to both journals.
func (l *liveSite) apply(c world.Component) {
	l.t.Helper()
	if res, err := l.replica.Apply(c.Key.Source, c); err != nil || res != world.Applied {
		l.t.Fatalf("replica apply of %s: %q, %v", label(c), res, err)
	}
	mustAppend(l.t, l.journal, c)
	mustAppend(l.t, l.auto, c)
	l.sequence[c.Key] = c.Version.Sequence
	l.records++
	if c.Tombstone {
		l.tombstones++
	}
	if c.Timing.SampleTime.Before(l.sampled[c.Key]) {
		l.disorder++
	} else {
		l.sampled[c.Key] = c.Timing.SampleTime
	}
}

// advance moves the clock on to next, the time of the odometry sample that is
// about to be handled, and does what is due by then.
func (l *liveSite) advance(next time.Time) {
	if l.rng.Float64() < stateSampling {
		l.record(next)
	}
	if !l.restarted && !next.Before(l.s.At(restartAt)) {
		// A restart under the running stream: the journal carries on. The
		// state just before it is always recorded.
		l.record(l.clock.t)
		if err := l.journal.Close(); err != nil {
			l.t.Fatal(err)
		}
		l.journal = mustOpen(l.t, l.dir, l.cfg)
		l.restarted = true
	}
	l.clock.t = next
	if !next.Before(l.nextScan) {
		l.nextScan = l.nextScan.Add(scanEvery)
		if sc, ok := l.s.Scan(spatialsim.Rover1, next); ok {
			l.scan(sc)
		}
	}
	l.loseTracks()
	l.reportChanges()
	if !next.Before(l.nextTiles) {
		l.nextTiles = l.nextTiles.Add(tileEvery)
		l.publishTiles()
	}
}

// record samples the live state. Every delta of the current clock reading has
// been applied; half of the time the clock first moves part of the way to its
// next reading, where only expiry can have changed the state.
func (l *liveSite) record(next time.Time) {
	if gap := next.Sub(l.clock.t); gap > 0 && l.rng.IntN(2) == 0 {
		l.clock.t = l.clock.t.Add(time.Duration(l.rng.Int64N(int64(gap))))
	}
	state, err := l.replica.Snapshot()
	if err != nil {
		l.t.Fatal(err)
	}
	l.live = append(l.live, liveState{l.clock.t, state})
}

// pose publishes the reference pose of a robot at one odometry sample in poseEvery.
func (l *liveSite) pose(o spatial.Odometry) {
	l.odometry[o.Device]++
	if l.odometry[o.Device]%poseEvery != 1 {
		return
	}
	pose, ok := l.ref.DevicePose(o.Device, o.When())
	if !ok {
		return
	}
	c := l.publish("localize", timing(o.When(), 0), poseTTL, []world.Input{l.s.Input(o)}, pose)
	if o.Device == spatialsim.Rover1 {
		entities, err := spatial.ComponentEntities(c)
		if err != nil || len(entities) != 1 {
			l.t.Fatalf("pose entity: %v", err)
		}
		l.roverKey = c.Key
		l.path = append(l.path, Fix{Time: o.When(), Entity: entities[0]})
	}
}

// scan integrates one scan of rover-1 into the map.
func (l *liveSite) scan(sc spatial.Scan) {
	cal, ok := l.ref.Calibration(sc.Device, sc.Sensor, l.clock.t)
	if !ok {
		l.t.Fatal("no reference calibration for rover-1's lidar")
	}
	at := sc.When().Add(-cal.TimeOffset)
	pose, ok := l.ref.DevicePose(sc.Device, at)
	mount, _, planar := cal.SensorToParent.Planar()
	if !ok || !planar {
		l.t.Fatal("no reference pose or planar mount for rover-1")
	}
	if err := l.grid.InsertScan(pose.Pose2().Compose(mount), sc); err != nil {
		l.t.Fatal(err)
	}
	l.scans, l.scanAt = l.scans+1, at
	l.lastScan = []world.Input{l.s.Input(sc)}
	l.scanInputs = append(l.scanInputs, l.lastScan[0])
}

// publishTiles republishes the map tiles whose cells changed since they were
// last published.
func (l *liveSite) publishTiles() {
	if len(l.scanInputs) == 0 {
		return
	}
	tiles, err := l.grid.Tiles(spatialsim.FloorGround, tileSide, 0, map[string]uint64{string(spatialsim.Rover1): l.scans})
	if err != nil {
		l.t.Fatal(err)
	}
	for _, tile := range tiles {
		key := spatial.KeyFor(testScope, tile)
		if bytes.Equal(l.cells[key], tile.Cells) {
			continue
		}
		l.cells[key] = tile.Cells
		l.publish("occupancy", timing(l.scanAt, 0), tileTTL, l.scanInputs, tile)
	}
	l.scanInputs = nil
}

// frame updates the track of every robot whose marker the camera detected.
func (l *liveSite) frame(f spatial.CameraFrame) {
	var seen []spatial.DeviceID
	for _, d := range f.Detections {
		dev, ok := strings.CutPrefix(d.ID, spatialsim.MarkerID(""))
		if d.Kind != spatial.DetectFiducial || !ok {
			continue
		}
		if id := spatial.DeviceID(dev); !slices.Contains(seen, id) {
			seen = append(seen, id)
		}
	}
	for _, dev := range seen {
		// The tracker has not estimated the camera's clock offset: it stamps
		// the track with the frame's own time and admits the possible error.
		at := f.When()
		pose, ok := l.ref.DevicePose(dev, at)
		if !ok {
			continue
		}
		p := pose.Point()
		p.X += l.rng.NormFloat64() * trackSigmaM
		p.Z += l.rng.NormFloat64() * trackSigmaM
		tr := l.tracks[dev]
		if tr == nil {
			l.born++
			v := trackSigmaM * trackSigmaM
			tr = &liveTrack{payload: spatial.Track{
				ID: fmt.Sprintf("t%d", l.born), Class: "robot", ClassConfidence: 0.9, RadiusM: 0.25,
				Covariance: [16]float64{0: v, 5: v, 10: 1, 15: 1}, FirstSeen: at, LastSeen: at,
			}}
			l.tracks[dev] = tr
		} else if dt := at.Sub(tr.sample).Seconds(); dt > 0 {
			tr.payload.VelocityMPS = p.XZ().Sub(tr.payload.Point().XZ()).Scale(1 / dt)
		}
		tr.hits++
		tr.payload.Floor, tr.payload.Location = pose.Floor, spatial.At(p, 2*trackSigmaM)
		tr.payload.State = spatial.TrackConfirmed
		if tr.hits < 3 {
			tr.payload.State = spatial.TrackTentative
		}
		if at.After(tr.payload.LastSeen) {
			tr.payload.LastSeen = at
		}
		camera := spatial.SensorRef{Device: f.Device, Sensor: f.Sensor}
		known := false
		for _, s := range tr.payload.Sensors {
			known = known || s == camera
		}
		if !known && len(tr.payload.Sensors) < spatial.MaxTrackSensors {
			tr.payload.Sensors = append(tr.payload.Sensors, camera)
		}
		tr.input, tr.sample, tr.seenAt = l.s.Input(f), at, l.clock.t
		tr.component = l.publish("objtrack", timing(at, l.s.Config().ClockOffsetMax), trackTTL, []world.Input{tr.input}, tr.payload)
	}
}

// loseTracks marks the tracks no camera has seen for a while as coasting, and
// then tombstones them; the robot's next sighting starts a new track.
func (l *liveSite) loseTracks() {
	now := l.clock.t
	for _, dev := range l.s.Devices() { // in a fixed order
		tr := l.tracks[dev]
		if tr == nil {
			continue
		}
		switch quiet := now.Sub(tr.seenAt); {
		case quiet >= trackLostAfter:
			c := tr.component
			c.Version.Sequence = l.sequence[c.Key] + 1
			c.Timing = timing(now, 0)
			c.Payload, c.Tombstone = nil, true
			l.apply(c)
			delete(l.tracks, dev)
		case quiet >= trackCoastAfter && tr.payload.State != spatial.TrackCoasting:
			tr.payload.State = spatial.TrackCoasting
			tr.component = l.publish("objtrack", timing(now, 0), trackTTL, []world.Input{tr.input}, tr.payload)
		}
	}
}

// reportChanges is the labelled stand-in for the changes feature.
func (l *liveSite) reportChanges() {
	now := l.clock.t
	if len(l.lastScan) == 0 {
		return
	}
	for i := range l.changes {
		ch := &l.changes[i]
		switch {
		case ch.stage == 0 && !now.Before(ch.happened.Add(changeLatency)):
			shift := func(p [3]float64) [3]float64 {
				p[0] += l.rng.NormFloat64() * changeSigmaM
				p[2] += l.rng.NormFloat64() * changeSigmaM
				return p
			}
			ev := &ch.event
			ev.PositionM, ev.UncertaintyM = shift(ev.PositionM), 2*changeSigmaM
			if ev.FromM != nil {
				from, to := shift(*ev.FromM), shift(*ev.ToM)
				ev.FromM, ev.ToM = &from, &to
			}
			ev.Confidence, ev.FirstObserved, ev.LastObserved = 0.6, now, now
		case ch.stage == 1 && !now.Before(ch.happened.Add(changeLatency+changeConfirm)):
			ch.event.Confidence, ch.event.LastObserved = 0.9, now
		default:
			continue
		}
		ch.stage++
		l.publish("changes", timing(now, 0), changeTTL, l.lastScan, ch.event)
	}
}

// verify compares j with every recorded live state at or after horizon, the
// difference between each two consecutive ones and rover-1's trajectory, and
// requires the typed refusal before the horizon. It returns the number of
// live states that did not reconstruct exactly.
func (l *liveSite) verify(j *Journal, phase string, horizon time.Time) (mismatched int) {
	t := l.t
	view := View{Axis: ApplyTime, HonourTTL: true}
	for i, st := range l.live {
		got, err := j.AsOf(st.at, view)
		if st.at.Before(horizon) {
			if !errors.Is(err, ErrCompacted) {
				t.Errorf("%s: as of %v, before the horizon: %v, want ErrCompacted", phase, st.at.Sub(l.s.Epoch()), err)
			}
			continue
		}
		if err != nil || !sameState(got, st.state) {
			mismatched++
			t.Errorf("%s: as of %v: %v\n%s", phase, st.at.Sub(l.s.Epoch()), err, firstDifference(got, st.state))
			continue
		}
		if i == 0 || l.live[i-1].at.Before(horizon) {
			continue
		}
		prev := l.live[i-1]
		d, err := j.Diff(prev.at, st.at, view)
		if want := differ(prev.state, st.state); err != nil || !sameDifference(d, want) {
			t.Errorf("%s: difference %v to %v: +%d -%d ~%d (%v), the live states differ by +%d -%d ~%d", phase,
				prev.at.Sub(l.s.Epoch()), st.at.Sub(l.s.Epoch()), len(d.Added), len(d.Removed), len(d.Changed), err, len(want.Added), len(want.Removed), len(want.Changed))
		}
	}

	// The trajectory of rover-1 is the poses that were published for it.
	from := l.s.At(acceptFrom)
	if horizon.After(from) {
		from = horizon
	}
	want := l.path
	for len(want) > 0 && want[0].Time.Before(from) {
		want = want[1:]
	}
	got, err := j.Trajectory(l.roverKey, from, l.clock.t, SampleTime)
	if err != nil || len(got) != len(want) || len(want) < 20 {
		t.Errorf("%s: trajectory of rover-1: %d fixes (%v), %d poses were published", phase, len(got), err, len(want))
		return mismatched
	}
	for i, fix := range got {
		if w := want[i]; !fix.Time.Equal(w.Time) || fix.Entity.Position != w.Entity.Position || fix.Entity.Yaw != w.Entity.Yaw ||
			fix.Entity.Floor != w.Entity.Floor || fix.Entity.UncertaintyM != w.Entity.UncertaintyM {
			t.Errorf("%s: trajectory fix %d is %+v, published %+v", phase, i, fix, w)
			break
		}
	}
	return mismatched
}

// differ is the difference between two recorded live states, from the
// definition.
func differ(before, after []world.Component) Difference {
	old, now := map[world.Key]world.Component{}, map[world.Key]bool{}
	for _, c := range before {
		old[c.Key] = c
	}
	var d Difference
	for _, c := range after {
		now[c.Key] = true
		if prev, ok := old[c.Key]; !ok {
			d.Added = append(d.Added, c)
		} else if prev.Version != c.Version {
			d.Changed = append(d.Changed, Change{From: prev, To: c})
		}
	}
	for _, c := range before {
		if !now[c.Key] {
			d.Removed = append(d.Removed, c)
		}
	}
	return d
}

// firstDifference names the first place two states part.
func firstDifference(got, want []world.Component) string {
	for i := 0; i < len(got) && i < len(want); i++ {
		if label(got[i]) != label(want[i]) {
			return fmt.Sprintf("entry %d of %d/%d: got %s, live %s", i, len(got), len(want), label(got[i]), label(want[i]))
		}
	}
	if len(got) != len(want) {
		return fmt.Sprintf("got %d components, live %d", len(got), len(want))
	}
	return "same keys and versions, different content"
}
