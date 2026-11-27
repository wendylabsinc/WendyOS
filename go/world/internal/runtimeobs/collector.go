// Package runtimeobs records bounded, process-local diagnostic measurements.
// It accepts only static metric enums and never accepts identities or labels.
package runtimeobs

import (
	"math"
	"sync"
	"time"
)

type DurationMetric uint8

const (
	HTTPChallenge DurationMetric = iota + 1
	HTTPAck
	HTTPEvaluate
	HTTPOther
	SupervisorExchange
	AuthorityLockWait
	ReplayCheckpointCommit
	TaskStoreCommit
	WorldStoreCommit
	ProofStoreCommit
	NodeLockWait
	HTTPStop
	durationMetricCount
)

type Counter uint8

const (
	HTTPFailures Counter = iota + 1
	SupervisorExchangeFailures
	ReplayCheckpointFailures
	ReplayCheckpointNoops
	TaskStoreCommitFailures
	WorldStoreCommitFailures
	WorldStoreSerializedBytes
	ProofStoreCommitFailures
	ProofStoreSerializedBytes
	RepairRounds
	RepairRoundFailures
	RepairRecordsFetched
	RepairApplied
	RepairDuplicate
	RepairObsolete
	RepairRejected
	counterCount
)

var durationNames = [...]string{"", "http.challenge", "http.ack", "http.evaluate", "http.other", "supervisor.exchange", "authority.lock_wait", "replay.checkpoint_commit", "task_store.commit", "world_store.commit", "proof_store.commit", "node.lock_wait", "http.stop"}
var counterNames = [...]string{"", "http.failures", "supervisor.exchange_failures", "replay.checkpoint_failures", "replay.checkpoint_noops", "task_store.commit_failures", "world_store.commit_failures", "world_store.serialized_bytes", "proof_store.commit_failures", "proof_store.serialized_bytes", "repair.rounds", "repair.round_failures", "repair.records_fetched", "repair.applied", "repair.duplicate", "repair.obsolete", "repair.rejected"}

// Bounds are inclusive. The extra final bucket contains all durations >60s.
var upperBounds = [...]time.Duration{
	time.Microsecond, 10 * time.Microsecond, 100 * time.Microsecond, 500 * time.Microsecond,
	time.Millisecond, 2 * time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond,
	25 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond, 200 * time.Millisecond,
	250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second,
	5 * time.Second, 10 * time.Second, 30 * time.Second, time.Minute,
}

const bucketCount = len(upperBounds) + 1

type durationState struct {
	count, sum, max uint64
	buckets         [bucketCount]uint64
	saturated       bool
}
type counterState struct {
	value     uint64
	saturated bool
}

// Collector has a usable zero value, fixed memory, and no background work.
// A nil *Collector disables observation. Never copy a Collector after use.
type Collector struct {
	mu        sync.Mutex
	durations [durationMetricCount]durationState
	counters  [counterCount]counterState
}

func New() *Collector { return &Collector{} }

func addSaturated(dst *uint64, n uint64) bool {
	if n > math.MaxUint64-*dst {
		*dst = math.MaxUint64
		return true
	}
	*dst += n
	return false
}

// Observe includes zero durations, ignores negative durations and unknown enums,
// and retains no individual samples. All amounts in snapshots are nanoseconds.
func (c *Collector) Observe(metric DurationMetric, d time.Duration) {
	if c == nil || metric == 0 || metric >= durationMetricCount || d < 0 {
		return
	}
	bucket := len(upperBounds)
	for i, upper := range upperBounds {
		if d <= upper {
			bucket = i
			break
		}
	}
	n := uint64(d)
	c.mu.Lock()
	defer c.mu.Unlock()
	state := &c.durations[metric]
	// Do not short-circuit additions when an earlier field has saturated.
	countOverflow := addSaturated(&state.count, 1)
	sumOverflow := addSaturated(&state.sum, n)
	bucketOverflow := addSaturated(&state.buckets[bucket], 1)
	state.saturated = state.saturated || countOverflow || sumOverflow || bucketOverflow
	if n > state.max {
		state.max = n
	}
}
func (c *Collector) Increment(counter Counter) { c.Add(counter, 1) }
func (c *Collector) Add(counter Counter, n uint64) {
	if c == nil || counter == 0 || counter >= counterCount {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	state := &c.counters[counter]
	overflow := addSaturated(&state.value, n)
	state.saturated = state.saturated || overflow
}

type DurationSnapshot struct {
	Name      string   `json:"name"`
	Count     uint64   `json:"count"`
	SumNS     uint64   `json:"sum_ns"`
	MaxNS     uint64   `json:"max_ns"`
	Buckets   []uint64 `json:"buckets"`
	Saturated bool     `json:"saturated"`
}
type CounterSnapshot struct {
	Name      string `json:"name"`
	Value     uint64 `json:"value"`
	Saturated bool   `json:"saturated"`
}
type Snapshot struct {
	Schema              string             `json:"schema"`
	Enabled             bool               `json:"enabled"`
	BucketSemantics     string             `json:"bucket_semantics"`
	BucketUpperBoundsNS []*uint64          `json:"bucket_upper_bounds_ns"`
	Durations           []DurationSnapshot `json:"durations"`
	Counters            []CounterSnapshot  `json:"counters"`
}

// Snapshot copies a consistent set of values under a short lock. Returned
// slices and pointers belong to the caller; mutating them cannot change later
// observations or snapshots. The final null upper bound means no finite bound.
func (c *Collector) Snapshot() Snapshot {
	var durations [durationMetricCount]durationState
	var counters [counterCount]counterState
	if c != nil {
		c.mu.Lock()
		durations = c.durations
		counters = c.counters
		c.mu.Unlock()
	}
	out := Snapshot{Schema: "wendy.world.runtimeobs.v2", Enabled: c != nil, BucketSemantics: "noncumulative", BucketUpperBoundsNS: make([]*uint64, bucketCount), Durations: make([]DurationSnapshot, 0, int(durationMetricCount)-1), Counters: make([]CounterSnapshot, 0, int(counterCount)-1)}
	for i, bound := range upperBounds {
		n := uint64(bound)
		out.BucketUpperBoundsNS[i] = &n
	}
	for metric := DurationMetric(1); metric < durationMetricCount; metric++ {
		state := durations[metric]
		out.Durations = append(out.Durations, DurationSnapshot{Name: durationNames[metric], Count: state.count, SumNS: state.sum, MaxNS: state.max, Buckets: append([]uint64(nil), state.buckets[:]...), Saturated: state.saturated})
	}
	for metric := Counter(1); metric < counterCount; metric++ {
		state := counters[metric]
		out.Counters = append(out.Counters, CounterSnapshot{Name: counterNames[metric], Value: state.value, Saturated: state.saturated})
	}
	return out
}

// PercentileUpperBound returns the inclusive bucket upper bound containing the
// nearest-rank percentile, NOT an exact quantile or an interpolated estimate.
// It returns false for empty/saturated/malformed snapshots, invalid percentiles,
// or a percentile in the >60s bucket, which has no finite upper bound.
func (s DurationSnapshot) PercentileUpperBound(percent uint8) (time.Duration, bool) {
	if percent < 1 || percent > 100 || s.Count == 0 || s.Saturated || len(s.Buckets) != bucketCount {
		return 0, false
	}
	var total uint64
	for _, n := range s.Buckets {
		if addSaturated(&total, n) {
			return 0, false
		}
	}
	if total != s.Count {
		return 0, false
	}
	rank := (s.Count/100)*uint64(percent) + (s.Count%100*uint64(percent)+99)/100
	var cumulative uint64
	for i, n := range s.Buckets {
		cumulative += n
		if cumulative >= rank {
			if i == len(upperBounds) {
				return 0, false
			}
			return upperBounds[i], true
		}
	}
	return 0, false
}
