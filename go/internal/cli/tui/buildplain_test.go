package tui

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPlainRendererWritesLinePerCompletedStep(t *testing.T) {
	var sb strings.Builder
	// heartbeat disabled: this test asserts only the per-step lines.
	emit, tally, stop := newBuildPlainRenderer(&sb, 0)
	defer stop()
	emit(BuildStepEvent{ID: "#1", Kind: BuildVertexSetup, Display: "load metadata", Status: BuildStepRunning})
	emit(BuildStepEvent{ID: "#1", Kind: BuildVertexSetup, Display: "load metadata", Status: BuildStepDone, Dur: 2 * time.Second})
	emit(BuildStepEvent{ID: "#6", Kind: BuildVertexStep, Display: "[1/6] FROM python", Status: BuildStepRunning})
	emit(BuildStepEvent{ID: "#6", Kind: BuildVertexStep, Display: "[1/6] FROM python", Status: BuildStepCached})
	emit(BuildStepEvent{ID: "#9", Kind: BuildVertexStep, Display: "[4/6] RUN pip install", Status: BuildStepRunning})
	emit(BuildStepEvent{ID: "#9", Kind: BuildVertexStep, Display: "[4/6] RUN pip install", Status: BuildStepDone, Dur: 4300 * time.Millisecond})

	out := sb.String()
	// Running events produce no line; only terminal states do.
	for _, want := range []string{
		"load metadata", "2.0s",
		"[1/6] FROM python", "cached",
		"[4/6] RUN pip install", "4.3s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plain output missing %q\n%s", want, out)
		}
	}
	if got := tally(); got.Cached != 1 || got.Rebuilt != 1 {
		t.Errorf("tally = %+v, want {Cached:1 Rebuilt:1}", got)
	}
}

// A long-running step must visibly advance in CI logs; otherwise a ten-minute
// compile is an unexplained silent gap that reads as a hang.
func TestPlainRendererHeartbeatReportsRunningStepProgress(t *testing.T) {
	var sb safeBuilder
	emit, _, stop := newBuildPlainRenderer(&sb, 20*time.Millisecond)
	defer stop()

	emit(BuildStepEvent{ID: "#9", Kind: BuildVertexStep, Display: "[4/6] RUN swift build", Status: BuildStepRunning})
	emit(BuildStepEvent{ID: "#9", Kind: BuildVertexStep, Display: "[4/6] RUN swift build",
		Status: BuildStepRunning, Detail: "[525/1027] 51%  Compiling WendyKit"})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(sb.String(), "[525/1027]") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	out := sb.String()
	if !strings.Contains(out, "[4/6] RUN swift build") || !strings.Contains(out, "[525/1027] 51%  Compiling WendyKit") {
		t.Fatalf("heartbeat missing step or detail:\n%s", out)
	}

	// Once the step finishes the heartbeat stops mentioning it.
	emit(BuildStepEvent{ID: "#9", Kind: BuildVertexStep, Display: "[4/6] RUN swift build", Status: BuildStepDone, Dur: time.Second})
	before := strings.Count(sb.String(), "...")
	time.Sleep(80 * time.Millisecond)
	if after := strings.Count(sb.String(), "..."); after != before {
		t.Fatalf("heartbeat kept reporting a finished step (%d -> %d)", before, after)
	}
}

func TestPlainRendererNoHeartbeatWhenDisabled(t *testing.T) {
	var sb safeBuilder
	emit, _, stop := newBuildPlainRenderer(&sb, 0)
	defer stop()
	emit(BuildStepEvent{ID: "#9", Kind: BuildVertexStep, Display: "[4/6] RUN x", Status: BuildStepRunning, Detail: "working"})
	time.Sleep(30 * time.Millisecond)
	if out := sb.String(); out != "" {
		t.Fatalf("want no output with heartbeat disabled, got %q", out)
	}
}

// A reopened step (a lazily pulled base image) prints nothing while it runs
// again, a second done line with the later duration when it finishes, and is
// one step in the tally. CI logs get no ticking timer, only the heartbeat.
func TestPlainRendererReopenedStepCountsOnce(t *testing.T) {
	var sb strings.Builder
	emit, tally, stop := newBuildPlainRenderer(&sb, 0)
	defer stop()
	from := BuildStepEvent{ID: "#5", Kind: BuildVertexStep, Display: "[1/5] FROM pytorch", Status: BuildStepRunning}
	emit(from)
	from.Status, from.Dur = BuildStepDone, 2*time.Second
	emit(from)
	before := sb.String()
	emit(BuildStepEvent{ID: "#5", Kind: BuildVertexStep, Display: "[1/5] FROM pytorch", Status: BuildStepRunning,
		Detail: "unpacking layer 1", Bytes: ByteProgress{Current: 3_100_000_000, Total: 7_800_000_000}})
	if sb.String() != before {
		t.Fatalf("a running event printed output:\n%s", sb.String())
	}
	from.Dur = 533700 * time.Millisecond
	emit(from)

	if out := sb.String(); !strings.Contains(out, "2.0s") || !strings.Contains(out, "533.7s") {
		t.Errorf("want both done lines:\n%s", out)
	}
	if got := tally(); got.Cached != 0 || got.Rebuilt != 1 {
		t.Errorf("tally = %+v, want the step counted once", got)
	}
}

// safeBuilder is a strings.Builder guarded for the heartbeat goroutine.
type safeBuilder struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *safeBuilder) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *safeBuilder) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}
