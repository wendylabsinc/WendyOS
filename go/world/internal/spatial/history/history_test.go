package history

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/spatial/grid"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

// handBuilt journals a history small enough to reason about by hand. Times
// are seconds after the test epoch; d1 moves along x so that its x names the
// version.
//
//	applied  key  version  sample  ttl
//	   10    d1      1        9     5
//	   20    d1      2       19     5
//	   21    d2      1        8   100    sampled before anything of d1
//	   30    d1      4       15     5    newer version, older sample
//	   31    d1      3       25     5    late arrival of a lower version
//	   40    d2      2       39   100    tombstone
func handBuilt(t *testing.T, j *Journal, clock *fakeClock) (d1, d2 world.Key) {
	t.Helper()
	b1 := poseAt(t, testScope, "d2", 1, at(8), 100*time.Second, 0, 7)
	steps := []struct {
		applied float64
		c       world.Component
	}{
		{10, poseAt(t, testScope, "d1", 1, at(9), 5*time.Second, 1, 0)},
		{20, poseAt(t, testScope, "d1", 2, at(19), 5*time.Second, 2, 0)},
		{21, b1},
		{30, poseAt(t, testScope, "d1", 4, at(15), 5*time.Second, 4, 0)},
		{31, poseAt(t, testScope, "d1", 3, at(25), 5*time.Second, 3, 0)},
		{40, tombstoneOf(b1, 2, at(39))},
	}
	for _, s := range steps {
		clock.t = at(s.applied)
		mustAppend(t, j, s.c)
	}
	return steps[0].c.Key, b1.Key
}

// short names a state as "<device>@<sequence>" per key.
func short(cs []world.Component) string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = fmt.Sprintf("%s@%d", strings.TrimPrefix(c.Key.Entity, "device/"), c.Version.Sequence)
	}
	return strings.Join(out, " ")
}

func TestAsOfHandBuilt(t *testing.T) {
	clock := &fakeClock{t: at(0)}
	j := mustOpen(t, t.TempDir(), Config{Now: clock.now})
	handBuilt(t, j, clock)

	cases := []struct {
		at   float64
		view View
		want string
	}{
		// What the node believed: versions in the order they arrived.
		{9.9, View{Axis: ApplyTime}, ""},
		{10, View{Axis: ApplyTime}, "d1@1"},
		{20, View{Axis: ApplyTime}, "d1@2"},
		{21, View{Axis: ApplyTime}, "d1@2 d2@1"},
		{30, View{Axis: ApplyTime}, "d1@4 d2@1"},
		{31, View{Axis: ApplyTime}, "d1@4 d2@1"}, // version 3 arrives obsolete
		{40, View{Axis: ApplyTime}, "d1@4"},      // d2 tombstoned
		{1e6, View{Axis: ApplyTime}, "d1@4"},
		// Applied at 10 with a sample already 1 s old: 4 s of life left.
		{13.999, View{Axis: ApplyTime, HonourTTL: true}, "d1@1"},
		{14, View{Axis: ApplyTime, HonourTTL: true}, ""},
		{23.999, View{Axis: ApplyTime, HonourTTL: true}, "d1@2 d2@1"},
		// Version 4 arrives 15 s after its sample with a 5 s life: never live,
		// and it hides version 2 all the same, as in a replica.
		{30, View{Axis: ApplyTime, HonourTTL: true}, "d2@1"},
		// What held when: versions by the time they describe.
		{7.9, View{Axis: SampleTime}, ""},
		{8, View{Axis: SampleTime}, "d2@1"},
		{9, View{Axis: SampleTime}, "d1@1 d2@1"},
		{15, View{Axis: SampleTime}, "d1@4 d2@1"},
		{19, View{Axis: SampleTime}, "d1@4 d2@1"}, // version 2 is sampled later but ranks lower
		{25, View{Axis: SampleTime}, "d1@4 d2@1"},
		{39, View{Axis: SampleTime}, "d1@4"},
		{13.999, View{Axis: SampleTime, HonourTTL: true}, "d1@1 d2@1"},
		{14, View{Axis: SampleTime, HonourTTL: true}, "d2@1"}, // 9 + 5, exclusive
		{15, View{Axis: SampleTime, HonourTTL: true}, "d1@4 d2@1"},
		{20, View{Axis: SampleTime, HonourTTL: true}, "d2@1"},
	}
	for _, tc := range cases {
		got, err := j.AsOf(at(tc.at), tc.view)
		if err != nil {
			t.Fatalf("as of %g %+v: %v", tc.at, tc.view, err)
		}
		if short(got) != tc.want {
			t.Errorf("as of %g %+v: got %q, want %q", tc.at, tc.view, short(got), tc.want)
		}
	}
}

func TestTimelineDiffTrajectoryHandBuilt(t *testing.T) {
	clock := &fakeClock{t: at(0)}
	j := mustOpen(t, t.TempDir(), Config{Now: clock.now})
	d1, d2 := handBuilt(t, j, clock)

	timeline := func(key world.Key, from, to float64, axis Axis) string {
		recs, err := j.Timeline(key, at(from), at(to), axis)
		if err != nil {
			t.Fatal(err)
		}
		return short(components(recs))
	}
	// Version 3 never took effect on either axis; version 2 never did by
	// sample time, because version 4 describes an earlier instant.
	for _, tc := range []struct {
		key      world.Key
		from, to float64
		axis     Axis
		want     string
	}{
		{d1, 0, 100, ApplyTime, "d1@1 d1@2 d1@4"},
		{d1, 0, 100, SampleTime, "d1@1 d1@4"},
		{d1, 20, 30, ApplyTime, "d1@2 d1@4"}, // both ends inclusive
		{d1, 20.001, 29.999, ApplyTime, ""},
		{d2, 0, 100, ApplyTime, "d2@1 d2@2"}, // the tombstone is an entry
		{d2, 0, 100, SampleTime, "d2@1 d2@2"},
	} {
		if got := timeline(tc.key, tc.from, tc.to, tc.axis); got != tc.want {
			t.Errorf("timeline %s [%g, %g] axis %d: got %q, want %q", tc.key.Entity, tc.from, tc.to, tc.axis, got, tc.want)
		}
	}
	if recs, err := j.Timeline(world.Key{Entity: "device/nobody", Type: spatial.TypeDevicePose}, at(0), at(100), ApplyTime); err != nil || len(recs) != 0 {
		t.Errorf("timeline of an unknown key: %d records, %v", len(recs), err)
	}

	fixes, err := j.Trajectory(d1, at(0), at(100), SampleTime)
	if err != nil {
		t.Fatal(err)
	}
	if len(fixes) != 2 || fixes[0].Entity.Position.X != 1 || fixes[1].Entity.Position.X != 4 ||
		!fixes[0].Time.Equal(at(9)) || !fixes[1].Time.Equal(at(15)) {
		t.Errorf("trajectory by sample time: %+v", fixes)
	}
	for _, f := range fixes {
		if f.Entity.UncertaintyM != 0.05 || !f.Entity.HasYaw || f.Entity.Kind != spatial.EntityDevice {
			t.Errorf("fix lost the published pose or its uncertainty: %+v", f.Entity)
		}
	}
	// The tombstone ends d2's path and is not a point on it.
	if fixes, err := j.Trajectory(d2, at(0), at(100), ApplyTime); err != nil || len(fixes) != 1 || !fixes[0].Time.Equal(at(21)) {
		t.Errorf("trajectory of a tombstoned key: %+v, %v", fixes, err)
	}

	diff := func(from, to float64, v View) string {
		d, err := j.Diff(at(from), at(to), v)
		if err != nil {
			t.Fatal(err)
		}
		changed := make([]string, len(d.Changed))
		for i, c := range d.Changed {
			changed[i] = short([]world.Component{c.From}) + ">" + short([]world.Component{c.To})
		}
		return "+[" + short(d.Added) + "] -[" + short(d.Removed) + "] ~[" + strings.Join(changed, " ") + "]"
	}
	for _, tc := range []struct {
		from, to float64
		view     View
		want     string
	}{
		{9.9, 21, View{Axis: ApplyTime}, "+[d1@2 d2@1] -[] ~[]"},
		{21, 40, View{Axis: ApplyTime}, "+[] -[d2@1] ~[d1@2>d1@4]"},
		{30, 31, View{Axis: ApplyTime}, "+[] -[] ~[]"},
		{9, 14, View{Axis: SampleTime, HonourTTL: true}, "+[] -[d1@1] ~[]"}, // expiry is a removal
		{9, 14, View{Axis: SampleTime}, "+[] -[] ~[]"},
	} {
		if got := diff(tc.from, tc.to, tc.view); got != tc.want {
			t.Errorf("diff %g to %g %+v: got %s, want %s", tc.from, tc.to, tc.view, got, tc.want)
		}
	}

	// A second source publishes its own pose of d1: one entity, two keys.
	other := testScope
	other.Source.Stream = "second"
	clock.t = at(50)
	mustAppend(t, j, poseAt(t, other, "d1", 1, at(12), time.Minute, 9, 9))
	recs, err := j.EntityTimeline(testScope.Tenant, testScope.Site, "device/d1", at(0), at(100), SampleTime)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := labels(components(recs)), "state|wendy.spatial.device_pose.v1|device/d1@1.1 second|wendy.spatial.device_pose.v1|device/d1@1.1 state|wendy.spatial.device_pose.v1|device/d1@1.4"; got != want {
		t.Errorf("entity timeline:\n got %s\nwant %s", got, want)
	}
}

func TestAppendContract(t *testing.T) {
	clock := &fakeClock{t: at(10)}
	dir := t.TempDir()
	j := mustOpen(t, dir, Config{Now: clock.now})
	c := poseAt(t, testScope, "d1", 5, at(9), time.Minute, 1, 1)
	mustAppend(t, j, c)

	// The same record again is a no-op, whatever its transit age hint.
	again := c
	again.Timing.AgeHint = time.Second
	if appended, err := j.Append(again); appended || err != nil {
		t.Errorf("duplicate: appended=%v err=%v", appended, err)
	}
	if _, err := j.Append(poseAt(t, testScope, "d1", 5, at(9), time.Minute, 2, 2)); !errors.Is(err, ErrConflict) {
		t.Errorf("same version, other content: %v, want ErrConflict", err)
	}
	for name, mutate := range map[string]func(*world.Component){
		"no entity":           func(c *world.Component) { c.Key.Entity = "" },
		"no type":             func(c *world.Component) { c.Key.Type = "" },
		"zero version":        func(c *world.Component) { c.Version = world.Version{} },
		"no ttl":              func(c *world.Component) { c.TTL = 0 },
		"no sample time":      func(c *world.Component) { c.Timing.SampleTime = time.Time{} },
		"payload not json":    func(c *world.Component) { c.Payload = []byte("{") },
		"tombstone with body": func(c *world.Component) { c.Tombstone = true },
	} {
		bad := poseAt(t, testScope, "d1", 6, at(9), time.Minute, 1, 1)
		mutate(&bad)
		if _, err := j.Append(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	clock.t = at(9)
	if _, err := j.Append(poseAt(t, testScope, "d1", 6, at(9), time.Minute, 1, 1)); !errors.Is(err, ErrClockRegression) {
		t.Errorf("clock moved back: %v, want ErrClockRegression", err)
	}
	if st := j.Stats(); st.Records != 1 || st.Keys != 1 || !st.LastApplied.Equal(at(10)) {
		t.Errorf("refused appends left a trace: %+v", st)
	}

	if _, err := j.AsOf(at(10), View{Axis: 7}); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown axis: %v", err)
	}
	if _, err := j.AsOf(time.Time{}, View{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("zero time: %v", err)
	}
	if _, err := j.Diff(at(10), at(9), View{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("reversed range: %v", err)
	}

	// The clock contract holds across a restart.
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := j.Append(c); !errors.Is(err, ErrClosed) {
		t.Errorf("append after close: %v", err)
	}
	if _, err := j.AsOf(at(10), View{}); !errors.Is(err, ErrClosed) {
		t.Errorf("query after close: %v", err)
	}
	j = mustOpen(t, dir, Config{Now: clock.now})
	if _, err := j.Append(poseAt(t, testScope, "d1", 6, at(9), time.Minute, 1, 1)); !errors.Is(err, ErrClockRegression) {
		t.Errorf("clock behind the journal after reopen: %v, want ErrClockRegression", err)
	}
	clock.t = at(11)
	if appended, err := j.Append(c); appended || err != nil {
		t.Errorf("replay after reopen: appended=%v err=%v", appended, err)
	}

	for _, cfg := range []Config{{}, {Now: clock.now, SegmentBytes: -1}, {Now: clock.now, Retention: -1}} {
		if _, err := Open(t.TempDir(), cfg); !errors.Is(err, ErrInvalid) {
			t.Errorf("open with %+v: %v, want ErrInvalid", cfg, err)
		}
	}
	if _, err := Open("", Config{Now: clock.now}); !errors.Is(err, ErrInvalid) {
		t.Errorf("open without a directory: %v", err)
	}
}

func TestTrajectoryRefusesWhatHasNoPose(t *testing.T) {
	clock := &fakeClock{t: at(10)}
	j := mustOpen(t, t.TempDir(), Config{Now: clock.now})

	g, err := grid.New(grid.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Set(grid.Cell{IX: 3, IZ: 4}, 1); err != nil {
		t.Fatal(err)
	}
	tile, err := g.Tile("ground", 0, 0, 16, 0, map[string]uint64{"d1": 1})
	if err != nil {
		t.Fatal(err)
	}
	timing := world.Timing{SampleTime: at(9), ClockDomain: testDomain, ErrorKnown: true}
	tileComponent, err := spatial.ToComponent(testScope, world.Version{Epoch: 1, Sequence: 1}, testFrame, timing, time.Minute, testProvenance(t, "occupancy", "d1/lidar/lidar2d/1"), tile)
	if err != nil {
		t.Fatal(err)
	}
	mustAppend(t, j, tileComponent)
	foreign := world.Component{
		Key:     world.Key{Tenant: "tenant", Site: "site", Entity: "thing/1", Type: "example.thing.v1", Source: testScope.Source},
		Version: world.Version{Epoch: 1, Sequence: 1}, Frame: testFrame, Timing: timing, TTL: time.Minute, Payload: []byte(`{"a": "<b>"}`),
	}
	mustAppend(t, j, foreign)

	for _, key := range []world.Key{tileComponent.Key, foreign.Key} {
		if _, err := j.Trajectory(key, at(0), at(100), SampleTime); !errors.Is(err, ErrNotPose) {
			t.Errorf("trajectory of %s: %v, want ErrNotPose", key.Type, err)
		}
	}
	// A payload the spatial layer does not know is journaled and returned
	// in canonical form all the same.
	got, err := j.AsOf(at(10), View{Axis: ApplyTime})
	if err != nil {
		t.Fatal(err)
	}
	if want := []world.Component{canon(t, foreign), canon(t, tileComponent)}; !sameState(got, want) {
		t.Errorf("state: got %s, want %s", labels(got), labels(want))
	}
}

// journalFiles lists the files of a journal directory with one suffix, in
// name order.
func journalFiles(t *testing.T, dir, suffix string) []string {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "*"+suffix))
	if err != nil {
		t.Fatal(err)
	}
	return names
}

// fiveRecords journals five poses in one segment and closes the journal. It
// returns the segment path and the offset of each frame.
func fiveRecords(t *testing.T, dir string, clock *fakeClock) (path string, offsets []int64, want []world.Component) {
	t.Helper()
	j := mustOpen(t, dir, Config{Now: clock.now})
	for i := 1; i <= 5; i++ {
		clock.t = at(float64(i))
		c := poseAt(t, testScope, fmt.Sprintf("d%d", i), 1, at(float64(i)), time.Hour, float64(i), 0)
		mustAppend(t, j, c)
		e := j.keys[c.Key].entries[0]
		offsets = append(offsets, e.off)
		want = append(want, canon(t, c))
	}
	path = j.active().path
	offsets = append(offsets, j.active().size)
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	return path, offsets, want
}

func TestTornTailIsTruncated(t *testing.T) {
	next := appendFrame(nil, []byte(`{"seq":6,"applied":"2026-01-01T00:00:06Z","component":{}}`))
	cases := []struct {
		name    string
		damage  func(data []byte, offsets []int64) []byte
		records int
	}{
		{"partial header", func(d []byte, _ []int64) []byte { return append(d, next[:5]...) }, 5},
		{"header without body", func(d []byte, _ []int64) []byte { return append(d, next[:frameHeader]...) }, 5},
		{"partial body", func(d []byte, _ []int64) []byte { return append(d, next[:len(next)-3]...) }, 5},
		{"zero fill", func(d []byte, _ []int64) []byte { return append(d, make([]byte, 4096)...) }, 5},
		{"last frame cut short", func(d []byte, o []int64) []byte { return d[:o[5]-7] }, 4},
		// A final frame of the right length that fails its checksum is what
		// a crash mid-write leaves when the file was extended first.
		{"last frame damaged", func(d []byte, o []int64) []byte { d[o[5]-2] ^= 0xff; return d }, 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			clock := &fakeClock{t: at(0)}
			path, offsets, want := fiveRecords(t, dir, clock)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			damaged := tc.damage(data, offsets)
			if err := os.WriteFile(path, damaged, 0o600); err != nil {
				t.Fatal(err)
			}

			j := mustOpen(t, dir, Config{Now: clock.now})
			st := j.Stats()
			if st.Records != tc.records || st.TruncatedBytes != int64(len(damaged))-offsets[tc.records] {
				t.Fatalf("after recovery: %+v, want %d records and %d bytes truncated", st, tc.records, int64(len(damaged))-offsets[tc.records])
			}
			got, err := j.AsOf(at(100), View{Axis: ApplyTime})
			if err != nil || !sameState(got, want[:tc.records]) {
				t.Fatalf("state after recovery: %s, %v", labels(got), err)
			}
			// The journal continues where the last whole record ended.
			clock.t = at(50)
			more := poseAt(t, testScope, "d9", 1, at(50), time.Hour, 9, 9)
			mustAppend(t, j, more)
			if err := j.Close(); err != nil {
				t.Fatal(err)
			}
			j = mustOpen(t, dir, Config{Now: clock.now})
			if st := j.Stats(); st.Records != tc.records+1 || st.TruncatedBytes != 0 {
				t.Fatalf("after reopen: %+v", st)
			}
			got, err = j.AsOf(at(100), View{Axis: ApplyTime})
			if err != nil || !sameState(got, append(want[:tc.records:tc.records], canon(t, more))) {
				t.Fatalf("state after continuing: %s, %v", labels(got), err)
			}
		})
	}
}

func TestCorruptionIsAnErrorNotLoss(t *testing.T) {
	t.Run("body damaged mid-segment", func(t *testing.T) {
		dir := t.TempDir()
		clock := &fakeClock{t: at(0)}
		path, offsets, _ := fiveRecords(t, dir, clock)
		flip(t, path, offsets[1]+frameHeader+20)
		if _, err := Open(dir, Config{Now: clock.now}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("open: %v, want ErrCorrupt", err)
		}
	})
	t.Run("length damaged mid-segment", func(t *testing.T) {
		// Without a checksum on the length this would read as a frame that
		// runs past the end of the file: a torn tail swallowing three records.
		dir := t.TempDir()
		clock := &fakeClock{t: at(0)}
		path, offsets, _ := fiveRecords(t, dir, clock)
		flip(t, path, offsets[1]+2)
		if _, err := Open(dir, Config{Now: clock.now}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("open: %v, want ErrCorrupt", err)
		}
	})
	t.Run("garbage after the last record", func(t *testing.T) {
		dir := t.TempDir()
		clock := &fakeClock{t: at(0)}
		path, _, _ := fiveRecords(t, dir, clock)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("this is not a frame header at all")); err != nil {
			t.Fatal(err)
		}
		f.Close()
		if _, err := Open(dir, Config{Now: clock.now}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("open: %v, want ErrCorrupt", err)
		}
	})

	// One record per segment, so every file but the last is sealed.
	sealed := func(t *testing.T) (dir string, clock *fakeClock, segments []string) {
		dir, clock = t.TempDir(), &fakeClock{t: at(0)}
		j := mustOpen(t, dir, Config{Now: clock.now, SegmentBytes: 1})
		for i := 1; i <= 4; i++ {
			clock.t = at(float64(i))
			mustAppend(t, j, poseAt(t, testScope, "d1", uint64(i), at(float64(i)), time.Hour, float64(i), 0))
		}
		if err := j.Close(); err != nil {
			t.Fatal(err)
		}
		segments = journalFiles(t, dir, segmentSuffix)
		if len(segments) != 4 {
			t.Fatalf("%d segments, want 4", len(segments))
		}
		return dir, clock, segments
	}
	t.Run("sealed segment cut short", func(t *testing.T) {
		dir, clock, segments := sealed(t)
		if err := os.Truncate(segments[1], 40); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, Config{Now: clock.now}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("open: %v, want ErrCorrupt", err)
		}
	})
	t.Run("segment missing", func(t *testing.T) {
		dir, clock, segments := sealed(t)
		if err := os.Remove(segments[1]); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, Config{Now: clock.now}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("open: %v, want ErrCorrupt", err)
		}
	})
	t.Run("damage found when a record is read", func(t *testing.T) {
		dir, clock, segments := sealed(t)
		j := mustOpen(t, dir, Config{Now: clock.now, SegmentBytes: 1})
		flip(t, segments[1], frameHeader+20)
		if _, err := j.AsOf(at(2), View{Axis: ApplyTime}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("as of the damaged record: %v, want ErrCorrupt", err)
		}
		if got, err := j.AsOf(at(3), View{Axis: ApplyTime}); err != nil || short(got) != "d1@3" {
			t.Fatalf("as of an intact record: %s, %v", short(got), err)
		}
	})
}

func flip(t *testing.T, path string, off int64) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var b [1]byte
	if _, err := f.ReadAt(b[:], off); err != nil {
		t.Fatal(err)
	}
	b[0] ^= 0x40
	if _, err := f.WriteAt(b[:], off); err != nil {
		t.Fatal(err)
	}
}

// compactable journals 3 devices over 100 s, one record per segment: each
// device publishes every 4 s with a 6 s lifetime, d2 is tombstoned at 30 s
// and d3 goes quiet after 20 s (so its last record must survive as the base).
func compactable(t *testing.T, j *Journal, clock *fakeClock) model {
	t.Helper()
	var m model
	add := func(c world.Component) {
		mustAppend(t, j, c)
		m = append(m, Record{Seq: uint64(len(m) + 1), Applied: clock.t, Component: canon(t, c)})
	}
	sequence := map[string]uint64{}
	for s := 0.0; s <= 100; s += 4 {
		clock.t = at(s + 0.5)
		for _, dev := range []string{"d1", "d2", "d3"} {
			if (dev == "d2" && s > 30) || (dev == "d3" && s > 20) {
				continue
			}
			sequence[dev]++
			c := poseAt(t, testScope, dev, sequence[dev], at(s), 6*time.Second, s, 0)
			if dev == "d2" && s > 26 {
				c = tombstoneOf(c, sequence[dev], at(s))
			}
			add(c)
		}
	}
	return m
}

// checkAgainstModel compares as-of on both axes at every half second from
// the horizon on, and requires ErrCompacted before it.
func checkAgainstModel(t *testing.T, j *Journal, m model, horizon float64) {
	t.Helper()
	for s := 0.0; s <= 110; s += 0.5 {
		for _, v := range []View{{Axis: ApplyTime}, {Axis: SampleTime}, {Axis: SampleTime, HonourTTL: true}} {
			got, err := j.AsOf(at(s), v)
			if s < horizon {
				if !errors.Is(err, ErrCompacted) {
					t.Fatalf("as of %g, before the horizon: %v, want ErrCompacted", s, err)
				}
				continue
			}
			if want := m.asOf(at(s), v); err != nil || !sameState(got, want) {
				t.Fatalf("as of %g %+v: got %q (%v), want %q", s, v, short(got), err, short(want))
			}
		}
	}
}

func TestCompaction(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{t: at(0)}
	cfg := Config{Now: clock.now, SegmentBytes: 1}
	j := mustOpen(t, dir, cfg)
	m := compactable(t, j, clock)
	checkAgainstModel(t, j, m, 0)
	before := j.Stats()

	const horizon = 50
	res, err := j.Compact(at(horizon))
	if err != nil {
		t.Fatal(err)
	}
	after := j.Stats()
	// What a query at or after 50 s can still see of the first 50 s: d1's
	// record in force, d2's tombstone, d3's last word.
	if res.Kept != 3 || res.Dropped == 0 || res.Segments == 0 || !res.Horizon.Equal(at(horizon)) || !after.Horizon.Equal(at(horizon)) {
		t.Fatalf("compaction: %+v, stats %+v", res, after)
	}
	if after.Records != before.Records-res.Dropped || after.Bytes >= before.Bytes || after.Segments != before.Segments-res.Segments || after.Keys != before.Keys {
		t.Fatalf("stats before %+v, after %+v, compaction %+v", before, after, res)
	}
	if n := len(journalFiles(t, dir, snapshotSuffix)); n != 1 {
		t.Fatalf("%d snapshot files, want 1", n)
	}
	checkAgainstModel(t, j, m, horizon)

	d1 := poseAt(t, testScope, "d1", 1, at(0), time.Second, 0, 0).Key
	if _, err := j.Timeline(d1, at(horizon-1), at(100), ApplyTime); !errors.Is(err, ErrCompacted) {
		t.Errorf("timeline from before the horizon: %v", err)
	}
	if _, err := j.Trajectory(d1, at(horizon-1), at(100), SampleTime); !errors.Is(err, ErrCompacted) {
		t.Errorf("trajectory from before the horizon: %v", err)
	}
	if _, err := j.Diff(at(horizon-1), at(100), View{}); !errors.Is(err, ErrCompacted) {
		t.Errorf("diff from before the horizon: %v", err)
	}
	for _, axis := range []Axis{ApplyTime, SampleTime} {
		got, err := j.Timeline(d1, at(horizon), at(100), axis)
		if want := m.timeline(d1, at(horizon), at(100), axis); err != nil || !sameRecords(got, want) {
			t.Errorf("timeline in the retained range, axis %d: got %s (%v), want %s", axis, short(components(got)), err, short(components(want)))
		}
	}

	// An earlier horizon is a no-op; the horizon never moves back.
	if res, err := j.Compact(at(horizon - 10)); err != nil || res.Dropped != 0 || !res.Horizon.Equal(at(horizon)) {
		t.Errorf("compacting to an earlier horizon: %+v, %v", res, err)
	}

	// A tombstone collapsed into the snapshot still outranks a lower version
	// that turns up afterwards.
	clock.t = at(120)
	late := poseAt(t, testScope, "d2", 1, at(0), time.Hour, 0, 0)
	if _, err := j.Append(late); err != nil {
		t.Fatal(err)
	}
	m = append(m, Record{Seq: uint64(len(m) + 1), Applied: clock.t, Component: canon(t, late)})
	checkAgainstModel(t, j, m, horizon)

	// Restart, with the debris of a compaction that crashed after its rename:
	// a stale older snapshot, a covered segment and an abandoned temporary.
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	snapshot, err := os.ReadFile(journalFiles(t, dir, snapshotSuffix)[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{fileName(snapshotPrefix, 0, snapshotSuffix), fileName(segmentPrefix, 1, segmentSuffix), "snap-x" + tmpSuffix} {
		if err := os.WriteFile(filepath.Join(dir, name), snapshot, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	j = mustOpen(t, dir, cfg)
	if st := j.Stats(); st.Records != after.Records+1 || !st.Horizon.Equal(at(horizon)) || !st.LastApplied.Equal(at(120)) {
		t.Fatalf("stats after reopen: %+v", st)
	}
	if n := len(journalFiles(t, dir, snapshotSuffix)) + len(journalFiles(t, dir, tmpSuffix)); n != 1 {
		t.Fatalf("%d snapshot and temporary files after reopen, want 1", n)
	}
	checkAgainstModel(t, j, m, horizon)

	// Compacting again folds the old snapshot into the new one, and a
	// horizon past every record leaves exactly the final state.
	if res, err := j.Compact(at(200)); err != nil || res.Kept != 3 {
		t.Fatalf("second compaction: %+v, %v", res, err)
	}
	if st := j.Stats(); st.Records != 3 || st.Segments != 1 {
		t.Fatalf("stats after collapsing everything: %+v", st)
	}
	got, err := j.AsOf(at(200), View{Axis: ApplyTime})
	if want := m.asOf(at(200), View{Axis: ApplyTime}); err != nil || !sameState(got, want) {
		t.Fatalf("final state: got %q (%v), want %q", short(got), err, short(want))
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	j = mustOpen(t, dir, cfg)
	// The newest apply time outlives the records that carried it.
	if st := j.Stats(); st.Records != 3 || !st.LastApplied.Equal(at(120)) {
		t.Fatalf("stats after the last reopen: %+v", st)
	}
	clock.t = at(119)
	if _, err := j.Append(poseAt(t, testScope, "d1", 999, at(119), time.Hour, 0, 0)); !errors.Is(err, ErrClockRegression) {
		t.Errorf("clock behind a compacted journal: %v, want ErrClockRegression", err)
	}
}

func TestCorruptSnapshotIsAnError(t *testing.T) {
	dir := t.TempDir()
	clock := &fakeClock{t: at(0)}
	cfg := Config{Now: clock.now, SegmentBytes: 1}
	j := mustOpen(t, dir, cfg)
	compactable(t, j, clock)
	if _, err := j.Compact(at(50)); err != nil {
		t.Fatal(err)
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	path := journalFiles(t, dir, snapshotSuffix)[0]
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// A snapshot is written whole, so even a short one is damage.
	if err := os.Truncate(path, st.Size()-5); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, cfg); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("open with a short snapshot: %v, want ErrCorrupt", err)
	}
}

func TestRetentionCompactsAsSegmentsFill(t *testing.T) {
	const retention = 30 * time.Second
	dir := t.TempDir()
	clock := &fakeClock{t: at(0)}
	j := mustOpen(t, dir, Config{Now: clock.now, SegmentBytes: 4 << 10, Retention: retention})
	m := compactable(t, j, clock)
	st := j.Stats()
	// The horizon trails the newest record by the retention, to within the
	// segment that was filling when it last moved.
	if st.Horizon.IsZero() || st.Horizon.After(st.LastApplied.Add(-retention)) || st.Horizon.Before(st.LastApplied.Add(-2*retention)) {
		t.Fatalf("horizon %v with the newest record at %v", st.Horizon.Sub(testEpoch), st.LastApplied.Sub(testEpoch))
	}
	if st.Records >= len(m) || len(journalFiles(t, dir, snapshotSuffix)) != 1 {
		t.Fatalf("nothing was collapsed: %+v of %d records", st, len(m))
	}
	checkAgainstModel(t, j, m, st.Horizon.Sub(testEpoch).Seconds())
}

func TestConcurrentReaders(t *testing.T) {
	const records, readers = 400, 4
	clock := &fakeClock{t: at(0)}
	j := mustOpen(t, t.TempDir(), Config{Now: clock.now, SegmentBytes: 16 << 10})
	key := poseAt(t, testScope, "d1", 1, at(0), time.Second, 0, 0).Key

	done := make(chan struct{})
	var wg sync.WaitGroup
	failures := make(chan error, readers)
	for r := 0; r < readers; r++ {
		wg.Go(func() {
			for {
				select {
				case <-done:
					return
				default:
				}
				// A reader sees a prefix of the history: whatever it gets
				// for a fixed past instant must be that instant's state, or
				// the typed refusal once the writer has compacted past it.
				st := j.Stats()
				got, err := j.AsOf(at(10), View{Axis: ApplyTime})
				switch {
				case errors.Is(err, ErrCompacted):
				case err != nil:
					failures <- err
					return
				case st.LastApplied.After(at(10)) && short(got) != "d1@10":
					failures <- fmt.Errorf("as of 10 s: %q", short(got))
					return
				}
				if _, err := j.Timeline(key, st.Horizon.Add(time.Hour), st.Horizon.Add(2*time.Hour), SampleTime); err != nil && !errors.Is(err, ErrCompacted) {
					failures <- err
					return
				}
				if _, err := j.Diff(st.Horizon.Add(time.Hour), st.Horizon.Add(2*time.Hour), View{Axis: SampleTime, HonourTTL: true}); err != nil && !errors.Is(err, ErrCompacted) {
					failures <- err
					return
				}
			}
		})
	}
	for i := 1; i <= records; i++ {
		clock.t = at(float64(i))
		mustAppend(t, j, poseAt(t, testScope, "d1", uint64(i), at(float64(i)), time.Hour, float64(i), 0))
		if i == records/2 {
			if _, err := j.Compact(at(float64(i - 20))); err != nil {
				t.Fatal(err)
			}
		}
	}
	close(done)
	wg.Wait()
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}
	if got, err := j.AsOf(at(records), View{Axis: ApplyTime}); err != nil || short(got) != fmt.Sprintf("d1@%d", records) {
		t.Fatalf("final state: %q, %v", short(got), err)
	}
}

// The apply-axis lifetime is the replica's: TTL less the sample's age at
// application, its clock error bound and its transit age hint. Each case is
// applied to a world.Replica and the journal at 10 s and both are probed
// around the instant the record should expire.
func TestApplyAxisExpiryMatchesReplica(t *testing.T) {
	cases := []struct {
		name    string
		sample  float64
		ttl     time.Duration
		bound   time.Duration
		hint    time.Duration
		expires float64
	}{
		{"fresh", 10, 5 * time.Second, 0, 0, 15},
		{"aged in transit", 9, 5 * time.Second, 0, 0, 14},
		{"clock error bound", 9, 5 * time.Second, 2 * time.Second, 0, 12},
		{"age hint above the measured age", 9, 5 * time.Second, 0, 3 * time.Second, 12},
		{"age hint below the measured age", 7, 5 * time.Second, 0, time.Second, 12},
		{"sample ahead of the clock", 11, 5 * time.Second, 0, 0, 15},
		{"dead on arrival", 2, 5 * time.Second, 0, 0, 10},
	}
	clock := &fakeClock{t: at(10)}
	replica, err := world.NewReplica(world.Config{
		Tenant: testScope.Tenant, Site: testScope.Site, ClockDomain: testDomain, Now: clock.now,
		MaxFutureSkew: 5 * time.Second, MaxEntries: 64, AllowSynthetic: true, Frames: []world.FrameRef{testFrame},
	})
	if err != nil {
		t.Fatal(err)
	}
	j := mustOpen(t, t.TempDir(), Config{Now: clock.now})
	expires := map[string]float64{}
	for i, tc := range cases {
		device := fmt.Sprintf("d%d", i)
		c := poseAt(t, testScope, device, 1, at(tc.sample), tc.ttl, 0, 0)
		c.Timing.ErrorBound, c.Timing.AgeHint = tc.bound, tc.hint
		mustAppend(t, j, c)
		if _, err := replica.Apply(c.Key.Source, c); err != nil {
			t.Fatal(err)
		}
		expires[device] = tc.expires
	}
	for s := 10.0; s <= 16; s += 0.5 {
		clock.t = at(s)
		want, err := replica.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		got, err := j.AsOf(at(s), View{Axis: ApplyTime, HonourTTL: true})
		if err != nil || !sameState(got, want) {
			t.Errorf("as of %g: got %q (%v), replica holds %q", s, short(got), err, short(want))
		}
		for _, c := range got {
			if device := strings.TrimPrefix(c.Key.Entity, "device/"); s >= expires[device] {
				t.Errorf("as of %g: %s is live past %g", s, device, expires[device])
			}
		}
		if alive := len(got); alive != countBelow(expires, s) {
			t.Errorf("as of %g: %d live, want %d", s, alive, countBelow(expires, s))
		}
	}
}

// countBelow counts the devices whose expiry is after s.
func countBelow(expires map[string]float64, s float64) int {
	n := 0
	for _, e := range expires {
		if s < e {
			n++
		}
	}
	return n
}

// A record applied before the horizon but sampled after it is still owed to
// sample-time queries, even when it is the state at the horizon on neither
// axis: here version 2, outranked by version 3 on arrival and not yet
// current by sample time.
//
//	applied  version  sample
//	   90       1       90
//	   98       2      101      stamped ahead of the clock
//	   99       3      103      stamped further ahead
func TestCompactionKeepsWhatIsSampledAfterTheHorizon(t *testing.T) {
	clock := &fakeClock{t: at(0)}
	j := mustOpen(t, t.TempDir(), Config{Now: clock.now, SegmentBytes: 1})
	var m model
	for _, s := range []struct {
		applied, sample float64
		version         uint64
	}{{90, 90, 1}, {98, 101, 2}, {99, 103, 3}, {120, 120, 9}} {
		clock.t = at(s.applied)
		c := poseAt(t, testScope, "d1", s.version, at(s.sample), time.Hour, float64(s.version), 0)
		if s.version == 9 {
			c = poseAt(t, testScope, "d2", 1, at(s.sample), time.Hour, 0, 0) // only pushes the others into sealed segments
		}
		mustAppend(t, j, c)
		m = append(m, Record{Seq: uint64(len(m) + 1), Applied: clock.t, Component: canon(t, c)})
	}
	res, err := j.Compact(at(100))
	if err != nil {
		t.Fatal(err)
	}
	if res.Segments != 3 || res.Kept != 3 || res.Dropped != 0 {
		t.Fatalf("compaction: %+v, want all three records of d1 kept", res)
	}
	for _, tc := range []struct {
		at   float64
		want string
	}{{100, "d1@1"}, {101, "d1@2"}, {102.9, "d1@2"}, {103, "d1@3"}, {120, "d1@3 d2@1"}} {
		got, err := j.AsOf(at(tc.at), View{Axis: SampleTime})
		if err != nil || short(got) != tc.want {
			t.Errorf("as of %g by sample time: %q (%v), want %q", tc.at, short(got), err, tc.want)
		}
	}
	checkAgainstModel(t, j, m, 100)
}
