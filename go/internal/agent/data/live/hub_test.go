package live

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func record(stream string, seq int, size int) *Record {
	return &Record{StreamID: stream, Schema: "test.v1.Sample", TimestampUnixNanos: int64(seq), Payload: make([]byte, size)}
}

func subscribe(t *testing.T, h *Hub, ids ...string) *Subscriber {
	t.Helper()
	s, err := h.Subscribe(Selection{StreamIDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// next fails the test instead of blocking when nothing is pending.
func next(t *testing.T, s *Subscriber) Delivery {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	d, err := s.Next(ctx)
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	return d
}

func requireEmpty(t *testing.T, s *Subscriber) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if d, err := s.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected nothing pending, got %+v %v", d, err)
	}
}

// With nobody subscribed, a publish updates its stream's entry and allocates
// nothing, whether or not the stream is advertised. This is the cost every
// app record pays when no viewer is open.
func TestOfferWithoutSubscribersAllocatesNothing(t *testing.T) {
	h := NewHub(DefaultLimits)
	r := record("apps/a/markers", 1, 64)
	unadvertised := record("apps/a/other", 1, 64)
	h.Advertise(StreamInfo{ID: r.StreamID, Schema: r.Schema})
	if allocs := testing.AllocsPerRun(1000, func() {
		if h.Subscribed() {
			t.Fatal("subscribed with nobody subscribed")
		}
		h.Offer(r)
		h.Offer(unadvertised)
	}); allocs != 0 {
		t.Fatalf("offer with no subscribers allocated %v times", allocs)
	}
	// Back to the fast path once the last subscriber leaves.
	s, err := h.Subscribe(Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if !h.Subscribed() {
		t.Fatal("not subscribed after Subscribe")
	}
	s.Close()
	if h.Subscribed() {
		t.Fatal("still subscribed after Close")
	}
	if allocs := testing.AllocsPerRun(1000, func() { h.Offer(r) }); allocs != 0 {
		t.Fatalf("offer after the last subscriber left allocated %v times", allocs)
	}
}

// A subscriber that never reads holds one record per stream, the newest, and
// counts the rest as skipped. Offer never waits for it.
func TestStalledSubscriberGetsNewestRecordPerStream(t *testing.T) {
	h := NewHub(DefaultLimits)
	s := subscribe(t, h)
	const n = 10000
	start := time.Now()
	for i := 1; i <= n; i++ {
		h.Offer(record("apps/a/fast", i, 16))
		if i%1000 == 0 {
			h.Offer(record("apps/a/slow", i, 16))
		}
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("offers to a stalled subscriber took %v", elapsed)
	}
	first, second := next(t, s), next(t, s)
	if first.Record.StreamID != "apps/a/fast" || first.Record.TimestampUnixNanos != n || first.Skipped != n-1 {
		t.Fatalf("fast stream: got %s #%d skipped %d", first.Record.StreamID, first.Record.TimestampUnixNanos, first.Skipped)
	}
	if second.Record.StreamID != "apps/a/slow" || second.Record.TimestampUnixNanos != n || second.Skipped != 9 {
		t.Fatalf("slow stream: got %s #%d skipped %d", second.Record.StreamID, second.Record.TimestampUnixNanos, second.Skipped)
	}
	requireEmpty(t, s)
}

// A flood on one stream must not cost another stream its record, which is
// what a single queue shared across streams does: an alarm's one Event record
// would be pushed out by markers.
func TestStreamsAreIndependent(t *testing.T) {
	h := NewHub(DefaultLimits)
	s := subscribe(t, h)
	h.Offer(record("apps/a/event", 1, 8))
	for i := 0; i < 1000; i++ {
		h.Offer(record("apps/a/markers", i, 8))
	}
	if d := next(t, s); d.Record.StreamID != "apps/a/event" || d.Skipped != 0 {
		t.Fatalf("event record lost behind markers: %+v", d)
	}
	if d := next(t, s); d.Record.StreamID != "apps/a/markers" || d.Record.TimestampUnixNanos != 999 || d.Skipped != 999 {
		t.Fatalf("markers: %+v", d)
	}
}

// Skipped is per stream and per subscriber: a fast reader skips nothing even
// while a stalled one skips everything.
func TestSkippedIsPerSubscriber(t *testing.T) {
	h := NewHub(DefaultLimits)
	fast, stalled := subscribe(t, h), subscribe(t, h)
	for i := 1; i <= 5; i++ {
		h.Offer(record("apps/a/s", i, 1))
		if d := next(t, fast); d.Record.TimestampUnixNanos != int64(i) || d.Skipped != 0 {
			t.Fatalf("fast reader: %+v", d)
		}
	}
	if d := next(t, stalled); d.Record.TimestampUnixNanos != 5 || d.Skipped != 4 {
		t.Fatalf("stalled reader: %+v", d)
	}
	// The count restarts after each delivery.
	h.Offer(record("apps/a/s", 6, 1))
	if d := next(t, stalled); d.Skipped != 0 {
		t.Fatalf("skipped not reset after delivery: %+v", d)
	}
}

func TestSelectionByStreamID(t *testing.T) {
	h := NewHub(DefaultLimits)
	only := subscribe(t, h, "apps/a/one", "apps/a/not-yet")
	all := subscribe(t, h)
	h.Offer(record("apps/a/two", 1, 1))
	h.Offer(record("apps/a/one", 2, 1))
	if d := next(t, only); d.Record.StreamID != "apps/a/one" {
		t.Fatalf("selection delivered %s", d.Record.StreamID)
	}
	requireEmpty(t, only)
	if a, b := next(t, all), next(t, all); a.Record.StreamID != "apps/a/two" || b.Record.StreamID != "apps/a/one" {
		t.Fatalf("empty selection: %s then %s", a.Record.StreamID, b.Record.StreamID)
	}
	// An id selected before its stream exists starts receiving once it does.
	h.Offer(record("apps/a/not-yet", 3, 1))
	if d := next(t, only); d.Record.StreamID != "apps/a/not-yet" {
		t.Fatalf("late stream: %s", d.Record.StreamID)
	}
}

// Ids and prefixes name streams; schemas narrow what they name. A selection
// of schemas alone applies to every stream.
func TestSelectionByPrefixAndSchema(t *testing.T) {
	h := NewHub(DefaultLimits)
	offer := func(id, schema string, seq int) {
		h.Offer(&Record{StreamID: id, Schema: schema, TimestampUnixNanos: int64(seq)})
	}
	appMarkers, err := h.Subscribe(Selection{StreamPrefixes: []string{"apps/a/"}, Schemas: []string{"wendy.data.v1.VideoMarkers"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(appMarkers.Close)
	idOrPrefix, err := h.Subscribe(Selection{StreamIDs: []string{"apps/b/log"}, StreamPrefixes: []string{"apps/a/"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(idOrPrefix.Close)
	anyMarkers, err := h.Subscribe(Selection{Schemas: []string{"wendy.data.v1.VideoMarkers"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(anyMarkers.Close)

	offer("apps/a/markers", "wendy.data.v1.VideoMarkers", 1)
	offer("apps/a/log", "", 2)
	offer("apps/b/markers", "wendy.data.v1.VideoMarkers", 3)
	offer("apps/b/log", "", 4)
	offer("apps/ab/markers", "wendy.data.v1.VideoMarkers", 5)

	for _, tc := range []struct {
		name string
		s    *Subscriber
		want []string
	}{
		{"prefix and schema", appMarkers, []string{"apps/a/markers"}},
		{"id or prefix", idOrPrefix, []string{"apps/a/markers", "apps/a/log", "apps/b/log"}},
		{"schema alone", anyMarkers, []string{"apps/a/markers", "apps/b/markers", "apps/ab/markers"}},
	} {
		for _, want := range tc.want {
			if d := next(t, tc.s); d.Record.StreamID != want {
				t.Fatalf("%s: got %s, want %s", tc.name, d.Record.StreamID, want)
			}
		}
		requireEmpty(t, tc.s)
	}
}

// A rate cap holds a fast stream back and replaces the held record with each
// newer one, so it never queues. A held stream does not hold back another.
func TestRateCapCoalescesToNewest(t *testing.T) {
	h := NewHub(DefaultLimits)
	const gap = 50 * time.Millisecond
	s, err := h.Subscribe(Selection{MaxRateHz: float64(time.Second / gap)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	h.Offer(record("apps/a/fast", 1, 1))
	if d := next(t, s); d.Record.TimestampUnixNanos != 1 {
		t.Fatalf("first record is not sent at once: %+v", d)
	}
	sent := time.Now()
	for i := 2; i <= 50; i++ {
		h.Offer(record("apps/a/fast", i, 1))
	}
	h.Offer(record("apps/a/slow", 1, 1))
	if d := next(t, s); d.Record.StreamID != "apps/a/slow" {
		t.Fatalf("a held stream held back another: got %s", d.Record.StreamID)
	}
	d := next(t, s)
	if elapsed := time.Since(sent); elapsed < gap-10*time.Millisecond {
		t.Fatalf("second record after %v, under the %v gap", elapsed, gap)
	}
	if d.Record.StreamID != "apps/a/fast" || d.Record.TimestampUnixNanos != 50 || d.Skipped != 48 {
		t.Fatalf("held stream: got %s #%d skipped %d", d.Record.StreamID, d.Record.TimestampUnixNanos, d.Skipped)
	}
	requireEmpty(t, s)
}

// A tiny rate asks for an absurd gap; it is bounded instead of overflowing.
func TestRateCapBounds(t *testing.T) {
	h := NewHub(DefaultLimits)
	for _, tc := range []struct {
		rate float64
		gap  time.Duration
	}{{0, 0}, {-5, 0}, {math.NaN(), 0}, {1e-30, maxRateGap}, {30, time.Second / 30}} {
		s, err := h.Subscribe(Selection{MaxRateHz: tc.rate})
		if err != nil {
			t.Fatal(err)
		}
		if s.minGap != tc.gap {
			t.Errorf("rate %v: gap %v, want %v", tc.rate, s.minGap, tc.gap)
		}
		s.Close()
	}
}

func TestPermits(t *testing.T) {
	for _, tc := range []struct {
		allow  []string
		stream string
		want   bool
	}{
		{nil, "apps/a/markers", true},
		{[]string{"apps/a/markers"}, "apps/a/markers", true},
		{[]string{"apps/a"}, "apps/a/markers", true},
		{[]string{"apps/a"}, "apps/a/worker/markers", true},
		{[]string{"apps/a/"}, "apps/a/markers", true},
		{[]string{"apps/"}, "apps/b/markers", true},
		// Prefixes match whole segments: an app id that extends another one is
		// a different app.
		{[]string{"apps/a"}, "apps/ab/markers", false},
		{[]string{"apps/com.example"}, "apps/com.example.other/markers", false},
		{[]string{"apps/a/markers"}, "apps/a/markers2", false},
		{[]string{"apps/a/markers"}, "apps/a", false},
		{[]string{"apps/a", "apps/b"}, "apps/b/markers", true},
		{[]string{"apps/a", "apps/b"}, "agent/inference", false},
		{[]string{"agent/campaign/door"}, "agent/campaign/door/markers/v4l2:/dev/video0", true},
		// A catalog entry is admitted with the stream it describes.
		{[]string{"apps/a"}, CatalogPrefix + "apps/a/markers", true},
		{[]string{"apps/a"}, CatalogPrefix + "apps/b/markers", false},
		{[]string{"apps/a"}, CatalogPrefix + "apps/ab/markers", false},
		{[]string{"apps/a"}, CatalogPrefix, false},
	} {
		if got := Permits(tc.allow, tc.stream); got != tc.want {
			t.Errorf("Permits(%q, %q) = %v, want %v", tc.allow, tc.stream, got, tc.want)
		}
	}
}

// Allow bounds what a subscriber may read whatever it selects: an empty
// selection reads only allowed streams, and selected ids outside it get
// nothing.
func TestSelectionAllow(t *testing.T) {
	h := NewHub(DefaultLimits)
	scoped, err := h.Subscribe(Selection{Allow: []string{"apps/a"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(scoped.Close)
	narrowed, err := h.Subscribe(Selection{StreamIDs: []string{"apps/a/one", "apps/b/one"}, Allow: []string{"apps/a"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(narrowed.Close)
	h.Offer(record("apps/b/one", 1, 1))
	h.Offer(record("apps/ab/one", 2, 1))
	h.Offer(record("apps/a/two", 3, 1))
	h.Offer(record("apps/a/one", 4, 1))
	if a, b := next(t, scoped), next(t, scoped); a.Record.StreamID != "apps/a/two" || b.Record.StreamID != "apps/a/one" {
		t.Fatalf("allowed streams: %s then %s", a.Record.StreamID, b.Record.StreamID)
	}
	requireEmpty(t, scoped)
	if d := next(t, narrowed); d.Record.StreamID != "apps/a/one" {
		t.Fatalf("selection outside allow delivered %s", d.Record.StreamID)
	}
	requireEmpty(t, narrowed)
}

// An allowlist hides the catalog entries of the streams it does not admit,
// both the current catalog that send_latest starts with and later changes.
func TestSelectionAllowFiltersCatalog(t *testing.T) {
	h, _ := clockedHub(DefaultLimits)
	h.Advertise(StreamInfo{ID: "apps/a/markers", Publisher: "a"})
	h.Advertise(StreamInfo{ID: "apps/b/markers", Publisher: "b"})
	s := subscribeWith(t, h, Selection{StreamPrefixes: []string{CatalogPrefix}, SendLatest: true, Allow: []string{"apps/a"}})
	if info := catalogEntry(t, next(t, s)); info.GetStreamId() != "apps/a/markers" {
		t.Fatalf("current catalog: %v", info)
	}
	requireEmpty(t, s)
	h.Advertise(StreamInfo{ID: "apps/b/other", Publisher: "b"})
	h.Offer(record("apps/b/markers", 1, 8))
	h.Unadvertise("apps/b/markers")
	requireEmpty(t, s)
	h.Advertise(StreamInfo{ID: "apps/a/other", Publisher: "a"})
	if info := catalogEntry(t, next(t, s)); info.GetStreamId() != "apps/a/other" {
		t.Fatalf("catalog change: %v", info)
	}
	requireEmpty(t, s)
}

// A stream Readable refuses is not delivered, not in the catalog, and not
// wanted, however the selection names it. Readable is asked about the stream,
// never its catalog id.
func TestSelectionReadable(t *testing.T) {
	h, _ := clockedHub(DefaultLimits)
	const camera = "agent/camera/grid/v4l2:/dev/video0"
	h.Advertise(StreamInfo{ID: camera, Publisher: "agent"})
	readable := func(id string) bool {
		if strings.HasPrefix(id, CatalogPrefix) {
			t.Errorf("Readable asked about catalog id %s", id)
		}
		return !strings.HasPrefix(id, "agent/camera/")
	}
	s := subscribeWith(t, h, Selection{StreamPrefixes: []string{"agent/", "apps/", CatalogPrefix}, SendLatest: true, Readable: readable})
	everything := subscribeWith(t, h, Selection{Readable: readable})
	requireEmpty(t, s)
	h.Offer(record(camera, 1, 8))
	h.Offer(record("apps/a/markers", 2, 8))
	for _, sub := range []*Subscriber{s, everything} {
		if d := next(t, sub); d.Record.StreamID != "apps/a/markers" {
			t.Fatalf("delivered %s", d.Record.StreamID)
		}
		requireEmpty(t, sub)
	}
	if h.Wanted(camera, "test.v1.Sample") {
		t.Fatal("wanted by a subscriber that may not read it")
	}
	if !h.Wanted("agent/campaign/door/markers/v4l2:/dev/video0", "") {
		t.Fatal("a readable stream the selection names is not wanted")
	}
	subscribeWith(t, h, Selection{StreamIDs: []string{camera}})
	if !h.Wanted(camera, "test.v1.Sample") {
		t.Fatal("not wanted by a subscriber without Readable")
	}
}

// A stream is wanted by a subscriber that names it by id, prefix or schema,
// within its scope, and by nobody else: an empty selection takes everything
// but asks for nothing.
func TestWanted(t *testing.T) {
	h := NewHub(DefaultLimits)
	const id, schema = "agent/camera/grid/v4l2:/dev/video0", "wendy.data.v1.Grid"
	wanted := func() bool { return h.Wanted(id, schema) }
	if wanted() {
		t.Fatal("wanted with nobody subscribed")
	}
	subscribeWith(t, h, Selection{})
	subscribeWith(t, h, Selection{StreamPrefixes: []string{CatalogPrefix}})
	subscribeWith(t, h, Selection{StreamIDs: []string{"agent/camera/grid/v4l2:/dev/video1"}})
	subscribeWith(t, h, Selection{Schemas: []string{"wendy.data.v1.VideoMarkers"}})
	subscribeWith(t, h, Selection{StreamIDs: []string{id}, Allow: []string{"apps/a"}})
	if wanted() {
		t.Fatal("wanted by subscribers that do not name the stream")
	}
	for _, sel := range []Selection{
		{StreamIDs: []string{id}},
		{StreamPrefixes: []string{"agent/camera/"}},
		{Schemas: []string{schema}},
		{StreamPrefixes: []string{"agent/"}, Allow: []string{"agent/camera"}},
	} {
		s, err := h.Subscribe(sel)
		if err != nil {
			t.Fatal(err)
		}
		if !wanted() {
			t.Errorf("not wanted by %+v", sel)
		}
		s.Close()
		if wanted() {
			t.Errorf("still wanted after %+v closed", sel)
		}
	}
}

// SubscribersChanged wakes a waiting producer once per arrival or departure,
// and not for a subscriber the cap turned away.
func TestSubscribersChanged(t *testing.T) {
	h := NewHub(Limits{MaxSubscribers: 1})
	closed := func(c <-chan struct{}) bool {
		select {
		case <-c:
			return true
		default:
			return false
		}
	}
	changed := h.SubscribersChanged()
	if closed(changed) || h.SubscribersChanged() != changed {
		t.Fatal("changed before anything happened")
	}
	s, err := h.Subscribe(Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if !closed(changed) {
		t.Fatal("a subscriber arrived unannounced")
	}
	changed = h.SubscribersChanged()
	if _, err := h.Subscribe(Selection{}); !errors.Is(err, ErrTooManySubscribers) {
		t.Fatalf("second subscriber: %v", err)
	}
	if closed(changed) {
		t.Fatal("a refused subscriber was announced")
	}
	s.Close()
	if !closed(changed) {
		t.Fatal("a subscriber left unannounced")
	}
	changed = h.SubscribersChanged()
	s.Close()
	if closed(changed) {
		t.Fatal("a second Close was announced")
	}
}

func TestSubscriberCap(t *testing.T) {
	h := NewHub(Limits{MaxSubscribers: 2})
	a := subscribe(t, h)
	subscribe(t, h)
	if _, err := h.Subscribe(Selection{}); !errors.Is(err, ErrTooManySubscribers) {
		t.Fatalf("third subscriber: %v", err)
	}
	a.Close()
	a.Close() // idempotent: must not free a second slot
	subscribe(t, h)
	if _, err := h.Subscribe(Selection{}); !errors.Is(err, ErrTooManySubscribers) {
		t.Fatalf("cap after double Close: %v", err)
	}
}

// Past the pending-bytes cap the oldest pending stream is dropped, and its
// loss is reported with that stream's next delivery.
func TestPendingBytesCap(t *testing.T) {
	h := NewHub(Limits{MaxPendingBytes: 100})
	s := subscribe(t, h)
	h.Offer(record("apps/a/one", 1, 40))
	h.Offer(record("apps/a/two", 2, 40))
	h.Offer(record("apps/a/three", 3, 40)) // 120 > 100: "one" is dropped
	if d := next(t, s); d.Record.StreamID != "apps/a/two" {
		t.Fatalf("oldest pending stream was not dropped: got %s first", d.Record.StreamID)
	}
	if d := next(t, s); d.Record.StreamID != "apps/a/three" {
		t.Fatalf("got %s", d.Record.StreamID)
	}
	h.Offer(record("apps/a/one", 4, 40))
	if d := next(t, s); d.Record.StreamID != "apps/a/one" || d.Skipped != 1 {
		t.Fatalf("drop not counted: %+v", d)
	}
	// Replacing a pending record frees its bytes rather than adding to them.
	for i := 0; i < 10; i++ {
		h.Offer(record("apps/a/one", 5+i, 60))
	}
	h.Offer(record("apps/a/two", 20, 40))
	if a, b := next(t, s), next(t, s); a.Record.StreamID != "apps/a/one" || b.Record.StreamID != "apps/a/two" {
		t.Fatalf("replacement was charged twice: %s then %s", a.Record.StreamID, b.Record.StreamID)
	}
	// A record larger than the cap on its own is still delivered.
	h.Offer(record("apps/a/one", 30, 40))
	h.Offer(record("apps/a/big", 31, 500))
	if d := next(t, s); d.Record.StreamID != "apps/a/big" {
		t.Fatalf("oversized record: got %s", d.Record.StreamID)
	}
	requireEmpty(t, s)
}

func TestCloseWakesNextAndContextCancels(t *testing.T) {
	h := NewHub(DefaultLimits)
	s := subscribe(t, h)
	errs := make(chan error, 1)
	go func() {
		_, err := s.Next(context.Background())
		errs <- err
	}()
	time.Sleep(10 * time.Millisecond)
	s.Close()
	if err := <-errs; !errors.Is(err, ErrClosed) {
		t.Fatalf("blocked Next after Close: %v", err)
	}
	h.Offer(record("apps/a/s", 1, 1)) // must not panic or deliver
	if _, err := s.Next(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Next after Close: %v", err)
	}

	s = subscribe(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Next: %v", err)
	}
}

// Publishers and slow readers run concurrently (run with -race). Every offered
// record is either delivered or counted as skipped, and the last record of
// each stream always arrives.
func TestConcurrentPublishersAndSlowReaders(t *testing.T) {
	h := NewHub(DefaultLimits)
	const streams, perStream, readers = 4, 2000, 3
	var subs []*Subscriber
	for i := 0; i < readers; i++ {
		subs = append(subs, subscribe(t, h))
	}
	type tally struct {
		accounted map[string]uint64
		last      map[string]int64
	}
	results := make([]tally, readers)
	done := make(chan struct{})
	var readersWG sync.WaitGroup
	for i, s := range subs {
		readersWG.Add(1)
		go func() {
			defer readersWG.Done()
			r := tally{accounted: map[string]uint64{}, last: map[string]int64{}}
			for {
				d, err := s.Next(context.Background())
				if err != nil {
					break
				}
				if d.Record.TimestampUnixNanos <= r.last[d.Record.StreamID] {
					t.Errorf("stream %s went backwards", d.Record.StreamID)
				}
				r.accounted[d.Record.StreamID] += 1 + d.Skipped
				r.last[d.Record.StreamID] = d.Record.TimestampUnixNanos
				if i > 0 {
					time.Sleep(time.Duration(i) * 50 * time.Microsecond)
				}
			}
			results[i] = r
		}()
	}
	var publishers sync.WaitGroup
	for p := 0; p < streams; p++ {
		publishers.Add(1)
		go func() {
			defer publishers.Done()
			id := fmt.Sprintf("apps/a/s%d", p)
			for i := 1; i <= perStream; i++ {
				h.Offer(record(id, i, 32))
			}
		}()
	}
	publishers.Wait()
	// Let the readers drain, then stop them.
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			drained := true
			for _, s := range subs {
				s.mu.Lock()
				drained = drained && len(s.order) == 0
				s.mu.Unlock()
			}
			if drained {
				break
			}
			time.Sleep(time.Millisecond)
		}
		close(done)
	}()
	<-done
	for _, s := range subs {
		s.Close()
	}
	readersWG.Wait()
	for i, r := range results {
		for p := 0; p < streams; p++ {
			id := fmt.Sprintf("apps/a/s%d", p)
			if r.accounted[id] != perStream || r.last[id] != perStream {
				t.Errorf("reader %d stream %s: accounted %d of %d, last #%d", i, id, r.accounted[id], perStream, r.last[id])
			}
		}
	}
}

func BenchmarkOfferNoSubscribers(b *testing.B) {
	h := NewHub(DefaultLimits)
	r := record("apps/a/markers", 1, 64)
	h.Advertise(StreamInfo{ID: r.StreamID, Schema: r.Schema})
	b.ReportAllocs()
	for b.Loop() {
		h.Offer(r)
	}
}

func BenchmarkOfferFourSubscribers(b *testing.B) {
	h := NewHub(DefaultLimits)
	h.Advertise(StreamInfo{ID: "apps/a/markers", Schema: "test.v1.Sample"})
	for i := 0; i < 4; i++ {
		s, err := h.Subscribe(Selection{})
		if err != nil {
			b.Fatal(err)
		}
		defer s.Close()
	}
	r := record("apps/a/markers", 1, 64)
	b.ReportAllocs()
	for b.Loop() {
		h.Offer(r)
	}
}
