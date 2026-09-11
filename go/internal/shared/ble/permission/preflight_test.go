package permission

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// resetPreflight clears the cached result and installs fakeRun as runCheck
// for the duration of the test, restoring both afterwards — resolved and
// resolvedErr otherwise persist across tests in the same binary the same way
// they persist across calls in production.
func resetPreflight(t *testing.T, fakeRun func(ctx context.Context, exe string) error) {
	t.Helper()
	origResolved, origErr, origRun := resolved, resolvedErr, runCheck
	resolved, resolvedErr = false, nil
	runCheck = fakeRun
	t.Cleanup(func() {
		resolved, resolvedErr, runCheck = origResolved, origErr, origRun
	})
}

func TestPreflightCachesSuccess(t *testing.T) {
	calls := 0
	resetPreflight(t, func(context.Context, string) error {
		calls++
		return nil
	})

	if err := Preflight(context.Background()); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if err := Preflight(context.Background()); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if calls != 1 {
		t.Errorf("runCheck called %d times, want 1", calls)
	}
}

// TestPreflightCachesGenuineFailure proves a real probe failure — e.g. the
// subprocess exiting non-zero because the platform actually denied
// Bluetooth TCC access — is cached forever, matching the original
// sync.Once behavior for this case.
func TestPreflightCachesGenuineFailure(t *testing.T) {
	calls := 0
	resetPreflight(t, func(context.Context, string) error {
		calls++
		return errors.New("exit status 1")
	})

	err1 := Preflight(context.Background())
	if err1 == nil {
		t.Fatal("first call: want an error, got nil")
	}
	err2 := Preflight(context.Background())
	if err2 != err1 {
		t.Errorf("second call returned %v, want the same cached error %v", err2, err1)
	}
	if calls != 1 {
		t.Errorf("runCheck called %d times, want 1", calls)
	}
}

// TestPreflightDoesNotCacheContextCancellation is the regression test for
// the bug: a probe killed by the CALLER's context ending must not poison the
// cache forever. The fake mirrors what exec.Cmd actually returns when
// CommandContext's Cancel kills a running child (see os/exec's watchCtx and
// Wait): a plain non-context error, NOT context.Canceled — proving Preflight
// distinguishes this case via ctx.Err() rather than errors.Is on the
// underlying error.
func TestPreflightDoesNotCacheContextCancellation(t *testing.T) {
	calls := 0
	started := make(chan struct{})
	resetPreflight(t, func(ctx context.Context, _ string) error {
		calls++
		close(started)
		<-ctx.Done() // simulate the probe still running when ctx ends
		return errors.New("signal: killed")
	})

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()

	if err := Preflight(ctx); err == nil {
		t.Fatal("want a non-nil error for an interrupted probe")
	}
	if resolved {
		t.Fatal("an interrupted probe must not be cached")
	}

	// The next call, with a live context, gets a real probe and IS cached.
	// Swap in a fake that succeeds instead of blocking on ctx.Done(), since
	// context.Background() here never closes.
	runCheck = func(context.Context, string) error {
		calls++
		return nil
	}
	if err := Preflight(context.Background()); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !resolved || calls != 2 {
		t.Errorf("resolved=%v calls=%d, want resolved=true calls=2", resolved, calls)
	}
}

// TestPreflightAlreadyCanceledContext covers the other ctx-cancellation
// path: ctx already done before Preflight is even called (mirrors
// exec.Cmd.Start's own early-return-without-spawning behavior).
func TestPreflightAlreadyCanceledContext(t *testing.T) {
	resetPreflight(t, func(ctx context.Context, _ string) error {
		return ctx.Err()
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := Preflight(ctx); err == nil {
		t.Fatal("want a non-nil error")
	}
	if resolved {
		t.Fatal("must not cache when ctx was already canceled")
	}
}

// TestPreflightSerializesConcurrentCallers proves concurrent callers still
// share one probe, matching sync.Once's original guarantee.
func TestPreflightSerializesConcurrentCallers(t *testing.T) {
	var calls int
	var countMu sync.Mutex
	release := make(chan struct{})
	resetPreflight(t, func(context.Context, string) error {
		countMu.Lock()
		calls++
		countMu.Unlock()
		<-release
		return nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = Preflight(context.Background())
		}()
	}
	close(release)
	wg.Wait()

	countMu.Lock()
	defer countMu.Unlock()
	if calls != 1 {
		t.Errorf("runCheck called %d times concurrently, want 1", calls)
	}
}
