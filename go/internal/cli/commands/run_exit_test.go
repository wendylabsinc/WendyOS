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
	shortenReplaceConfirm(t, sigkillTestWindow) // a crash is confirmed before it is reported
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
	shortenReplaceConfirm(t, sigkillTestWindow) // a crash is confirmed before it is reported
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
	shortenReplaceConfirm(t, sigkillTestWindow) // a crash is confirmed before it is reported
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
	// The rest of the agent's replace: it deletes the killed task's
	// container (the SIGKILL record may never be listed: recording it races
	// the delete), prepares the new image while the app is not listed, and
	// creates the new container — stopped with no exit recorded, or
	// crash-looping while the old registration's count is above 0 — which it
	// starts afterwards.
	{name: "the whole replace sequence", lists: 2, snapshots: replaceSequence(killRecord(), notListed, notListed, createdStopped(), runningSnapshot(0))},
	{name: "no SIGKILL record: not listed, then running", lists: 3, snapshots: replaceSequence(notListed, notListed, runningSnapshot(0))},
	{name: "no SIGKILL record: not listed, then created", lists: 2, snapshots: replaceSequence(notListed, createdStopped(), runningSnapshot(0))},
	{name: "created, not started, then running", lists: 2, snapshots: replaceSequence(createdStopped(), runningSnapshot(0))},
	{name: "created, not started, listed as crash-looping, then running", lists: 2, snapshots: replaceSequence(createdLooping(), runningSnapshot(0))},
	// Agents with exit reporting record every crash's exit: only a
	// replacement's container that has not started is listed like this.
	{name: "crash-looping with no exit recorded for the window", minTime: sigkillTestWindow, snapshots: replaceSequence(createdLooping())},
	{name: "a crash recorded after a stop with no exit is judged by that crash", crash: "exit code 3", lists: 2, snapshots: replaceSequence(createdStopped(),
		appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 3, "crashed", 1))},
	{name: "restarted by the restart policy", crash: "exit code 137", lists: 2, snapshots: []*agentpb.AppContainer{
		appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "crashed", 0),
		appSnapshot("app", agentpb.AppRunningState_RUNNING, 0, "", 1),
	}},
	// Every recorded exit is confirmed before it is reported: the agent
	// never clears an exit it recorded, so the killed app of a replace can be
	// listed with an exit it recorded earlier (a stale one, from a crash its
	// restart policy recovered from) when the kill's own exit loses the race
	// with the delete.
	{name: "a recorded crash stays for the window", crash: "exit code 3", minTime: sigkillTestWindow, snapshots: replaceSequence(
		appSnapshot("app", agentpb.AppRunningState_STOPPED, 3, "crashed", 0))},
	{name: "a recorded crash, then a fresh app running", lists: 2, snapshots: replaceSequence(
		appSnapshot("app", agentpb.AppRunningState_STOPPED, 3, "crashed", 0), runningSnapshot(0))},
	{name: "a crash, then restarted by the restart policy", crash: "exit code 3", lists: 2, snapshots: replaceSequence(
		appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 3, "crashed", 0), runningSnapshot(1))},
	{name: "an OOM kill stays for the window", crash: `"oom_killed"`, minTime: sigkillTestWindow, snapshots: replaceSequence(
		appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "oom_killed", 0))},
	{name: "an OOM kill, then a fresh app running", lists: 2, snapshots: replaceSequence(
		appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "oom_killed", 0), runningSnapshot(0))},
	{name: "a stale recorded crash, then the rest of the replace", lists: 2, snapshots: replaceSequence(
		appSnapshot("app", agentpb.AppRunningState_STOPPED, 1, "crashed", 0), notListed, createdStopped(), runningSnapshot(0))},
	{name: "a stale recorded crash, then the new container created", lists: 2, snapshots: replaceSequence(
		appSnapshot("app", agentpb.AppRunningState_STOPPED, 1, "crashed", 0), createdStopped(), runningSnapshot(0))},
	{name: "a stale crash-loop record, then the new app running", lists: 2, snapshots: replaceSequence(
		appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 1, "crashed", 0), runningSnapshot(0))},
	{name: "a stale recorded crash, then the new app's own crash", lists: 2, snapshots: replaceSequence(
		appSnapshot("app", agentpb.AppRunningState_STOPPED, 1, "crashed", 0), appSnapshot("app", agentpb.AppRunningState_STOPPED, 3, "crashed", 0))},
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
		{name: "a recorded crash loop, then a fresh app running", lists: 2, snapshots: []*agentpb.AppContainer{
			withVersion(appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 2, "crashed", 3), "1.0"),
			appSnapshot("app", agentpb.AppRunningState_RUNNING, 0, "", 0),
		}},
		{name: "a recorded crash loop stays for the window", crash: "exit code 2", minTime: sigkillTestWindow, snapshots: []*agentpb.AppContainer{
			withVersion(appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 2, "crashed", 3), "1.0"),
		}},
		{name: "a crash loop restarted again by the restart policy", crash: "exit code 2", lists: 2, snapshots: []*agentpb.AppContainer{
			appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 2, "crashed", 3), runningSnapshot(4),
		}},
		// A stale exit (from a crash before the follow began) on the killed
		// app of a replace, the recorded count carried over.
		{name: "a stale recorded crash, then the rest of the replace", lists: 2, snapshots: replaceSequence(
			appSnapshot("app", agentpb.AppRunningState_STOPPED, 1, "crashed", 2), notListed,
			appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 0, "", 2), runningSnapshot(0))},
		{name: "a stale recorded crash, then the new app's own crash", lists: 2, snapshots: replaceSequence(
			appSnapshot("app", agentpb.AppRunningState_STOPPED, 1, "crashed", 2), appSnapshot("app", agentpb.AppRunningState_STOPPED, 3, "crashed", 0))},
		// The new container, created but not started, is listed with the old
		// registration's count, as crash-looping, and no exit recorded.
		{name: "created with the old count, then running", lists: 2, snapshots: replaceSequence(
			appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 0, "", 2), runningSnapshot(0))},
		{name: "the whole replace sequence", lists: 2, snapshots: replaceSequence(
			appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 137, "crashed", 2), notListed, notListed,
			appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 0, "", 2), runningSnapshot(0))},
		{name: "not listed, then created with the old count", lists: 2, snapshots: replaceSequence(
			notListed, appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 0, "", 2), runningSnapshot(0))},
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

// When the window ends on a record that is still uncertain, the verdict
// depends on what the run knows: an app not listed at all is a replacement
// for a run whose own task ended (nothing of it is listed; the new image is
// still being prepared), while a follow, which started no task, ends as a
// stop. An app stopped with no exit recorded is a stop for both — on agents
// with exit reporting that is how a stop by the user is listed.
func TestUncertainRecordForTheWholeWindow(t *testing.T) {
	shortenReplaceConfirm(t, sigkillTestWindow)
	for _, tc := range []struct {
		name, path   string
		record       *agentpb.AppContainer
		wantReplaced bool
	}{
		{name: "not listed", path: "attached", record: notListed, wantReplaced: true},
		{name: "not listed", path: "follow", record: notListed},
		{name: "stopped, no exit recorded", path: "attached", record: createdStopped()},
		{name: "stopped, no exit recorded", path: "follow", record: createdStopped()},
	} {
		t.Run(tc.name+"/"+tc.path, func(t *testing.T) {
			fake := &scriptedContainerClient{snapshots: []*agentpb.AppContainer{tc.record}}
			conn := &grpcclient.AgentConnection{ContainerService: fake, TelemetryService: &runLogsFakeClient{stream: &runLogsFakeStream{response: testRunLogsResponse(false, &logspb.ScopeLogs{})}}}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var err error
			out := captureStderr(t, func() {
				if tc.path == "attached" {
					err = attachedExitOutcome(ctx, conn, &appconfig.AppConfig{AppID: "app"})
				} else {
					err = followExistingContainer(ctx, conn, &appconfig.AppConfig{AppID: "app"}, runOptions{}, appBaseline{})
				}
			})
			if err != nil {
				t.Fatalf("err = %v, want nil", err)
			}
			if fake.lists() <= 2 {
				t.Errorf("ListContainers calls = %d, want the window's repeated polls", fake.lists())
			}
			replaced := strings.Contains(out, "Application app was replaced by another deployment.")
			stopped := strings.Contains(out, "Application app stopped.")
			if replaced != tc.wantReplaced || stopped == tc.wantReplaced {
				t.Fatalf("replaced = %v, stopped = %v, want replaced = %v:\n%s", replaced, stopped, tc.wantReplaced, out)
			}
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

// The device repro: an app that crashed once and was restarted by its restart
// policy (failure_count 1; the agent keeps its recorded exit, 1 / "crashed")
// runs unchanged, so `wendy run` follows it; another deployment replaces it,
// and the kill's own exit loses the race with the delete, so the follow finds
// the killed app listed with the stale crash. That crash is confirmed before
// it is reported, and the rest of the replace shows it was not this app's.
// A genuine crash is still reported, once it stays for the window or the
// restart policy restarts the app.
func TestFollowExistingContainer_StaleExitOfAReplacedApp(t *testing.T) {
	shortenReplaceConfirm(t, sigkillTestWindow)
	staleCrash := func(state agentpb.AppRunningState) *agentpb.AppContainer {
		return appSnapshot("app", state, 1, "crashed", 1)
	}
	for _, tc := range []struct {
		name      string
		base      appBaseline
		snapshots []*agentpb.AppContainer
		crash     string
	}{
		{name: "stale exit, stopped", base: appBaseline{failures: 1, version: "1.0"}, snapshots: replaceSequence(withAppVersion(runningSnapshot(1), "1.0"),
			staleCrash(agentpb.AppRunningState_STOPPED), notListed, withAppVersion(createdStopped(), "1.0"), runningSnapshot(0))},
		{name: "stale exit, crash-looping", base: appBaseline{failures: 1, version: "1.0"}, snapshots: replaceSequence(withAppVersion(runningSnapshot(1), "1.0"),
			staleCrash(agentpb.AppRunningState_CRASH_LOOPING), notListed, appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 0, "", 1), runningSnapshot(0))},
		{name: "stale exit, new version label", base: appBaseline{failures: 1, version: "1.0"}, snapshots: replaceSequence(withAppVersion(runningSnapshot(1), "1.0"),
			staleCrash(agentpb.AppRunningState_STOPPED), withAppVersion(createdStopped(), "2.0"), runningSnapshot(0))},
		{name: "a genuine crash stays for the window", crash: "exit code 3", snapshots: replaceSequence(runningSnapshot(0),
			appSnapshot("app", agentpb.AppRunningState_STOPPED, 3, "crashed", 0))},
		{name: "a genuine crash restarted by the restart policy", crash: "exit code 3", snapshots: replaceSequence(runningSnapshot(0),
			appSnapshot("app", agentpb.AppRunningState_CRASH_LOOPING, 3, "crashed", 0), runningSnapshot(1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &scriptedContainerClient{snapshots: tc.snapshots}
			telemetry := &runLogsFakeClient{stream: &runLogsFakeStream{response: testRunLogsResponse(false, &logspb.ScopeLogs{})}}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			var err error
			out := captureStderr(t, func() {
				err = followExistingContainer(ctx, &grpcclient.AgentConnection{ContainerService: fake, TelemetryService: telemetry}, &appconfig.AppConfig{AppID: "app"}, runOptions{}, tc.base)
			})
			if tc.crash == "" {
				if err != nil || strings.Count(out, "Application app was replaced by another deployment.") != 1 {
					t.Fatalf("err = %v, output %q; want nil and the replaced notice once", err, out)
				}
				return
			}
			if ErrorClass(err) != "app_crashed" || !strings.Contains(err.Error(), tc.crash) {
				t.Fatalf("err = %v, want app_crashed naming %s", err, tc.crash)
			}
			if strings.Contains(out, "replaced") {
				t.Fatalf("a crash reported as a replacement: %q", out)
			}
		})
	}
}

// The same repro end to end: the fast path's lookup baselines the follow on
// failure_count 1.
func TestTryDeployFastPath_FollowSeesAReplacementPastAStaleExit(t *testing.T) {
	shortenReplaceConfirm(t, sigkillTestWindow)
	isolateFingerprintCache(t)
	cfg := &appconfig.AppConfig{AppID: "app"}
	saveDeployFingerprint(cfg.AppID, "device", deployFingerprint{InputHash: "inputs", LayerDiffIDs: []string{"layer"}})
	fake := &scriptedContainerClient{snapshots: replaceSequence(
		runningSnapshot(1), // the fast path's lookup
		runningSnapshot(1), // the follow's first poll
		appSnapshot("app", agentpb.AppRunningState_STOPPED, 1, "crashed", 1), // the killed app, its stale exit
		notListed, createdStopped(), runningSnapshot(0))}
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
	if !strings.Contains(out, "Application app was replaced by another deployment.") || strings.Contains(out, "stopped unexpectedly") {
		t.Fatalf("output %q, want the replaced notice and no crash", out)
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
