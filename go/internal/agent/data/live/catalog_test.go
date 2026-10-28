package live

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/protobuf/proto"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// clockedHub runs on a clock the test moves. It never sweeps on its own; the
// test calls sweepNow.
func clockedHub(limits Limits) (*Hub, *fakeClock) {
	h := NewHub(limits)
	c := &fakeClock{t: time.Unix(1_800_000_000, 0)}
	h.now, h.sweepEvery = c.now, time.Hour
	return h, c
}

func sweepNow(h *Hub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sweepLocked(h.now())
}

func subscribeWith(t *testing.T, h *Hub, sel Selection) *Subscriber {
	t.Helper()
	s, err := h.Subscribe(sel)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func catalogEntry(t *testing.T, d Delivery) *agentpbv2.DataStreamInfo {
	t.Helper()
	if d.Record.Schema != CatalogSchema {
		t.Fatalf("not a catalog record: %s %s", d.Record.StreamID, d.Record.Schema)
	}
	info := new(agentpbv2.DataStreamInfo)
	if err := proto.Unmarshal(d.Record.Payload, info); err != nil {
		t.Fatal(err)
	}
	if d.Record.StreamID != CatalogPrefix+info.GetStreamId() {
		t.Fatalf("catalog record %s describes %s", d.Record.StreamID, info.GetStreamId())
	}
	return info
}

func TestCatalogSchemaIsTheMessageName(t *testing.T) {
	if got := string((&agentpbv2.DataStreamInfo{}).ProtoReflect().Descriptor().FullName()); got != CatalogSchema {
		t.Fatalf("CatalogSchema %q, message %q", CatalogSchema, got)
	}
}

// Only the catalog's own streams carry CatalogSchema. An app may not declare
// it, and a stream that does anyway cannot pose as catalog entries to a
// subscriber that selects the catalog by schema.
func TestCatalogSchemaIsReserved(t *testing.T) {
	err := appconfig.ValidateRecordingStreams(map[string]appconfig.RecordingStream{"fake": {Mode: "lightweight", MediaType: "application/protobuf", Schema: CatalogSchema}})
	if err == nil {
		t.Fatal("an app stream declared the catalog schema")
	}

	h, _ := clockedHub(DefaultLimits)
	h.Advertise(StreamInfo{ID: "apps/victim/markers", Publisher: "victim"})
	h.Advertise(StreamInfo{ID: "apps/evil/fake", Schema: CatalogSchema, Publisher: "evil"})
	s := subscribeWith(t, h, Selection{Schemas: []string{CatalogSchema}})
	forged, err := proto.Marshal(&agentpbv2.DataStreamInfo{StreamId: "apps/victim/markers", State: agentpbv2.DataStreamInfo_STATE_GONE})
	if err != nil {
		t.Fatal(err)
	}
	h.Offer(&Record{StreamID: "apps/evil/fake", Schema: CatalogSchema, Payload: forged})
	if info := catalogEntry(t, next(t, s)); info.GetStreamId() != "apps/evil/fake" || info.GetState() != agentpbv2.DataStreamInfo_STATE_ACTIVE {
		t.Fatalf("got %v, want apps/evil/fake active", info)
	}
	requireEmpty(t, s)
	requireEmpty(t, subscribeWith(t, h, Selection{Schemas: []string{CatalogSchema}, StreamPrefixes: []string{"apps/"}, SendLatest: true}))
}

// A new subscriber that asks for it starts with the newest record of each
// selected stream, oldest first, so it shows state at once. Records too old,
// too large, or of a stream nobody advertised are not kept.
func TestSendLatest(t *testing.T) {
	h, clock := clockedHub(Limits{MaxLatestBytes: 100})
	for _, id := range []string{"apps/a/stale", "apps/a/events", "apps/a/markers", "apps/a/grid"} {
		h.Advertise(StreamInfo{ID: id, Schema: "test.v1.Sample"})
	}
	h.Offer(record("apps/a/stale", 1, 8))
	clock.advance(DefaultLimits.MaxLatestAge + time.Second)
	h.Offer(record("apps/a/events", 2, 8))
	clock.advance(time.Millisecond)
	h.Offer(record("apps/a/markers", 3, 8))
	h.Offer(record("apps/a/markers", 4, 8))
	h.Offer(record("apps/a/grid", 5, 8))
	h.Offer(record("apps/a/grid", 6, 101)) // too large to keep, and newer than #5
	h.Offer(record("apps/a/unadvertised", 7, 8))

	s := subscribeWith(t, h, Selection{SendLatest: true})
	for _, want := range []struct {
		id  string
		seq int64
	}{{"apps/a/events", 2}, {"apps/a/markers", 4}} {
		if d := next(t, s); d.Record.StreamID != want.id || d.Record.TimestampUnixNanos != want.seq || d.Skipped != 0 {
			t.Fatalf("got %s #%d skipped %d, want %s #%d", d.Record.StreamID, d.Record.TimestampUnixNanos, d.Skipped, want.id, want.seq)
		}
	}
	requireEmpty(t, s)

	// The selection applies, and without SendLatest nothing is sent.
	only := subscribeWith(t, h, Selection{StreamIDs: []string{"apps/a/markers"}, SendLatest: true})
	if d := next(t, only); d.Record.StreamID != "apps/a/markers" {
		t.Fatalf("selection: got %s", d.Record.StreamID)
	}
	requireEmpty(t, only)
	requireEmpty(t, subscribeWith(t, h, Selection{}))

	// A live record offered before the first Next replaces the kept one in
	// place: the subscriber never sees an older record after a newer one.
	s = subscribeWith(t, h, Selection{StreamIDs: []string{"apps/a/markers"}, SendLatest: true})
	h.Offer(record("apps/a/markers", 8, 8))
	if d := next(t, s); d.Record.TimestampUnixNanos != 8 {
		t.Fatalf("got #%d, want the live #8", d.Record.TimestampUnixNanos)
	}
	requireEmpty(t, s)

	// An unadvertised stream's record is no longer kept.
	h.Unadvertise("apps/a/markers")
	requireEmpty(t, subscribeWith(t, h, Selection{StreamIDs: []string{"apps/a/markers"}, SendLatest: true}))
}

// The catalog follows a stream through its life: idle when advertised,
// active with its first record, repeated with the measured rate while active,
// idle once records stop, and gone when unadvertised.
func TestCatalogLifecycle(t *testing.T) {
	h, clock := clockedHub(DefaultLimits)
	cat := subscribeWith(t, h, Selection{StreamPrefixes: []string{CatalogPrefix}})
	data := subscribeWith(t, h, Selection{})
	h.Advertise(StreamInfo{ID: "apps/a/markers", Schema: "wendy.data.v1.VideoMarkers", MediaType: "application/protobuf", Publisher: "a"})
	info := catalogEntry(t, next(t, cat))
	if info.GetState() != agentpbv2.DataStreamInfo_STATE_IDLE || info.GetSchema() != "wendy.data.v1.VideoMarkers" || info.GetMediaType() != "application/protobuf" || info.GetPublisher() != "a" || info.GetLastTimestampUnixNanos() != 0 {
		t.Fatalf("advertised: %v", info)
	}

	// 51 records 200 ms apart: active at the first, repeated with the rate
	// once announceEvery has passed, and silent in between.
	for i := 1; i <= 51; i++ {
		clock.advance(200 * time.Millisecond)
		h.Offer(record("apps/a/markers", i, 8))
		if i == 1 {
			if info = catalogEntry(t, next(t, cat)); info.GetState() != agentpbv2.DataStreamInfo_STATE_ACTIVE || info.GetLastTimestampUnixNanos() != 1 {
				t.Fatalf("first record: %v", info)
			}
			requireEmpty(t, cat)
		}
	}
	info = catalogEntry(t, next(t, cat))
	if info.GetState() != agentpbv2.DataStreamInfo_STATE_ACTIVE || info.GetLastTimestampUnixNanos() != 51 || info.GetRateHz() < 4.9 || info.GetRateHz() > 5.1 {
		t.Fatalf("repeat: %v", info)
	}
	requireEmpty(t, cat)

	clock.advance(idleAfter - time.Millisecond)
	sweepNow(h)
	requireEmpty(t, cat)
	clock.advance(time.Millisecond)
	sweepNow(h)
	if info = catalogEntry(t, next(t, cat)); info.GetState() != agentpbv2.DataStreamInfo_STATE_IDLE {
		t.Fatalf("after records stopped: %v", info)
	}

	h.Unadvertise("apps/a/markers")
	if info = catalogEntry(t, next(t, cat)); info.GetState() != agentpbv2.DataStreamInfo_STATE_GONE {
		t.Fatalf("unadvertised: %v", info)
	}
	requireEmpty(t, cat)

	// A subscriber of every stream gets the records and no catalog.
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		d, err := data.Next(ctx)
		cancel()
		if err != nil {
			break
		}
		if d.Record.Schema == CatalogSchema {
			t.Fatalf("empty selection received catalog record %s", d.Record.StreamID)
		}
	}
}

// A catalog entry's rate covers the stream's current active period: it is
// measured from the period's first record, however long the stream was quiet
// before, and an idle stream reports none, however recently it was active.
func TestCatalogRate(t *testing.T) {
	h, clock := clockedHub(DefaultLimits)
	cat := subscribeWith(t, h, Selection{StreamPrefixes: []string{CatalogPrefix}})
	h.Advertise(StreamInfo{ID: "apps/a/s"})
	catalogEntry(t, next(t, cat))
	want := func(d Delivery, state agentpbv2.DataStreamInfo_State, low, high float32) {
		t.Helper()
		if info := catalogEntry(t, d); info.GetState() != state || info.GetRateHz() < low || info.GetRateHz() > high {
			t.Fatalf("got %v %v/s, want %v at %v..%v/s", info.GetState(), info.GetRateHz(), state, low, high)
		}
	}

	// A minute after it was advertised, the stream runs at 30 Hz for 12 s.
	// Its first entry has no rate yet; the next one, 10 s later, has 30/s.
	clock.advance(time.Minute)
	for i := 0; i < 12*30; i++ {
		h.Offer(record("apps/a/s", i, 8))
		if i == 0 {
			want(next(t, cat), agentpbv2.DataStreamInfo_STATE_ACTIVE, 0, 0)
		}
		clock.advance(time.Second / 30)
	}
	want(next(t, cat), agentpbv2.DataStreamInfo_STATE_ACTIVE, 29, 31)
	requireEmpty(t, cat)
	want(next(t, subscribeWith(t, h, Selection{StreamPrefixes: []string{CatalogPrefix}, SendLatest: true})), agentpbv2.DataStreamInfo_STATE_ACTIVE, 29, 31)

	// Once records stop it is idle at no rate, then and an hour later.
	clock.advance(idleAfter)
	sweepNow(h)
	want(next(t, cat), agentpbv2.DataStreamInfo_STATE_IDLE, 0, 0)
	clock.advance(time.Hour)
	want(next(t, subscribeWith(t, h, Selection{StreamPrefixes: []string{CatalogPrefix}, SendLatest: true})), agentpbv2.DataStreamInfo_STATE_IDLE, 0, 0)

	// The next active period, at 5 Hz, is measured on its own.
	for i := 0; i <= 50; i++ {
		if i > 0 {
			clock.advance(200 * time.Millisecond)
		}
		h.Offer(record("apps/a/s", i, 8))
		if i == 0 {
			want(next(t, cat), agentpbv2.DataStreamInfo_STATE_ACTIVE, 0, 0)
		}
	}
	want(next(t, cat), agentpbv2.DataStreamInfo_STATE_ACTIVE, 4.9, 5.1)
	requireEmpty(t, cat)
}

// With send_latest a catalog subscriber gets every current entry at once, in
// stream id order, then its records.
func TestCatalogSendLatest(t *testing.T) {
	h, clock := clockedHub(DefaultLimits)
	for _, id := range []string{"apps/b/log", "apps/a/markers", "apps/a/gone"} {
		h.Advertise(StreamInfo{ID: id, Publisher: id[5:6]})
	}
	h.Offer(record("apps/a/markers", 1, 8))
	h.Unadvertise("apps/a/gone")
	clock.advance(time.Second)

	for _, sel := range []Selection{
		{StreamPrefixes: []string{CatalogPrefix}, SendLatest: true},
		{Schemas: []string{CatalogSchema}, SendLatest: true},
	} {
		s := subscribeWith(t, h, sel)
		for _, want := range []struct {
			id    string
			state agentpbv2.DataStreamInfo_State
		}{{"apps/a/markers", agentpbv2.DataStreamInfo_STATE_ACTIVE}, {"apps/b/log", agentpbv2.DataStreamInfo_STATE_IDLE}} {
			if info := catalogEntry(t, next(t, s)); info.GetStreamId() != want.id || info.GetState() != want.state {
				t.Fatalf("%+v: got %v, want %s %v", sel, info, want.id, want.state)
			}
		}
		requireEmpty(t, s)
	}

	// Both: the catalog first, then the newest records.
	s := subscribeWith(t, h, Selection{StreamPrefixes: []string{CatalogPrefix, "apps/a/"}, SendLatest: true})
	for _, want := range []string{CatalogPrefix + "apps/a/markers", CatalogPrefix + "apps/b/log", "apps/a/markers"} {
		if d := next(t, s); d.Record.StreamID != want {
			t.Fatalf("got %s, want %s", d.Record.StreamID, want)
		}
	}
	requireEmpty(t, s)
}

// Catalog entries are sent once per change, so the pending-bytes cap never
// drops one: not the entries a new subscriber starts with, not a GONE queued
// before large records, and not a GONE offered while the subscriber is at the
// cap. Which data records the cap drops is not this test's concern.
func TestPendingBytesCapKeepsCatalog(t *testing.T) {
	h, clock := clockedHub(Limits{MaxPendingBytes: 1000, MaxLatestBytes: 400})
	streams := []string{"apps/a/1", "apps/a/2", "apps/a/3"}
	for i, id := range streams {
		h.Advertise(StreamInfo{ID: id})
		clock.advance(time.Millisecond)
		h.Offer(record(id, i, 400))
	}
	// catalog drains s and returns the catalog entries it held, in order.
	catalog := func(s *Subscriber) []string {
		t.Helper()
		var entries []string
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			d, err := s.Next(ctx)
			cancel()
			if err != nil {
				return entries
			}
			if strings.HasPrefix(d.Record.StreamID, CatalogPrefix) {
				info := catalogEntry(t, d)
				entries = append(entries, info.GetStreamId()+" "+info.GetState().String())
			}
		}
	}
	s := subscribeWith(t, h, Selection{StreamPrefixes: []string{CatalogPrefix, "apps/"}, SendLatest: true})
	if got, want := catalog(s), []string{"apps/a/1 STATE_ACTIVE", "apps/a/2 STATE_ACTIVE", "apps/a/3 STATE_ACTIVE"}; !slices.Equal(got, want) {
		t.Fatalf("snapshot entries %q, want %q", got, want)
	}

	h.Unadvertise("apps/a/1")
	h.Offer(record("apps/b/1", 1, 600))
	h.Offer(record("apps/b/2", 2, 600))
	if got, want := catalog(s), []string{"apps/a/1 STATE_GONE"}; !slices.Equal(got, want) {
		t.Fatalf("entries after large records %q, want %q", got, want)
	}

	h.Offer(record("apps/b/1", 3, 990))
	h.Unadvertise("apps/a/2")
	if got, want := catalog(s), []string{"apps/a/2 STATE_GONE"}; !slices.Equal(got, want) {
		t.Fatalf("entries offered at the cap %q, want %q", got, want)
	}
}

// Nothing runs on a timer while nobody is subscribed.
func TestSweeperRunsOnlyWhileSubscribed(t *testing.T) {
	h := NewHub(DefaultLimits)
	h.sweepEvery = time.Millisecond
	sweeping := func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.sweeper != nil
	}
	s, err := h.Subscribe(Selection{})
	if err != nil {
		t.Fatal(err)
	}
	if !sweeping() {
		t.Fatal("no sweeper with a subscriber")
	}
	time.Sleep(10 * time.Millisecond)
	s.Close()
	deadline := time.Now().Add(2 * time.Second)
	for sweeping() {
		if time.Now().After(deadline) {
			t.Fatal("sweeper still running after the last subscriber left")
		}
		time.Sleep(time.Millisecond)
	}
}
