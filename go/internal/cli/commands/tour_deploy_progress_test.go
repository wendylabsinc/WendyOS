package commands

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestTourBuildProgressKeepsLiveCountersWithoutCommandOutput(t *testing.T) {
	var model tea.Model = tui.NewBuildStepsModel("Building your robot app...")
	emit := tourBuildEvents(func(e tui.BuildStepEvent) {
		model, _ = model.Update(tui.BuildStepMsg(e))
	})
	emit(tui.BuildStepEvent{ID: "first", Kind: tui.BuildVertexStep, Display: "RUN internal-command", Status: tui.BuildStepCached})
	emit(tui.BuildStepEvent{ID: "second", Kind: tui.BuildVertexPull, Display: "private-image:tag", Status: tui.BuildStepRunning,
		Detail: "internal build detail", Bytes: tui.ByteProgress{Current: 1_000_000, Total: 2_000_000}})
	view := model.View()
	if !strings.Contains(view, "1.0MB/2.0MB") || !strings.Contains(view, "Downloading app files") {
		t.Fatalf("live build progress lost: %s", view)
	}
	for _, internal := range []string{"internal-command", "private-image", "internal build detail", "cached"} {
		if strings.Contains(view, internal) {
			t.Fatalf("tour displayed %q: %s", internal, view)
		}
	}
	if strings.Count(view, "Robot app") != 1 {
		t.Fatalf("build events did not collapse to one row: %s", view)
	}
}

func TestTourBuildProgressRetainsFailureDiagnostics(t *testing.T) {
	defer forceBuildProgressInteractive(false)()
	var out strings.Builder
	defer setBuildProgressOut(&out)()
	originalPersist := persistBuildFailureLog
	t.Cleanup(func() { persistBuildFailureLog = originalPersist })
	var saved string
	persistBuildFailureLog = func(_ string, raw string) (string, error) {
		saved = raw
		return "/tmp/robot-build.log", nil
	}
	ctx := context.WithValue(context.Background(), tourPresentationKey{}, true)
	wantErr := errors.New("docker build failed")
	err := runBuildWithProgress(ctx, "OCI internals", dumpRawAlways, func(_ context.Context, stream, setup io.Writer) error {
		_, _ = io.WriteString(setup, "bootstrapping internal builder\n")
		_, _ = io.WriteString(stream, "#9 [4/6] RUN pip install\n#9 12.34 ERROR: could not find the robot dependency\n")
		return wantErr
	})
	if !errors.Is(err, wantErr) || !strings.Contains(out.String(), "could not find the robot dependency") || !strings.Contains(out.String(), "/tmp/robot-build.log") {
		t.Fatalf("failure diagnostics lost: %v\n%s", err, out.String())
	}
	if !strings.Contains(saved, "bootstrapping internal builder") || !strings.Contains(saved, "RUN pip install") {
		t.Fatalf("full failure log lost: %s", saved)
	}
	if strings.Contains(out.String(), "OCI internals") || strings.Contains(out.String(), "Built & pushed") {
		t.Fatalf("tour printed internal build heading or summary: %s", out.String())
	}
}

func TestTourBuildProgressCancellationStillStopsBuilder(t *testing.T) {
	defer forceBuildProgressInteractive(true)()
	originalProgram := buildProgressProgram
	t.Cleanup(func() { buildProgressProgram = originalProgram })
	buildProgressProgram = func(model tea.Model) *tea.Program {
		return tui.NewProgressProgram(model, tea.WithInput(strings.NewReader("\x03")), tea.WithOutput(io.Discard))
	}
	ctx := context.WithValue(context.Background(), tourPresentationKey{}, true)
	exited := make(chan struct{})
	err := runBuildWithProgress(ctx, "Building", dumpRawAlways, func(ctx context.Context, _, _ io.Writer) error {
		<-ctx.Done()
		close(exited)
		return ctx.Err()
	})
	if !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("builder still running after cancellation")
	}
}

func TestTourUploadProgressReportsBytesWithoutRepeatedDetails(t *testing.T) {
	snap := chunkPushSnapshot{SentBytes: 1_000_000, PlannedBytes: 2_000_000, TotalChunks: 20, MissingChunks: 10}
	msg := tourChunkPushUpdateMsg(snap)
	if msg.Percent != .5 || msg.Written != snap.SentBytes || msg.Total != snap.PlannedBytes || msg.Detail != "" {
		t.Fatalf("wrong live transfer counters: %+v", msg)
	}
	snap.Preparing = true
	msg = tourChunkPushUpdateMsg(snap)
	if msg.Written != 0 || msg.Total != 0 || msg.Detail != "" || !strings.Contains(msg.Title, "Starting") {
		t.Fatalf("preparation kept repeating upload details: %+v", msg)
	}
	prog := newChunkPushProgress()
	prog.SetLayerCounts(2, 0)
	prog.LayerPlanned(20, 10, 2_000_000)
	prog.ChunkSent(1_000_000)
	var out chunkRenderSafeBuilder
	stop := startChunkPushHeartbeatForTour(prog, &out, 5*time.Millisecond, true)
	defer stop()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !strings.Contains(out.String(), "1.0MB/2.0MB") {
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(out.String(), "1.0MB/2.0MB") {
		t.Fatalf("tour transfer went silent: %s", out.String())
	}
	if strings.Contains(out.String(), "chunks") || strings.Contains(out.String(), "layers") {
		t.Fatalf("tour heartbeat printed internal counters: %s", out.String())
	}
}

func TestTourUploadPreservesPreparationErrorsAndHidesSummary(t *testing.T) {
	defer forceBuildProgressInteractive(false)()
	var out strings.Builder
	defer setBuildProgressOut(&out)()
	ctx := context.WithValue(context.Background(), tourPresentationKey{}, true)
	client := &fakeContainerClient{queryFn: func(*agentpb.QueryChunksRequest) *agentpb.QueryChunksResponse {
		return &agentpb.QueryChunksResponse{}
	}}
	logs := captureStderr(t, func() {
		_, err := pushLayersWithProgress(ctx, client, nil, func(context.Context, []*agentpb.RunContainerLayerHeader) error {
			return status.Error(codes.DataLoss, "image verification failed")
		}, gzipChunkUploadConfig, nil)
		if status.Code(err) != codes.DataLoss || !strings.Contains(err.Error(), "image verification failed") {
			t.Fatalf("transfer failure lost: %v", err)
		}
		if _, err := pushLayersWithProgress(ctx, client, nil, nil, gzipChunkUploadConfig, nil); err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(logs, "chunk(s)") || strings.Contains(logs, "layer(s)") || strings.Contains(logs, "Sent ") {
		t.Fatalf("tour transfer printed completion stats: %s", logs)
	}
}
