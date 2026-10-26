package containerd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestNANRuleFailedReloadRemainsPending(t *testing.T) {
	m := newNANRuleManager()
	rules := []nanUnmanagedRule{{filepath.Join(t.TempDir(), "app.network"), "Unmanaged=yes\n"}}
	failure := errors.New("reload failed")
	calls := 0
	reload := func(context.Context) error {
		calls++
		if calls == 1 {
			return failure
		}
		return nil
	}
	if err := m.ensure(context.Background(), rules, reload); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(rules[0].path); err != nil || string(data) != rules[0].content {
		t.Fatalf("expected already-written rules: %q %v", data, err)
	}
	if err := m.ensure(context.Background(), rules, reload); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("matching files suppressed failed reload retry: calls=%d", calls)
	}
	if err := m.ensure(context.Background(), rules, reload); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("completed identical rules reloaded: %d", calls)
	}
	rules[0].content += "# changed\n"
	if err := m.ensure(context.Background(), rules, reload); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("changed rules not reloaded: %d", calls)
	}
	// A new agent instance must not trust matching files left by an old process.
	if err := newNANRuleManager().ensure(context.Background(), rules, reload); err != nil {
		t.Fatal(err)
	}
	if calls != 4 {
		t.Fatalf("restart did not confirm rules: %d", calls)
	}
}

func TestNANRuleConcurrentAppWaitsForConfirmedReload(t *testing.T) {
	m := newNANRuleManager()
	rules := []nanUnmanagedRule{{filepath.Join(t.TempDir(), "app.network"), "Unmanaged=yes\n"}}
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	reload := func(context.Context) error {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return nil
	}
	first := make(chan error, 1)
	second := make(chan error, 1)
	go func() { first <- m.ensure(context.Background(), rules, reload) }()
	<-entered
	go func() { second <- m.ensure(context.Background(), rules, reload) }()
	select {
	case err := <-second:
		t.Fatalf("second app passed before reload completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("redundant reloads: %d", calls.Load())
	}
}

func TestNANRuleQueuedCancellationHasNoSideEffects(t *testing.T) {
	m := newNANRuleManager()
	m.gate <- struct{}{}
	rules := []nanUnmanagedRule{{filepath.Join(t.TempDir(), "app.network"), "Unmanaged=yes\n"}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := m.ensure(ctx, rules, func(context.Context) error { t.Fatal("canceled waiter reloaded"); return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	<-m.gate
	if _, err := os.Stat(rules[0].path); !os.IsNotExist(err) {
		t.Fatalf("canceled waiter wrote rules: %v", err)
	}
}

func TestNANRuleCanceledReloadRetried(t *testing.T) {
	m := newNANRuleManager()
	rules := []nanUnmanagedRule{{filepath.Join(t.TempDir(), "app.network"), "Unmanaged=yes\n"}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := m.ensure(ctx, rules, func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	called := false
	if err := m.ensure(context.Background(), rules, func(context.Context) error { called = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("canceled reload was not retried")
	}
}
