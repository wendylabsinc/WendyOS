package sessionbroker

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func embeddedSpec() Spec {
	return Spec{Key: "mobile-fixture", Host: "device", Addr: "device:50052", CertFingerprint: "selected",
		Expected: certs.WendyIdentity{OrgID: 7, EntityType: "asset", EntityID: "42"}}
}

func TestEmbeddedHostSharesOneOwnerAndJoinsBeforeResume(t *testing.T) {
	var starts atomic.Int32
	var running atomic.Int32
	h := newEmbeddedRuntime(func(ctx context.Context, _ Spec) error {
		starts.Add(1)
		if running.Add(1) != 1 {
			t.Error("overlapping router generations")
		}
		defer running.Add(-1)
		<-ctx.Done()
		return ctx.Err()
	})
	var clients sync.WaitGroup
	for range 16 {
		clients.Add(1)
		go func() {
			defer clients.Done()
			if err := h.start(embeddedSpec()); err != nil {
				t.Error(err)
			}
		}()
	}
	clients.Wait()
	if err := h.setActive(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 1 || running.Load() != 0 {
		t.Fatalf("owners: starts=%d running=%d", starts.Load(), running.Load())
	}
	if err := h.start(embeddedSpec()); !errors.Is(err, ErrEmbeddedSuspended) {
		t.Fatalf("background start: %v", err)
	}
	if err := h.setActive(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := h.start(embeddedSpec()); err != nil {
		t.Fatal(err)
	}
	if err := h.setActive(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if starts.Load() != 2 || running.Load() != 0 {
		t.Fatal("resume did not own and join a new generation")
	}
}

func TestEmbeddedTimedOutDrainKeepsAdmissionClosed(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	h := newEmbeddedRuntime(func(ctx context.Context, _ Spec) error { close(started); <-ctx.Done(); <-release; return ctx.Err() })
	if err := h.start(embeddedSpec()); err != nil {
		t.Fatal(err)
	}
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := h.setActive(ctx, false); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("drain: %v", err)
	}
	if err := h.start(embeddedSpec()); !errors.Is(err, ErrEmbeddedSuspended) {
		t.Fatalf("admission reopened early: %v", err)
	}
	close(release)
	if err := h.setActive(context.Background(), true); err != nil {
		t.Fatal(err)
	}
}

func TestEmbeddedRejectsCredentialReplacementBeforeDrain(t *testing.T) {
	h := newEmbeddedRuntime(func(ctx context.Context, _ Spec) error { <-ctx.Done(); return ctx.Err() })
	defer h.setActive(context.Background(), false)
	if err := h.start(embeddedSpec()); err != nil {
		t.Fatal(err)
	}
	replacement := embeddedSpec()
	replacement.CertFingerprint = "renewed"
	if err := h.start(replacement); err == nil {
		t.Fatal("replaced live credentials without joining the previous owner")
	}
	if err := h.setActive(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if err := h.setActive(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := h.start(replacement); err != nil {
		t.Fatal(err)
	}
}
