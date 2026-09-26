package models_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	"github.com/wendylabsinc/wendy/go/internal/agent/models/modelstest"
)

// next returns the watch's next message, failing after a second.
func next(t *testing.T, w *models.Watch) models.WatchMessage {
	t.Helper()
	select {
	case msg, ok := <-w.C:
		if !ok {
			t.Fatal("the watch closed")
		}
		return msg
	case <-time.After(time.Second):
		t.Fatal("no message within a second")
	}
	return models.WatchMessage{}
}

// drainToEnd reads until the watch closes and returns the final state it reports.
func drainToEnd(t *testing.T, w *models.Watch) *models.InstanceInfo {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case _, ok := <-w.C:
			if !ok {
				return w.Final()
			}
		case <-deadline:
			t.Fatal("the watch never closed")
		}
	}
}

// advanceAlive moves the clock by d in five-second steps, with the host
// reporting ready at every step the way a healthy host heartbeats.
func (h *harness) advanceAlive(id string, d time.Duration) {
	for elapsed := time.Duration(0); elapsed < d; elapsed += 5 * time.Second {
		h.sup.PublishApplicationRecord(models.AppIDPrefix+id, modelstest.Status(models.HostReady))
		h.clock.Advance(5 * time.Second)
	}
}

func TestNeverWatchedInstanceExpires(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	h.advanceAlive(info.ID, 55*time.Second)
	time.Sleep(10 * time.Millisecond)
	if h.info(info.ID).State != models.StateReady {
		t.Fatal("the instance stopped before its grace period ended")
	}
	h.advanceAlive(info.ID, 10*time.Second)
	modelstest.Eventually(t, "the unwatched instance to stop", func() bool { return h.info(info.ID).State == 0 })
	if !slices.Contains(h.runtime.Removed(), info.ID) {
		t.Fatal("the host was not removed")
	}
}

func TestLostWatchKeepsInstanceForGrace(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	w, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	h.advanceAlive(info.ID, 5*time.Minute) // watched: never expires
	h.sup.Detach(info.ID, w.ID)
	h.advanceAlive(info.ID, 30*time.Second)
	w2, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID})
	if err != nil {
		t.Fatalf("re-attach within the grace period: %v", err)
	}
	h.advanceAlive(info.ID, 5*time.Minute)
	if h.info(info.ID).State != models.StateReady {
		t.Fatal("a re-attached instance expired")
	}
	h.sup.Detach(info.ID, w2.ID)
	h.advanceAlive(info.ID, 65*time.Second)
	modelstest.Eventually(t, "expiry after the last watch is lost", func() bool { return h.info(info.ID).State == 0 })
}

func TestStopWithLastWatchStopsAtOnce(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	w1, _, _, _ := h.sup.Watch(models.WatchRequest{InstanceID: info.ID, Label: "front door"})
	w2, _, _, _ := h.sup.Watch(models.WatchRequest{InstanceID: info.ID, Label: "porch"})
	still, err := h.sup.Stop(context.Background(), info.ID, w1.ID)
	if err != nil || still.State != models.StateReady || still.Watchers != 1 || !slices.Equal(still.WatchLabels, []string{"porch"}) {
		t.Fatalf("after the first watch stops: %+v, %v", still, err)
	}
	final, err := h.sup.Stop(context.Background(), info.ID, w2.ID)
	if err != nil || final.State != models.StateStopped || final.StateDetail != "stopped by its last watcher" {
		t.Fatalf("after the last watch stops: %+v, %v", final, err)
	}
	if _, err := h.sup.Stop(context.Background(), info.ID, "w-unknown"); !errors.Is(err, models.ErrUnknownInstance) {
		t.Fatalf("stop after removal: %v", err)
	}
}

func TestStoppedInstanceEndsWatchesWithFinalState(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	w, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.sup.Stop(context.Background(), info.ID, ""); err != nil {
		t.Fatal(err)
	}
	if final := drainToEnd(t, w); final == nil || final.State != models.StateStopped || final.StateDetail != "stopped" {
		t.Fatalf("final = %+v", final)
	}
}

func TestWatchSeesStateChanges(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info, _, err := h.sup.Start(context.Background(), "coco-detector", frontDoor)
	if err != nil {
		t.Fatal(err)
	}
	w, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	modelstest.Eventually(t, "the host to start", func() bool { return h.runtime.Running(info.ID) })
	h.sup.PublishApplicationRecord(models.AppIDPrefix+info.ID, modelstest.Status(models.HostReady))
	for {
		if msg := next(t, w); msg.Status != nil && msg.Status.State == models.StateReady {
			return
		}
	}
}

func TestWatchRejectsBadFilters(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	for _, f := range []models.Filter{{Classes: []string{"unicorn"}}, {Types: []string{"appeared"}}, {MinConfidence: 1.5}} {
		if _, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID, Filter: f}); !errors.Is(err, models.ErrInvalidFilter) {
			t.Fatalf("filter %+v: %v", f, err)
		}
	}
	if _, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: "m-missing"}); !errors.Is(err, models.ErrUnknownInstance) {
		t.Fatalf("unknown instance: %v", err)
	}
}

// TestWatchRefusesAnInstanceStillBeingAdmitted holds a Start in flight behind
// a blocked camera acquisition: the instance already exists in the
// supervisor's map, but Start has not yet settled whether it may run. A Watch
// during that window must be refused, not attached to an instance that may
// never exist. Once Start settles (admits the instance), Watch succeeds.
func TestWatchRefusesAnInstanceStillBeingAdmitted(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	h.cameras.BlockAcquire = make(chan struct{})

	done := make(chan struct{})
	var startErr error
	go func() {
		_, _, startErr = h.sup.Start(context.Background(), "coco-detector", frontDoor)
		close(done)
	}()
	modelstest.Eventually(t, "the instance to be held for admission", func() bool { return len(h.sup.List()) == 1 })
	id := h.sup.List()[0].ID

	if _, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: id}); !errors.Is(err, models.ErrUnknownInstance) {
		t.Fatalf("watch on an instance still being admitted: %v", err)
	}

	close(h.cameras.BlockAcquire)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("start did not return after the camera was acquired")
	}
	if startErr != nil {
		t.Fatal(startErr)
	}

	if _, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: id}); err != nil {
		t.Fatalf("watch after admission: %v", err)
	}
}

// TestReuseRestartsTheUnwatchedLease covers the review fix: a Start that
// reuses an instance which has no watch yet must restart the instance's
// lease, not leave the timer from its first Start (or an earlier reuse)
// running toward its own, now-stale, expiry.
func TestReuseRestartsTheUnwatchedLease(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor) // T0
	h.advanceAlive(info.ID, 50*time.Second)

	again, reused, err := h.sup.Start(context.Background(), "coco-detector", frontDoor)
	if err != nil || !reused || again.ID != info.ID {
		t.Fatalf("reuse at 50s = %+v, reused=%v, %v", again, reused, err)
	}

	// 80s since T0: past the original, un-restarted T0+60 expiry. Without the
	// fix this Watch fails because the instance already stopped.
	h.advanceAlive(info.ID, 30*time.Second)
	w, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID})
	if err != nil {
		t.Fatalf("watch after a reuse should have restarted the lease: %v", err)
	}

	h.sup.Detach(info.ID, w.ID) // unwatched again: starts a fresh, ordinary grace
	h.advanceAlive(info.ID, 65*time.Second)
	modelstest.Eventually(t, "the instance to expire once its restarted lease runs out", func() bool { return h.info(info.ID).State == 0 })
}
