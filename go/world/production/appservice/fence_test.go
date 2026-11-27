package appservice

import (
	"context"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
)

func TestRemovedGenerationCannotEnterDelayedAdmission(t *testing.T) {
	started := make(chan struct{})
	resume := make(chan struct{})
	done := make(chan struct{})
	f := setup(t, func(cfg *Config) {
		accept := cfg.Accept
		cfg.Accept = func(ctx context.Context, scope serviceorigin.Scope, record serviceorigin.SignedRecord, fence *Fence) error {
			defer close(done)
			close(started)
			<-resume
			return accept(ctx, scope, record, fence)
		}
	})
	client, _ := dial(t, f.mount(t, f.scope).SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	errCh := make(chan error, 1)
	go func() { _, err := client.Publish(ctx, request()); errCh <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("sink not entered")
	}
	f.manager.Release(f.scope.App, f.scope.Service)
	close(resume)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("sink did not exit")
	}
	if err := <-errCh; err == nil {
		t.Fatal("removed generation acknowledged")
	}
	if f.count() != 0 {
		t.Fatal("late sink persisted removed generation")
	}
}

func TestAcknowledgementRequiresSuccessfulFenceCommit(t *testing.T) {
	var retained *Fence
	f := setup(t, func(cfg *Config) {
		cfg.Accept = func(_ context.Context, _ serviceorigin.Scope, _ serviceorigin.SignedRecord, fence *Fence) error {
			retained = fence
			return nil
		}
	})
	client, _ := dial(t, f.mount(t, f.scope).SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if reply, err := client.Publish(ctx, request()); err == nil || reply != nil {
		t.Fatal("sink skipped generation fence but acknowledged")
	}
	if retained == nil {
		t.Fatal("sink not called")
	}
	if err := retained.Commit(context.Background(), func(context.Context) error { t.Fatal("retained fence admitted after RPC"); return nil }); err == nil {
		t.Fatal("retained fence remains usable")
	}
}

func TestClockRecheckedAtAtomicAdmission(t *testing.T) {
	var f *fixture
	f = setup(t, func(cfg *Config) {
		accept := cfg.Accept
		cfg.Accept = func(ctx context.Context, scope serviceorigin.Scope, record serviceorigin.SignedRecord, fence *Fence) error {
			f.clock.advance(11 * time.Minute)
			return accept(ctx, scope, record, fence)
		}
	})
	client, _ := dial(t, f.mount(t, f.scope).SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.Publish(ctx, request()); err == nil {
		t.Fatal("expired signing authority admitted")
	}
	if f.count() != 0 {
		t.Fatal("expired record reached persistence")
	}
}

func TestCommittedButInvalidatedResponseDoesNotClaimRollback(t *testing.T) {
	var f *fixture
	f = setup(t, func(cfg *Config) {
		accept := cfg.Accept
		cfg.Accept = func(ctx context.Context, scope serviceorigin.Scope, record serviceorigin.SignedRecord, fence *Fence) error {
			if err := accept(ctx, scope, record, fence); err != nil {
				return err
			}
			f.mu.Lock()
			p := f.publishers[scope.Service]
			f.mu.Unlock()
			_ = p.Close()
			return nil
		}
	})
	client, _ := dial(t, f.mount(t, f.scope).SocketPath)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := client.Publish(ctx, request()); err == nil {
		t.Fatal("invalidated response acknowledged")
	}
	if f.count() != 1 {
		t.Fatal("test failed to exercise a committed unknown outcome")
	}
}
