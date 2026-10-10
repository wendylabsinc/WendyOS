package models_test

import (
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/models"
	"github.com/wendylabsinc/wendy/go/internal/agent/models/modelstest"
)

// nextEvent skips status messages and returns the next event.
func nextEvent(t *testing.T, w *models.Watch) models.Event {
	t.Helper()
	for {
		if msg := next(t, w); msg.Event != nil {
			return *msg.Event
		}
	}
}

func TestWatchGetsFilteredNumberedEvents(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	w, _, last, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID,
		Filter: models.Filter{Classes: []string{"person"}, MinConfidence: 0.5}})
	if err != nil || last != 0 {
		t.Fatalf("watch: last=%d err=%v", last, err)
	}
	app := models.AppIDPrefix + info.ID
	h.sup.PublishApplicationRecord(app, modelstest.Entered("person", 0.9, 1))
	h.sup.PublishApplicationRecord(app, modelstest.Entered("car", 0.9, 2))
	h.sup.PublishApplicationRecord(app, modelstest.Entered("person", 0.3, 3))
	h.sup.PublishApplicationRecord(app, modelstest.Left("person", 0.9, 1))
	first, second := nextEvent(t, w), nextEvent(t, w)
	if first.Sequence != 1 || first.Type != models.EventEntered || first.Class != "person" || first.SampleID != 1 {
		t.Fatalf("first = %+v", first)
	}
	if second.Sequence != 4 || second.Type != models.EventLeft {
		t.Fatalf("second = %+v; the car and the unsure person must be filtered out", second)
	}
}

func TestReattachReplaysMissedEvents(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	app := models.AppIDPrefix + info.ID
	for i := 1; i <= 105; i++ {
		h.sup.PublishApplicationRecord(app, modelstest.Entered("person", 0.9, i))
	}
	w, _, last, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID, AfterSequence: 3})
	if err != nil || last != 105 {
		t.Fatalf("watch: last=%d err=%v", last, err)
	}
	if msg := next(t, w); msg.Gap == nil || *msg.Gap != (models.Gap{FirstMissing: 4, LastMissing: 5}) {
		t.Fatalf("first message = %+v, want the evicted range 4-5", msg)
	}
	for want := uint64(6); want <= 105; want++ {
		if e := nextEvent(t, w); e.Sequence != want {
			t.Fatalf("replayed %d, want %d", e.Sequence, want)
		}
	}
}

func TestSlowWatchNeverBlocksIntake(t *testing.T) {
	h := newHarness(t, models.EngineONNXRuntime)
	info := h.startReady(t, frontDoor)
	w, _, _, err := h.sup.Watch(models.WatchRequest{InstanceID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	app := models.AppIDPrefix + info.ID
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 1; i <= 300; i++ {
			h.sup.PublishApplicationRecord(app, modelstest.Entered("person", 0.9, i))
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("record intake blocked on a watch nobody reads")
	}
	var lastSeen uint64
	for i := 0; i < 128; i++ { // drain what fit in the buffer
		if msg := next(t, w); msg.Event != nil {
			lastSeen = msg.Event.Sequence
		}
	}
	h.sup.PublishApplicationRecord(app, modelstest.Entered("person", 0.9, 301))
	msg := next(t, w)
	if msg.Gap == nil || msg.Gap.FirstMissing != lastSeen+1 || msg.Gap.LastMissing != 300 {
		t.Fatalf("after draining: %+v, want a gap from %d to 300", msg, lastSeen+1)
	}
	if e := nextEvent(t, w); e.Sequence != 301 {
		t.Fatalf("next event %d, want 301", e.Sequence)
	}
}
