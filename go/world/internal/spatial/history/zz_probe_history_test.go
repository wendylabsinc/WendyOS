package history

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/spatial"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func probeFill(t *testing.T, dir string, clock *fakeClock, n int, segBytes int64) *Journal {
	j := mustOpen(t, dir, Config{Now: clock.now, SegmentBytes: segBytes})
	for i := 1; i <= n; i++ {
		clock.t = at(float64(100 + i))
		mustAppend(t, j, poseAt(t, testScope, fmt.Sprintf("d%d", i%3), uint64(i), clock.t, time.Hour, float64(i), 1))
	}
	return j
}

func probeSegs(t *testing.T, dir string) []string {
	names, _ := filepath.Glob(filepath.Join(dir, "seg-*.seg"))
	sort.Strings(names)
	return names
}

func TestZZProbeStorageDamage(t *testing.T) {
	clock := &fakeClock{t: at(100)}
	build := func() (string, []string) {
		dir := t.TempDir()
		j := probeFill(t, dir, clock, 60, 8<<10)
		if err := j.Close(); err != nil {
			t.Fatal(err)
		}
		return dir, probeSegs(t, dir)
	}
	open := func(dir string) (*Journal, error) {
		j, err := Open(dir, Config{Now: clock.now, SegmentBytes: 8 << 10})
		if err == nil {
			t.Cleanup(func() { j.Close() })
		}
		return j, err
	}

	dir, segs := build()
	t.Logf("PROBE segments=%d", len(segs))
	if len(segs) < 4 {
		t.Fatalf("need several segments, got %d", len(segs))
	}

	// 1. truncate mid-frame in active segment
	last := segs[len(segs)-1]
	st, _ := os.Stat(last)
	if st.Size() < 40 {
		t.Logf("PROBE active segment small: %d", st.Size())
	}
	if err := os.Truncate(last, st.Size()-7); err != nil {
		t.Fatal(err)
	}
	j, err := open(dir)
	if err != nil {
		t.Errorf("PROBE torn tail: open error %v", err)
	} else {
		s := j.Stats()
		t.Logf("PROBE torn tail: records=%d truncated=%d", s.Records, s.TruncatedBytes)
		if s.Records != 59 || s.TruncatedBytes == 0 {
			t.Errorf("PROBE torn tail: unexpected stats %+v", s)
		}
		clock.t = at(500)
		mustAppend(t, j, poseAt(t, testScope, "d0", 1000, clock.t, time.Hour, 1, 1))
		j.Close()
		j2, err := open(dir)
		if err != nil || j2.Stats().Records != 60 || j2.Stats().TruncatedBytes != 0 {
			t.Errorf("PROBE torn tail continue: %v", err)
		} else {
			j2.Close()
		}
	}

	// 2. flip a byte in the middle of the first (sealed) segment
	dir, segs = build()
	flip := func(path string, off int64) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if off < 0 {
			off += int64(len(b))
		}
		b[off] ^= 0x40
		if err := os.WriteFile(path, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	flip(segs[0], 200)
	_, err = open(dir)
	t.Logf("PROBE flip in sealed segment body: err=%v corrupt=%v", err, errors.Is(err, ErrCorrupt))
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("PROBE sealed flip not ErrCorrupt")
	}

	// 3. flip last byte of a sealed segment (final frame of sealed)
	dir, segs = build()
	flip(segs[1], -1)
	_, err = open(dir)
	t.Logf("PROBE flip in final frame of sealed segment: corrupt=%v", errors.Is(err, ErrCorrupt))
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("PROBE sealed final-frame flip not ErrCorrupt: %v", err)
	}

	// 4. flip last byte of the active segment -> declared: silently truncated
	dir, segs = build()
	flip(segs[len(segs)-1], -1)
	j, err = open(dir)
	if err != nil {
		t.Logf("PROBE flip in final frame of active: err=%v", err)
	} else {
		t.Logf("PROBE flip in final frame of active: opened, records=%d truncated=%d (synced record lost silently)", j.Stats().Records, j.Stats().TruncatedBytes)
		j.Close()
	}

	// 5. delete the newest segment(s)
	dir, segs = build()
	os.Remove(segs[len(segs)-1])
	os.Remove(segs[len(segs)-2])
	j, err = open(dir)
	if err != nil {
		t.Logf("PROBE deleted newest two segments: err=%v", err)
	} else {
		t.Logf("PROBE deleted newest two segments: OPENED WITHOUT ERROR, records=%d of 60", j.Stats().Records)
		j.Close()
	}

	// 6. delete a middle segment
	dir, segs = build()
	os.Remove(segs[1])
	_, err = open(dir)
	t.Logf("PROBE deleted middle segment: corrupt=%v", errors.Is(err, ErrCorrupt))
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("PROBE middle segment loss not ErrCorrupt: %v", err)
	}
	// 6b delete first segment
	dir, segs = build()
	os.Remove(segs[0])
	_, err = open(dir)
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("PROBE first segment loss not ErrCorrupt: %v", err)
	}

	// 7. garbage (non-zero) partial header appended at the tail: 5 bytes
	dir, segs = build()
	f, _ := os.OpenFile(segs[len(segs)-1], os.O_WRONLY|os.O_APPEND, 0)
	f.Write([]byte{9, 9, 9, 9, 9})
	f.Close()
	j, err = open(dir)
	if err != nil {
		t.Errorf("PROBE 5 garbage bytes at tail: err=%v", err)
	} else {
		t.Logf("PROBE 5 garbage bytes at tail: truncated=%d records=%d", j.Stats().TruncatedBytes, j.Stats().Records)
		j.Close()
	}
	// 7b. full garbage header (12+ bytes non-zero) at tail
	dir, segs = build()
	f, _ = os.OpenFile(segs[len(segs)-1], os.O_WRONLY|os.O_APPEND, 0)
	f.Write(bytes.Repeat([]byte{0xAB}, 40))
	f.Close()
	_, err = open(dir)
	t.Logf("PROBE 40 garbage bytes at tail: err=%v", err)
	// 7c. zero fill at tail
	dir, segs = build()
	f, _ = os.OpenFile(segs[len(segs)-1], os.O_WRONLY|os.O_APPEND, 0)
	f.Write(make([]byte, 4096))
	f.Close()
	j, err = open(dir)
	if err != nil {
		t.Errorf("PROBE zero fill at tail: err=%v", err)
	} else {
		t.Logf("PROBE zero fill at tail: truncated=%d records=%d", j.Stats().TruncatedBytes, j.Stats().Records)
		j.Close()
	}
	// 7d. a valid whole frame whose body is not a record, at the tail
	dir, segs = build()
	f, _ = os.OpenFile(segs[len(segs)-1], os.O_WRONLY|os.O_APPEND, 0)
	f.Write(appendFrame(nil, []byte(`{"seq":999}`)))
	f.Close()
	_, err = open(dir)
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("PROBE bogus frame: %v", err)
	}

	// 8. snapshot damage after compaction; snapshot removed
	dir, _ = build()
	j, err = open(dir)
	if err != nil {
		t.Fatal(err)
	}
	res, err := j.Compact(at(140))
	t.Logf("PROBE compact: %+v err=%v", res, err)
	j.Close()
	snaps, _ := filepath.Glob(filepath.Join(dir, "snap-*.snap"))
	if len(snaps) != 1 {
		t.Fatalf("snaps %v", snaps)
	}
	flip(snaps[0], -1)
	_, err = open(dir)
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("PROBE snapshot tail flip not ErrCorrupt: %v", err)
	}
	flip(snaps[0], -1) // restore
	if j, err = open(dir); err != nil {
		t.Errorf("PROBE restored snapshot: %v", err)
	} else {
		j.Close()
	}
	os.Remove(snaps[0])
	_, err = open(dir)
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("PROBE snapshot removed not ErrCorrupt: %v", err)
	}
}

func TestZZProbeRefusals(t *testing.T) {
	clock := &fakeClock{t: at(100)}
	dir := t.TempDir()
	j := probeFill(t, dir, clock, 10, 0)
	good := poseAt(t, testScope, "dx", 1, at(105), time.Minute, 1, 1)

	bad := map[string]func(c *world.Component){
		"empty entity":      func(c *world.Component) { c.Key.Entity = "" },
		"empty type":        func(c *world.Component) { c.Key.Type = "" },
		"zero epoch":        func(c *world.Component) { c.Version.Epoch = 0 },
		"zero seq":          func(c *world.Component) { c.Version.Sequence = 0 },
		"zero ttl":          func(c *world.Component) { c.TTL = 0 },
		"neg ttl":           func(c *world.Component) { c.TTL = -1 },
		"zero sample":       func(c *world.Component) { c.Timing.SampleTime = time.Time{} },
		"tomb with payload": func(c *world.Component) { c.Tombstone = true },
		"payload NaN":       func(c *world.Component) { c.Payload = json.RawMessage(`{"x":NaN}`) },
		"payload empty":     func(c *world.Component) { c.Payload = nil },
		"payload huge":      func(c *world.Component) { c.Payload = json.RawMessage(`"` + string(bytes.Repeat([]byte("a"), 5<<20)) + `"`) },
		"neg errorbound":    func(c *world.Component) { c.Timing.ErrorBound = -time.Second },
		"neg agehint":       func(c *world.Component) { c.Timing.AgeHint = -time.Second },
	}
	names := make([]string, 0, len(bad))
	for n := range bad {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		c := good
		bad[name](&c)
		ok, err := j.Append(c)
		t.Logf("PROBE append %-18s appended=%v invalid=%v err=%.80v", name, ok, errors.Is(err, ErrInvalid), err)
		if ok || err == nil {
			t.Errorf("PROBE append %s accepted", name)
		}
	}
	if n := j.Stats().Records; n != 10 {
		t.Errorf("PROBE records after rejected appends: %d", n)
	}

	// clock
	clock.t = at(50)
	_, err := j.Append(good)
	t.Logf("PROBE clock backwards: %v", err)
	if !errors.Is(err, ErrClockRegression) {
		t.Errorf("clock regression not refused")
	}
	clock.t = time.Time{}
	if _, err := j.Append(good); !errors.Is(err, ErrClockRegression) {
		t.Errorf("zero clock not refused: %v", err)
	}
	clock.t = at(110)
	// duplicate timestamps
	mustAppend(t, j, poseAt(t, testScope, "dup", 1, at(110), time.Minute, 1, 1))
	mustAppend(t, j, poseAt(t, testScope, "dup", 2, at(110), time.Minute, 2, 1))
	mustAppend(t, j, poseAt(t, testScope, "dup", 3, at(110), time.Minute, 3, 1))
	dupKey := poseAt(t, testScope, "dup", 3, at(110), time.Minute, 3, 1).Key
	for _, axis := range []Axis{SampleTime, ApplyTime} {
		tl, err := j.Timeline(dupKey, at(100), at(120), axis)
		t.Logf("PROBE duplicate timestamps axis %d: timeline len=%d err=%v", axis, len(tl), err)
	}

	// queries
	q := func(name string, err error, want error) {
		t.Logf("PROBE query %-28s err=%v", name, err)
		if !errors.Is(err, want) {
			t.Errorf("PROBE query %s: got %v want %v", name, err, want)
		}
	}
	_, err = j.AsOf(time.Time{}, View{})
	q("zero time", err, ErrInvalid)
	_, err = j.AsOf(at(105), View{Axis: 7})
	q("bad axis", err, ErrInvalid)
	_, err = j.AsOf(at(105), View{Axis: -1})
	q("neg axis", err, ErrInvalid)
	_, err = j.Diff(at(108), at(105), View{})
	q("reversed diff", err, ErrInvalid)
	_, err = j.Timeline(good.Key, at(108), at(105), SampleTime)
	q("reversed timeline", err, ErrInvalid)
	_, err = j.EntityTimeline("tenant", "site", "device/d0", at(108), at(105), SampleTime)
	q("reversed entity timeline", err, ErrInvalid)
	_, err = j.Trajectory(good.Key, time.Time{}, at(105), SampleTime)
	q("zero trajectory from", err, ErrInvalid)

	tl, err := j.Timeline(world.Key{Tenant: "nope"}, at(100), at(200), SampleTime)
	t.Logf("PROBE unknown key timeline: %d records, err=%v", len(tl), err)
	st, err := j.AsOf(at(1), View{})
	t.Logf("PROBE as-of before any record: %d components, err=%v", len(st), err)
	st, err = j.AsOf(at(1e6), View{Axis: ApplyTime, HonourTTL: true})
	t.Logf("PROBE as-of far future (now=%v): %d components, err=%v  <- answers for a time the clock has not reached", clock.t.Sub(testEpoch), len(st), err)
	st, err = j.AsOf(at(1e6), View{Axis: ApplyTime})
	t.Logf("PROBE as-of far future no TTL: %d components, err=%v", len(st), err)

	// compaction refusals
	_, err = j.Compact(time.Time{})
	q("compact zero", err, ErrInvalid)
	res, err := j.Compact(at(1e5)) // far future horizon
	t.Logf("PROBE compact to a future horizon (now=110s): %+v err=%v", res, err)
	_, err = j.AsOf(clock.t, View{})
	t.Logf("PROBE as-of now after future-horizon compaction: err=%v", err)
	clock.t = at(120)
	ok, err := j.Append(poseAt(t, testScope, "late", 1, at(120), time.Minute, 1, 1))
	t.Logf("PROBE append after future-horizon compaction: appended=%v err=%v", ok, err)
	_, err = j.AsOf(clock.t, View{Axis: ApplyTime})
	t.Logf("PROBE as-of now for the record just appended: err=%v", err)

	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = j.AsOf(at(105), View{})
	q("closed asof", err, ErrClosed)
	_, err = j.Append(good)
	q("closed append", err, ErrClosed)
	_, err = j.Compact(at(105))
	q("closed compact", err, ErrClosed)
	q("closed sync", j.Sync(), ErrClosed)

	// Open refusals
	for name, cfg := range map[string]Config{"no clock": {}, "neg seg": {Now: clock.now, SegmentBytes: -1}, "neg retention": {Now: clock.now, Retention: -1}} {
		_, err := Open(t.TempDir(), cfg)
		q("open "+name, err, ErrInvalid)
	}
	_, err = Open("", Config{Now: clock.now})
	q("open empty dir", err, ErrInvalid)

	// restart with an earlier clock
	clock.t = at(60)
	j2 := mustOpen(t, dir, Config{Now: clock.now})
	_, err = j2.Append(poseAt(t, testScope, "zz", 1, at(60), time.Minute, 1, 1))
	q("restart earlier clock", err, ErrClockRegression)
}

// Two identical feeds must give byte-identical directories and answers.
func TestZZProbeDeterminism(t *testing.T) {
	run := func() (map[string][]byte, []byte) {
		clock := &fakeClock{t: at(100)}
		dir := t.TempDir()
		j := probeFill(t, dir, clock, 80, 6<<10)
		// add tombstones, tiles, out of order
		clock.t = at(300)
		for i := 0; i < 20; i++ {
			c := poseAt(t, testScope, fmt.Sprintf("k%d", i%7), uint64(100-i), at(float64(290-i)), time.Hour, float64(i), 2)
			if i%5 == 0 {
				c = tombstoneOf(c, c.Version.Sequence, at(float64(290-i)))
			}
			mustAppend(t, j, c)
		}
		if _, err := j.Compact(at(150)); err != nil {
			t.Fatal(err)
		}
		var answers bytes.Buffer
		for _, v := range []View{{Axis: ApplyTime, HonourTTL: true}, {Axis: SampleTime}, {Axis: SampleTime, HonourTTL: true}, {Axis: ApplyTime}} {
			for _, q := range []float64{150, 170, 200, 280, 300} {
				st, err := j.AsOf(at(q), v)
				if err != nil {
					t.Fatal(err)
				}
				b, _ := json.Marshal(st)
				answers.Write(b)
				d, err := j.Diff(at(150), at(q), v)
				if err != nil {
					t.Fatal(err)
				}
				b, _ = json.Marshal(d)
				answers.Write(b)
			}
			et, err := j.EntityTimeline("tenant", "site", "device/k1", at(150), at(400), v.Axis)
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(et)
			answers.Write(b)
		}
		j.Close()
		files := map[string][]byte{}
		names, _ := os.ReadDir(dir)
		for _, n := range names {
			b, _ := os.ReadFile(filepath.Join(dir, n.Name()))
			files[n.Name()] = b
		}
		return files, answers.Bytes()
	}
	f1, a1 := run()
	for i := 0; i < 4; i++ {
		f2, a2 := run()
		if !bytes.Equal(a1, a2) {
			t.Errorf("PROBE determinism: answers differ on rerun %d", i)
		}
		if len(f1) != len(f2) {
			t.Errorf("PROBE determinism: file sets differ")
		}
		for n, b := range f1 {
			if !bytes.Equal(b, f2[n]) {
				t.Errorf("PROBE determinism: file %s differs", n)
			}
		}
	}
	t.Logf("PROBE determinism: %d files, %d answer bytes, identical over 5 runs", len(f1), len(a1))
}

// Replica behaviours the journal may or may not mirror.
func TestZZProbeReplicaEdges(t *testing.T) {
	clock := &fakeClock{t: at(1000)}
	replica, err := world.NewReplica(world.Config{
		Tenant: testScope.Tenant, Site: testScope.Site, ClockDomain: testDomain, Now: clock.now,
		MaxFutureSkew: 5 * time.Second, MaxEntries: 1 << 12, AllowSynthetic: true, Frames: []world.FrameRef{testFrame},
	})
	if err != nil {
		t.Fatal(err)
	}
	j := mustOpen(t, t.TempDir(), Config{Now: clock.now})
	both := func(c world.Component) (world.Result, bool) {
		res, err := replica.Apply(c.Key.Source, c)
		if err != nil {
			t.Fatalf("replica: %v", err)
		}
		ok, err := j.Append(c)
		if err != nil {
			t.Fatalf("journal: %v", err)
		}
		return res, ok
	}
	cmp := func(name string) bool {
		want, err := replica.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		got, err := j.AsOf(clock.t, View{Axis: ApplyTime, HonourTTL: true})
		same := err == nil && sameState(got, want)
		t.Logf("PROBE replica edge %-44s same=%v (journal %d, replica %d) err=%v", name, same, len(got), len(want), err)
		return same
	}

	// a: higher version arrives already expired; lower one still alive
	both(poseAt(t, testScope, "a", 1, at(999), time.Minute, 1, 1))
	both(poseAt(t, testScope, "a", 2, at(900), 10*time.Second, 2, 1))
	if !cmp("already-expired higher version hides key") {
		t.Errorf("mismatch a")
	}
	// b: future sample within skew
	both(poseAt(t, testScope, "b", 1, at(1003), 2*time.Second, 1, 1))
	cmp("future sample t=1000")
	clock.t = at(1001.9)
	if !cmp("future sample t=1001.9") {
		t.Errorf("mismatch b")
	}
	clock.t = at(1002)
	if !cmp("future sample t=1002 (expiry = apply+ttl)") {
		t.Errorf("mismatch b2")
	}
	// c: whitespace payload, non-UTC sample time, monotonic reading
	c := poseAt(t, testScope, "c", 1, at(1001).In(time.FixedZone("x", 7200)), time.Minute, 1, 1)
	var pretty bytes.Buffer
	json.Indent(&pretty, c.Payload, "", "   ")
	c.Payload = pretty.Bytes()
	both(c)
	if !cmp("indented payload + non-UTC sample time") {
		t.Errorf("mismatch c")
	}
	// c2: html-ish characters in payload strings
	c2 := poseAt(t, testScope, "c2", 1, at(1001), time.Minute, 1, 1)
	c2.Payload = json.RawMessage(bytes.Replace(c2.Payload, []byte(`"test"`), []byte(`"<a&b> "`), 1))
	both(c2)
	if !cmp("html characters in payload") {
		t.Errorf("mismatch c2")
	}
	// d: exact replay later
	d := poseAt(t, testScope, "d", 1, at(1001), 5*time.Second, 1, 1)
	both(d)
	clock.t = at(1004)
	res, ok := both(d)
	t.Logf("PROBE exact replay: replica=%s journal appended=%v", res, ok)
	clock.t = at(1005.5)
	if !cmp("after exact replay") {
		t.Errorf("mismatch d")
	}
	// e: duplicate with larger age hint (declared gap)
	e := poseAt(t, testScope, "e", 1, at(1005), 30*time.Second, 1, 1)
	both(e)
	e2 := e
	e2.Timing.AgeHint = 29 * time.Second
	clock.t = at(1006)
	res, ok = both(e2)
	t.Logf("PROBE dup with larger age hint: replica=%s journal appended=%v", res, ok)
	clock.t = at(1010)
	cmp("DECLARED GAP dup with larger AgeHint")

	// f: ClampExpiry and Fence on the replica have no journal counterpart
	f := poseAt(t, testScope, "f", 1, at(1010), time.Minute, 1, 1)
	both(f)
	if err := replica.Fence(f.Key, f.Version); err != nil {
		t.Fatal(err)
	}
	cmp("UNDECLARED replica.Fence")
	g := poseAt(t, testScope, "g", 1, at(1010), time.Minute, 1, 1)
	both(g)
	if err := replica.ClampExpiry(g.Key, g.Version, at(1011)); err != nil {
		t.Fatal(err)
	}
	clock.t = at(1012)
	cmp("UNDECLARED replica.ClampExpiry")
}

// Concurrent readers during appends, auto compaction and manual compaction.
func TestZZProbeConcurrency(t *testing.T) {
	clock := &fakeClock{t: at(100)}
	var mu sync.Mutex
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock.t }
	j := mustOpen(t, t.TempDir(), Config{Now: now, SegmentBytes: 4 << 10, Retention: 20 * time.Second})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var failures sync.Map
	for r := 0; r < 6; r++ {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				mu.Lock()
				q := clock.t.Add(-time.Duration(i%25) * time.Second)
				mu.Unlock()
				v := View{Axis: Axis(i % 2), HonourTTL: i%3 == 0}
				st, err := j.AsOf(q, v)
				if err != nil && !errors.Is(err, ErrCompacted) {
					failures.Store(fmt.Sprint("asof ", err), true)
				}
				for k := 1; k < len(st); k++ {
					if keyOrder(st[k-1].Key) >= keyOrder(st[k].Key) {
						failures.Store("order", true)
					}
				}
				if _, err := j.Diff(q, q.Add(3*time.Second), v); err != nil && !errors.Is(err, ErrCompacted) {
					failures.Store(fmt.Sprint("diff ", err), true)
				}
				if _, err := j.EntityTimeline("tenant", "site", "device/d1", q, q.Add(5*time.Second), v.Axis); err != nil && !errors.Is(err, ErrCompacted) {
					failures.Store(fmt.Sprint("et ", err), true)
				}
				j.Stats()
			}
		}(r)
	}
	for i := 1; i <= 600; i++ {
		mu.Lock()
		clock.t = at(100 + float64(i)*0.25)
		ts := clock.t
		mu.Unlock()
		mustAppend(t, j, poseAt(t, testScope, fmt.Sprintf("d%d", i%5), uint64(i), ts, 3*time.Second, float64(i), 1))
		if i%97 == 0 {
			if _, err := j.Compact(ts.Add(-10 * time.Second)); err != nil {
				t.Fatal(err)
			}
		}
	}
	close(stop)
	wg.Wait()
	failures.Range(func(k, _ any) bool { t.Errorf("PROBE concurrency: %v", k); return true })
	t.Logf("PROBE concurrency: stats %+v", j.Stats())
}

// Retention bounds: how the snapshot and the key set grow when every key is
// short-lived (tracks, change events).
func TestZZProbeRetentionGrowth(t *testing.T) {
	clock := &fakeClock{t: at(100)}
	j := mustOpen(t, t.TempDir(), Config{Now: clock.now, SegmentBytes: 8 << 10, Retention: 10 * time.Second})
	for i := 1; i <= 2000; i++ {
		clock.t = at(100 + float64(i))
		c := poseAt(t, testScope, fmt.Sprintf("track%d", i), 1, clock.t, time.Second, 1, 1)
		mustAppend(t, j, c)
		mustAppend(t, j, tombstoneOf(c, 2, clock.t))
		if i%500 == 0 {
			s := j.Stats()
			t.Logf("PROBE retention growth after %d dead keys: records=%d keys=%d bytes=%d KiB segments=%d", i, s.Records, s.Keys, s.Bytes>>10, s.Segments)
		}
	}
}

// Published payloads: does the package expose anything publishable?
func TestZZProbeEntityTimelineAndTiles(t *testing.T) {
	clock := &fakeClock{t: at(100)}
	j := mustOpen(t, t.TempDir(), Config{Now: clock.now})
	c := poseAt(t, testScope, "r", 1, at(100), time.Minute, 1, 1)
	mustAppend(t, j, c)
	fixes, err := j.Trajectory(c.Key, at(0), at(200), SampleTime)
	t.Logf("PROBE trajectory: %d fixes err=%v uncertainty=%v", len(fixes), err, fixes[0].Entity.UncertaintyM)
	_, err = j.Trajectory(c.Key, at(0), at(200), Axis(9))
	t.Logf("PROBE trajectory bad axis: %v", err)
	var _ = spatial.TypeDevicePose
}
