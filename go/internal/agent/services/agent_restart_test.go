package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// restartRecorder stands in for what an agent restart touches: the stop step
// main injects (Wendy Net) and the process exit.
type restartRecorder struct {
	mu     sync.Mutex
	events []string
	exited chan int
	// stopCtx is the context the stop step was given.
	stopCtx context.Context
}

func newRestartRecorder() *restartRecorder {
	return &restartRecorder{exited: make(chan int, 1)}
}

func (r *restartRecorder) record(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *restartRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.events...)
}

func (r *restartRecorder) stop(ctx context.Context) error {
	r.mu.Lock()
	r.stopCtx = ctx
	r.mu.Unlock()
	r.record("stop")
	return nil
}

func (r *restartRecorder) exit(code int) {
	r.record("exit")
	r.exited <- code
}

// waitExit returns the exit code the restart asked for.
func (r *restartRecorder) waitExit(t *testing.T) int {
	t.Helper()
	select {
	case code := <-r.exited:
		return code
	case <-time.After(5 * time.Second):
		t.Fatalf("the agent never exited to restart; saw %v", r.seen())
		return -1
	}
}

// An update over the v1 stream: the reply reaches the client and the stream
// ends before anything is stopped, then the stop step runs, then the exit.
func TestUpdateAgentRestartStopsAfterTheReplyAndThenExits(t *testing.T) {
	rec := newRestartRecorder()
	replyDelivered := make(chan struct{})
	var waited time.Duration
	restart := &AgentRestart{
		Logger: zap.NewNop(),
		Stop:   rec.stop,
		Exit:   rec.exit,
		// The reply delay ends when the test has seen the whole reply.
		sleep: func(d time.Duration) { waited = d; <-replyDelivered },
	}

	execPath := filepath.Join(t.TempDir(), "wendy-agent")
	if err := os.WriteFile(execPath, []byte("original-binary-contents"), 0o755); err != nil {
		t.Fatal(err)
	}
	client, cleanup := startAgentServer(t, &mockNetworkManager{}, &mockHardwareDiscoverer{}, &mockBluetoothManager{},
		func(svc *AgentService) {
			svc.execPathResolver = func() (string, os.FileMode, error) { return execPath, 0o755, nil }
			svc.SetRestartFunc(restart.Schedule)
		})
	defer cleanup()

	binaryData := []byte("new-agent-binary-contents-for-restart-test")
	sum := sha256.Sum256(binaryData)
	stream, err := client.UpdateAgent(context.Background())
	if err != nil {
		t.Fatalf("UpdateAgent: %v", err)
	}
	if err := sendV1UpdateStream(t, stream, binaryData, hex.EncodeToString(sum[:]), nil); err != nil {
		t.Fatalf("the update was not acknowledged: %v", err)
	}
	if _, err := stream.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("the update stream ended with %v, want a clean end", err)
	}
	if got := rec.seen(); len(got) != 0 {
		t.Fatalf("%v ran while the reply was still on its way to the client", got)
	}

	close(replyDelivered)
	if code := rec.waitExit(t); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got, want := rec.seen(), []string{"stop", "exit"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("restart steps = %v, want %v", got, want)
	}
	if waited != agentRestartAckDelay {
		t.Fatalf("reply delay = %s, want the default %s", waited, agentRestartAckDelay)
	}
	deadline, bounded := rec.stopCtx.Deadline()
	if !bounded {
		t.Fatal("the stop step ran without a deadline")
	}
	if left := time.Until(deadline); left > agentRestartStopTimeout {
		t.Fatalf("the stop step may run for another %s, want at most %s", left, agentRestartStopTimeout)
	}
}

// The v2 service restarts the same way; an online update has no reply to wait
// for, but its restart goes through the same steps.
func TestOnlineUpdateRestartStopsBeforeItExits(t *testing.T) {
	svc, _, _, _ := onlineServiceFixture(t, false)
	rec := newRestartRecorder()
	svc.SetRestartFunc((&AgentRestart{Logger: zap.NewNop(), Stop: rec.stop, Exit: rec.exit, AckDelay: time.Millisecond}).Schedule)

	if _, err := svc.StartOnlineUpdate(context.Background(), &pb.StartOnlineUpdateRequest{Version: "2.0.0"}); err != nil {
		t.Fatal(err)
	}
	if code := rec.waitExit(t); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got, want := rec.seen(), []string{"stop", "exit"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("restart steps = %v, want %v", got, want)
	}
}

// A stop step that never returns, and ignores its context, must not keep the
// old binary running: the agent exits when the step's time is up.
func TestAgentRestartExitsWhenStopHangs(t *testing.T) {
	rec := newRestartRecorder()
	release := make(chan struct{})
	defer close(release)
	entered := make(chan context.Context, 1)
	restart := &AgentRestart{
		Logger: zap.NewNop(),
		Stop: func(ctx context.Context) error {
			entered <- ctx
			<-release
			return nil
		},
		Exit:        rec.exit,
		AckDelay:    time.Millisecond,
		StopTimeout: 30 * time.Millisecond,
	}

	start := time.Now()
	restart.Schedule()
	if code := rec.waitExit(t); code != 0 {
		t.Fatalf("exit code = %d, want 0: a restart that was asked for is not a failure", code)
	}
	if took := time.Since(start); took < 30*time.Millisecond {
		t.Fatalf("exited after %s, before the stop step had its %s", took, 30*time.Millisecond)
	}
	select {
	case ctx := <-entered:
		if ctx.Err() == nil {
			t.Fatal("the stop step was not told its time is up")
		}
	default:
		t.Fatal("the agent exited without running the stop step")
	}
}

func TestAgentRestartExitsWhenStopFails(t *testing.T) {
	rec := newRestartRecorder()
	restart := &AgentRestart{
		Logger:   zap.NewNop(),
		Stop:     func(ctx context.Context) error { _ = rec.stop(ctx); return errors.New("link busy") },
		Exit:     rec.exit,
		AckDelay: time.Millisecond,
	}

	restart.Schedule()
	if code := rec.waitExit(t); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got, want := rec.seen(), []string{"stop", "exit"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("restart steps = %v, want %v", got, want)
	}
}

// Without a stop step the restart is what it always was: the reply delay, then
// the exit. This is also what a service does when main gave it no restart.
func TestAgentRestartWithoutStopOnlyWaitsForTheReply(t *testing.T) {
	rec := newRestartRecorder()
	var waited []time.Duration
	restart := &AgentRestart{Exit: rec.exit, sleep: func(d time.Duration) { waited = append(waited, d) }}

	restart.Schedule()
	// A second request joins the restart under way.
	restart.Schedule()
	if code := rec.waitExit(t); code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if got, want := waited, []time.Duration{agentRestartAckDelay}; !reflect.DeepEqual(got, want) {
		t.Fatalf("waited %v before exiting, want %v", got, want)
	}
	select {
	case <-rec.exited:
		t.Fatal("the agent exited twice")
	case <-time.After(20 * time.Millisecond):
	}
}
