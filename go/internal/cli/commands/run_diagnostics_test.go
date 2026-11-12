package commands

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRunStreamFailureUsesObservedStateAndPreservesCause(t *testing.T) {
	oldWindow, oldPoll := appReplaceConfirmWindow, appReplaceConfirmPoll
	appReplaceConfirmWindow, appReplaceConfirmPoll = 5*time.Millisecond, time.Millisecond
	t.Cleanup(func() { appReplaceConfirmWindow, appReplaceConfirmPoll = oldWindow, oldPoll })
	for _, tc := range []struct {
		name        string
		snapshot    *agentpb.AppContainer
		started     bool
		want, class string
	}{
		{"running", appSnapshot("app", agentpb.AppRunningState_RUNNING, 0, "", 0), true, "still running", "grpc_unavailable"},
		{"crash", appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "oom_killed", 0), true, "exit code 137", "app_crashed"},
		{"missing", nil, true, "could not be verified", "grpc_unavailable"},
		{"unconfirmed", appSnapshot("app", agentpb.AppRunningState_STOPPED, 1, "crashed", 0), false, "before the device confirmed", "grpc_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &scriptedContainerClient{snapshots: []*agentpb.AppContainer{tc.snapshot}}
			conn := &grpcclient.AgentConnection{ContainerService: client}
			cause := status.Error(codes.Unavailable, "transport disconnected")
			err := diagnoseRunStreamFailure(context.Background(), conn, &appconfig.AppConfig{AppID: "app"}, tc.started, cause)
			if !strings.Contains(err.Error(), tc.want) || ExecutionErrorClass(err) != tc.class || !errors.Is(err, cause) {
				t.Fatalf("wrong diagnosis: %v; class=%s", err, ExecutionErrorClass(err))
			}
			if len(client.stops()) != 0 {
				t.Fatal("diagnosis stopped the app")
			}
			if !tc.started && client.lists() != 0 {
				t.Fatal("unconfirmed start inspected a possibly stale app")
			}
		})
	}
}

func TestRunStreamFailureDoesNotClaimStateWhenInspectionFails(t *testing.T) {
	client := &scriptedContainerClient{listErr: status.Error(codes.Unauthenticated, "expired")}
	cause := status.Error(codes.Unavailable, "lost stream")
	err := diagnoseRunStreamFailure(context.Background(), &grpcclient.AgentConnection{ContainerService: client}, &appconfig.AppConfig{AppID: "app"}, true, cause)
	if !strings.Contains(err.Error(), "could not be verified") || status.Code(err) != codes.Unavailable {
		t.Fatal(err)
	}
}

func TestRunStreamFailureDoesNotMisattributeAReplacement(t *testing.T) {
	shortenReplaceConfirm(t, sigkillTestWindow)
	client := &scriptedContainerClient{snapshots: []*agentpb.AppContainer{
		appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "oom_killed", 0),
		appSnapshot("app", agentpb.AppRunningState_RUNNING, 0, "", 0),
	}}
	cause := status.Error(codes.Unavailable, "lost stream")
	err := diagnoseRunStreamFailure(context.Background(), &grpcclient.AgentConnection{ContainerService: client}, &appconfig.AppConfig{AppID: "app"}, true, cause)
	if errors.Is(err, errAppCrashed) || !errors.Is(err, cause) {
		t.Fatalf("replacement reported as a crash or cause lost: %v", err)
	}
}

func TestAttachedRunPathsPreserveRunningAppAfterConnectionLoss(t *testing.T) {
	for _, path := range []string{"new", "existing"} {
		t.Run(path, func(t *testing.T) {
			cause := status.Error(codes.Unavailable, "lost stream")
			client := &scriptedContainerClient{
				snapshots: []*agentpb.AppContainer{appSnapshot("app", agentpb.AppRunningState_RUNNING, 0, "", 0)},
				stream:    &deploymentAckStream{remaining: 1, err: cause},
			}
			conn := &grpcclient.AgentConnection{ContainerService: client}
			cfg := &appconfig.AppConfig{AppID: "app"}
			var err error
			if path == "new" {
				err = streamRunContainerWithStarted(context.Background(), conn, client.stream, cfg, runOptions{}, nil)
			} else {
				err = startExistingContainer(context.Background(), conn, cfg, runOptions{})
			}
			if !errors.Is(err, cause) || ExecutionErrorClass(err) != "grpc_unavailable" || !strings.Contains(err.Error(), "still running") {
				t.Fatalf("wrong diagnosis: %v", err)
			}
			if len(client.stops()) != 0 {
				t.Fatal("lost output connection stopped the running app")
			}
		})
	}
}

func TestRecentRuntimeOutputIsBoundedPrivateAndFailureOnly(t *testing.T) {
	old := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = old })
	recent := recentRunOutput{}
	recent.stderr = appendRunTail(nil, []byte(strings.Repeat("x", recentRunOutputLimit*2)))
	recent.stderr = appendRunTail(recent.stderr, []byte("final traceback"))
	if len(recent.stderr) != recentRunOutputLimit || !strings.HasSuffix(string(recent.stderr), "final traceback") {
		t.Fatal("tail is not bounded")
	}
	if recent.failure(nil) != nil || !errors.Is(recent.failure(context.Canceled), context.Canceled) {
		t.Fatal("nonfailure changed")
	}
	cause := errors.New("crashed")
	err := recent.failure(cause)
	if !errors.Is(err, cause) {
		t.Fatal("cause lost")
	}
	steps := NextSteps(err)
	if len(steps) != 1 {
		t.Fatalf("missing local artifact: %v", err)
	}
	path := strings.TrimPrefix(steps[0], "Recent runtime output saved locally: ")
	t.Cleanup(func() { os.Remove(path) })
	info, e := os.Stat(path)
	if e != nil {
		t.Fatal(e)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("permissions=%v", info.Mode())
	}
	data, e := os.ReadFile(path)
	if e != nil || !strings.Contains(string(data), "final traceback") {
		t.Fatal("missing recent stderr")
	}
}

// runContextAtStep is a `wendy run` context whose last activation step is step.
func runContextAtStep(t *testing.T, step string) context.Context {
	t.Helper()
	ctx, finish := startActivationOperation(context.Background(), "run", "wendy run")
	t.Cleanup(func() { finish(nil) })
	activationStep(ctx, step, nil)
	return ctx
}

// A status the device returned before confirming start is the failure itself,
// not a lost connection: say what it reported and do not send the user to
// check the network.
func TestRunStreamFailureReportsADeviceErrorBeforeStart(t *testing.T) {
	cause := status.Error(codes.Internal, "failed to create container: bad entrypoint")
	err := diagnoseRunStreamFailure(context.Background(), nil, &appconfig.AppConfig{AppID: "app"}, false, cause)
	text := err.Error()
	if !strings.Contains(text, "failed to create container: bad entrypoint") || !strings.Contains(text, "Internal") {
		t.Fatalf("device error or its code missing: %v", err)
	}
	if strings.Contains(strings.ToLower(text), "connection") {
		t.Fatalf("device-reported failure described as a connection problem: %v", err)
	}
	if !errors.Is(err, cause) || ExecutionErrorClass(err) != "grpc_internal" {
		t.Fatalf("cause or class lost: %v (%s)", err, ExecutionErrorClass(err))
	}
	if got := runPhaseFailure(context.Background(), err).Error(); !strings.HasPrefix(got, "run failed during app start: ") {
		t.Fatalf("wrong phase: %s", got)
	}
}

// The crash confirmation window must not share a deadline with the lookup
// before it: a slow first lookup would otherwise expire the window and turn a
// recorded crash into "could not be confirmed".
func TestRunStreamFailureConfirmsACrashAfterASlowLookup(t *testing.T) {
	oldLookup, oldWindow, oldPoll := appExitLookupTimeout, appReplaceConfirmWindow, appReplaceConfirmPoll
	appExitLookupTimeout, appReplaceConfirmWindow, appReplaceConfirmPoll = 400*time.Millisecond, 300*time.Millisecond, 10*time.Millisecond
	t.Cleanup(func() {
		appExitLookupTimeout, appReplaceConfirmWindow, appReplaceConfirmPoll = oldLookup, oldWindow, oldPoll
	})
	client := &scriptedContainerClient{snapshots: []*agentpb.AppContainer{appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "oom_killed", 0)}}
	client.onList = func(call int) {
		if call == 1 {
			time.Sleep(250 * time.Millisecond)
		}
	}
	err := diagnoseRunStreamFailure(context.Background(), &grpcclient.AgentConnection{ContainerService: client}, &appconfig.AppConfig{AppID: "app"}, true, status.Error(codes.Unavailable, "lost"))
	if !errors.Is(err, errAppCrashed) || ExecutionErrorClass(err) != "app_crashed" {
		t.Fatalf("recorded crash not confirmed: %v (%s)", err, ExecutionErrorClass(err))
	}
}

func TestRunPhaseNamesAConfirmedCrashAsAppExecution(t *testing.T) {
	shortenReplaceConfirm(t, 5*time.Millisecond)
	client := &scriptedContainerClient{snapshots: []*agentpb.AppContainer{appSnapshot("app", agentpb.AppRunningState_STOPPED, 137, "oom_killed", 0)}}
	err := diagnoseRunStreamFailure(context.Background(), &grpcclient.AgentConnection{ContainerService: client}, &appconfig.AppConfig{AppID: "app"}, true, status.Error(codes.Unavailable, "lost"))
	if got := runPhaseFailure(context.Background(), err).Error(); !strings.HasPrefix(got, "run failed during app execution: ") {
		t.Fatalf("wrong phase: %s", got)
	}
}

// The activation step is an analytics marker and can lag behind the work. A
// failure that knows its own phase must not be named after a stale step.
func TestRunPhasePrefersTheFailuresOwnPhaseOverTheStep(t *testing.T) {
	ctx := runContextAtStep(t, "build")
	err := inRunPhase("app start", errors.New("starting container: no such image"))
	if got := runPhaseFailure(ctx, err).Error(); got != "run failed during app start: starting container: no such image" {
		t.Fatal(got)
	}
	if got := runPhaseFailure(ctx, errors.New("compile error")).Error(); got != "run failed during build: compile error" {
		t.Fatal(got)
	}
}

func TestRunPhaseOmitsStepsItCannotName(t *testing.T) {
	cause := errors.New("no device selected")
	if got := runPhaseFailure(runContextAtStep(t, "select_target"), cause); got != cause {
		t.Fatalf("internal step leaked into the message: %v", got)
	}
}

// The no-build fast path starts an existing container while the step is still
// "build".
func TestFastPathStartFailureIsNotNamedBuild(t *testing.T) {
	ctx := runContextAtStep(t, "build")
	client := &scriptedContainerClient{stream: &deploymentAckStream{err: status.Error(codes.NotFound, "no such container")}}
	err := startExistingContainer(ctx, &grpcclient.AgentConnection{ContainerService: client}, &appconfig.AppConfig{AppID: "app"}, runOptions{detach: true})
	if got := runPhaseFailure(ctx, err).Error(); !strings.HasPrefix(got, "run failed during app start: ") {
		t.Fatalf("wrong phase: %s", got)
	}
}

// Compose runs never advance the step past "connect".
func TestComposeBuildRefusalIsNotNamedConnect(t *testing.T) {
	fakeAllBuildDisks(t, 1<<20)
	conn, dir, _ := startComposeBuildProject(t)
	cacheLinuxDeviceWithFree(conn, 100<<30)
	ctx := runContextAtStep(t, "connect")
	err := runComposeWithAgent(ctx, conn, dir, runOptions{deploy: true})
	if got := runPhaseFailure(ctx, err).Error(); !strings.HasPrefix(got, "run failed during build: ") {
		t.Fatalf("wrong phase: %s", got)
	}
}
