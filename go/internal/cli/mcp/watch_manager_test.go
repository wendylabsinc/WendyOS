package mcp

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

type fakeWatchHandle struct {
	updates   chan watchUpdate
	stopped   atomic.Int32
	stopWait  time.Duration
	closeOnce sync.Once
}

func (h *fakeWatchHandle) Updates() <-chan watchUpdate { return h.updates }
func (h *fakeWatchHandle) Stop(ctx context.Context) error {
	h.stopped.Add(1)
	if h.stopWait > 0 {
		select {
		case <-time.After(h.stopWait):
		case <-ctx.Done():
		}
	}
	h.closeOnce.Do(func() { close(h.updates) })
	return nil
}

type fakeWatchBackend struct {
	mu       sync.Mutex
	specs    []watchSpec
	handles  []*fakeWatchHandle
	err      error
	stopWait time.Duration
	started  chan struct{} // optional: Start blocks until it is closed
}

func (b *fakeWatchBackend) Start(ctx context.Context, _ *grpcclient.AgentConnection, spec watchSpec) (watchHandle, error) {
	if b.started != nil {
		<-b.started
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.err != nil {
		return nil, b.err
	}
	h := &fakeWatchHandle{updates: make(chan watchUpdate, 256), stopWait: b.stopWait}
	b.specs = append(b.specs, spec)
	b.handles = append(b.handles, h)
	return h, nil
}

func (b *fakeWatchBackend) handle(i int) *fakeWatchHandle {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.handles[i]
}

type sentNotification struct {
	method string
	params map[string]any
}

type notificationLog struct {
	mu   sync.Mutex
	sent []sentNotification
	fail atomic.Int32 // fail this many sends
}

func (l *notificationLog) notify(method string, params map[string]any) error {
	if l.fail.Load() > 0 {
		l.fail.Add(-1)
		return errors.New("notification channel blocked")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sent = append(l.sent, sentNotification{method, params})
	return nil
}

func (l *notificationLog) waitFor(t *testing.T, match func(sentNotification) bool) sentNotification {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		l.mu.Lock()
		for _, n := range l.sent {
			if match(n) {
				l.mu.Unlock()
				return n
			}
		}
		l.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("expected notification was not sent")
	return sentNotification{}
}

func newTestWatchManager(backend watchBackend) (*watchManager, *notificationLog, *atomic.Uint64) {
	log := &notificationLog{}
	revision := &atomic.Uint64{}
	revision.Store(1)
	m := newWatchManager(backend, log.notify, revision.Load)
	m.startWait = 100 * time.Millisecond
	return m, log, revision
}

func startTestWatch(t *testing.T, m *watchManager, label string) watchView {
	t.Helper()
	view, err := m.start(context.Background(), &grpcclient.AgentConnection{}, 1, watchSpec{CameraID: "v4l2:/dev/video0", CameraName: "Brio 101", Classes: []string{"person"}, MinConfidence: 0.5, Label: label})
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func waitState(t *testing.T, m *watchManager, id string, state watchState) watchView {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, v := range m.list() {
			if v.WatchID == id && v.State == string(state) {
				return v
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("watch %s never reached %s: %+v", id, state, m.list())
	return watchView{}
}

func TestWatchManagerAssignsIdentitiesAndCapsActiveWatches(t *testing.T) {
	backend := &fakeWatchBackend{}
	m, _, _ := newTestWatchManager(backend)
	first, second := startTestWatch(t, m, "front door"), startTestWatch(t, m, "garage")
	if first.WatchID != "w1" || second.WatchID != "w2" {
		t.Fatalf("ids %q %q", first.WatchID, second.WatchID)
	}
	if !regexp.MustCompile(`^chat-[0-9a-f]{8}-1$`).MatchString(backend.specs[0].Name) || !strings.HasSuffix(backend.specs[1].Name, "-2") {
		t.Fatalf("campaign names %q %q", backend.specs[0].Name, backend.specs[1].Name)
	}
	if first.State != string(watchPreparing) || m.freeSlots() != 0 {
		t.Fatalf("view %+v, free %d", first, m.freeSlots())
	}
	_, err := m.start(context.Background(), &grpcclient.AgentConnection{}, 1, watchSpec{Label: "third", Classes: []string{"person"}})
	if !errors.Is(err, errWatchLimit) || !strings.Contains(err.Error(), "front door") || !strings.Contains(err.Error(), "garage") {
		t.Fatalf("third watch: %v", err)
	}
	if _, err := m.stop(context.Background(), "w1", watchStoppedReason); err != nil {
		t.Fatal(err)
	}
	if backend.handle(0).stopped.Load() != 1 {
		t.Fatal("stop did not stop the backend watch")
	}
	if third := startTestWatch(t, m, "third"); third.WatchID != "w3" {
		t.Fatalf("a freed slot was not reused: %+v", third)
	}
	if _, err := m.stop(context.Background(), "w9", watchStoppedReason); !errors.Is(err, errWatchNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	if v, err := m.stop(context.Background(), "w1", watchStoppedReason); err != nil || v.State != string(watchEnded) {
		t.Fatalf("stopping an ended watch must be harmless: %+v %v", v, err)
	}
}

func TestWatchManagerStartReturnsOnReadyOrAfterTheWait(t *testing.T) {
	backend := &fakeWatchBackend{}
	m, log, _ := newTestWatchManager(backend)
	m.startWait = time.Second
	go func() {
		for {
			backend.mu.Lock()
			n := len(backend.handles)
			backend.mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(time.Millisecond)
		}
		backend.handle(0).updates <- watchUpdate{Status: &watchStatusUpdate{State: watchReady}}
	}()
	if v := startTestWatch(t, m, "front door"); v.State != string(watchReady) {
		t.Fatalf("start returned %+v, want READY", v)
	}
	log.waitFor(t, func(n sentNotification) bool {
		return n.method == watchStatusMethod && n.params["state"] == "READY" && n.params["camera"] == "Brio 101"
	})
	m.startWait = 50 * time.Millisecond
	began := time.Now()
	if v := startTestWatch(t, m, "garage"); v.State != string(watchPreparing) || time.Since(began) < 50*time.Millisecond {
		t.Fatalf("start returned %+v after %s", v, time.Since(began))
	}
}

func TestWatchManagerBuffersLastHundredEvents(t *testing.T) {
	backend := &fakeWatchBackend{}
	m, log, _ := newTestWatchManager(backend)
	startTestWatch(t, m, "front door")
	h := backend.handle(0)
	for i := 0; i < 105; i++ {
		h.updates <- watchUpdate{Event: &watchEventUpdate{Kind: "entered", Classes: []watchClass{{Label: "person", Score: 0.9}}, OccurredAt: time.Now()}}
	}
	log.waitFor(t, func(n sentNotification) bool {
		return n.method == watchEventMethod && n.params["sequence"] == uint64(105)
	})
	_, events, gap, err := m.events(context.Background(), "w1", 0, 0)
	if err != nil || len(events) != 100 || events[0].Sequence != 6 || events[99].Sequence != 105 || !gap {
		t.Fatalf("got %d events from %d, gap %v, err %v", len(events), events[0].Sequence, gap, err)
	}
	_, events, gap, _ = m.events(context.Background(), "w1", 100, 0)
	if len(events) != 5 || gap {
		t.Fatalf("after 100: %d events, gap %v", len(events), gap)
	}
	if v := m.list()[0]; v.LastSequence != 105 || v.LastEventAt == "" {
		t.Fatalf("view %+v", v)
	}
}

func TestWatchManagerEventsWaitsForTheNextEvent(t *testing.T) {
	backend := &fakeWatchBackend{}
	m, _, _ := newTestWatchManager(backend)
	startTestWatch(t, m, "front door")
	go func() {
		time.Sleep(50 * time.Millisecond)
		backend.handle(0).updates <- watchUpdate{Event: &watchEventUpdate{Kind: "entered", OccurredAt: time.Now()}}
	}()
	began := time.Now()
	_, events, _, err := m.events(context.Background(), "w1", 0, 2*time.Second)
	if err != nil || len(events) != 1 || time.Since(began) > time.Second {
		t.Fatalf("events %+v err %v after %s", events, err, time.Since(began))
	}
	if _, events, _, _ := m.events(context.Background(), "w1", 1, 30*time.Millisecond); len(events) != 0 {
		t.Fatal("a wait with no new event must return empty")
	}
}

func TestWatchManagerCountsFailedNotifications(t *testing.T) {
	backend := &fakeWatchBackend{}
	m, log, _ := newTestWatchManager(backend)
	startTestWatch(t, m, "front door")
	log.fail.Store(1)
	backend.handle(0).updates <- watchUpdate{Event: &watchEventUpdate{Kind: "entered", OccurredAt: time.Now()}}
	backend.handle(0).updates <- watchUpdate{Status: &watchStatusUpdate{State: watchReady}}
	n := log.waitFor(t, func(n sentNotification) bool { return n.method == watchStatusMethod && n.params["state"] == "READY" })
	if n.params["missed_notifications"] != 1 {
		t.Fatalf("status %+v must report the missed event notification", n.params)
	}
	if _, events, _, _ := m.events(context.Background(), "w1", 0, 0); len(events) != 1 {
		t.Fatal("an event whose notification failed must still be in the buffer")
	}
}

func TestWatchManagerReportsGapsWithoutChangingState(t *testing.T) {
	backend := &fakeWatchBackend{}
	m, log, _ := newTestWatchManager(backend)
	startTestWatch(t, m, "front door")
	backend.handle(0).updates <- watchUpdate{Status: &watchStatusUpdate{State: watchReady}}
	backend.handle(0).updates <- watchUpdate{Gap: "some detections may have been missed"}
	log.waitFor(t, func(n sentNotification) bool {
		return n.method == watchStatusMethod && n.params["state"] == "READY" && n.params["reason"] == "some detections may have been missed"
	})
	if v := waitState(t, m, "w1", watchReady); v.Reason != "" {
		t.Fatalf("a gap must not become the watch's standing reason: %+v", v)
	}
}

func TestWatchManagerEndsWatchesWhenTheBackendEndsThem(t *testing.T) {
	backend := &fakeWatchBackend{}
	m, _, _ := newTestWatchManager(backend)
	startTestWatch(t, m, "front door")
	backend.handle(0).updates <- watchUpdate{Status: &watchStatusUpdate{State: watchEnded, Reason: "the device restarted or the watch expired"}}
	if v := waitState(t, m, "w1", watchEnded); v.Reason != "the device restarted or the watch expired" || m.freeSlots() != 2 {
		t.Fatalf("view %+v, free %d", v, m.freeSlots())
	}
}

func TestWatchManagerEndsStaleWatchesOnConnectionChange(t *testing.T) {
	backend := &fakeWatchBackend{}
	m, log, revision := newTestWatchManager(backend)
	startTestWatch(t, m, "front door")
	revision.Store(2)
	m.endStale(2)
	if v := waitState(t, m, "w1", watchEnded); v.Reason != "device changed" {
		t.Fatalf("view %+v", v)
	}
	log.waitFor(t, func(n sentNotification) bool {
		return n.params["state"] == "ENDED" && n.params["reason"] == "device changed"
	})
	deadline := time.Now().Add(2 * time.Second)
	for backend.handle(0).stopped.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if backend.handle(0).stopped.Load() == 0 {
		t.Fatal("the stale watch's renewals were not stopped")
	}
}

func TestWatchManagerEndsWatchStartedDuringConnectionChange(t *testing.T) {
	backend := &fakeWatchBackend{started: make(chan struct{})}
	m, _, revision := newTestWatchManager(backend)
	done := make(chan watchView, 1)
	go func() {
		v, _ := m.start(context.Background(), &grpcclient.AgentConnection{}, 1, watchSpec{Classes: []string{"person"}, Label: "front door"})
		done <- v
	}()
	time.Sleep(20 * time.Millisecond)
	revision.Store(2)
	m.endStale(2)
	close(backend.started)
	v := <-done
	if v.State != string(watchEnded) || v.Reason != "device changed" {
		t.Fatalf("a watch deployed across a connection change must end: %+v", v)
	}
	if backend.handle(0).stopped.Load() == 0 {
		t.Fatal("its backend watch was not stopped")
	}
}

func TestWatchManagerShutdownStopsConcurrently(t *testing.T) {
	backend := &fakeWatchBackend{stopWait: 400 * time.Millisecond}
	m, _, _ := newTestWatchManager(backend)
	startTestWatch(t, m, "front door")
	startTestWatch(t, m, "garage")
	began := time.Now()
	m.shutdown()
	if elapsed := time.Since(began); elapsed > 700*time.Millisecond {
		t.Fatalf("shutdown took %s; removals must run concurrently", elapsed)
	}
	if backend.handle(0).stopped.Load() != 1 || backend.handle(1).stopped.Load() != 1 {
		t.Fatal("not every watch was stopped")
	}
	if _, err := m.start(context.Background(), &grpcclient.AgentConnection{}, 1, watchSpec{Classes: []string{"person"}}); err == nil {
		t.Fatal("a closed manager must refuse new watches")
	}
}
