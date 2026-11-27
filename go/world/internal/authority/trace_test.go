package authority

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/diagnostics"
	"path/filepath"
	"testing"
	"time"
)

func tracedContext(t *testing.T) (context.Context, *diagnostics.Collector, *diagnostics.Request) {
	t.Helper()
	c, e := diagnostics.New("0123456789abcdef0123456789abcdef", diagnostics.Limits{WindowMS: 3000, GraceMS: 3000, MaxRequests: 8, MaxEvents: 128})
	if e != nil {
		t.Fatal(e)
	}
	if e = c.Arm("0123456789abcdef0123456789abcdef"); e != nil {
		t.Fatal(e)
	}
	ctx, r := c.Start(context.Background(), diagnostics.Ack)
	return ctx, c, r
}
func TestTraceReplayActualAppendNoopAndFailure(t *testing.T) {
	cache, e := OpenReplay(filepath.Join(t.TempDir(), "replay"), 16)
	if e != nil {
		t.Fatal(e)
	}
	defer cache.Close()
	now := time.Unix(1700000000, 0)
	if e = cache.Checkpoint(1, "digest", now); e != nil {
		t.Fatal(e)
	}
	ctx, c, r := tracedContext(t)
	if e = cache.CheckpointContext(ctx, 1, "digest", now); e != nil {
		t.Fatal(e)
	}
	if e = cache.CheckpointAndConsumeContext(ctx, 1, "digest", "one", now.Add(time.Minute), now.Add(time.Millisecond)); e != nil {
		t.Fatal(e)
	}
	cache.appendWrite = func(b []byte) (int, error) { return len(b) - 1, nil }
	if e = cache.CheckpointContext(ctx, 1, "digest", now.Add(2*time.Millisecond)); e == nil {
		t.Fatal("short write admitted")
	}
	r.Finish(503)
	counts := map[string]int{}
	failed := map[string]bool{}
	for _, event := range c.Snapshot().Events {
		counts[event.Phase]++
		failed[event.Phase] = failed[event.Phase] || event.Failed
	}
	if counts["replay_commit"] != 2 || counts["replay_append"] != 2 || counts["replay_sync"] != 1 || !failed["replay_append"] || !failed["replay_commit"] {
		t.Fatal(counts, failed)
	}
	if cache.Checkpoint(1, "digest", now.Add(3*time.Millisecond)) == nil {
		t.Fatal("poison behavior changed")
	}
}

type overriddenReplay struct {
	*ReplayCache
	checkpoint, admission int
}

var traceOverrideError = errors.New("override denies")

func (r *overriddenReplay) Checkpoint(uint64, string, time.Time) error {
	r.checkpoint++
	return traceOverrideError
}
func (r *overriddenReplay) CheckpointAndConsume(uint64, string, string, time.Time, time.Time) error {
	r.admission++
	return traceOverrideError
}
func TestTracePreservesEmbeddedCustomReplayOverrides(t *testing.T) {
	ctx, _, request := tracedContext(t)
	defer request.Finish(401)
	r := &overriddenReplay{ReplayCache: NewMemoryReplay(8)}
	now := time.Now()
	if !errors.Is(replayCheckpoint(ctx, r, 1, "digest", now), traceOverrideError) || r.checkpoint != 1 {
		t.Fatal("checkpoint override bypassed")
	}
	if !errors.Is(replayAdmission(ctx, r, 1, "digest", "nonce", now.Add(time.Minute), now), traceOverrideError) || r.admission != 1 {
		t.Fatal("admission override bypassed")
	}
}
