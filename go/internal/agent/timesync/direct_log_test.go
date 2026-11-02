package timesync

import (
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// This runs unmodified on the previous source as a negative control. The
// scripted clock writer never touches host UTC, so a successful query alone
// must not log a successful synchronization or confirmed UTC.
func TestRunDirectNoopApplyDoesNotClaimClockSynchronized(t *testing.T) {
	boot, err := bootTimeNanos()
	if err != nil {
		t.Fatal(err)
	}
	target := time.Now().Add(598 * time.Second)
	c := Consensus{Confidence: ConfidenceDegraded, Quorum: 2, LowerOffsetNanos: target.Add(-time.Second).UnixNano() - boot, UpperOffsetNanos: target.Add(time.Second).UnixNano() - boot}
	core, logs := observer.New(zap.DebugLevel)
	h := newDirectHarness(1, directResult{consensus: c})
	h.manager.logger = zap.New(core)
	h.run(t)
	if logs.FilterMessage("timesync: synced via Roughtime").Len() != 0 {
		t.Fatal("query/no-op writer was logged as a successful clock synchronization")
	}
	complete := logs.FilterMessage("timesync: direct Roughtime apply complete").All()
	if len(complete) != 1 || complete[0].ContextMap()["outcome"] != "unconfirmed" || complete[0].ContextMap()["utc_within_verified_interval"] != false {
		t.Fatal("no-op write falsely confirmed UTC", complete)
	}
}
