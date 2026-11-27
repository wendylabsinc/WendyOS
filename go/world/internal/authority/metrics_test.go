package authority

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
)

func metricDuration(t *testing.T, c *runtimeobs.Collector, name string) runtimeobs.DurationSnapshot {
	t.Helper()
	for _, v := range c.Snapshot().Durations {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("missing metric %s", name)
	return runtimeobs.DurationSnapshot{}
}
func metricCounter(t *testing.T, c *runtimeobs.Collector, name string) uint64 {
	t.Helper()
	for _, v := range c.Snapshot().Counters {
		if v.Name == name {
			return v.Value
		}
	}
	t.Fatalf("missing counter %s", name)
	return 0
}
func TestReplayMetricsCountWritesAndExplicitNoops(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-replay.json")
	cache, err := OpenReplay(path, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	metrics := runtimeobs.New()
	cache.SetMetrics(metrics)
	now := time.Unix(1700000000, 0)
	if err = cache.Checkpoint(1, "test-digest", now); err != nil {
		t.Fatal(err)
	}
	if err = cache.ValidateCheckpoint(1, "test-digest", now); err != nil {
		t.Fatal(err)
	}
	if err = cache.Checkpoint(1, "test-digest", now); err != nil {
		t.Fatal(err)
	}
	if got := metricDuration(t, metrics, "replay.checkpoint_commit").Count; got != 1 {
		t.Fatal(got)
	}
	if got := metricCounter(t, metrics, "replay.checkpoint_noops"); got != 1 {
		t.Fatal(got)
	}
	// The current baseline still writes when only its durable clock advances.
	now = now.Add(time.Millisecond)
	if err = cache.Checkpoint(1, "test-digest", now); err != nil {
		t.Fatal(err)
	}
	nonce := "private-nonce-not-a-metric"
	if err = cache.Consume(nonce, now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if err = cache.Consume(nonce, now.Add(time.Minute), now); !errors.Is(err, ErrReplay) {
		t.Fatal(err)
	}
	if got := metricDuration(t, metrics, "replay.checkpoint_commit"); got.Count != 3 || got.MaxNS == 0 {
		t.Fatal(got)
	}
	raw, _ := json.Marshal(metrics.Snapshot())
	if strings.Contains(string(raw), nonce) || strings.Contains(string(raw), path) || strings.Contains(string(raw), "test-digest") {
		t.Fatal("private state entered metrics")
	}
	cache.Close()
	cache, err = OpenReplay(path, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	if err = cache.Consume(nonce, now.Add(time.Minute), now); !errors.Is(err, ErrReplay) {
		t.Fatal("instrumentation changed durable replay", err)
	}
	memory := NewMemoryReplay(16)
	memory.SetMetrics(metrics)
	if err = memory.Consume("memory", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if got := metricDuration(t, metrics, "replay.checkpoint_commit").Count; got != 3 {
		t.Fatal("memory operation counted as disk commit", got)
	}
}
func TestReplayMetricsReportFailedAttemptWithoutAdmission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "blocked-ledger")
	cache, err := OpenReplay(path, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	metrics := runtimeobs.New()
	cache.SetMetrics(metrics)
	if err = os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0)
	if err = cache.Consume("not-admitted", now.Add(time.Minute), now); err == nil {
		t.Fatal("failed filesystem commit accepted")
	}
	if len(cache.state.Nonces) != 0 {
		t.Fatal("failed commit changed memory")
	}
	if metricDuration(t, metrics, "replay.checkpoint_commit").Count != 1 || metricCounter(t, metrics, "replay.checkpoint_failures") != 1 {
		t.Fatal(metrics.Snapshot())
	}
	cache.SetMetrics(nil)
	if err = cache.Consume("still-denied", now.Add(time.Minute), now); err == nil {
		t.Fatal("failed filesystem commit accepted")
	}
	if metricDuration(t, metrics, "replay.checkpoint_commit").Count != 1 {
		t.Fatal("disabled metrics changed")
	}
}
func TestVerifierMetricsObserveActualMutexAcquisition(t *testing.T) {
	fixture := newFixture(t, nil)
	metrics := runtimeobs.New()
	fixture.verifier.cfg.Metrics = metrics
	fixture.verifier.mu.Lock()
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() { close(entered); _, err := fixture.verifier.VerifyGrant(fixture.grant, "read"); done <- err }()
	<-entered
	select {
	case err := <-done:
		fixture.verifier.mu.Unlock()
		t.Fatal("verification bypassed held mutex", err)
	case <-time.After(10 * time.Millisecond):
	}
	fixture.verifier.mu.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("verification blocked")
	}
	if got := metricDuration(t, metrics, "authority.lock_wait"); got.Count != 1 || got.MaxNS == 0 {
		t.Fatal(got)
	}
	fixture.verifier.cfg.Metrics = nil
	if _, err := fixture.verifier.VerifyGrant(fixture.grant, "read"); err != nil {
		t.Fatal(err)
	}
	if got := metricDuration(t, metrics, "authority.lock_wait").Count; got != 1 {
		t.Fatal(got)
	}
}
