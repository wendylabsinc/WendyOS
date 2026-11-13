package backend

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestUnitreeAttemptCancellationBeforeDispatch(t *testing.T) {
	var attempts UnitreeAttempts
	first, second := attempts.New(), attempts.New()
	defer attempts.Release(second)
	attempts.Cancel(first)
	if !errors.Is(attempts.Context(first).Err(), context.Canceled) {
		t.Fatal("queued attempt was not canceled")
	}
	if attempts.Context(second).Err() != nil {
		t.Fatal("cancel affected another attempt")
	}
	attempts.Release(first)
	attempts.Cancel(first)
	if !errors.Is(attempts.Context(first).Err(), context.Canceled) {
		t.Fatal("released attempt became usable")
	}
}

func TestUnitreeAttemptDeadline(t *testing.T) {
	var attempts UnitreeAttempts
	id := attempts.newWithTimeout(10 * time.Millisecond)
	defer attempts.Release(id)
	ctx := attempts.Context(id)
	select {
	case <-ctx.Done():
		if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
			t.Fatal(ctx.Err())
		}
	case <-time.After(time.Second):
		t.Fatal("attempt did not time out")
	}
}

func TestUnitreePairingCanCancelWhileBackendBusy(t *testing.T) {
	b := &Backend{}
	b.mu.Lock()
	defer b.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- b.PairUnitreeContext(ctx, 1, "GO2-123", "Go2", "192.168.1.2", "global", "", "") }()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel waited for unrelated backend operation")
	}
}
