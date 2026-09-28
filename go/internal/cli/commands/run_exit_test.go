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
	onList      func(call int) // after the call-th ListContainers (1-based) is served
}

func (c *scriptedContainerClient) ListContainers(context.Context, *agentpb.ListContainersRequest, ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.ListContainersResponse], error) {
	c.mu.Lock()
	if c.listErr != nil {
		c.mu.Unlock()
		return nil, c.listErr
	}
	var snap *agentpb.AppContainer
	if n := len(c.snapshots); n > 0 {
		snap = c.snapshots[min(c.listCalls, n-1)]
	}
	c.listCalls++
	call, onList := c.listCalls, c.onList
	c.mu.Unlock()
	if onList != nil {
		onList(call)
	}
	return &fakeListContainersStream{resp: &agentpb.ListContainersResponse{Container: snap}}, nil
}

func (c *scriptedContainerClient) lists() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.listCalls
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
	err := followExistingContainer(ctx, &grpcclient.AgentConnection{ContainerService: fake, TelemetryService: telemetry}, &appconfig.AppConfig{AppID: "app"}, runOptions{}, appBaseline{})
	if got := ErrorClass(err); got != "app_crashed" {
		t.Fatalf("class = %q (err %v), want app_crashed", got, err)
	}
}

// shortenReplaceConfirm shrinks the window a run spends confirming a SIGKILL
// exit, so the tests that wait it out stay fast.
func shortenReplaceConfirm(t *testing.T, window time.Duration) {
	t.Helper()
	origWindow, origPoll := appReplaceConfirmWindow, appReplaceConfirmPoll
	appReplaceConfirmWindow, appReplaceConfirmPoll = window, 5*time.Millisecond
	t.Cleanup(func() { appReplaceConfirmWindow, appReplaceConfirmPoll = origWindow, origPoll })
}

const sigkillTestWindow = 200 * time.Millisecond

// sigkillExitCases script what the agent reports after an attached or
// followed app stopped. The agent's replace path SIGKILLs the old task, which
// it records as exit code 137 / "crashed" — the same record a genuine SIGKILL
// leaves — so a 137 record is re-checked for a short window: a fresh app
// (running with failure_count 0, created but not started, or gone) means
// another deployment replaced it; a restart by the restart policy or the same
// record for the whole window means it crashed. Any other exit is decided at
// once, with no further ListContainers call.
var sigkillExitCases = []struct {
	name      string
	snapshots []*agentpb.AppContainer
	crash     string        // the error's expected exit detail; "" means the run succeeds
	lists     int           // exact ListContainers calls, 0 = more than two
	minTime   time.Duration // the least the decision may take
}{
	{name: "replaced by a running app", lists: 2, snapshots: []*agentpb.AppContainer{
		appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "crashed", 0),
		appSnapshot("app", agentpb.AppRunningState_RUNNING, 0, "", 0),
	}},
	{name: "replaced and no longer reported", lists: 2, snapshots: []*agentpb.AppContainer{
		appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "crashed", 0),
		nil,
	}},
	{name: "replaced by an app not started yet", lists: 3, snapshots: []*agentpb.AppContainer{
		appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "crashed", 0),
		appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "crashed", 0),
		appSnapshot("app", agentpb.AppRunningState_STOPPED, 0, "", 0),
	}},
	{name: "SIGKILL record stays for the window", crash: "exit code 137", minTime: sigkillTestWindow, snapshots: []*agentpb.AppContainer{
		appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "crashed", 0),
	}},
	// While the agent replaces the app it can list the killed task as
	// crash-looping: its restart decision ignores the replace in progress.
	{name: "replaced while the killed task is listed as crash-looping", lists: 2, snapshots: []*agentpb.AppContainer{
		appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 137, "crashed", 1),
		appSnapshot("app", agentpb.AppRunningState_RUNNING, 0, "", 0),
	}},
	{name: "crash-looping SIGKILL record stays for the window", crash: "exit code 137", minTime: sigkillTestWindow, snapshots: []*agentpb.AppContainer{
		appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 137, "crashed", 1),
	}},
	{name: "restarted by the restart policy", crash: "exit code 137", lists: 2, snapshots: []*agentpb.AppContainer{
		appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "crashed", 0),
		appSnapshot("app", agentpb.AppRunningState_RUNNING, 0, "", 1),
	}},
	{name: "other exit code is not re-checked", crash: "exit code 3", lists: 1, snapshots: []*agentpb.AppContainer{
		appSnapshot("app", agentpb.AppRunningState_STOPPED, 3, "crashed", 0),
		appSnapshot("app", agentpb.AppRunningState_RUNNING, 0, "", 0),
	}},
	{name: "OOM kill is not re-checked", crash: `"oom_killed"`, lists: 1, snapshots: []*agentpb.AppContainer{
		appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "oom_killed", 0),
		appSnapshot("app", agentpb.AppRunningState_RUNNING, 0, "", 0),
	}},
}

func checkSIGKILLExit(t *testing.T, fake *scriptedContainerClient, crash string, lists int, minTime, took time.Duration, out string, err error) {
	t.Helper()
	if crash == "" {
		if err != nil {
			t.Fatalf("returned %v, want nil", err)
		}
		if !strings.Contains(out, "Application app was replaced by another deployment.") {
			t.Errorf("missing the replaced notice: %q", out)
		}
		if strings.Contains(out, "stopped.") {
			t.Errorf("a replaced app also reported as stopped: %q", out)
		}
	} else {
		if got := ErrorClass(err); got != "app_crashed" {
			t.Fatalf("class = %q (err %v), want app_crashed", got, err)
		}
		if !strings.Contains(err.Error(), crash) {
			t.Errorf("error %q does not name %s", err, crash)
		}
		if strings.Contains(out, "replaced") {
			t.Errorf("a crash reported as a replacement: %q", out)
		}
	}
	switch got := fake.lists(); {
	case lists == 0 && got <= 2:
		t.Errorf("ListContainers calls = %d, want the window's repeated polls", got)
	case lists != 0 && got != lists:
		t.Errorf("ListContainers calls = %d, want %d", got, lists)
	}
	if took < minTime {
		t.Errorf("decided after %s, before the %s window ended", took, minTime)
	}
	if stops := fake.stops(); len(stops) != 0 {
		t.Errorf("stopped %v; the run's app had already exited", stops)
	}
}

func TestAttachedExitOutcome_ConfirmsSIGKILLBeforeReportingACrash(t *testing.T) {
	shortenReplaceConfirm(t, sigkillTestWindow)
	for _, tc := range sigkillExitCases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &scriptedContainerClient{snapshots: tc.snapshots}
			conn := &grpcclient.AgentConnection{ContainerService: fake}
			var err error
			start := time.Now()
			out := captureStderr(t, func() {
				err = attachedExitOutcome(context.Background(), conn, &appconfig.AppConfig{AppID: "app"})
			})
			checkSIGKILLExit(t, fake, tc.crash, tc.lists, tc.minTime, time.Since(start), out, err)
		})
	}
}

// The attached chunk-diff path reaches attachedExitOutcome when the agent
// closes the output stream — which it does when another deployment kills the
// task to replace it.
func TestStreamRunContainer_AppReplacedByAnotherDeployIsNotACrash(t *testing.T) {
	shortenReplaceConfirm(t, sigkillTestWindow)
	fake := &scriptedContainerClient{snapshots: sigkillExitCases[0].snapshots}
	conn := &grpcclient.AgentConnection{Host: "127.0.0.1", ContainerService: fake}
	out := captureStderr(t, func() {
		if err := streamRunContainerWithStarted(context.Background(), conn, &deploymentAckStream{remaining: 1, err: io.EOF}, &appconfig.AppConfig{AppID: "app"}, runOptions{}, nil); err != nil {
			t.Fatalf("replaced app returned %v, want nil", err)
		}
	})
	if !strings.Contains(out, "replaced by another deployment") {
		t.Fatalf("missing the replaced notice: %q", out)
	}
}

func TestFollowExistingContainer_ConfirmsSIGKILLBeforeReportingACrash(t *testing.T) {
	shortenReplaceConfirm(t, sigkillTestWindow)
	for _, tc := range sigkillExitCases {
		t.Run(tc.name, func(t *testing.T) {
			// The follow loop's first state poll comes a second in, so time
			// the decision from that poll rather than from the call.
			var firstPoll time.Time
			fake := &scriptedContainerClient{snapshots: tc.snapshots, onList: func(call int) {
				if call == 1 {
					firstPoll = time.Now()
				}
			}}
			telemetry := &runLogsFakeClient{stream: &runLogsFakeStream{response: testRunLogsResponse(false, &logspb.ScopeLogs{})}}
			conn := &grpcclient.AgentConnection{ContainerService: fake, TelemetryService: telemetry}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var err error
			var took time.Duration
			out := captureStderr(t, func() {
				err = followExistingContainer(ctx, conn, &appconfig.AppConfig{AppID: "app"}, runOptions{}, appBaseline{})
				took = time.Since(firstPoll)
			})
			checkSIGKILLExit(t, fake, tc.crash, tc.lists, tc.minTime, took, out, err)
		})
	}
}

// A followed app may have been restarted before the follow began (after a
// device reboot, say), so its failure_count starts above 0. Each restart by
// the restart policy raises the count and a replacement's start resets it, so
// a SIGKILL record is judged against the count the follow started from, and a
// different app_version is another deployment's outright.
func TestFollowExistingContainer_JudgesAgainstTheFollowedBaseline(t *testing.T) {
	shortenReplaceConfirm(t, sigkillTestWindow)
	base := appBaseline{failures: 2, version: "1.0"}
	withVersion := func(c *agentpb.AppContainer, version string) *agentpb.AppContainer {
		c.AppVersion = version
		return c
	}
	for _, tc := range []struct {
		name      string
		snapshots []*agentpb.AppContainer
		crash     string
		lists     int
		minTime   time.Duration
	}{
		{name: "replaced while the killed task is listed as crash-looping", lists: 2, snapshots: []*agentpb.AppContainer{
			appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 137, "crashed", 3),
			appSnapshot("app", agentpb.AppRunningState_RUNNING, 0, "", 0),
		}},
		{name: "replaced after a SIGKILL stop", lists: 2, snapshots: []*agentpb.AppContainer{
			appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "crashed", 2),
			appSnapshot("app", agentpb.AppRunningState_RUNNING, 0, "", 0),
		}},
		{name: "replaced by an app its restart policy already restarted", lists: 2, snapshots: []*agentpb.AppContainer{
			appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "crashed", 2),
			appSnapshot("app", agentpb.AppRunningState_RUNNING, 0, "", 1),
		}},
		{name: "replaced by another version", lists: 2, snapshots: []*agentpb.AppContainer{
			withVersion(appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "crashed", 2), "1.0"),
			withVersion(appSnapshot("app", agentpb.AppRunningState_RUNNING, 0, "", 5), "2.0"),
		}},
		{name: "another version's crash is not this app's", lists: 1, snapshots: []*agentpb.AppContainer{
			withVersion(appSnapshot("app", agentpb.AppRunningState_STOPPED, 3, "crashed", 0), "2.0"),
		}},
		{name: "restarted by the restart policy", crash: "exit code 137", lists: 2, snapshots: []*agentpb.AppContainer{
			appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "crashed", 2),
			appSnapshot("app", agentpb.AppRunningState_RUNNING, 0, "", 3),
		}},
		{name: "crash-looping SIGKILL record stays for the window", crash: "exit code 137", minTime: sigkillTestWindow, snapshots: []*agentpb.AppContainer{
			appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 137, "crashed", 3),
		}},
		{name: "other exit code is not re-checked", crash: "exit code 2", lists: 1, snapshots: []*agentpb.AppContainer{
			withVersion(appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 2, "crashed", 3), "1.0"),
			appSnapshot("app", agentpb.AppRunningState_RUNNING, 0, "", 0),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var firstPoll time.Time
			fake := &scriptedContainerClient{snapshots: tc.snapshots, onList: func(call int) {
				if call == 1 {
					firstPoll = time.Now()
				}
			}}
			telemetry := &runLogsFakeClient{stream: &runLogsFakeStream{response: testRunLogsResponse(false, &logspb.ScopeLogs{})}}
			conn := &grpcclient.AgentConnection{ContainerService: fake, TelemetryService: telemetry}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var err error
			var took time.Duration
			out := captureStderr(t, func() {
				err = followExistingContainer(ctx, conn, &appconfig.AppConfig{AppID: "app"}, runOptions{}, base)
				took = time.Since(firstPoll)
			})
			checkSIGKILLExit(t, fake, tc.crash, tc.lists, tc.minTime, took, out, err)
		})
	}
}

// The no-change fast path follows a running app it did not start, and its
// own lookup is the follow's baseline: taken before the follow starts, so a
// replacement cannot be mistaken for it.
func TestTryDeployFastPath_FollowIsBaselinedOnTheFastPathLookup(t *testing.T) {
	shortenReplaceConfirm(t, sigkillTestWindow)
	isolateFingerprintCache(t)
	cfg := &appconfig.AppConfig{AppID: "app"}
	saveDeployFingerprint(cfg.AppID, "device", deployFingerprint{InputHash: "inputs", LayerDiffIDs: []string{"layer"}})
	fake := &scriptedContainerClient{snapshots: []*agentpb.AppContainer{
		appSnapshot("app", agentpb.AppRunningState_RUNNING, 0, "", 2),          // the fast path's lookup
		appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "crashed", 2), // the follow's first poll
		appSnapshot("app", agentpb.AppRunningState_RUNNING, 0, "", 1),          // a fresh app, restarted once
	}}
	telemetry := &runLogsFakeClient{stream: &runLogsFakeStream{response: testRunLogsResponse(false, &logspb.ScopeLogs{})}}
	conn := &grpcclient.AgentConnection{ContainerService: fastPathScriptedClient{fake}, TelemetryService: telemetry}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var done bool
	var err error
	out := captureStderr(t, func() {
		done, err = tryDeployFastPath(ctx, conn, cfg, "device", "inputs", runOptions{})
	})
	if !done || err != nil {
		t.Fatalf("done=%v err=%v, want the follow to end on the replacement", done, err)
	}
	if !strings.Contains(out, "Application app was replaced by another deployment.") {
		t.Fatalf("missing the replaced notice: %q", out)
	}
}

// Ctrl-C (or SIGTERM) while the run is confirming a SIGKILL record ends it as
// a cancellation, never a success, and stops nothing: the app on the device
// may already be the other deployment's.
func TestAttachedExitOutcome_InterruptWhileConfirmingSIGKILL(t *testing.T) {
	shortenReplaceConfirm(t, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &scriptedContainerClient{
		snapshots: []*agentpb.AppContainer{appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "crashed", 0)},
		onList: func(call int) {
			if call == 2 {
				cancel()
			}
		},
	}
	conn := &grpcclient.AgentConnection{ContainerService: fake}
	start := time.Now()
	err := attachedExitOutcome(ctx, conn, &appconfig.AppConfig{AppID: "app"})
	if !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("attachedExitOutcome = %v, want ErrUserCancelled", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("took %s to notice the interrupt", took)
	}
	if stops := fake.stops(); len(stops) != 0 {
		t.Fatalf("stopped %v while the app may belong to another deployment", stops)
	}
}

func TestFollowExistingContainer_InterruptWhileConfirmingSIGKILL(t *testing.T) {
	shortenReplaceConfirm(t, 10*time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := &scriptedContainerClient{
		snapshots: []*agentpb.AppContainer{appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "crashed", 0)},
		onList: func(call int) {
			if call == 2 {
				cancel()
			}
		},
	}
	telemetry := &runLogsFakeClient{stream: &runLogsFakeStream{response: testRunLogsResponse(false, &logspb.ScopeLogs{})}}
	conn := &grpcclient.AgentConnection{ContainerService: fake, TelemetryService: telemetry}
	start := time.Now()
	err := followExistingContainer(ctx, conn, &appconfig.AppConfig{AppID: "app"}, runOptions{}, appBaseline{})
	if !errors.Is(err, ErrUserCancelled) {
		t.Fatalf("followExistingContainer = %v, want ErrUserCancelled", err)
	}
	if took := time.Since(start); took > 6*time.Second {
		t.Fatalf("took %s to notice the interrupt", took)
	}
	if stops := fake.stops(); len(stops) != 0 {
		t.Fatalf("stopped %v; following never stops the app", stops)
	}
}
