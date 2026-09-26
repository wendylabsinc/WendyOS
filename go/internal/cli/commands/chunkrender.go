package commands

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// chunkPushInteractiveTickInterval is how often the interactive progress bar
// polls chunkPushProgress for a fresh chunkPushSnapshot. There is no native
// event stream to drive it (unlike createContainerWithProgressTUI, which
// renders directly off CreateContainerWithProgress responses) — the push
// goroutines only mutate shared counters — so a ticker is the render loop.
const chunkPushInteractiveTickInterval = 200 * time.Millisecond

// pushLayersWithProgress wraps the chunk push with live progress: a
// periodic heartbeat line on non-interactive terminals (CI/piped output,
// same shape as tui.NewBuildPlainRenderer's heartbeat), or an interactive
// Bubble Tea progress bar on a real terminal, mirroring
// createContainerWithProgressTUI's plain/interactive split. Either way, a
// resume-legible Summary() line is printed once the push completes.
// prepare, when non-nil, runs device-side image preparation concurrently
// with the upload (see pushLayersByChunksWithPrepare).
//
// Detach needs no branch here: it only diverges after Started, downstream
// of this call.
func pushLayersWithProgress(ctx context.Context, cs agentpb.WendyContainerServiceClient, layers []localLayer, prepare imagePrepareFunc, cfg chunkUploadConfig) ([]*agentpb.RunContainerLayerHeader, error) {
	prog := newChunkPushProgress()

	if !buildProgressInteractive() {
		stop := startChunkPushHeartbeat(prog, buildProgressOut, tui.PlainHeartbeatInterval)
		headers, err := pushLayersByChunksWithPrepareMode(ctx, cs, layers, prepare, nil, false, prog, cfg)
		stop()
		if err != nil {
			return nil, err
		}
		cliLogln("%s", prog.Snapshot().Summary())
		return headers, nil
	}

	// Interactive: mirror createContainerWithProgressTUI's shape — a
	// background goroutine does the real work and signals completion via
	// ProgressDoneMsg, cancellation on user quit ("q"/ctrl+c) propagates
	// through ctx, and the final summary prints after the program exits.
	pushCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	tp := tui.NewProgressProgram(tui.NewProgress("Sending changed layers to device...").WithoutErrorView())

	var (
		headers []*agentpb.RunContainerLayerHeader
		pushErr error
		done    = make(chan struct{})
	)
	go func() {
		defer close(done)
		h, err := pushLayersByChunksWithPrepareMode(pushCtx, cs, layers, prepare, nil, false, prog, cfg)
		headers, pushErr = h, err
		tp.Send(tui.ProgressDoneMsg{Err: err})
	}()

	go func() {
		ticker := time.NewTicker(chunkPushInteractiveTickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				tp.Send(chunkPushUpdateMsg(prog.Snapshot()))
			}
		}
	}()

	finalModel, err := tp.Run()
	if err != nil {
		cancel()
		<-done
		return nil, fmt.Errorf("progress TUI: %w", err)
	}
	if progressModelUserCancelled(finalModel) {
		cancel()
		<-done
		return nil, ErrUserCancelled
	}

	<-done
	if pushErr != nil {
		return nil, pushErr
	}
	cliLogln("%s", prog.Snapshot().Summary())
	return headers, nil
}

// startChunkPushHeartbeat starts a background goroutine that writes a
// heartbeat line to w every interval, carrying prog's current
// chunkPushSnapshot, so a long non-interactive chunk push visibly advances
// instead of going silent — the same purpose as, and same line shape as,
// tui.NewBuildPlainRenderer's running-step heartbeat
// ("  ...     <display>  <detail>  (<elapsed>)"):
//
//	...     sending chunks  38%  98.4MB/259.1MB  8.0MB/s (device already has 1846/2258 chunks, 3 layers)  (30.0s)
//
// The returned stop func halts the goroutine and blocks until it has fully
// exited — so once stop() returns, w will never receive another heartbeat
// write — and is safe to call multiple times (only the first call closes
// done). interval<=0 disables the heartbeat entirely (no goroutine starts;
// stop is then an immediate no-op), matching newBuildPlainRenderer's
// heartbeat<=0 case.
func startChunkPushHeartbeat(prog *chunkPushProgress, w io.Writer, interval time.Duration) (stop func()) {
	done := make(chan struct{})
	var stopOnce sync.Once

	if interval <= 0 {
		return func() { stopOnce.Do(func() { close(done) }) }
	}

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				snap := prog.Snapshot()
				fmt.Fprintf(w, "  ...     %s  %s  (%s)\n", snap.Activity(), snap.Line(), formatChunkPushElapsed(snap.Elapsed))
			}
		}
	}()

	return func() {
		stopOnce.Do(func() { close(done) })
		<-stopped
	}
}

// chunkPushUpdateMsg renders a chunkPushSnapshot as a tui.ProgressUpdateMsg
// for the interactive progress bar. Percent is bytes sent over bytes
// planned, clamped to 1 so a snapshot taken between "last chunk sent" and
// "goroutine notices totals settled" never overshoots the bar; it is 0 (not
// NaN or a divide-by-zero panic) when nothing has been planned yet — the
// state of a fresh push before any layer's chunk-diff plan is known. Detail
// is the same live progress line the non-interactive heartbeat prints.
func chunkPushUpdateMsg(s chunkPushSnapshot) tui.ProgressUpdateMsg {
	var percent float64
	if s.PlannedBytes > 0 {
		percent = float64(s.SentBytes) / float64(s.PlannedBytes)
		if percent > 1 {
			percent = 1
		}
	}
	return tui.ProgressUpdateMsg{
		Percent: percent,
		Detail:  s.Line(),
	}
}
