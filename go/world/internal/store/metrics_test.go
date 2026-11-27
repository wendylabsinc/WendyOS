package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

func metricCounts(t *testing.T, metrics *runtimeobs.Collector) (commits, failures, serialized uint64) {
	t.Helper()
	snapshot := metrics.Snapshot()
	for _, row := range snapshot.Durations {
		if row.Name == "world_store.commit" {
			commits = row.Count
		}
	}
	for _, row := range snapshot.Counters {
		switch row.Name {
		case "world_store.commit_failures":
			failures = row.Value
		case "world_store.serialized_bytes":
			serialized = row.Value
		}
	}
	return
}

func TestMetricsCountActualReadAndMutationWritesAndExactEnvelopeBytes(t *testing.T) {
	path, cfg, _, c := fixture(t)
	s := open(t, path, cfg)
	metrics := runtimeobs.New()
	s.SetMetrics(metrics)
	var writes, serialized uint64
	s.write = func(path string, data []byte) error {
		writes++
		serialized += uint64(len(data))
		var e envelope
		if err := decode(data, &e); err != nil || e.SHA256 == "" {
			t.Fatal("counted bytes are not the final serialized envelope", err)
		}
		return writeAtomic(path, data)
	}
	apply(t, s, c)
	if got := apply(t, s, c); got != world.Duplicate {
		t.Fatal("duplicate semantics changed", got)
	}
	if _, err := s.NextEpoch(c.Key.Source); err != nil {
		t.Fatal(err)
	}
	if err := s.SetFrame(c.Frame); err != nil {
		t.Fatal(err)
	}
	// These APIs currently persist the observed clock even when named as reads.
	if _, err := s.CurrentEpoch(c.Key.Source); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Records(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Snapshot(); err != nil {
		t.Fatal(err)
	}
	if _, found, err := s.Get(c.Key); err != nil || !found {
		t.Fatal("read failed", found, err)
	}
	if matches, err := s.MatchesFrame(c.Frame); err != nil || !matches {
		t.Fatal("frame lookup failed", err)
	}
	commits, failures, measuredBytes := metricCounts(t, metrics)
	if commits != 8 || writes != commits || failures != 0 || measuredBytes != serialized || serialized == 0 {
		t.Fatalf("wrong write accounting: commits=%d writes=%d failures=%d bytes=%d expected=%d", commits, writes, failures, measuredBytes, serialized)
	}
	raw, _ := json.Marshal(metrics.Snapshot())
	for _, private := range []string{path, c.Key.Source.Device, c.Key.Entity, string(c.Payload)} {
		if bytes.Contains(raw, []byte(private)) {
			t.Fatal("private state appeared in metrics")
		}
	}
	s.Close()
	s = open(t, path, cfg)
	if got := apply(t, s, c); got != world.Duplicate {
		t.Fatal("instrumentation changed persisted high water", got)
	}
}

func TestMetricsFailedWriteIsCountedOnceAndDoesNotAdmitMutation(t *testing.T) {
	path, cfg, _, c := fixture(t)
	s := open(t, path, cfg)
	apply(t, s, c)
	metrics := runtimeobs.New()
	s.SetMetrics(metrics)
	injected := errors.New("test-only writer failure")
	var bytesHandedToWriter uint64
	s.write = func(_ string, data []byte) error {
		bytesHandedToWriter = uint64(len(data))
		return injected
	}
	c.Version.Sequence++
	if _, err := s.Apply(c.Key.Source, c); !errors.Is(err, injected) {
		t.Fatal("failed write accepted", err)
	}
	commits, failures, measuredBytes := metricCounts(t, metrics)
	if commits != 1 || failures != 1 || measuredBytes != bytesHandedToWriter || measuredBytes == 0 {
		t.Fatal("failed attempt accounting mismatch", commits, failures, measuredBytes)
	}
	if _, err := s.Records(); !errors.Is(err, ErrFaulted) {
		t.Fatal("failed store continued serving", err)
	}
	if after, _, _ := metricCounts(t, metrics); after != 1 {
		t.Fatal("fault refusal counted as a disk attempt")
	}
	s.Close()
	s = open(t, path, cfg)
	got, found, err := s.Get(c.Key)
	if err != nil || !found || got.Version.Sequence != 1 {
		t.Fatal("failed mutation survived reopen", got.Version, err)
	}
}

func TestMetricsDisabledAndPreflightRefusalsDoNotCountWrites(t *testing.T) {
	path, cfg, now, c := fixture(t)
	s := open(t, path, cfg)
	metrics := runtimeobs.New()
	s.SetMetrics(metrics)
	invalid := c
	invalid.TTL = -1
	if _, err := s.Apply(c.Key.Source, invalid); err == nil {
		t.Fatal("invalid component accepted")
	}
	*now = now.Add(-time.Second)
	if _, err := s.Snapshot(); err == nil {
		t.Fatal("clock rollback accepted")
	}
	*now = now.Add(time.Second)
	if commits, failures, serialized := metricCounts(t, metrics); commits != 0 || failures != 0 || serialized != 0 {
		t.Fatal("preflight refusal counted a disk attempt")
	}
	s.SetMetrics(nil)
	apply(t, s, c)
	if _, err := s.Records(); err != nil {
		t.Fatal(err)
	}
	if commits, failures, serialized := metricCounts(t, metrics); commits != 0 || failures != 0 || serialized != 0 {
		t.Fatal("disabled collection changed retained metrics")
	}
	s.SetMetrics(metrics)
	if _, err := s.Snapshot(); err != nil {
		t.Fatal(err)
	}
	if commits, failures, serialized := metricCounts(t, metrics); commits != 1 || failures != 0 || serialized == 0 {
		t.Fatal("reenabling metrics failed")
	}
}

func TestMetricsCanBeReconfiguredConcurrentlyWithReads(t *testing.T) {
	path, cfg, _, c := fixture(t)
	s := open(t, path, cfg)
	apply(t, s, c)
	metrics := runtimeobs.New()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			s.SetMetrics(metrics)
			s.SetMetrics(nil)
		}
	}()
	for i := 0; i < 20; i++ {
		if _, found, err := s.Get(c.Key); err != nil || !found {
			t.Fatal("concurrent metrics change affected read", err)
		}
	}
	wg.Wait()
}
