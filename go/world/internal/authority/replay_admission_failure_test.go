package authority

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
)

func TestReplayAdmissionUncertainDirectorySyncPoisonsUntilReopen(t *testing.T) {
	for _, denyNonce := range []bool{false, true} {
		t.Run(map[bool]string{false: "admission", true: "denial"}[denyNonce], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger")
			c := openAdmissionCache(t, path, 8)
			metrics := runtimeobs.New()
			c.SetMetrics(metrics)
			injected := errors.New("test-only directory sync failure")
			calls := 0
			c.syncDirectory = func(directory string) error {
				calls++
				if directory != filepath.Dir(c.path) {
					t.Fatal("wrong directory sync target")
				}
				return injected
			}
			key := "new"
			if denyNonce {
				key = ""
			}
			err := c.CheckpointAndConsume(2, "two", key, admissionTestNow.Add(time.Minute), admissionTestNow)
			if !errors.Is(err, injected) || !c.poisoned || calls != 1 {
				t.Fatalf("uncertain commit did not poison exactly once: %v", err)
			}
			if c.state.Revision != 0 || len(c.state.Nonces) != 0 {
				t.Fatal("uncertain transaction became in-memory authority")
			}
			for _, attempt := range []func() error{
				func() error {
					return c.CheckpointAndConsume(2, "two", "another", admissionTestNow.Add(time.Minute), admissionTestNow)
				},
				func() error { return c.Checkpoint(2, "two", admissionTestNow) },
				func() error { return c.ValidateCheckpoint(2, "two", admissionTestNow) },
				func() error { return c.Consume("another", admissionTestNow.Add(time.Minute), admissionTestNow) },
			} {
				if err := attempt(); err == nil {
					t.Fatal("poisoned cache remained usable")
				}
			}
			if calls != 1 || metricDuration(t, metrics, "replay.checkpoint_commit").Count != 1 || metricCounter(t, metrics, "replay.checkpoint_failures") != 1 {
				t.Fatal("poisoned cache performed another write")
			}
			c.Close()
			c = openAdmissionCache(t, path, 8)
			// Rename completed in this injected failure. This is not a power-loss
			// simulation; reopening must honor whichever complete ledger exists.
			if c.state.Revision != 2 || c.state.LastUnixMS != admissionTestNow.UnixMilli() {
				t.Fatal("renamed checkpoint missing on reopen")
			}
			if denyNonce {
				if len(c.state.Nonces) != 0 {
					t.Fatal("nonce denial recorded a nonce")
				}
			} else if err := c.CheckpointAndConsume(2, "two", key, admissionTestNow.Add(time.Minute), admissionTestNow); !errors.Is(err, ErrReplay) {
				t.Fatalf("renamed nonce not enforced: %v", err)
			}
			if err := c.ValidateCheckpoint(2, "two", admissionTestNow.Add(-time.Millisecond)); err == nil {
				t.Fatal("renamed clock floor not enforced")
			}
		})
	}
}
