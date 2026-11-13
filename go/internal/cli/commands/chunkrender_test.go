package commands

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// chunkRenderSafeBuilder is a strings.Builder guarded for concurrent writes,
// mirroring tui/buildplain_test.go's safeBuilder: the heartbeat writes from
// its own goroutine while the test reads/asserts on the main goroutine.
type chunkRenderSafeBuilder struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *chunkRenderSafeBuilder) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *chunkRenderSafeBuilder) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

// TestChunkPushHeartbeatPrintsProgressLines drives startChunkPushHeartbeat
// against a chunkPushProgress that already has some progress recorded, at a
// fast 10ms interval so the test doesn't wait around, and asserts a
// heartbeat line carrying the byte progress (via chunkPushSnapshot.Line())
// appears — then that stop() halts further output, tui/buildplain_test.go
// style (TestPlainRendererHeartbeatReportsRunningStepProgress).
func TestChunkPushHeartbeatPrintsProgressLines(t *testing.T) {
	prog := newChunkPushProgress()
	prog.SetLayerCounts(2, 0)
	prog.LayerPlanned(20, 20, 2_000_000)
	prog.ChunkSent(1_000_000) // 1.0MB of 2.0MB planned

	var sb chunkRenderSafeBuilder
	stop := startChunkPushHeartbeat(prog, &sb, 10*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(sb.String(), "...") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	out := sb.String()
	if !strings.Contains(out, "...") || !strings.Contains(out, "sending chunks") || !strings.Contains(out, "1.0MB/2.0MB") {
		t.Fatalf("heartbeat missing progress line:\n%s", out)
	}

	stop()
	before := len(sb.String())
	time.Sleep(80 * time.Millisecond)
	if after := len(sb.String()); after != before {
		t.Fatalf("heartbeat kept writing after stop() (%d -> %d bytes)", before, after)
	}
}

// TestChunkPushUpdateMsgPercentAndDetail is a pure table over
// chunkPushUpdateMsg: Percent is Sent/Planned bytes clamped to 1, and is 0
// (not NaN or a panic) when Planned==0 — the state of a fresh push before
// any layer's chunk-diff plan is known. Detail always mirrors Line().
func TestChunkPushUpdateMsgPercentAndDetail(t *testing.T) {
	tests := []struct {
		name        string
		snap        chunkPushSnapshot
		wantPercent float64
	}{
		{
			name:        "planned zero: fresh push, nothing planned yet",
			snap:        chunkPushSnapshot{},
			wantPercent: 0,
		},
		{
			name:        "halfway",
			snap:        chunkPushSnapshot{SentBytes: 50, PlannedBytes: 100},
			wantPercent: 0.5,
		},
		{
			name:        "fully sent",
			snap:        chunkPushSnapshot{SentBytes: 100, PlannedBytes: 100},
			wantPercent: 1,
		},
		{
			name:        "clamped: sent bytes exceed planned (e.g. rounding across layers)",
			snap:        chunkPushSnapshot{SentBytes: 150, PlannedBytes: 100},
			wantPercent: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msg := chunkPushUpdateMsg(tc.snap)
			if msg.Percent != tc.wantPercent {
				t.Errorf("Percent = %v, want %v", msg.Percent, tc.wantPercent)
			}
			if msg.Detail != tc.snap.Line() {
				t.Errorf("Detail = %q, want %q", msg.Detail, tc.snap.Line())
			}
		})
	}
}

// TestChunkPushUpdateMsgTitlesTheDevicePhase: once the upload is done the bar
// sits full while the device works, so the title says what is being waited
// on. While chunks are sending, the title is left to the program's own.
func TestChunkPushUpdateMsgTitlesTheDevicePhase(t *testing.T) {
	preparing := chunkPushSnapshot{SentBytes: 100, PlannedBytes: 100, Uploaded: true, Preparing: true, DeviceTime: 3 * time.Second}
	if got, want := chunkPushUpdateMsg(preparing).Title, "Device preparing image..."; got != want {
		t.Errorf("Title while preparing = %q, want %q", got, want)
	}
	sending := chunkPushSnapshot{SentBytes: 50, PlannedBytes: 100}
	if got := chunkPushUpdateMsg(sending).Title; got != "" {
		t.Errorf("Title while sending = %q, want empty (keep the program's title)", got)
	}
}
