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
