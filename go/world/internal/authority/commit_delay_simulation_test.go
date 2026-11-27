//go:build world_simulation_faults

package authority

import (
	"github.com/wendylabsinc/wendy/go/world/internal/runtimeobs"
	"path/filepath"
	"testing"
	"time"
)

func TestTaggedReplayDelayIsMeasuredOnlyOnDurableAttempts(t *testing.T) {
	c, err := OpenReplay(filepath.Join(t.TempDir(), "ledger"), 16)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	metrics := runtimeobs.New()
	c.SetMetrics(metrics)
	if err = c.SetCommitDelayForSimulation(15 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err = c.Checkpoint(1, "digest", now); err != nil {
		t.Fatal(err)
	}
	if got := metricDuration(t, metrics, "replay.checkpoint_commit"); got.Count != 1 || got.MaxNS < uint64(15*time.Millisecond) {
		t.Fatal(got)
	}
	if err = c.Checkpoint(1, "digest", now); err != nil {
		t.Fatal(err)
	}
	if metricDuration(t, metrics, "replay.checkpoint_commit").Count != 1 {
		t.Fatal("noop caused delayed commit")
	}
	for _, bad := range []time.Duration{-1, time.Second + 1} {
		if c.SetCommitDelayForSimulation(bad) == nil {
			t.Fatal("invalid delay accepted")
		}
	}
	if err = c.SetCommitDelayForSimulation(0); err != nil || c.beforeCommit != nil {
		t.Fatal("delay not cleared", err)
	}
	if NewMemoryReplay(1).SetCommitDelayForSimulation(time.Millisecond) == nil {
		t.Fatal("memory replay accepted disk delay")
	}
}
