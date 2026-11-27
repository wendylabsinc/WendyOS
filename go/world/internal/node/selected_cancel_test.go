package node

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/selection"
)

func TestSelectedCancelWhileQueued(t *testing.T) {
	for _, count := range []int{1, maxSelectedWatchers} {
		name := "one"
		if count > 1 {
			name = "capacity"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, nil, "cancel-queue")
			c := selectedClient(t, f, f.credentials)
			f.n.lock()
			// Keep contention in place through all assertions. Cancellation must not
			// depend on the publication/storage operation that currently owns this lock.
			defer f.n.mu.Unlock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, count)
			for i := 0; i < count; i++ {
				go func() { _, err := c.WatchSelected(ctx, selection.WatchRequest{WaitMS: 5000}); done <- err }()
			}
			until := time.Now().Add(2 * time.Second)
			for f.n.selectedWatchers.Load() != int64(count) && time.Now().Before(until) {
				time.Sleep(time.Millisecond)
			}
			if got := f.n.selectedWatchers.Load(); got != int64(count) {
				t.Fatalf("queued watches: got %d want %d", got, count)
			}
			if count == maxSelectedWatchers {
				capacityCtx, stopCapacity := context.WithTimeout(context.Background(), time.Second)
				defer stopCapacity()
				_, err := c.WatchSelected(capacityCtx, selection.WatchRequest{WaitMS: 0})
				var status *client.HTTPStatusError
				if !errors.As(err, &status) || status.StatusCode != http.StatusTooManyRequests {
					t.Fatalf("capacity not enforced: %v", err)
				}
			}
			cancel()
			for i := 0; i < count; i++ {
				select {
				case err := <-done:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("client cancel: %v", err)
					}
				case <-time.After(time.Second):
					t.Fatal("client did not cancel")
				}
			}
			until = time.Now().Add(time.Second)
			for f.n.selectedWatchers.Load() != 0 && time.Now().Before(until) {
				time.Sleep(time.Millisecond)
			}
			if got := f.n.selectedWatchers.Load(); got != 0 {
				t.Fatalf("cancelled watches retain %d slots while node lock remains held", got)
			}
		})
	}
}
