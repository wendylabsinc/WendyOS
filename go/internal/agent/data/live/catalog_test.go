package live

import (
	"context"
	"sync"
	"testing"
	"time"

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
