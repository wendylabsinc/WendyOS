package commands

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type recoveryLogStream struct {
	frames []*agentpb.StreamLogsResponse
	err    error
}

func (s *recoveryLogStream) Recv() (*agentpb.StreamLogsResponse, error) {
	if len(s.frames) > 0 {
		f := s.frames[0]
		s.frames = s.frames[1:]
		return f, nil
	}
	return nil, s.err
}
func TestLogRecoveryIsBoundedAndSkipsDuplicateHistory(t *testing.T) {
	calls, frames, notices := 0, 0, 0
	open := func(context.Context) (logStreamReceiver, error) {
		calls++
		if calls == 1 {
			return &recoveryLogStream{frames: []*agentpb.StreamLogsResponse{{IsHistory: true}, {}}, err: status.Error(codes.Unavailable, "connection lost")}, nil
		}
		return &recoveryLogStream{frames: []*agentpb.StreamLogsResponse{{IsHistory: true}, {}}, err: io.EOF}, nil
	}
	_, err := consumeLogStreamWithRetry(context.Background(), open, true, func(*agentpb.StreamLogsResponse) { frames++ }, func(s string) {
		notices++
		if notices == 1 && !strings.Contains(s, "some logs may be missing") {
			t.Fatal(s)
		}
	}, []time.Duration{0, 0})
	if err != nil || calls != 2 || frames != 3 || notices != 2 {
		t.Fatalf("calls=%d frames=%d notices=%d err=%v", calls, frames, notices, err)
	}
}

func TestLogRecoveryRetainsInitialHistoryAfterOpenFailure(t *testing.T) {
	calls, frames := 0, 0
	_, err := consumeLogStreamWithRetry(context.Background(), func(context.Context) (logStreamReceiver, error) {
		calls++
		if calls == 1 {
			return nil, status.Error(codes.Unavailable, "not connected yet")
		}
		return &recoveryLogStream{frames: []*agentpb.StreamLogsResponse{{IsHistory: true}}, err: io.EOF}, nil
	}, true, func(*agentpb.StreamLogsResponse) { frames++ }, nil, []time.Duration{0})
	if err != nil || frames != 1 || calls != 2 {
		t.Fatalf("lost initial replay: calls=%d frames=%d err=%v", calls, frames, err)
	}
}
func TestLogRecoveryDoesNotRetrySecurityErrorsOrFiniteReads(t *testing.T) {
	for _, tc := range []struct {
		code   codes.Code
		follow bool
		calls  int
	}{
		{codes.Unavailable, true, 3}, {codes.Unavailable, false, 1}, {codes.PermissionDenied, true, 1}, {codes.Unauthenticated, true, 1}, {codes.Internal, true, 1},
	} {
		calls := 0
		_, err := consumeLogStreamWithRetry(context.Background(), func(context.Context) (logStreamReceiver, error) {
			calls++
			return nil, status.Error(tc.code, "failure")
		}, tc.follow, func(*agentpb.StreamLogsResponse) {}, nil, []time.Duration{0, 0})
		if calls != tc.calls || status.Code(err) != tc.code {
			t.Fatalf("%v: calls=%d err=%v", tc, calls, err)
		}
	}
}
func TestLogRecoveryCancellationInterruptsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	_, err := consumeLogStreamWithRetry(ctx, func(context.Context) (logStreamReceiver, error) {
		calls++
		return nil, status.Error(codes.Unavailable, "lost")
	}, true, func(*agentpb.StreamLogsResponse) {}, func(string) { cancel() }, []time.Duration{time.Hour})
	if err != nil || calls != 1 {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

// The retry budget is per interruption: a follow that recovered must be able
// to recover again, however many times the connection blips over a session.
func TestLogRecoveryBudgetResetsAfterRestoredStream(t *testing.T) {
	calls := 0
	var notices []string
	open := func(context.Context) (logStreamReceiver, error) {
		calls++
		end := status.Error(codes.Unavailable, "blip")
		if calls == 5 {
			end = io.EOF
		}
		return &recoveryLogStream{frames: []*agentpb.StreamLogsResponse{{}}, err: end}, nil
	}
	_, err := consumeLogStreamWithRetry(context.Background(), open, true, func(*agentpb.StreamLogsResponse) {},
		func(s string) { notices = append(notices, s) }, []time.Duration{0, 0})
	if err != nil || calls != 5 {
		t.Fatalf("recovered streams must not exhaust the budget: calls=%d err=%v", calls, err)
	}
	for _, n := range notices {
		if strings.Contains(n, "(2/2)") {
			t.Fatalf("an interruption after a restored stream counted against the previous one: %q", notices)
		}
	}
}

// A quiet app sends no frames after a reconnect; a stream that stayed open
// long enough still proves the connection recovered.
func TestLogRecoveryBudgetResetsAfterStableQuietStream(t *testing.T) {
	old := logRetryStableAfter
	logRetryStableAfter = time.Millisecond
	t.Cleanup(func() { logRetryStableAfter = old })
	calls := 0
	open := func(context.Context) (logStreamReceiver, error) {
		calls++
		end := status.Error(codes.Unavailable, "blip")
		if calls == 5 {
			end = io.EOF
		}
		return &slowLogStream{wait: 10 * time.Millisecond, err: end}, nil
	}
	_, err := consumeLogStreamWithRetry(context.Background(), open, true, func(*agentpb.StreamLogsResponse) {}, nil, []time.Duration{0, 0})
	if err != nil || calls != 5 {
		t.Fatalf("stable quiet streams must not exhaust the budget: calls=%d err=%v", calls, err)
	}
}

type slowLogStream struct {
	wait time.Duration
	err  error
}

func (s *slowLogStream) Recv() (*agentpb.StreamLogsResponse, error) {
	time.Sleep(s.wait)
	return nil, s.err
}

// Ctrl-C while the stream is opening is a cancellation, not an empty replay:
// --no-follow must not print the "No log history received" hint for it.
func TestLogRecoveryReportsCancellationDuringOpen(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	res, err := consumeLogStreamWithRetry(ctx, func(context.Context) (logStreamReceiver, error) {
		cancel()
		return nil, status.Error(codes.Canceled, "context canceled")
	}, false, func(*agentpb.StreamLogsResponse) {}, nil, nil)
	if err != nil || res.end != replayEndedCancelled {
		t.Fatalf("end=%d err=%v", res.end, err)
	}
	if hint := noFollowHint(res, 50); hint != "" {
		t.Fatalf("cancelled run printed a hint: %q", hint)
	}
}

func TestLogRecoveryNamesAFailedOpen(t *testing.T) {
	_, err := consumeLogStreamWithRetry(context.Background(), func(context.Context) (logStreamReceiver, error) {
		return nil, status.Error(codes.PermissionDenied, "not allowed")
	}, true, func(*agentpb.StreamLogsResponse) {}, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "starting log stream") || status.Code(err) != codes.PermissionDenied {
		t.Fatalf("open failure lost its context or code: %v", err)
	}
}
