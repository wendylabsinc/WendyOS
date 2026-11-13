//go:build linux

package nanprovider

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"testing"
	"time"
)

func TestCleanupRetriesTerminatedHelperWithinSameBudget(t *testing.T) {
	terminated := exec.Command("sh", "-c", "kill -TERM $$").Run()
	if terminated == nil {
		t.Fatal("helper was not terminated")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	calls := 0
	err := retryInterruptedCleanup(ctx, func(actual context.Context) error {
		calls++
		if actual != ctx {
			t.Error("retry got a fresh cleanup budget")
		}
		if calls == 1 {
			return fmt.Errorf("NAN helper: %w", terminated)
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("interrupted cleanup: calls=%d, err=%v", calls, err)
	}
}

func TestCleanupPreservesOrdinaryFailureAndCancellation(t *testing.T) {
	failed := exec.Command("sh", "-c", "exit 1").Run()
	terminated := exec.Command("sh", "-c", "kill -TERM $$").Run()
	for _, tc := range []struct {
		name   string
		err    error
		cancel bool
	}{
		{"helper refused operation", failed, false},
		{"parent budget ended", terminated, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			err := retryInterruptedCleanup(ctx, func(context.Context) error {
				calls++
				if tc.cancel {
					cancel()
				}
				return tc.err
			})
			if calls != 1 || !errors.Is(err, tc.err) {
				t.Fatalf("ordinary/canceled failure was retried: calls=%d, err=%v", calls, err)
			}
		})
	}
}
