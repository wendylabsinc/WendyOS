package runtimeobs

import (
	"encoding/json"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestFixedBucketsIncludeBoundariesAndOverflow(t *testing.T) {
	c := New()
	for _, bound := range upperBounds {
		c.Observe(HTTPChallenge, bound)
	}
	c.Observe(HTTPChallenge, time.Minute+1)
	s := c.Snapshot()
	h := s.Durations[0]
	if h.Count != uint64(bucketCount) || h.MaxNS != uint64(time.Minute+1) || h.Saturated {
		t.Fatal(h)
	}
	var sum uint64
	for i, bound := range upperBounds {
		sum += uint64(bound)
		if s.BucketUpperBoundsNS[i] == nil || *s.BucketUpperBoundsNS[i] != uint64(bound) {
			t.Fatal("changed bound", i)
		}
	}
	sum += uint64(time.Minute + 1)
	if h.SumNS != sum || s.BucketUpperBoundsNS[len(upperBounds)] != nil {
		t.Fatal("bad sum/overflow bound", s)
	}
	for i, n := range h.Buckets {
		if n != 1 {
			t.Fatal("noncumulative bucket mismatch", i, n)
		}
	}
	c.Observe(HTTPChallenge, 0)
	c.Observe(HTTPChallenge, upperBounds[0]+1)
	h = c.Snapshot().Durations[0]
	if h.Buckets[0] != 2 || h.Buckets[1] != 2 {
		t.Fatal(h.Buckets)
	}
}

func TestDisabledUnknownAndNegativeInputs(t *testing.T) {
	var disabled *Collector
	disabled.Observe(HTTPAck, time.Second)
	disabled.Increment(HTTPFailures)
	disabled.Add(HTTPFailures, math.MaxUint64)
	s := disabled.Snapshot()
	if s.Enabled || len(s.Durations) != int(durationMetricCount)-1 || len(s.Counters) != int(counterCount)-1 {
		t.Fatal(s)
	}
	c := New()
	before := c.Snapshot()
	for _, metric := range []DurationMetric{0, durationMetricCount, 255} {
		c.Observe(metric, time.Second)
	}
	c.Observe(HTTPChallenge, -1)
	for _, counter := range []Counter{0, counterCount, 255} {
		c.Increment(counter)
	}
	if after := c.Snapshot(); !reflect.DeepEqual(before, after) {
		t.Fatal("invalid inputs changed measurements", after)
	}
}

func TestAllMetricNamesAreFixedAndIndependent(t *testing.T) {
	c := New()
	for i := DurationMetric(1); i < durationMetricCount; i++ {
		c.Observe(i, time.Duration(i))
	}
	for i := Counter(1); i < counterCount; i++ {
		c.Add(i, uint64(i))
	}
	snapshot := c.Snapshot()
	names := map[string]bool{}
	for i, d := range snapshot.Durations {
		if d.Name != durationNames[i+1] || names[d.Name] || d.Count != 1 || d.SumNS != uint64(i+1) {
			t.Fatal(d)
		}
		names[d.Name] = true
	}
	for i, d := range snapshot.Counters {
		if d.Name != counterNames[i+1] || names[d.Name] || d.Value != uint64(i+1) {
			t.Fatal(d)
		}
		names[d.Name] = true
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Snapshot
	if err = json.Unmarshal(raw, &decoded); err != nil || !reflect.DeepEqual(snapshot, decoded) {
		t.Fatal("snapshot JSON did not roundtrip", err)
	}
	if snapshot.Schema != "wendy.world.runtimeobs.v2" || snapshot.BucketSemantics != "noncumulative" {
		t.Fatal(snapshot)
	}
}

func TestSnapshotOwnsAllMutableMemory(t *testing.T) {
	c := New()
	c.Observe(HTTPAck, time.Millisecond)
	c.Increment(HTTPFailures)
	expected := c.Snapshot()
	copy := c.Snapshot()
	*copy.BucketUpperBoundsNS[0] = 0
	copy.Durations[1].Buckets[0] = 999
	copy.Durations[1].Name = "caller-mutation"
	copy.Counters[0].Value = 100
	if got := c.Snapshot(); !reflect.DeepEqual(got, expected) {
		t.Fatal("caller changed collector", got)
	}
	c.Observe(HTTPAck, 2*time.Millisecond)
	if expected.Durations[1].Count != 1 {
		t.Fatal("future observation changed previous snapshot")
	}
}

func TestSaturatesCountersAndDurationSum(t *testing.T) {
	c := New()
	c.Add(HTTPFailures, math.MaxUint64)
	c.Add(HTTPFailures, 0)
	if s := c.Snapshot().Counters[0]; s.Value != math.MaxUint64 || s.Saturated {
		t.Fatal(s)
	}
	c.Increment(HTTPFailures)
	if s := c.Snapshot().Counters[0]; s.Value != math.MaxUint64 || !s.Saturated {
		t.Fatal(s)
	}
	c.Observe(HTTPChallenge, time.Duration(math.MaxInt64))
	c.Observe(HTTPChallenge, time.Duration(math.MaxInt64))
	c.Observe(HTTPChallenge, 1)
	if s := c.Snapshot().Durations[0]; s.SumNS != math.MaxUint64 || s.Saturated {
		t.Fatal(s)
	}
	c.Observe(HTTPChallenge, 1)
	s := c.Snapshot().Durations[0]
	if s.SumNS != math.MaxUint64 || !s.Saturated || s.Count != 4 || s.MaxNS != math.MaxInt64 {
		t.Fatal(s)
	}
	if _, ok := s.PercentileUpperBound(50); ok {
		t.Fatal("saturated quantile claimed")
	}
	// Reach count/bucket overflow without executing an infeasible number of calls.
	c.durations[HTTPAck].count = math.MaxUint64
	c.durations[HTTPAck].buckets[0] = math.MaxUint64
	c.Observe(HTTPAck, 1)
	s = c.Snapshot().Durations[1]
	if s.Count != math.MaxUint64 || s.Buckets[0] != math.MaxUint64 || s.SumNS != 1 || !s.Saturated {
		t.Fatal(s)
	}
}

func TestPercentilesAreUpperBoundsNotExactQuantiles(t *testing.T) {
	c := New()
	for _, d := range []time.Duration{time.Millisecond, 2 * time.Millisecond, 5 * time.Millisecond, 20 * time.Millisecond, 99 * time.Millisecond} {
		c.Observe(HTTPEvaluate, d)
	}
	h := c.Snapshot().Durations[2]
	for p, want := range map[uint8]time.Duration{1: time.Millisecond, 50: 5 * time.Millisecond, 95: 100 * time.Millisecond, 100: 100 * time.Millisecond} {
		if upper, ok := h.PercentileUpperBound(p); !ok || upper != want {
			t.Fatal(p, upper, ok)
		}
	}
	for _, p := range []uint8{0, 101, 255} {
		if _, ok := h.PercentileUpperBound(p); ok {
			t.Fatal("invalid percentile", p)
		}
	}
	if _, ok := c.Snapshot().Durations[0].PercentileUpperBound(50); ok {
		t.Fatal("empty quantile")
	}
	c.Observe(HTTPEvaluate, time.Minute+1)
	if _, ok := c.Snapshot().Durations[2].PercentileUpperBound(100); ok {
		t.Fatal("overflow bucket has no finite upper bound")
	}
	h.Count++
	if _, ok := h.PercentileUpperBound(50); ok {
		t.Fatal("inconsistent snapshot quantile")
	}
	// Nearest-rank arithmetic must not overflow at uint64-sized counts.
	huge := DurationSnapshot{Count: math.MaxUint64, Buckets: make([]uint64, bucketCount)}
	huge.Buckets[0] = math.MaxUint64
	if upper, ok := huge.PercentileUpperBound(99); !ok || upper != time.Microsecond {
		t.Fatal(upper, ok)
	}
}

func TestConcurrentSnapshotsRemainConsistent(t *testing.T) {
	c := New()
	const workers = 8
	const each = 2000
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < each; j++ {
				c.Observe(AuthorityLockWait, 2*time.Millisecond)
				c.Increment(HTTPFailures)
			}
		}()
	}
	for i := 0; i < 200; i++ {
		s := c.Snapshot().Durations[int(AuthorityLockWait)-1]
		var count uint64
		for _, n := range s.Buckets {
			count += n
		}
		if count != s.Count || s.SumNS != s.Count*uint64(2*time.Millisecond) {
			t.Fatal("inconsistent snapshot", s)
		}
	}
	wg.Wait()
	s := c.Snapshot()
	if s.Durations[int(AuthorityLockWait)-1].Count != workers*each || s.Counters[0].Value != workers*each {
		t.Fatal("lost concurrent measurements", s)
	}
}
func TestObservationsDoNotAllocate(t *testing.T) {
	c := New()
	allocations := testing.AllocsPerRun(1000, func() { c.Observe(ReplayCheckpointCommit, time.Millisecond); c.Increment(ReplayCheckpointFailures) })
	if allocations != 0 {
		t.Fatalf("observation allocates: %g", allocations)
	}
}

func TestVersionTwoStorageRepairAndStopMetrics(t *testing.T) {
	durations := map[DurationMetric]string{WorldStoreCommit: "world_store.commit", ProofStoreCommit: "proof_store.commit", NodeLockWait: "node.lock_wait", HTTPStop: "http.stop"}
	counters := map[Counter]string{WorldStoreCommitFailures: "world_store.commit_failures", WorldStoreSerializedBytes: "world_store.serialized_bytes", ProofStoreCommitFailures: "proof_store.commit_failures", ProofStoreSerializedBytes: "proof_store.serialized_bytes", RepairRounds: "repair.rounds", RepairRoundFailures: "repair.round_failures", RepairRecordsFetched: "repair.records_fetched", RepairApplied: "repair.applied", RepairDuplicate: "repair.duplicate", RepairObsolete: "repair.obsolete", RepairRejected: "repair.rejected"}
	c := New()
	var disabled *Collector
	for metric := range durations {
		c.Observe(metric, 2*time.Millisecond)
		disabled.Observe(metric, time.Second)
	}
	for metric := range counters {
		c.Add(metric, math.MaxUint64)
		c.Increment(metric)
		disabled.Add(metric, math.MaxUint64)
	}
	snapshot := c.Snapshot()
	if snapshot.Schema != "wendy.world.runtimeobs.v2" || len(snapshot.Durations) != 12 || len(snapshot.Counters) != 16 {
		t.Fatal("v2 shape changed")
	}
	for metric, name := range durations {
		row := snapshot.Durations[int(metric)-1]
		if row.Name != name || row.Count != 1 || row.SumNS != uint64(2*time.Millisecond) {
			t.Fatal(row)
		}
	}
	for metric, name := range counters {
		row := snapshot.Counters[int(metric)-1]
		if row.Name != name || row.Value != math.MaxUint64 || !row.Saturated {
			t.Fatal(row)
		}
	}
	for _, row := range disabled.Snapshot().Counters {
		if row.Value != 0 || row.Saturated {
			t.Fatal("disabled collector retained counter")
		}
	}
}
