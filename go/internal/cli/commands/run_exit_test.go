package commands

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
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

// scriptedContainerClient is a WendyContainerServiceClient whose
// ListContainers walks a script of snapshots (repeating the last one), whose
// StartContainer hands out a canned stream (AttachContainer reports an old
// agent, so the registry path falls back to it), and whose StopContainer
// records what it was asked to stop. That is enough to drive every attached
// and detached run path against a device whose app state changes over time.
type scriptedContainerClient struct {
	agentpb.WendyContainerServiceClient // embedded nil — satisfies the interface

	mu          sync.Mutex
	snapshots   []*agentpb.AppContainer
	listErr     error
	listCalls   int
	stream      grpc.ServerStreamingClient[agentpb.RunContainerLayersResponse]
	stopped     []string
	stopCtxErrs []error
	stopErr     error
	onStop      func()
}

func (c *scriptedContainerClient) ListContainers(context.Context, *agentpb.ListContainersRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.ListContainersResponse], error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.listErr != nil {
		return nil, c.listErr
	}
	var snap *agentpb.AppContainer
	if n := len(c.snapshots); n > 0 {
		snap = c.snapshots[min(c.listCalls, n-1)]
	}
	c.listCalls++
	return &fakeListContainersStream{resp: &agentpb.ListContainersResponse{Container: snap}}, nil
}

func (c *scriptedContainerClient) AttachContainer(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[agentpb.AttachContainerRequest, agentpb.RunContainerLayersResponse], error) {
	return nil, status.Error(codes.Unimplemented, "scripted agent predates AttachContainer")
}

func (c *scriptedContainerClient) StartContainer(context.Context, *agentpb.StartContainerRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.RunContainerLayersResponse], error) {
	return c.stream, nil
}

func (c *scriptedContainerClient) StopContainer(ctx context.Context, req *agentpb.StopContainerRequest, _ ...grpc.CallOption) (*agentpb.StopContainerResponse, error) {
	c.mu.Lock()
	c.stopped = append(c.stopped, req.GetAppName())
	c.stopCtxErrs = append(c.stopCtxErrs, ctx.Err())
	onStop := c.onStop
	c.mu.Unlock()
	if onStop != nil {
		onStop()
	}
	return &agentpb.StopContainerResponse{}, c.stopErr
}

func (c *scriptedContainerClient) stops() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.stopped...)
}

func appSnapshot(name string, state agentpb.AppRunningState, exitCode int32, reason string, failures uint32) *agentpb.AppContainer {
	return &agentpb.AppContainer{AppName: name, RunningState: state, ExitCode: exitCode, TerminationReason: reason, FailureCount: failures}
}

func TestAppExitFailure(t *testing.T) {
	for _, tc := range []struct {
		name  string
		c     *agentpb.AppContainer
		crash bool
		want  []string
	}{
		{name: "not reported", c: nil},
		{name: "running", c: appSnapshot("app", agentpb.AppRunningState_RUNNING, 0, "", 0)},
		{name: "old agent records no exit", c: appSnapshot("app", agentpb.AppRunningState_STOPPED, 0, "", 0)},
		{name: "clean exit", c: appSnapshot("app", agentpb.AppRunningState_STOPPED, 0, "exited", 0)},
		{name: "crash", c: appSnapshot("app", agentpb.AppRunningState_STOPPED, 3, "crashed", 0), crash: true,
			want: []string{"app app stopped unexpectedly", "exit code 3", `termination reason "crashed"`, "wendy device logs --app app"}},
		{name: "oom", c: appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "oom_killed", 0), crash: true,
			want: []string{"exit code 137", `"oom_killed"`}},
		{name: "never started", c: appSnapshot("app", agentpb.AppRunningState_STOPPED, -1, "start_failed", 0), crash: true,
			want: []string{"exit code -1", `"start_failed"`}},
		{name: "crash loop", c: appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 2, "crashed", 4), crash: true,
			want: []string{"app app is crash-looping", "exit code 2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := appExitFailure("app", tc.c)
			if !tc.crash {
				if err != nil {
					t.Fatalf("appExitFailure = %v, want nil", err)
				}
				return
			}
			if got := ErrorClass(err); got != "app_crashed" {
				t.Fatalf("class = %q (err %v), want app_crashed", got, err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q is missing %q", err, want)
				}
			}
		})
	}
}

func TestStreamRunContainer_AttachedCrashExitsNonZero(t *testing.T) {
	fake := &scriptedContainerClient{snapshots: []*agentpb.AppContainer{appSnapshot("crash-app", agentpb.AppRunningState_STOPPED, 3, "crashed", 0)}}
	conn := &grpcclient.AgentConnection{Host: "127.0.0.1", ContainerService: fake}
	err := streamRunContainerWithStarted(context.Background(), conn, &deploymentAckStream{remaining: 1, err: io.EOF}, &appconfig.AppConfig{AppID: "crash-app"}, runOptions{}, nil)
	if got := ErrorClass(err); got != "app_crashed" {
		t.Fatalf("class = %q (err %v), want app_crashed", got, err)
	}
	if !strings.Contains(err.Error(), "exit code 3") {
		t.Fatalf("error %q does not name the exit code", err)
	}
}

func TestStreamRunContainer_AttachedCleanExitStaysSuccessful(t *testing.T) {
	fake := &scriptedContainerClient{snapshots: []*agentpb.AppContainer{appSnapshot("done-app", agentpb.AppRunningState_STOPPED, 0, "exited", 0)}}
	conn := &grpcclient.AgentConnection{Host: "127.0.0.1", ContainerService: fake}
	out := captureStderr(t, func() {
		if err := streamRunContainerWithStarted(context.Background(), conn, &deploymentAckStream{remaining: 1, err: io.EOF}, &appconfig.AppConfig{AppID: "done-app"}, runOptions{}, nil); err != nil {
			t.Fatalf("clean exit returned %v", err)
		}
	})
	if !strings.Contains(out, "stopped") {
		t.Fatalf("missing the stopped line: %q", out)
	}
}

// A crash-looping app the monitor restarted before the post-EOF lookup reads
// as RUNNING with its exit labels hidden: the CLI cannot prove a crash, so it
// says so and keeps the historical success rather than guessing.
func TestAttachedExitOutcome_RestartedBeforeLookupIsNotAFailure(t *testing.T) {
	fake := &scriptedContainerClient{snapshots: []*agentpb.AppContainer{appSnapshot("loop-app", agentpb.AppRunningState_RUNNING, 0, "", 1)}}
	conn := &grpcclient.AgentConnection{ContainerService: fake}
	out := captureStderr(t, func() {
		if err := attachedExitOutcome(context.Background(), conn, &appconfig.AppConfig{AppID: "loop-app"}); err != nil {
			t.Fatalf("attachedExitOutcome = %v, want nil", err)
		}
	})
	if !strings.Contains(out, "restarted by its restart policy") {
		t.Fatalf("missing restart notice: %q", out)
	}
}

func TestAttachedExitOutcome_UnreadableStatusKeepsSuccess(t *testing.T) {
	fake := &scriptedContainerClient{listErr: errors.New("agent busy")}
	conn := &grpcclient.AgentConnection{ContainerService: fake}
	if err := attachedExitOutcome(context.Background(), conn, &appconfig.AppConfig{AppID: "app"}); err != nil {
		t.Fatalf("attachedExitOutcome = %v, want nil", err)
	}
}

func TestStartExistingContainer_AttachedCrashExitsNonZero(t *testing.T) {
	fake := &scriptedContainerClient{
		snapshots: []*agentpb.AppContainer{appSnapshot("crash-app", agentpb.AppRunningState_STOPPED, 3, "crashed", 0)},
		stream:    &deploymentAckStream{remaining: 1, err: io.EOF},
	}
	conn := &grpcclient.AgentConnection{Host: "127.0.0.1", ContainerService: fake}
	err := startExistingContainer(context.Background(), conn, &appconfig.AppConfig{AppID: "crash-app"}, runOptions{})
	if got := ErrorClass(err); got != "app_crashed" {
		t.Fatalf("class = %q (err %v), want app_crashed", got, err)
	}
}

func TestFollowExistingContainer_CrashLoopExitsNonZero(t *testing.T) {
	fake := &scriptedContainerClient{snapshots: []*agentpb.AppContainer{
		appSnapshot("app", agentpb.AppRunningState_RUNNING, 0, "", 0),
		appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 2, "crashed", 1),
	}}
	telemetry := &runLogsFakeClient{stream: &runLogsFakeStream{response: testRunLogsResponse(false, &logspb.ScopeLogs{})}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := followExistingContainer(ctx, &grpcclient.AgentConnection{ContainerService: fake, TelemetryService: telemetry}, &appconfig.AppConfig{AppID: "app"}, runOptions{})
	if got := ErrorClass(err); got != "app_crashed" {
		t.Fatalf("class = %q (err %v), want app_crashed", got, err)
	}
}
