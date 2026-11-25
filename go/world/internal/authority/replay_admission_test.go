package authority

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
)

var admissionTestNow = time.Unix(1700000000, 0)

func openAdmissionCache(t *testing.T, path string, max int) *ReplayCache {
	t.Helper()
	c, err := OpenReplay(path, max)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestReplayAdmissionCommitsOnceAndSurvivesProcessExit(t *testing.T) {
	// A subprocess exits immediately after admission without Close: no shutdown
	// hook may be necessary to retain either the nonce or its authority floor.
	if path := os.Getenv("WENDY_REPLAY_ADMISSION_TEST_PATH"); path != "" {
		c, err := OpenReplay(path, 8)
		if err != nil {
			t.Fatal(err)
		}
		metrics := runtimeobs.New()
		c.SetMetrics(metrics)
		if err = c.CheckpointAndConsume(2, "revision-two", "admitted", admissionTestNow.Add(time.Minute), admissionTestNow); err != nil {
			t.Fatal(err)
		}
		if metricDuration(t, metrics, "replay.checkpoint_commit").Count != 1 {
			t.Fatal("admission did not use one durable write")
		}
		os.Exit(0)
	}
	path := filepath.Join(t.TempDir(), "ledger")
	cmd := exec.Command(os.Args[0], "-test.run=^TestReplayAdmissionCommitsOnceAndSurvivesProcessExit$")
	cmd.Env = append(os.Environ(), "WENDY_REPLAY_ADMISSION_TEST_PATH="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("child admission: %v: %s", err, output)
	}
	c := openAdmissionCache(t, path, 8)
	if err := c.CheckpointAndConsume(2, "revision-two", "admitted", admissionTestNow.Add(time.Minute), admissionTestNow); !errors.Is(err, ErrReplay) {
		t.Fatalf("nonce lost on abrupt exit: %v", err)
	}
	if err := c.ValidateCheckpoint(1, "old", admissionTestNow); err == nil {
		t.Fatal("revision lost on abrupt exit")
	}
	if err := c.ValidateCheckpoint(2, "revision-two", admissionTestNow.Add(-time.Millisecond)); err == nil {
		t.Fatal("clock floor lost on abrupt exit")
	}
}

func TestReplayAdmissionDenialsPersistCheckpointBeforeReturning(t *testing.T) {
	for _, kind := range []string{"duplicate", "quota", "empty", "long", "expired"} {
		t.Run(kind, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger")
			c := openAdmissionCache(t, path, 1)
			metrics := runtimeobs.New()
			c.SetMetrics(metrics)
			if err := c.CheckpointAndConsume(1, "one", "existing", admissionTestNow.Add(time.Minute), admissionTestNow); err != nil {
				t.Fatal(err)
			}
			key, expiry := "new", admissionTestNow.Add(time.Minute)
			var expected error
			switch kind {
			case "duplicate":
				key, expected = "existing", ErrReplay
			case "empty":
				key, expected = "", ErrDenied
			case "long":
				key, expected = strings.Repeat("n", 1025), ErrDenied
			case "expired":
				expiry, expected = admissionTestNow, ErrDenied
			}
			now := admissionTestNow.Add(time.Second)
			err := c.CheckpointAndConsume(2, "two", key, expiry, now)
			if err == nil || (expected != nil && !errors.Is(err, expected)) {
				t.Fatalf("wrong denial: %v", err)
			}
			if metricDuration(t, metrics, "replay.checkpoint_commit").Count != 2 {
				t.Fatal("denial did not durably advance checkpoint")
			}
			// Repeating the denial at the same floor needs no disk write.
			if err := c.CheckpointAndConsume(2, "two", key, expiry, now); err == nil {
				t.Fatal("repeat denial accepted")
			}
			if metricDuration(t, metrics, "replay.checkpoint_commit").Count != 2 || metricCounter(t, metrics, "replay.checkpoint_noops") != 1 {
				t.Fatal("unchanged denial checkpoint wrote again")
			}
			c.Close()
			c = openAdmissionCache(t, path, 1)
			if c.state.Revision != 2 || c.state.Digest != "two" || c.state.LastUnixMS != now.UnixMilli() || len(c.state.Nonces) != 1 {
				t.Fatal("denial checkpoint or nonce state lost")
			}
			if err := c.ValidateCheckpoint(2, "two", admissionTestNow); err == nil {
				t.Fatal("denial allowed clock rollback")
			}
		})
	}
}

func TestReplayAdmissionRejectsInvalidCheckpointWithoutWriting(t *testing.T) {
	c := openAdmissionCache(t, filepath.Join(t.TempDir(), "ledger"), 8)
	metrics := runtimeobs.New()
	c.SetMetrics(metrics)
	if err := c.CheckpointAndConsume(2, "two", "first", admissionTestNow.Add(time.Minute), admissionTestNow); err != nil {
		t.Fatal(err)
	}
	for _, input := range []struct {
		revision uint64
		digest   string
		now      time.Time
	}{{0, "", admissionTestNow}, {1, "one", admissionTestNow}, {2, "conflict", admissionTestNow}, {3, "", admissionTestNow}, {3, "three", admissionTestNow.Add(-time.Millisecond)}} {
		if err := c.CheckpointAndConsume(input.revision, input.digest, "new", admissionTestNow.Add(time.Minute), input.now); err == nil {
			t.Fatal("invalid checkpoint accepted")
		}
	}
	if metricDuration(t, metrics, "replay.checkpoint_commit").Count != 1 || len(c.state.Nonces) != 1 {
		t.Fatal("checkpoint refusal changed durable state")
	}
}

func TestReplayAdmissionFailedWriteNeverAdmitsAndDenialFailureTakesPrecedence(t *testing.T) {
	for _, denied := range []bool{false, true} {
		t.Run(map[bool]string{false: "admission", true: "denial"}[denied], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ledger")
			c := openAdmissionCache(t, path, 8)
			metrics := runtimeobs.New()
			c.SetMetrics(metrics)
			if err := os.Mkdir(path, 0700); err != nil {
				t.Fatal(err)
			}
			key := "new"
			if denied {
				key = ""
			}
			err := c.CheckpointAndConsume(1, "one", key, admissionTestNow.Add(time.Minute), admissionTestNow)
			if err == nil || errors.Is(err, ErrDenied) || errors.Is(err, ErrReplay) {
				t.Fatalf("filesystem failure did not take precedence: %v", err)
			}
			if c.state.Revision != 0 || c.state.LastUnixMS != 0 || len(c.state.Nonces) != 0 || c.poisoned {
				t.Fatal("pre-rename failure changed state")
			}
			if metricDuration(t, metrics, "replay.checkpoint_commit").Count != 1 || metricCounter(t, metrics, "replay.checkpoint_failures") != 1 {
				t.Fatal("failed operation was retried or not measured")
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := c.CheckpointAndConsume(1, "one", "new", admissionTestNow.Add(time.Minute), admissionTestNow); err != nil {
				t.Fatal("pre-rename failure accidentally consumed nonce", err)
			}
		})
	}
}

func TestReplayAdmissionSerializedSizeDenialStillPersistsCheckpoint(t *testing.T) {
	c := openAdmissionCache(t, filepath.Join(t.TempDir(), "ledger"), 2)
	metrics := runtimeobs.New()
	c.SetMetrics(metrics)
	// The legacy store accepts arbitrary nonempty digest strings. Fill close to
	// its byte quota without adding thousands of fixture nonce records.
	digest := strings.Repeat("d", (16<<20)-128)
	if err := c.Checkpoint(1, digest, admissionTestNow); err != nil {
		t.Fatal(err)
	}
	now := admissionTestNow.Add(time.Second)
	err := c.CheckpointAndConsume(2, digest, strings.Repeat("n", 1024), now.Add(time.Minute), now)
	if !errors.Is(err, errReplayStateTooLarge) {
		t.Fatalf("expected serialized size refusal: %v", err)
	}
	if c.state.Revision != 2 || c.state.LastUnixMS != now.UnixMilli() || len(c.state.Nonces) != 0 {
		t.Fatal("size refusal discarded checkpoint or consumed nonce")
	}
	if metricDuration(t, metrics, "replay.checkpoint_commit").Count != 2 || metricCounter(t, metrics, "replay.checkpoint_failures") != 0 {
		t.Fatal("preflight size refusal counted as disk attempt")
	}
	path := c.path
	c.Close()
	c = openAdmissionCache(t, path, 2)
	if c.state.Revision != 2 || len(c.state.Nonces) != 0 {
		t.Fatal("size refusal checkpoint missing after restart")
	}
}

func TestReplayAdmissionConcurrentDuplicateAndExpiry(t *testing.T) {
	c := openAdmissionCache(t, filepath.Join(t.TempDir(), "ledger"), 1)
	var wg sync.WaitGroup
	results := make(chan error, 16)
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- c.CheckpointAndConsume(1, "one", "shared", admissionTestNow.Add(time.Second), admissionTestNow)
		}()
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else if !errors.Is(err, ErrReplay) {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("accepted %d duplicate requests", winners)
	}
	// Expired nonces are removed atomically with the next admission, allowing
	// bounded capacity reuse without forgetting the durable clock floor.
	now := admissionTestNow.Add(time.Second)
	if err := c.CheckpointAndConsume(1, "one", "next", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, exists := c.state.Nonces["shared"]; exists || len(c.state.Nonces) != 1 {
		t.Fatal("expired nonce retained after admission")
	}
}
