package commands

import (
	"context"
	"errors"
	"fmt"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type deploymentAckStream struct {
	grpc.ServerStreamingClient[agentpb.RunContainerLayersResponse]
	remaining   int
	err         error
	beforeError func()
}

func (s *deploymentAckStream) Recv() (*agentpb.RunContainerLayersResponse, error) {
	if s.remaining > 0 {
		s.remaining--
		return &agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_Started_{Started: &agentpb.RunContainerLayersResponse_Started{}}}, nil
	}
	if s.beforeError != nil {
		s.beforeError()
	}
	return nil, s.err
}

func TestStreamRunContainer_CommitsDeploymentBeforeLogCancellation(t *testing.T) {
	isolateFingerprintCache(t)
	cfg := &appconfig.AppConfig{AppID: "ack-app"}
	calls := 0
	stream := &deploymentAckStream{remaining: 2, err: context.Canceled, beforeError: func() {
		fp, ok := loadDeployFingerprint(cfg.AppID, "device")
		if !ok || fp.InputHash != "inputs" || len(fp.LayerDiffIDs) != 1 {
			t.Fatal("deployment was not recorded before log streaming")
		}
	}}
	err := streamRunContainerWithStarted(context.Background(), &grpcclient.AgentConnection{}, stream, cfg, runOptions{}, func() {
		calls++
		saveDeployFingerprint(cfg.AppID, "device", deployFingerprint{InputHash: "inputs", LayerDiffIDs: []string{"layer"}})
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("err=%v, commits=%d; want cancellation and one commit", err, calls)
	}
	// Cancellation must leave a usable fingerprint for the next invocation.
	client := &fastPathContainerClient{appName: cfg.AppID, state: agentpb.AppRunningState_RUNNING, presentLayers: map[string]bool{"layer": true}}
	done, err := tryDeployFastPath(context.Background(), &grpcclient.AgentConnection{ContainerService: client}, cfg, "device", "inputs", runOptions{detach: true})
	if !done || err != nil {
		t.Fatalf("next run did not reuse deployment: %v, %v", done, err)
	}
}

func TestStreamRunContainer_DoesNotCommitBeforeStarted(t *testing.T) {
	for _, recvErr := range []error{io.EOF, context.Canceled, errors.New("startup failed")} {
		t.Run(recvErr.Error(), func(t *testing.T) {
			committed := false
			err := streamRunContainerWithStarted(context.Background(), &grpcclient.AgentConnection{}, &deploymentAckStream{err: recvErr}, &appconfig.AppConfig{AppID: "unstarted"}, runOptions{}, func() { committed = true })
			if err == nil || committed {
				t.Fatalf("err=%v committed=%v", err, committed)
			}
		})
	}
}

func TestStreamRunContainer_DetachedCommitsAtStarted(t *testing.T) {
	calls := 0
	err := streamRunContainerWithStarted(context.Background(), &grpcclient.AgentConnection{}, &deploymentAckStream{remaining: 1, err: errors.New("must not read logs")}, &appconfig.AppConfig{AppID: "detached"}, runOptions{detach: true}, func() { calls++ })
	if err != nil || calls != 1 {
		t.Fatalf("err=%v commits=%d", err, calls)
	}
}

type foregroundFastPathClient struct {
	fastPathContainerClient
	stream grpc.ServerStreamingClient[agentpb.RunContainerLayersResponse]
}

func (f *foregroundFastPathClient) AttachContainer(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[agentpb.AttachContainerRequest, agentpb.RunContainerLayersResponse], error) {
	return nil, status.Error(codes.Unimplemented, "old agent")
}
func (f *foregroundFastPathClient) StartContainer(ctx context.Context, _ *agentpb.StartContainerRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.RunContainerLayersResponse], error) {
	f.startCalls++
	f.startCtx = ctx
	return f.stream, nil
}

func TestTryDeployFastPath_AttachedStoppedRunsHostLifecycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses touch")
	}
	isolateFingerprintCache(t)
	sentinel := filepath.Join(t.TempDir(), "ready-hook")
	cfg := &appconfig.AppConfig{AppID: "foreground-app", Hooks: &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{CLI: fmt.Sprintf("touch %q", sentinel)}}}
	saveDeployFingerprint(cfg.AppID, "device", deployFingerprint{InputHash: "inputs", LayerDiffIDs: []string{"layer"}})
	fake := &foregroundFastPathClient{fastPathContainerClient: fastPathContainerClient{appName: cfg.AppID, state: agentpb.AppRunningState_STOPPED, presentLayers: map[string]bool{"layer": true}}, stream: &attachedRunStream{waitFor: sentinel, deadline: time.Now().Add(10 * time.Second)}}
	conn := &grpcclient.AgentConnection{Host: "localhost", ContainerService: fake}
	done, err := tryDeployFastPath(context.Background(), conn, cfg, "device", "inputs", runOptions{})
	if !done || err != nil || fake.startCalls != 1 {
		t.Fatalf("done=%v err=%v starts=%d", done, err, fake.startCalls)
	}
	waitForFile(t, sentinel, time.Second)
}

func TestTryDeployFastPath_AttachedStartFailureDoesNotRebuild(t *testing.T) {
	isolateFingerprintCache(t)
	cfg := &appconfig.AppConfig{AppID: "failed-app"}
	saveDeployFingerprint(cfg.AppID, "device", deployFingerprint{InputHash: "inputs", LayerDiffIDs: []string{"layer"}})
	fake := &foregroundFastPathClient{fastPathContainerClient: fastPathContainerClient{appName: cfg.AppID, state: agentpb.AppRunningState_STOPPED, presentLayers: map[string]bool{"layer": true}}, stream: &deploymentAckStream{err: errors.New("start failed")}}
	done, err := tryDeployFastPath(context.Background(), &grpcclient.AgentConnection{ContainerService: fake}, cfg, "device", "inputs", runOptions{})
	if !done || err == nil {
		t.Fatalf("done=%v err=%v; must surface failure without rebuilding", done, err)
	}
}

func TestTryDeployFastPath_AttachedRunningPreservesTaskAndFollowsLogs(t *testing.T) {
	isolateFingerprintCache(t)
	cfg := &appconfig.AppConfig{AppID: "running-app", Hooks: &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{OpenURL: "http://example.test"}}}
	saveDeployFingerprint(cfg.AppID, "device", deployFingerprint{InputHash: "inputs", LayerDiffIDs: []string{"layer"}})
	fake := &fastPathContainerClient{appName: cfg.AppID, state: agentpb.AppRunningState_RUNNING, presentLayers: map[string]bool{"layer": true}}
	telemetry := &watchTelemetryClient{}
	conn := &grpcclient.AgentConnection{Host: "127.0.0.1", AgentService: &lifecycleFakeAgentClient{}, ContainerService: fake}
	conn.TelemetryService = telemetry
	opened := make(chan struct{}, 1)
	previous := browserOpen
	browserOpen = func(string) error { opened <- struct{}{}; return nil }
	t.Cleanup(func() { browserOpen = previous })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
		select {
		case <-opened:
			cancel()
		case <-ctx.Done():
		}
	}()
	done, err := tryDeployFastPath(ctx, conn, cfg, "device", "inputs", runOptions{})
	if !done || err != nil || fake.startCalls != 0 {
		t.Fatalf("done=%v err=%v starts=%d", done, err, fake.startCalls)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("host lifecycle did not run")
	}
	telemetry.mu.Lock()
	defer telemetry.mu.Unlock()
	if len(telemetry.requests) != 1 || telemetry.requests[0].GetAppName() != cfg.AppID {
		t.Fatalf("log requests=%v", telemetry.requests)
	}
}

func TestTryDeployFastPath_AttachedRejectsUnverifiedDeployment(t *testing.T) {
	for _, scenario := range []string{"changed inputs", "missing layer", "missing app", "missing fingerprint"} {
		t.Run(scenario, func(t *testing.T) {
			isolateFingerprintCache(t)
			cfg := &appconfig.AppConfig{AppID: "verified-app"}
			fake := &fastPathContainerClient{appName: cfg.AppID, state: agentpb.AppRunningState_RUNNING, presentLayers: map[string]bool{"layer": true}}
			if scenario != "missing fingerprint" {
				saveDeployFingerprint(cfg.AppID, "device", deployFingerprint{InputHash: "inputs", LayerDiffIDs: []string{"layer"}})
			}
			inputHash := "inputs"
			switch scenario {
			case "changed inputs":
				inputHash = "changed"
			case "missing layer":
				fake.presentLayers = nil
			case "missing app":
				fake.appName = "different-app"
			}
			done, err := tryDeployFastPath(context.Background(), &grpcclient.AgentConnection{ContainerService: fake}, cfg, "device", inputHash, runOptions{})
			if done || err != nil || fake.startCalls != 0 {
				t.Fatalf("done=%v err=%v starts=%d; want normal build path", done, err, fake.startCalls)
			}
		})
	}
}

func TestFollowExistingContainer_LogFailureDoesNotRestart(t *testing.T) {
	want := errors.New("connection lost")
	fake := &fastPathContainerClient{appName: "app", state: agentpb.AppRunningState_RUNNING}
	telemetry := &runLogsFakeClient{stream: &runLogsFakeStream{receiveErr: want}}
	err := followExistingContainer(context.Background(), &grpcclient.AgentConnection{ContainerService: fake, TelemetryService: telemetry}, &appconfig.AppConfig{AppID: "app"}, runOptions{})
	if !errors.Is(err, want) || fake.startCalls != 0 {
		t.Fatalf("err=%v starts=%d", err, fake.startCalls)
	}
}

func TestFollowExistingContainer_ShowsContainerLogsAndExitsWhenStopped(t *testing.T) {
	fake := &fastPathContainerClient{appName: "app", state: agentpb.AppRunningState_STOPPED}
	record := testLogRecord("preserved stdout")
	record.Attributes = []*commonpb.KeyValue{testStringAttribute("stream", "stdout")}
	telemetry := &runLogsFakeClient{stream: &runLogsFakeStream{response: testRunLogsResponse(false, &logspb.ScopeLogs{Scope: &commonpb.InstrumentationScope{Name: "wendy.container"}, LogRecords: []*logspb.LogRecord{record}})}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	output := captureStdout(t, func() {
		err := followExistingContainer(ctx, &grpcclient.AgentConnection{ContainerService: fake, TelemetryService: telemetry}, &appconfig.AppConfig{AppID: "app"}, runOptions{})
		if err != nil {
			t.Fatal(err)
		}
	})
	if ctx.Err() != nil {
		t.Fatal("did not exit when the app stopped")
	}
	if !strings.Contains(output, "preserved stdout") {
		t.Fatalf("missing container logs: %q", output)
	}
}
