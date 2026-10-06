package commands

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeLogStream replays scripted frames the way a gRPC client stream does:
// frames in order, then endErr (io.EOF when nil) once frames is closed. Recv
// blocks while nothing is queued, and returns the gRPC Canceled error when
// ctx is cancelled — exactly what a real stream returns on Ctrl-C.
type fakeLogStream struct {
	ctx    context.Context
	frames chan *agentpb.StreamLogsResponse
	endErr error
}

func newFakeLogStream(ctx context.Context, frames ...*agentpb.StreamLogsResponse) *fakeLogStream {
	f := &fakeLogStream{ctx: ctx, frames: make(chan *agentpb.StreamLogsResponse, len(frames)+1)}
	for _, fr := range frames {
		f.frames <- fr
	}
	return f
}

func (f *fakeLogStream) Recv() (*agentpb.StreamLogsResponse, error) {
	select {
	case r, ok := <-f.frames:
		if !ok {
			if f.endErr != nil {
				return nil, f.endErr
			}
			return nil, io.EOF
		}
		return r, nil
	case <-f.ctx.Done():
		return nil, status.FromContextError(f.ctx.Err()).Err()
	}
}

func logsFrame(history bool, body string) *agentpb.StreamLogsResponse {
	return &agentpb.StreamLogsResponse{
		IsHistory: history,
		Logs: &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
			ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{{
				Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}},
			}}}},
		}}},
	}
}

func historyFrame(body string) *agentpb.StreamLogsResponse { return logsFrame(true, body) }
func liveFrame(body string) *agentpb.StreamLogsResponse    { return logsFrame(false, body) }

// heartbeatFrame is the empty frame agents send after 15 s of quiet (WDY-2912).
func heartbeatFrame() *agentpb.StreamLogsResponse { return &agentpb.StreamLogsResponse{} }

// frameRecorder collects the bodies of the frames consumeLogStream handles.
type frameRecorder struct {
	mu     sync.Mutex
	bodies []string
}

func (r *frameRecorder) handle(resp *agentpb.StreamLogsResponse) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rl := range resp.GetLogs().GetResourceLogs() {
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				r.bodies = append(r.bodies, lr.GetBody().GetStringValue())
			}
		}
	}
}

func (r *frameRecorder) got() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.bodies, ",")
}

// setNoFollowTimings shrinks or stretches the --no-follow timers for one test.
func setNoFollowTimings(t *testing.T, firstFrame, idleGap time.Duration) {
	t.Helper()
	prevFirst, prevIdle := noFollowFirstFrameWait, noFollowIdleGap
	noFollowFirstFrameWait, noFollowIdleGap = firstFrame, idleGap
	t.Cleanup(func() { noFollowFirstFrameWait, noFollowIdleGap = prevFirst, prevIdle })
}

// runNoFollow runs consumeLogStream without follow and cancels the stream
// context afterwards, as the command does.
func runNoFollow(t *testing.T, frames ...*agentpb.StreamLogsResponse) (string, time.Duration, error) {
	t.Helper()
	got, elapsed, _, err := runNoFollowResult(t, frames...)
	return got, elapsed, err
}

// runNoFollowResult is runNoFollow that also returns how the replay ended.
func runNoFollowResult(t *testing.T, frames ...*agentpb.StreamLogsResponse) (string, time.Duration, logReplayResult, error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var rec frameRecorder
	start := time.Now()
	res, err := consumeLogStream(ctx, newFakeLogStream(ctx, frames...), false, rec.handle)
	return rec.got(), time.Since(start), res, err
}

func TestConsumeLogStream_NoFollowStopsAtFirstLiveFrame(t *testing.T) {
	// Idle timers far longer than the test: only the live frame can end it.
	setNoFollowTimings(t, time.Minute, time.Minute)
	got, elapsed, err := runNoFollow(t, historyFrame("h1"), historyFrame("h2"), liveFrame("live"), historyFrame("never"))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "h1,h2" {
		t.Fatalf("handled %q, want only the replayed frames h1,h2", got)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("took %s; the live frame should end the replay immediately", elapsed)
	}
}

func TestConsumeLogStream_NoFollowStopsAtHeartbeat(t *testing.T) {
	setNoFollowTimings(t, time.Minute, time.Minute)
	got, _, err := runNoFollow(t, historyFrame("h1"), heartbeatFrame())
	if err != nil || got != "h1" {
		t.Fatalf("got %q, err %v; want h1 and a clean stop at the heartbeat", got, err)
	}
}

func TestConsumeLogStream_NoFollowStopsWhenReplayGoesQuiet(t *testing.T) {
	// Pre-WDY-2912 agents send no heartbeat: after the burst, nothing arrives.
	setNoFollowTimings(t, time.Minute, 50*time.Millisecond)
	got, elapsed, err := runNoFollow(t, historyFrame("h1"), historyFrame("h2"))
	if err != nil || got != "h1,h2" {
		t.Fatalf("got %q, err %v; want h1,h2 and a clean stop", got, err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("took %s; want to stop after the idle gap", elapsed)
	}
}

func TestConsumeLogStream_NoFollowWithoutHistoryGivesUpAfterFirstFrameWait(t *testing.T) {
	setNoFollowTimings(t, 50*time.Millisecond, time.Minute)
	got, elapsed, err := runNoFollow(t)
	if err != nil || got != "" {
		t.Fatalf("got %q, err %v; want nothing and a clean stop", got, err)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("took %s; want to give up after noFollowFirstFrameWait", elapsed)
	}
}

func TestConsumeLogStream_NoFollowReportsStreamErrors(t *testing.T) {
	setNoFollowTimings(t, time.Minute, time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newFakeLogStream(ctx, historyFrame("h1"))
	stream.endErr = status.Error(codes.Unavailable, "agent went away")
	close(stream.frames)
	var rec frameRecorder
	_, err := consumeLogStream(ctx, stream, false, rec.handle)
	if err == nil || !strings.Contains(err.Error(), "receiving logs") {
		t.Fatalf("err = %v, want the stream failure reported", err)
	}
}

// Ctrl-C while following used to end with "receiving logs: rpc error: code =
// Canceled desc = context canceled" and exit status 1.
func TestConsumeLogStream_FollowCancelIsCleanExit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var rec frameRecorder
	handled := 0
	_, err := consumeLogStream(ctx, newFakeLogStream(ctx, historyFrame("h1"), liveFrame("l1")), true, func(resp *agentpb.StreamLogsResponse) {
		rec.handle(resp)
		if handled++; handled == 2 {
			cancel() // the user presses Ctrl-C while following
		}
	})
	if err != nil {
		t.Fatalf("err = %v, want a clean exit on cancel", err)
	}
	if got := rec.got(); got != "h1,l1" {
		t.Fatalf("handled %q, want h1,l1 (follow mode passes live frames through)", got)
	}
}

func TestConsumeLogStream_FollowReportsStreamErrorsAndEOF(t *testing.T) {
	ctx := context.Background()

	eof := newFakeLogStream(ctx, liveFrame("l1"))
	close(eof.frames)
	if _, err := consumeLogStream(ctx, eof, true, func(*agentpb.StreamLogsResponse) {}); err != nil {
		t.Fatalf("EOF: err = %v, want nil", err)
	}

	broken := newFakeLogStream(ctx)
	broken.endErr = status.Error(codes.Unavailable, "agent went away")
	close(broken.frames)
	_, err := consumeLogStream(ctx, broken, true, func(*agentpb.StreamLogsResponse) {})
	if err == nil || !strings.Contains(err.Error(), "receiving logs") || !strings.Contains(err.Error(), "agent went away") {
		t.Fatalf("err = %v, want the stream failure reported", err)
	}
}

func TestDeviceLogsHasNoFollowFlag(t *testing.T) {
	f := newDeviceLogsCmd().Flags().Lookup("no-follow")
	if f == nil {
		t.Fatal("device logs is missing --no-follow")
	}
	if f.DefValue != "false" {
		t.Fatalf("--no-follow default = %q, want false (following stays the default)", f.DefValue)
	}
}

// --no-follow can end with nothing printed: agents released before
// 2026-08-19 flag only the --tail disk replay as history, older ones none of
// it, and a slow disk scan can outlast noFollowFirstFrameWait. The command
// must be able to tell that apart from a replay that printed logs.
func TestConsumeLogStream_NoFollowReportsHowItEnded(t *testing.T) {
	cases := []struct {
		name        string
		firstFrame  time.Duration
		frames      []*agentpb.StreamLogsResponse
		wantEnd     logReplayEnd
		wantHistory int
	}{
		{"replay then live frame", time.Minute, []*agentpb.StreamLogsResponse{historyFrame("h1"), historyFrame("h2"), liveFrame("l")}, replayEndedLive, 2},
		{"old agent: only unflagged frames", time.Minute, []*agentpb.StreamLogsResponse{liveFrame("cached"), liveFrame("cached2")}, replayEndedLive, 0},
		{"nothing within the first-frame wait", 50 * time.Millisecond, nil, replayEndedNoFrames, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setNoFollowTimings(t, tc.firstFrame, time.Minute)
			_, _, res, err := runNoFollowResult(t, tc.frames...)
			if err != nil {
				t.Fatalf("err = %v", err)
			}
			if res.end != tc.wantEnd || res.history != tc.wantHistory {
				t.Fatalf("result = %+v, want end %v with %d replayed frames", res, tc.wantEnd, tc.wantHistory)
			}
		})
	}
}

func TestNoFollowHint(t *testing.T) {
	if hint := noFollowHint(logReplayResult{end: replayEndedLive, history: 3}, 20); hint != "" {
		t.Fatalf("a replay that printed logs needs no hint, got %q", hint)
	}
	if hint := noFollowHint(logReplayResult{end: replayEndedCancelled}, 0); hint != "" {
		t.Fatalf("Ctrl-C needs no hint, got %q", hint)
	}
	noTail := noFollowHint(logReplayResult{end: replayEndedLive}, 0)
	for _, want := range []string{"No log history received", "--tail", "2026-08-19"} {
		if !strings.Contains(noTail, want) {
			t.Errorf("hint without --tail %q does not mention %q", noTail, want)
		}
	}
	withTail := noFollowHint(logReplayResult{end: replayEndedNoFrames}, 20)
	for _, want := range []string{"No log history received", "10s", "2026-05-22"} {
		if !strings.Contains(withTail, want) {
			t.Errorf("hint with --tail %q does not mention %q", withTail, want)
		}
	}
}

func TestDeviceLogsNoFollowHelpMentionsAgentCaveat(t *testing.T) {
	f := newDeviceLogsCmd().Flags().Lookup("no-follow")
	if f == nil || !strings.Contains(f.Usage, "2026-08-19") {
		t.Fatalf("--no-follow help should say older agents replay history only with --tail; got %q", f.Usage)
	}
}

// timedFrame is a frame a fakeLogStream delivers after a delay.
type timedFrame struct {
	after time.Duration // since the previous frame (or the start)
	frame *agentpb.StreamLogsResponse
}

// newTimedLogStream delivers frames on a schedule, like a replay crossing a
// slow link, then stays open (no EOF) as a live stream would.
func newTimedLogStream(ctx context.Context, schedule ...timedFrame) *fakeLogStream {
	f := &fakeLogStream{ctx: ctx, frames: make(chan *agentpb.StreamLogsResponse, len(schedule))}
	go func() {
		for _, s := range schedule {
			select {
			case <-time.After(s.after):
				f.frames <- s.frame
			case <-ctx.Done():
				return
			}
		}
	}()
	return f
}

// Over LTE or a cloud tunnel the replay burst can pause between frames. A
// fixed 1.5 s idle gap then cut the history short without a word. The gap now
// stretches to 3x the longest pause seen so far in the replay.
func TestConsumeLogStream_NoFollowIdleGapAdaptsToSlowReplay(t *testing.T) {
	setNoFollowTimings(t, time.Minute, 400*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream := newTimedLogStream(ctx,
		timedFrame{0, historyFrame("h1")},
		timedFrame{250 * time.Millisecond, historyFrame("h2")}, // within the 400ms base gap; the gap becomes 750ms
		timedFrame{550 * time.Millisecond, historyFrame("h3")}, // a fixed 400ms gap would have stopped before this
	)
	var rec frameRecorder
	res, err := consumeLogStream(ctx, stream, false, rec.handle)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := rec.got(); got != "h1,h2,h3" {
		t.Fatalf("handled %q, want the whole slow replay h1,h2,h3", got)
	}
	if res.end != replayEndedIdle || res.idleGap < time.Second {
		t.Fatalf("result = %+v, want an idle end with a gap stretched past 1s by the 550ms pause", res)
	}
}

func TestAdaptiveIdleGap(t *testing.T) {
	cases := []struct{ longest, want time.Duration }{
		{0, 1500 * time.Millisecond},                      // a fast burst: the floor
		{400 * time.Millisecond, 1500 * time.Millisecond}, // 3x is still under the floor
		{time.Second, 3 * time.Second},                    // a slow link: 3x its longest pause
		{5 * time.Second, 10 * time.Second},               // capped
	}
	for _, tc := range cases {
		if got := adaptiveIdleGap(tc.longest); got != tc.want {
			t.Errorf("adaptiveIdleGap(%s) = %s, want %s", tc.longest, got, tc.want)
		}
	}
}

// Ending on the idle gap is a guess, so the command says so — but only when
// the replay came up short of what it could hold (--tail N, or the agents'
// 20 cached batches without it). A quiet app always ends on the gap, and a
// full replay is not worth a caveat on every run.
func TestNoFollowHint_IdleEndIsNoted(t *testing.T) {
	idle := func(history int) logReplayResult {
		return logReplayResult{end: replayEndedIdle, history: history, idleGap: 1500 * time.Millisecond}
	}
	cases := []struct {
		name     string
		res      logReplayResult
		tail     int32
		wantNote bool
	}{
		{"--tail 20, 4 replayed", idle(4), 20, true},
		{"--tail 20, 20 replayed", idle(20), 20, false},
		{"--tail 5, more than 5 replayed", idle(7), 5, false},
		{"no --tail, 5 replayed", idle(5), 0, true},
		{"no --tail, the agent's full 20-batch cache", idle(agentRecentLogBatches), 0, false},
		{"ended by a live frame", logReplayResult{end: replayEndedLive, history: 4}, 20, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hint := noFollowHint(tc.res, tc.tail)
			if !tc.wantNote {
				if hint != "" {
					t.Fatalf("want no note, got %q", hint)
				}
				return
			}
			for _, want := range []string{"1.5s", "slow connection"} {
				if !strings.Contains(hint, want) {
					t.Errorf("idle-end note %q does not mention %q", hint, want)
				}
			}
		})
	}
	if agentRecentLogBatches != 20 {
		t.Fatalf("agentRecentLogBatches = %d; the Go and Swift agents cache 20 recent log batches", agentRecentLogBatches)
	}
}
