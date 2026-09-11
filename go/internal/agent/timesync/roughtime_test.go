package timesync

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/roughtime"
)

// directResult is one scripted answer from the replaced Roughtime query.
type directResult struct {
	consensus Consensus
	err       error
}

// directHarness drives RunDirect with the network, the clock write and the wait
// all replaced, so a test asserts the loop's policy rather than a real sync.
type directHarness struct {
	mu sync.Mutex

	// results are returned in order, one per query; the last one repeats.
	results []directResult
	queries int
	applied []time.Time
	waits   []time.Duration

	// stopAfter cancels the context once this many waits have been observed,
	// so the loop terminates without depending on wall-clock time.
	stopAfter int
	cancel    context.CancelFunc
	manager   *Manager
}

func newDirectHarness(stopAfter int, results ...directResult) *directHarness {
	h := &directHarness{results: results, stopAfter: stopAfter}
	m := NewManager(nil, "")
	m.query = func(context.Context, []roughtime.Server) (Consensus, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		i := min(h.queries, len(h.results)-1)
		h.queries++
		return h.results[i].consensus, h.results[i].err
	}
	m.apply = func(t time.Time) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.applied = append(h.applied, t)
	}
	m.sleep = func(d time.Duration) <-chan time.Time {
		h.mu.Lock()
		h.waits = append(h.waits, d)
		last := len(h.waits) >= h.stopAfter
		h.mu.Unlock()
		if last {
			h.cancel()
			// A cancelled context wins the select, so this never fires.
			return make(chan time.Time)
		}
		fired := make(chan time.Time, 1)
		fired <- time.Now()
		return fired
	}
	h.manager = m
	return h
}

func (h *directHarness) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	defer cancel()
	done := make(chan struct{})
	go func() {
		h.manager.RunDirect(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunDirect did not return after its context was cancelled")
	}
}

func consensusResult(confidence string, quorum int) directResult {
	return directResult{consensus: Consensus{
		Confidence:       confidence,
		Quorum:           quorum,
		LowerOffsetNanos: 1_000,
		UpperOffsetNanos: 3_000,
	}}
}

// Two independent signed responses that agree are applied. Before this, only a
// quorum of three set the clock, so a device that could reach two servers never
// advanced its clock at all while logging a successful sync every six hours.
func TestRunDirectAppliesDegradedConsensus(t *testing.T) {
	h := newDirectHarness(1, consensusResult(ConfidenceDegraded, 2))
	h.run(t)

	if len(h.applied) != 1 {
		t.Fatalf("applied %d time(s), want the degraded consensus to set the clock once", len(h.applied))
	}
	if len(h.waits) != 1 || h.waits[0] != resyncInterval {
		t.Errorf("waits = %v, want one re-sync interval of %v after a successful discipline", h.waits, resyncInterval)
	}
	latest, ok := h.manager.LatestConsensus()
	if !ok || latest.Confidence != ConfidenceDegraded {
		t.Errorf("latest consensus = %+v (recorded: %v), want the degraded result recorded", latest, ok)
	}
}

// A single-server result does not set the clock, and it does not cost six hours
// either: it retries on the backoff ladder, because the servers that were
// unreachable may be reachable again shortly.
func TestRunDirectRetriesWhenNotVerified(t *testing.T) {
	h := newDirectHarness(3, consensusResult(ConfidenceUnbounded, 1))
	h.run(t)

	if len(h.applied) != 0 {
		t.Fatalf("applied %v; a single unverified response must not set the clock", h.applied)
	}
	want := []time.Duration{backoffSchedule[0], backoffSchedule[1], backoffSchedule[2]}
	if len(h.waits) != len(want) {
		t.Fatalf("waits = %v, want the backoff ladder %v", h.waits, want)
	}
	for i, d := range want {
		if h.waits[i] != d {
			t.Errorf("wait %d = %v, want %v", i, h.waits[i], d)
		}
		if h.waits[i] == resyncInterval {
			t.Errorf("wait %d slept the full re-sync interval on an unsynced clock", i)
		}
	}
}

// A weak result followed by a strong one syncs on the second attempt, and the
// wait after it is the re-sync interval rather than another backoff step.
func TestRunDirectRecoversFromWeakToVerified(t *testing.T) {
	h := newDirectHarness(2,
		consensusResult(ConfidenceUnbounded, 1),
		consensusResult(ConfidenceVerified, 3),
	)
	h.run(t)

	if len(h.applied) != 1 {
		t.Fatalf("applied %d time(s), want exactly the verified result", len(h.applied))
	}
	if len(h.waits) != 2 || h.waits[0] != backoffSchedule[0] || h.waits[1] != resyncInterval {
		t.Errorf("waits = %v, want [%v %v]", h.waits, backoffSchedule[0], resyncInterval)
	}
}

// A query error keeps the existing behaviour: warn, back off, retry.
func TestRunDirectBacksOffOnQueryError(t *testing.T) {
	h := newDirectHarness(2, directResult{err: errors.New("no valid Roughtime evidence")})
	h.run(t)

	if len(h.applied) != 0 {
		t.Fatalf("applied %v after a failed query", h.applied)
	}
	if len(h.waits) != 2 || h.waits[0] != backoffSchedule[0] || h.waits[1] != backoffSchedule[1] {
		t.Errorf("waits = %v, want the backoff ladder", h.waits)
	}
}

func TestDisciplinesClock(t *testing.T) {
	for confidence, want := range map[string]bool{
		ConfidenceVerified:  true,
		ConfidenceDegraded:  true,
		ConfidenceUnbounded: false,
		"":                  false,
	} {
		if got := disciplinesClock(Consensus{Confidence: confidence}); got != want {
			t.Errorf("disciplinesClock(%q) = %v, want %v", confidence, got, want)
		}
	}
}
