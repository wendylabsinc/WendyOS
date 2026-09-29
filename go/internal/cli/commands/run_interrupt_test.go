package commands

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	logspb "go.opentelemetry.io/proto/otlp/logs/v1"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// interruptOnSecondRecvStream acknowledges Started, then on the next Recv
// calls interrupt (a Ctrl-C or SIGTERM stand-in) and ends the way a real
// stream does once its context is cancelled.
type interruptOnSecondRecvStream struct {
	grpc.ServerStreamingClient[agentpb.RunContainerLayersResponse]
	ctx       context.Context
	interrupt func()
	calls     int
}

func (s *interruptOnSecondRecvStream) Recv() (*agentpb.RunContainerLayersResponse, error) {
	s.calls++
	if s.calls == 1 {
		return &agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_Started_{Started: &agentpb.RunContainerLayersResponse_Started{}}}, nil
	}
	s.interrupt()
	<-s.ctx.Done()
	return nil, status.Error(codes.Canceled, "context canceled")
}

func withInterruptNotes(ctx context.Context) (context.Context, *runInterruptNotes) {
	notes := &runInterruptNotes{}
	return context.WithValue(ctx, runInterruptNotesKey{}, notes), notes
}

func TestStreamRunContainer_InterruptStopsAttachedApp(t *testing.T) {
	fake := &scriptedContainerClient{}
	conn := &grpcclient.AgentConnection{Host: "127.0.0.1", ContainerService: fake}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, notes := withInterruptNotes(ctx)
	stream := &interruptOnSecondRecvStream{ctx: ctx, interrupt: cancel}
	err := streamRunContainerWithStarted(ctx, conn, stream, &appconfig.AppConfig{AppID: "int-app"}, runOptions{}, nil)
	if !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("interrupted attached run returned %v, want ErrUserCancelled (exit 0, reported as a cancellation)", err)
	}
	if got := fake.stops(); len(got) != 1 || got[0] != "int-app" {
		t.Fatalf("StopContainer calls = %v, want [int-app]", got)
	}
	if fake.stopCtxErrs[0] != nil {
		t.Fatalf("StopContainer ran on a cancelled context: %v", fake.stopCtxErrs[0])
	}
	if notes.outcome != interruptedAppStopped || notes.app != "int-app" {
		t.Fatalf("notes = %+v, want int-app stopped", notes)
	}
}

func TestStartExistingContainer_InterruptStopsAttachedApp(t *testing.T) {
	fake := &scriptedContainerClient{}
	conn := &grpcclient.AgentConnection{Host: "127.0.0.1", ContainerService: fake}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, notes := withInterruptNotes(ctx)
	fake.stream = &interruptOnSecondRecvStream{ctx: ctx, interrupt: cancel}
	err := startExistingContainer(ctx, conn, &appconfig.AppConfig{AppID: "int-app"}, runOptions{})
	if !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("interrupted attached run returned %v, want ErrUserCancelled (exit 0, reported as a cancellation)", err)
	}
	if got := fake.stops(); len(got) != 1 || got[0] != "int-app" {
		t.Fatalf("StopContainer calls = %v, want [int-app]", got)
	}
	if notes.outcome != interruptedAppStopped {
		t.Fatalf("notes = %+v, want stopped", notes)
	}
}

// The no-change fast path only follows an app it did not start; an interrupt
// leaves it running, exactly as before, and says so on SIGTERM.
func TestFollowExistingContainer_InterruptLeavesAppRunning(t *testing.T) {
	fake := &scriptedContainerClient{snapshots: []*agentpb.AppContainer{appSnapshot("app", agentpb.AppRunningState_RUNNING, 0, "", 0)}}
	telemetry := &runLogsFakeClient{stream: &runLogsFakeStream{response: testRunLogsResponse(false, &logspb.ScopeLogs{})}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	time.AfterFunc(1500*time.Millisecond, cancel)
	ctx, notes := withInterruptNotes(ctx)
	err := followExistingContainer(ctx, &grpcclient.AgentConnection{ContainerService: fake, TelemetryService: telemetry}, &appconfig.AppConfig{AppID: "app"}, runOptions{}, appBaseline{})
	if !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("followExistingContainer = %v, want ErrUserCancelled", err)
	}
	if len(fake.stops()) != 0 {
		t.Fatalf("follow stopped an app it did not start: %v", fake.stops())
	}
	if notes.outcome != interruptedAppLeftRunning {
		t.Fatalf("notes = %+v, want left running", notes)
	}
}

// End to end through the signal wrapper: SIGTERM during an attached chunk-diff
// run stops the app on the device and exits non-zero, naming the app.
func TestRunWithInterruptChannel_SIGTERMStopsAttachedApp(t *testing.T) {
	fake := &scriptedContainerClient{}
	conn := &grpcclient.AgentConnection{Host: "127.0.0.1", ContainerService: fake}
	signals := make(chan os.Signal, 1)
	err := runWithInterruptChannel(context.Background(), signals, func(ctx context.Context) error {
		stream := &interruptOnSecondRecvStream{ctx: ctx, interrupt: func() { signals <- syscall.SIGTERM }}
		return streamRunContainerWithStarted(ctx, conn, stream, &appconfig.AppConfig{AppID: "sig-app"}, runOptions{}, nil)
	})
	if got := ErrorClass(err); got != "terminated" {
		t.Fatalf("class = %q (err %v), want terminated", got, err)
	}
	if !strings.Contains(err.Error(), "app sig-app was stopped") {
		t.Fatalf("message %q does not say the app was stopped", err)
	}
	if got := fake.stops(); len(got) != 1 || got[0] != "sig-app" {
		t.Fatalf("StopContainer calls = %v, want [sig-app]", got)
	}
}

// End to end through the signal wrapper: SIGINT (Ctrl-C) during an attached
// chunk-diff run stops the app on the device but is still reported as a clean
// user cancellation, never as errTerminated.
func TestRunWithInterruptChannel_SIGINTStopsAttachedApp(t *testing.T) {
	fake := &scriptedContainerClient{}
	conn := &grpcclient.AgentConnection{Host: "127.0.0.1", ContainerService: fake}
	signals := make(chan os.Signal, 1)
	err := runWithInterruptChannel(context.Background(), signals, func(ctx context.Context) error {
		stream := &interruptOnSecondRecvStream{ctx: ctx, interrupt: func() { signals <- os.Interrupt }}
		return streamRunContainerWithStarted(ctx, conn, stream, &appconfig.AppConfig{AppID: "sig-app"}, runOptions{}, nil)
	})
	if !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("SIGINT returned %v, want ErrUserCancelled", err)
	}
	if got := ErrorClass(err); got == "terminated" {
		t.Fatalf("class = %q, want anything but terminated for SIGINT", got)
	}
	if got := fake.stops(); len(got) != 1 || got[0] != "sig-app" {
		t.Fatalf("StopContainer calls = %v, want [sig-app]", got)
	}
}

// Stop failures surface on SIGTERM, since the app may still be running.
func TestStopInterruptedApp_ReportsStopFailure(t *testing.T) {
	fake := &scriptedContainerClient{stopErr: status.Error(codes.Unavailable, "device went away")}
	ctx, notes := withInterruptNotes(context.Background())
	stopInterruptedApp(ctx, &grpcclient.AgentConnection{ContainerService: fake}, &appconfig.AppConfig{AppID: "app"})
	if notes.outcome != interruptedAppStopFailed || notes.stopErr == nil {
		t.Fatalf("notes = %+v, want stop failure", notes)
	}
	if msg := notes.terminatedError().Error(); !strings.Contains(msg, "stopping app app failed") {
		t.Fatalf("message = %q", msg)
	}
}
