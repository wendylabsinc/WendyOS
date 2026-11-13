package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type appInspectAgent struct {
	agentpb.UnimplementedWendyContainerServiceServer
	agentpb.UnimplementedWendyTelemetryServiceServer
	container         *agentpb.AppContainer
	stats             []*agentpb.ContainerStats
	resources         []*agentpb.ResourceContainerStats
	statsErr, logsErr error
	logs              []*agentpb.StreamLogsResponse
	logRequest        chan *agentpb.StreamLogsRequest
	holdLogs          bool
}

func (a *appInspectAgent) ListContainers(_ *agentpb.ListContainersRequest, stream grpc.ServerStreamingServer[agentpb.ListContainersResponse]) error {
	if a.container == nil {
		return nil
	}
	return stream.Send(&agentpb.ListContainersResponse{Container: a.container})
}
func (a *appInspectAgent) ListContainerStats(context.Context, *agentpb.ListContainerStatsRequest) (*agentpb.ListContainerStatsResponse, error) {
	return &agentpb.ListContainerStatsResponse{Stats: a.stats}, a.statsErr
}
func (a *appInspectAgent) GetResourceStats(context.Context, *agentpb.GetResourceStatsRequest) (*agentpb.GetResourceStatsResponse, error) {
	return &agentpb.GetResourceStatsResponse{Containers: a.resources}, a.statsErr
}
func (a *appInspectAgent) StreamLogs(req *agentpb.StreamLogsRequest, stream grpc.ServerStreamingServer[agentpb.StreamLogsResponse]) error {
	if a.logRequest != nil {
		a.logRequest <- req
	}
	for _, batch := range a.logs {
		if err := stream.Send(batch); err != nil {
			return err
		}
	}
	if a.holdLogs {
		<-stream.Context().Done()
		return stream.Context().Err()
	}
	return a.logsErr
}

func appInspectConnection(t *testing.T, agent *appInspectAgent) *grpcclient.AgentConnection {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	agentpb.RegisterWendyContainerServiceServer(srv, agent)
	agentpb.RegisterWendyTelemetryServiceServer(srv, agent)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	cc, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	conn := grpcclient.NewFromConn(cc)
	conn.Addr, conn.Host = ln.Addr().String(), "127.0.0.1"
	return conn
}

func appInspectLog(body string, history bool) *agentpb.StreamLogsResponse {
	return &agentpb.StreamLogsResponse{IsHistory: history, Logs: &collogspb.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{{Key: "service.name", Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "robot_api"}}}}},
		ScopeLogs: []*logspb.ScopeLogs{{Scope: &commonpb.InstrumentationScope{Name: "test"}, LogRecords: []*logspb.LogRecord{{
			TimeUnixNano: 1234, SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_ERROR, Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}},
		}}}},
	}}}}
}

func TestAppInspectAggregatesExactMembersAndPreservesLogEvidence(t *testing.T) {
	agent := &appInspectAgent{
		container: &agentpb.AppContainer{AppName: "robot", AppVersion: "v1", RunningState: agentpb.AppRunningState_RUNNING, FailureCount: 2, Services: []*agentpb.ServiceEntry{{Name: "api", RunningState: agentpb.AppRunningState_RUNNING}, {Name: "worker", RunningState: agentpb.AppRunningState_CRASH_LOOPING}}},
		stats:     []*agentpb.ContainerStats{{AppName: "robot_api", MemoryBytes: 10, StorageBytes: 100}, {AppName: "robot_api_other", MemoryBytes: 99}, {AppName: "robot_worker", MemoryBytes: 20}},
		resources: []*agentpb.ResourceContainerStats{{AppName: "robot_api", MemoryBytes: 11, CpuUsageNanos: 4321}},
		logs:      []*agentpb.StreamLogsResponse{{}, appInspectLog("old", true), appInspectLog("new", false)}, logRequest: make(chan *agentpb.StreamLogsRequest, 1),
	}
	s := New(&config.Config{}, nil)
	s.SetConn(appInspectConnection(t, agent))
	result, err := s.handleAppInspect(context.Background(), callToolReq("app_inspect", map[string]any{"app_name": "robot", "max_logs": 1}))
	if err != nil || result.IsError {
		t.Fatalf("inspection: %v %+v", err, result)
	}
	out := structuredMap(t, result)
	state := out["state"].(map[string]any)
	if state["all_services_running"] != false || state["running_state"] != "RUNNING" {
		t.Fatalf("mixed state lost: %+v", state)
	}
	if exit := state["last_exit"].(map[string]any); exit["status"] != "unknown" || exit["code"] != nil {
		t.Fatalf("invented exit: %+v", exit)
	}
	usage := out["usage"].(map[string]any)["containers"].([]map[string]any)
	if len(usage) != 2 || usage[0]["container_name"] != "robot_api" || usage[0]["memory_bytes"] != int64(11) || usage[0]["cpu_usage_nanos"] != uint64(4321) || usage[0]["image_content_bytes"] != int64(100) {
		t.Fatalf("usage: %+v", usage)
	}
	logs := out["recent_logs"].(map[string]any)
	events := logs["records"].([]map[string]any)
	if len(events) != 1 || events[0]["body"] != "new" || events[0]["timeUnixNano"] != "1234" || events[0]["is_history"] != false || events[0]["resource"] == nil || events[0]["scope"] == nil || logs["omitted"] != 1 {
		t.Fatalf("log evidence: %+v", logs)
	}
	request := <-agent.logRequest
	if request.GetAppName() != "robot" || request.GetLastN() != 20 || request.GetMinSeverity() != 13 {
		t.Fatalf("wrong log request: %+v", request)
	}
	if out["readiness"].(map[string]any)["status"] != "unknown" {
		t.Fatal("undeclared health must stay unknown")
	}
}

func TestAppInspectPartialFailuresRetainExitAndState(t *testing.T) {
	agent := &appInspectAgent{container: &agentpb.AppContainer{AppName: "robot", TerminationReason: "oom_killed", ExitCode: 137}, statsErr: status.Error(codes.Unimplemented, "old agent"), logsErr: status.Error(codes.Unavailable, "logs unavailable")}
	s := New(&config.Config{}, nil)
	s.SetConn(appInspectConnection(t, agent))
	result, err := s.handleAppInspect(context.Background(), callToolReq("app_inspect", map[string]any{"app_name": "robot"}))
	if err != nil || result.IsError {
		t.Fatalf("partial inspection failed: %v %+v", err, result)
	}
	out := structuredMap(t, result)
	exit := out["state"].(map[string]any)["last_exit"].(map[string]any)
	if exit["reason"] != "oom_killed" || exit["code"] != int32(137) || exit["status"] != "recorded" {
		t.Fatalf("exit: %+v", exit)
	}
	if out["recent_logs"].(map[string]any)["status"] != "unknown" || len(out["usage"].(map[string]any)["warnings"].([]string)) != 2 {
		t.Fatalf("missing partial errors: %+v", out)
	}
}

func TestAppInspectBoundsIdleStreamAndLargeLogs(t *testing.T) {
	agent := &appInspectAgent{container: &agentpb.AppContainer{AppName: "robot", RunningState: agentpb.AppRunningState_RUNNING}, logs: []*agentpb.StreamLogsResponse{appInspectLog(strings.Repeat("x", 6000), true)}, holdLogs: true}
	s := New(&config.Config{}, nil)
	s.SetConn(appInspectConnection(t, agent))
	started := time.Now()
	result, err := s.handleAppInspect(context.Background(), callToolReq("app_inspect", map[string]any{"app_name": "robot", "timeout_seconds": 1, "max_bytes": 1024}))
	if err != nil || result.IsError {
		t.Fatalf("inspection: %v %+v", err, result)
	}
	if time.Since(started) > 3*time.Second {
		t.Fatal("log stream was not bounded")
	}
	data, _ := json.Marshal(result.StructuredContent)
	if len(data) > 1024 {
		t.Fatalf("result has %d bytes", len(data))
	}
	out := structuredMap(t, result)
	if out["state"] == nil && out["running_state"] != "RUNNING" {
		t.Fatalf("budget discarded state: %+v", out)
	}
}

func TestAppInspectValidationAndMissingApp(t *testing.T) {
	s := New(&config.Config{}, nil)
	for _, args := range []map[string]any{{"app_name": "../robot"}, {"app_name": "robot", "timeout_seconds": 1.5}, {"app_name": "robot", "max_logs": -1}, {"app_name": "robot", "project_path": false}} {
		result, _ := s.handleAppInspect(context.Background(), callToolReq("app_inspect", args))
		if !result.IsError {
			t.Fatalf("accepted %+v", args)
		}
	}
	result, _ := s.handleAppInspect(context.Background(), callToolReq("app_inspect", map[string]any{"app_name": "robot"}))
	if !result.IsError {
		t.Fatal("accepted disconnected inspection")
	}
	s.SetConn(appInspectConnection(t, &appInspectAgent{}))
	result, _ = s.handleAppInspect(context.Background(), callToolReq("app_inspect", map[string]any{"app_name": "robot"}))
	if !result.IsError || structuredMap(t, result)["error_code"] != "NOT_FOUND" {
		t.Fatalf("missing app: %+v", result)
	}
}

func TestAppInspectDeclaredReadinessDirectAndCloud(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "wendy.json"), []byte(fmt.Sprintf(`{"appId":"robot","readiness":{"tcpSocket":{"port":%d}},"services":{"api":{"readiness":{"tcpSocket":{"port":%d}}}}}`, port, port)), 0600); err != nil {
		t.Fatal(err)
	}
	probes, err := appInspectDeclarations(project, "robot")
	if err != nil || len(probes) != 2 {
		t.Fatalf("declarations: %+v %v", probes, err)
	}
	conn := &grpcclient.AgentConnection{Addr: "127.0.0.1:50051"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := appInspectReadiness(ctx, conn, "direct", commandTarget{}, project, probes, map[string]bool{"robot_api": true}, nil)
	if result["status"] != "passed" || result["deployed_configuration_verified"] != false {
		t.Fatalf("direct probes: %+v", result)
	}
	conn.RegistryDialer = func(context.Context, int) (net.Conn, error) {
		t.Error("unacknowledged tunnel must not be probed")
		return nil, nil
	}
	result = appInspectReadiness(ctx, conn, "cloud", commandTarget{}, project, probes, map[string]bool{"robot_api": true}, nil)
	if result["status"] != "unknown" {
		t.Fatalf("cloud falsely passed: %+v", result)
	}
	if _, err := appInspectDeclarations(project, "another-app"); err == nil {
		t.Fatal("accepted unrelated project")
	}
}

func TestAppInspectReadinessExactServiceAndSimulatorRoute(t *testing.T) {
	conn := &grpcclient.AgentConnection{Addr: "127.0.0.1:50051", SimulatorName: "test-vm"}
	called := false
	resolver := func(_ context.Context, got *grpcclient.AgentConnection, port int) (string, error) {
		called = true
		if got != conn || port != 8080 {
			t.Fatalf("wrong mapping %v %d", got, port)
		}
		return "127.0.0.1:18080", nil
	}
	address, err := appInspectProbeAddress(context.Background(), conn, "direct", commandTarget{}, 8080, resolver)
	if err != nil || address != "127.0.0.1:18080" || !called {
		t.Fatalf("simulator mapping: %s %v", address, err)
	}
	result := appInspectReadiness(context.Background(), conn, "direct", commandTarget{}, "project", []appInspectProbe{{Port: 8080}}, map[string]bool{"robot": true}, resolver)
	if result["status"] != "unknown" {
		t.Fatalf("local simulator forward claimed guest readiness: %+v", result)
	}
	called = false
	result = appInspectReadiness(context.Background(), conn, "direct", commandTarget{}, "project", []appInspectProbe{{Service: "api", Container: "robot_api", Port: 8080}}, map[string]bool{"robot_other_api": true}, resolver)
	if called || result["status"] != "unknown" {
		t.Fatalf("matched wrong service: %+v", result)
	}
	conn.SimulatorName, conn.IsSessionProxy = "", true
	if _, err := appInspectProbeAddress(context.Background(), conn, "direct", commandTarget{}, 8080, resolver); err == nil {
		t.Fatal("session proxy reused as app address")
	}
}

func TestAppInspectDirectProbeFailureAndNoDeclarations(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	conn := &grpcclient.AgentConnection{Addr: "127.0.0.1:50051"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := appInspectReadiness(ctx, conn, "direct", commandTarget{}, "project", []appInspectProbe{{Port: port}}, map[string]bool{"robot": true}, nil)
	if result["status"] != "failed" {
		t.Fatalf("closed port passed: %+v", result)
	}
	result = appInspectReadiness(ctx, conn, "direct", commandTarget{}, "project", nil, nil, nil)
	if result["status"] != "unknown" {
		t.Fatalf("missing probe passed: %+v", result)
	}
}

func TestAppInspectIsReadOnly(t *testing.T) {
	s := New(&config.Config{}, nil)
	protocol := server.NewMCPServer("test", "1")
	s.registerAppInspectTools(protocol)
	tool := protocol.ListTools()["app_inspect"].Tool
	if tool.Annotations.ReadOnlyHint == nil || !*tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
		t.Fatalf("annotations: %+v", tool.Annotations)
	}
}

func TestAppInspectStoppedApplicationCannotPassListeningPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	conn := &grpcclient.AgentConnection{Addr: "127.0.0.1:50051"}
	for _, tc := range []struct {
		probe   appInspectProbe
		members map[string]bool
	}{
		{appInspectProbe{Port: port}, map[string]bool{"robot": false}},
		{appInspectProbe{Port: port, Service: "api", Container: "robot_api"}, map[string]bool{"robot_api": false, "robot_worker": true}},
		{appInspectProbe{Port: port}, map[string]bool{"robot_api": false, "robot_worker": true}},
	} {
		result := appInspectReadiness(context.Background(), conn, "direct", commandTarget{}, "project", []appInspectProbe{tc.probe}, tc.members, nil)
		if result["status"] != "failed" {
			t.Fatalf("stopped app passed open port: %+v", result)
		}
	}
}

func TestAppInspectZeroResourceCountersStayUnknown(t *testing.T) {
	agent := &appInspectAgent{stats: []*agentpb.ContainerStats{{AppName: "robot"}}, resources: []*agentpb.ResourceContainerStats{{AppName: "robot"}}}
	result := appInspectUsage(context.Background(), appInspectConnection(t, agent), map[string]bool{"robot": false})
	row := result["containers"].([]map[string]any)[0]
	if row["status"] != "reported" {
		t.Fatalf("row status: %+v", row)
	}
	for _, key := range []string{"memory_bytes", "image_content_bytes", "cpu_usage_nanos"} {
		if row[key] != nil || row[key+"_status"] != "unknown" {
			t.Fatalf("ambiguous counter reported as measurement: %+v", row)
		}
	}
}
