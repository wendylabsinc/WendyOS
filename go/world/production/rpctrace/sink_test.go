package rpctrace

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"
)

func TestCollectorAndQueueCapacityAreIndependentBounds(t *testing.T) {
	s := sink(t, Config{Capacity: 2})
	ctx := context.Background()
	_, first := Begin(ctx, s, labels())
	_, second := Begin(ctx, s, labels())
	_, refused := Begin(ctx, s, labels())
	refused(OK)
	if got := s.Stats(); got.Active != 2 || got.DroppedInFlight != 1 {
		t.Fatalf("collector bound: %+v", got)
	}
	first(OK)
	second(OK)
	_, third := Begin(ctx, s, labels())
	third(OK)
	if got := s.Stats(); got.Active != 0 || got.Enqueued != 2 || got.Queued != 2 || got.DroppedQueueFull != 1 {
		t.Fatalf("queue bound: %+v", got)
	}
	events, _ := drain(t, s)
	_, closed := Begin(ctx, s, labels())
	closed(OK)
	if len(events) != 2 || s.Stats().DroppedClosed != 1 {
		t.Fatal("sealed sink accepted another collector")
	}
}

func TestLifetimeEventAndExactByteBudgets(t *testing.T) {
	for _, cfg := range []Config{{MaxEvents: 2}, {MaxBytes: 1000}, {MaxBytes: 1}} {
		s := sink(t, cfg)
		for range 100 {
			_, finish := Begin(context.Background(), s, labels())
			finish(OK)
		}
		events, raw := drain(t, s)
		got := s.Stats()
		if got.DroppedBudget == 0 || got.Enqueued != uint64(len(events)) || got.EnqueuedBytes != uint64(len(raw)) || len(events) > s.cfg.MaxEvents || len(raw) > s.cfg.MaxBytes || got.Active != 0 {
			t.Fatalf("lifetime limits %+v: %+v", cfg, got)
		}
	}
}

type blockedWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}
func prompt(t *testing.T, work func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { work(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("diagnostics blocked caller")
	}
}

func TestBlockedWriterNeverBlocksFinishSaturationCancellationOrClose(t *testing.T) {
	s := sink(t, Config{Capacity: 2})
	w := &blockedWriter{entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx, w) }()
	_, finish := Begin(context.Background(), s, labels())
	finish(OK)
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("writer never started")
	}
	prompt(t, func() {
		for range 1000 {
			_, finish := Begin(context.Background(), s, labels())
			finish(OK)
		}
	})
	if got := s.Stats(); got.Queued != 2 || got.Active != 0 || got.DroppedQueueFull == 0 {
		t.Fatalf("blocked writer grew queue: %+v", got)
	}
	cancel()
	prompt(t, s.Close)
	// Run cannot interrupt an arbitrary blocked io.Writer. Close must return
	// independently; releasing the caller-owned writer lets cancellation end it.
	select {
	case err := <-done:
		t.Fatalf("writer falsely reported cancellation before release: %v", err)
	default:
	}
	close(w.release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("released writer did not stop")
	}
}

func TestSubmissionContentionDropsInsteadOfWaiting(t *testing.T) {
	s := sink(t, Config{})
	_, finish := Begin(context.Background(), s, labels())
	s.submitMu.Lock()
	prompt(t, func() { finish(OK) })
	prompt(t, s.Close)
	s.submitMu.Unlock()
	events, _ := drain(t, s)
	if len(events) != 0 || s.Stats().DroppedBusy != 1 || s.Stats().Active != 0 {
		t.Fatalf("submission contention: %+v", s.Stats())
	}
}

func TestCloseDrainsPreSealReservationsWithoutJoiningLiveCollectors(t *testing.T) {
	s := sink(t, Config{})
	_, late := Begin(context.Background(), s, labels())
	if !s.enterEmit() {
		t.Fatal("reservation refused")
	}
	prompt(t, s.Close)
	done := make(chan error, 1)
	go func() { done <- s.Run(context.Background(), io.Discard) }()
	select {
	case err := <-done:
		t.Fatalf("missed in-flight emission reservation: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	s.leaveEmit()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("writer joined unfinished RPC collector")
	}
	late(OK)
	if got := s.Stats(); got.DroppedClosed != 1 || got.Active != 0 {
		t.Fatalf("late finish: %+v", got)
	}
}

type errorWriter struct{ short bool }

func (w errorWriter) Write(p []byte) (int, error) {
	if w.short {
		return len(p) / 2, nil
	}
	return 0, errors.New("/private/secret-token must not escape")
}

func TestWriterErrorsAreFixedAndRunCannotBeRepeated(t *testing.T) {
	for _, short := range []bool{false, true} {
		s := sink(t, Config{})
		_, finish := Begin(context.Background(), s, labels())
		finish(OK)
		s.Close()
		if err := s.Run(context.Background(), errorWriter{short}); !errors.Is(err, ErrWriter) || err.Error() != ErrWriter.Error() {
			t.Fatalf("writer error escaped: %v", err)
		}
		if err := s.Run(context.Background(), io.Discard); !errors.Is(err, ErrWriter) {
			t.Fatal("repeated writer")
		}
		if got := s.Stats(); got.WriterErrors != 1 || got.Written != 0 || !got.Closed {
			t.Fatalf("writer stats: %+v", got)
		}
	}
}

func TestConcurrentCloseAndFinishPreserveEveryAcceptedLine(t *testing.T) {
	// Each bounded iteration joins its single writer and 65 worker goroutines
	// before the next iteration starts. No goroutine survives the test fixture.
	for range 25 {
		s := sink(t, Config{})
		var output bytes.Buffer
		done := make(chan error, 1)
		go func() { done <- s.Run(context.Background(), &output) }()
		start := make(chan struct{})
		var work sync.WaitGroup
		for range 64 {
			ctx, finish := Begin(context.Background(), s, labels())
			work.Go(func() {
				<-start
				Add(ctx, StagePeer, time.Microsecond)
				finish(OK)
			})
		}
		work.Go(func() { <-start; s.Close() })
		close(start)
		work.Wait()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		got := s.Stats()
		if got.Active != 0 || got.Enqueued != got.Written || got.Queued != 0 || got.EnqueuedBytes != uint64(output.Len()) || got.WrittenBytes != uint64(output.Len()) {
			t.Fatalf("lost accepted output across seal: %+v", got)
		}
	}
}
