package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type verifiedTestStream struct {
	grpc.ClientStream
	messages []*agentpb.RunContainerLayersResponse
	read     int
	err      error
}

func (s *verifiedTestStream) Recv() (*agentpb.RunContainerLayersResponse, error) {
	if s.read < len(s.messages) {
		r := s.messages[s.read]
		s.read++
		return r, nil
	}
	if s.err != nil {
		return nil, s.err
	}
	return nil, io.EOF
}

type verifiedTestClient struct {
	agentpb.WendyContainerServiceClient
	requests []*agentpb.RunContainerLayersRequest
	streams  []*verifiedTestStream
	onSubmit func(int)
	err      error
	attached *verifiedTestBidiStream
}

type verifiedTestBidiStream struct {
	verifiedTestStream
	inputs []*agentpb.AttachContainerRequest
	closed chan struct{}
}

func (s *verifiedTestBidiStream) Send(input *agentpb.AttachContainerRequest) error {
	s.inputs = append(s.inputs, proto.Clone(input).(*agentpb.AttachContainerRequest))
	return nil
}

func (s *verifiedTestBidiStream) CloseSend() error { close(s.closed); return nil }

func (c *verifiedTestClient) AttachContainer(context.Context, ...grpc.CallOption) (grpc.BidiStreamingClient[agentpb.AttachContainerRequest, agentpb.RunContainerLayersResponse], error) {
	return c.attached, c.err
}

func (c *verifiedTestClient) RunContainer(_ context.Context, req *agentpb.RunContainerLayersRequest, _ ...grpc.CallOption) (grpc.ServerStreamingClient[agentpb.RunContainerLayersResponse], error) {
	i := len(c.requests)
	if c.onSubmit != nil {
		c.onSubmit(i)
	}
	c.requests = append(c.requests, req)
	if c.err != nil {
		return nil, c.err
	}
	return c.streams[i], nil
}

func deploymentTestResponse(state agentpb.DeploymentState) *agentpb.RunContainerLayersResponse {
	return &agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_Deployment{Deployment: &agentpb.DeploymentResult{
		State: state, Message: "test outcome",
	}}}
}

func deploymentStartedResponse() *agentpb.RunContainerLayersResponse {
	return &agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_Started_{Started: &agentpb.RunContainerLayersResponse_Started{}}}
}

func TestVerifiedDetachedWaitsForOutcomeWithoutStartingAgain(t *testing.T) {
	stream := &verifiedTestStream{messages: []*agentpb.RunContainerLayersResponse{deploymentStartedResponse(), deploymentTestResponse(agentpb.DeploymentState_READY)}}
	client := &verifiedTestClient{streams: []*verifiedTestStream{stream}}
	cfg := &appconfig.AppConfig{AppID: "app", Readiness: &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: 8080}}}
	request := &agentpb.CreateContainerRequest{AppName: "app", ImageName: "localhost:5000/app:latest", Cmd: "serve", WorkingDir: "/app", UserArgs: []string{"--port", "8080"}, Env: []string{"KEY=value"}}
	err := startAndStreamContainer(context.Background(), &grpcclient.AgentConnection{ContainerService: client}, cfg, request, runOptions{verifiedDeployment: true, detach: true, waitReady: true, readinessTimeout: 45 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if stream.read != 2 || len(client.requests) != 1 {
		t.Fatalf("read %d responses; sent %d requests", stream.read, len(client.requests))
	}
	got := client.requests[0]
	var sent appconfig.AppConfig
	if err := json.Unmarshal(got.AppConfig, &sent); err != nil {
		t.Fatal(err)
	}
	if !got.Verified || !sent.Readiness.HasProbe() || sent.Readiness.TimeoutSeconds != 45 || len(got.Layers) != 0 || got.WorkingDir != "/app" || got.Cmd != "serve" || strings.Join(got.Env, ",") != "KEY=value" || len(got.UserArgs) != 2 {
		t.Fatalf("request lost deployment options: %+v %+v", got, sent.Readiness)
	}
}

func TestVerifiedAttachedForwardsStdinOnOriginalTransaction(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	oldStdin := os.Stdin
	os.Stdin = reader
	t.Cleanup(func() { os.Stdin = oldStdin })
	if _, err := writer.WriteString("hello application\n"); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	stream := &verifiedTestBidiStream{
		verifiedTestStream: verifiedTestStream{messages: []*agentpb.RunContainerLayersResponse{deploymentStartedResponse(), deploymentTestResponse(agentpb.DeploymentState_RUNNING)}},
		closed:             make(chan struct{}),
	}
	client := &verifiedTestClient{attached: stream}
	err = startAndStreamContainer(context.Background(), &grpcclient.AgentConnection{ContainerService: client}, &appconfig.AppConfig{AppID: "app"}, &agentpb.CreateContainerRequest{AppName: "app", ImageName: "app:latest"}, runOptions{verifiedDeployment: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-stream.closed:
	case <-time.After(time.Second):
		t.Fatal("stdin EOF was not forwarded")
	}
	if len(client.requests) != 0 || len(stream.inputs) != 2 {
		t.Fatalf("unary calls=%d bidi messages=%d", len(client.requests), len(stream.inputs))
	}
	if !stream.inputs[0].GetDeploy().GetVerified() || stream.inputs[0].GetDeploy().GetAppName() != "app" || string(stream.inputs[1].GetStdinData()) != "hello application\n" {
		t.Fatalf("lost deployment or stdin: %v", stream.inputs)
	}
}

func TestVerifiedDeploymentFailureIsTerminalAndJSON(t *testing.T) {
	oldJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJSON })
	for _, state := range []agentpb.DeploymentState{agentpb.DeploymentState_ROLLED_BACK, agentpb.DeploymentState_FAILED, agentpb.DeploymentState_ROLLBACK_FAILED} {
		t.Run(state.String(), func(t *testing.T) {
			stream := &verifiedTestStream{messages: []*agentpb.RunContainerLayersResponse{deploymentStartedResponse(), deploymentTestResponse(state)}}
			var gotErr error
			stdout, _ := captureBoth(t, func() {
				gotErr = streamRunContainer(context.Background(), nil, stream, &appconfig.AppConfig{AppID: "app"}, runOptions{verifiedDeployment: true, detach: true})
			})
			if gotErr == nil || !isSubmittedDeploymentError(fmt.Errorf("wrapped: %w", gotErr)) {
				t.Fatalf("failure may fall back to another deploy: %v", gotErr)
			}
			var result map[string]any
			if err := json.Unmarshal([]byte(stdout), &result); err != nil {
				t.Fatalf("invalid JSON %q: %v", stdout, err)
			}
			if result["state"] != state.String() || result["app_name"] != "app" || result["message"] != "test outcome" {
				t.Fatalf("missing structured failure: %v", result)
			}
		})
	}
}

func TestVerifiedDeploymentRejectsIncompleteOrFalseSuccess(t *testing.T) {
	for _, tc := range []struct {
		name     string
		messages []*agentpb.RunContainerLayersResponse
		require  bool
	}{
		{"started-only", []*agentpb.RunContainerLayersResponse{deploymentStartedResponse()}, false},
		{"required-but-running", []*agentpb.RunContainerLayersResponse{deploymentTestResponse(agentpb.DeploymentState_RUNNING)}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := streamRunContainer(context.Background(), nil, &verifiedTestStream{messages: tc.messages}, &appconfig.AppConfig{AppID: "app"}, runOptions{verifiedDeployment: true, detach: true, waitReady: tc.require})
			if err == nil || !isSubmittedDeploymentError(err) {
				t.Fatalf("accepted incomplete deployment: %v", err)
			}
		})
	}
}

func TestVerifiedSubmissionErrorNeverFallsBack(t *testing.T) {
	client := &verifiedTestClient{err: status.Error(codes.Unimplemented, "agent changed after discovery")}
	_, err := openVerifiedDeployment(context.Background(), &grpcclient.AgentConnection{ContainerService: client}, &appconfig.AppConfig{AppID: "app"}, &agentpb.RunContainerLayersRequest{AppName: "app"}, runOptions{detach: true})
	if !isSubmittedDeploymentError(err) {
		t.Fatalf("submission may be retried: %v", err)
	}
}

func TestVerifiedPreflightRequiresCapabilityAndProbe(t *testing.T) {
	capable := &agentpb.GetAgentVersionResponse{Featureset: []string{"verified-deployment"}}
	probe := &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: 8080}}
	for _, tc := range []struct {
		name              string
		version           *agentpb.GetAgentVersionResponse
		cfg               *appconfig.AppConfig
		opts              runOptions
		wantErr, verified bool
	}{
		{"old-explicit", &agentpb.GetAgentVersionResponse{}, &appconfig.AppConfig{AppID: "app", Readiness: probe}, runOptions{waitReady: true}, true, false},
		{"old-auto", &agentpb.GetAgentVersionResponse{}, &appconfig.AppConfig{AppID: "app", Readiness: probe}, runOptions{}, false, false},
		{"missing-probe", capable, &appconfig.AppConfig{AppID: "app"}, runOptions{waitReady: true}, true, false},
		{"running-only", capable, &appconfig.AppConfig{AppID: "app"}, runOptions{}, false, true},
		{"shared-explicit", capable, &appconfig.AppConfig{AppID: "app", Readiness: probe, Isolation: "shared-network"}, runOptions{waitReady: true}, true, false},
		{"create-only", capable, &appconfig.AppConfig{AppID: "app"}, runOptions{deploy: true}, false, false},
		{"bad-timeout", capable, &appconfig.AppConfig{AppID: "app", Readiness: probe}, runOptions{readinessTimeout: 1500 * time.Millisecond}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := configureVerifiedDeployment(tc.version, tc.cfg, &tc.opts)
			if (err != nil) != tc.wantErr || tc.opts.verifiedDeployment != tc.verified {
				t.Fatalf("err=%v verified=%v", err, tc.opts.verifiedDeployment)
			}
		})
	}
}

func TestVerifiedServicesWaitInOrderAndPreserveProbeScope(t *testing.T) {
	full := &appconfig.AppConfig{AppID: "app", ServiceName: "db", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: 8080}}}
	db := verifiedServiceConfig(full, &appconfig.AppConfig{AppID: "app", ServiceName: "db"})
	if db.Readiness != nil || len(db.Entitlements) != 1 {
		t.Fatal("inherited HTTP changed runtime access or became a database readiness probe")
	}
	web := verifiedServiceConfig(&appconfig.AppConfig{AppID: "app", ServiceName: "web"}, &appconfig.AppConfig{AppID: "app", ServiceName: "web", Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: 8080}}})
	streams := []*verifiedTestStream{
		{messages: []*agentpb.RunContainerLayersResponse{deploymentStartedResponse(), deploymentTestResponse(agentpb.DeploymentState_RUNNING)}},
		{messages: []*agentpb.RunContainerLayersResponse{deploymentTestResponse(agentpb.DeploymentState_READY)}},
	}
	client := &verifiedTestClient{streams: streams, onSubmit: func(i int) {
		if i == 1 && streams[0].read != 2 {
			t.Fatal("dependent deployed before previous outcome")
		}
	}}
	cfgs := map[string]*appconfig.AppConfig{"db": db, "web": web}
	opts := runOptions{verifiedDeployment: true, serviceDeployment: true, detach: true}
	requests := map[string]*agentpb.CreateContainerRequest{"db": {AppName: "app_db"}, "web": {AppName: "app_web"}}
	if err := runVerifiedServiceGroup(context.Background(), &grpcclient.AgentConnection{ContainerService: client}, []string{"db", "web"}, cfgs, cfgs, requests, nil, opts); err != nil {
		t.Fatal(err)
	}
	var sentDB appconfig.AppConfig
	if err := json.Unmarshal(client.requests[0].AppConfig, &sentDB); err != nil {
		t.Fatal(err)
	}
	if !client.requests[0].Verified || sentDB.Readiness != nil {
		t.Fatal("group implicitly probes inherited HTTP entitlement")
	}
}

func TestRunWithAgentMultiServiceReadinessPreflightBeforeBuild(t *testing.T) {
	probe := &appconfig.ReadinessConfig{TCPSocket: &appconfig.TCPSocketProbe{Port: 8080}}
	for _, tc := range []struct {
		name      string
		features  []string
		readiness *appconfig.ReadinessConfig
		isolation string
		opts      runOptions
		want      string
	}{
		{"old-agent", nil, probe, "", runOptions{waitReady: true}, "does not support verified deployment"},
		{"app-probe-is-not-service-probe", []string{"verified-deployment"}, nil, "", runOptions{waitReady: true}, "requires a readiness probe for app_worker"},
		{"shared-namespace", []string{"verified-deployment"}, probe, "shared-network", runOptions{waitReady: true}, "does not support shared-namespace"},
		{"create-only", []string{"verified-deployment"}, probe, "", runOptions{deploy: true, waitReady: true}, "--deploy only creates"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Any access to provisioning/build/container operations would panic:
			// invalid readiness must fail at the real run entry point first.
			conn := &grpcclient.AgentConnection{}
			conn.CacheAgentVersion(&agentpb.GetAgentVersionResponse{Os: "linux", CpuArchitecture: "arm64", Featureset: tc.features})
			cfg := &appconfig.AppConfig{AppID: "app", Isolation: tc.isolation, Readiness: probe,
				Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: 8080}},
				Services:     map[string]*appconfig.ServiceConfig{"worker": {Context: ".", Readiness: tc.readiness}},
			}
			err := runWithAgent(context.Background(), conn, t.TempDir(), cfg, tc.opts)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("runWithAgent error = %v, want %q", err, tc.want)
			}
		})
	}
}

type verifiedTestProvisioningClient struct {
	agentpb.WendyProvisioningServiceClient
}

func (*verifiedTestProvisioningClient) IsProvisioned(context.Context, *agentpb.IsProvisionedRequest, ...grpc.CallOption) (*agentpb.IsProvisionedResponse, error) {
	return &agentpb.IsProvisionedResponse{}, nil
}

func TestRunWithAgentMultiServiceEnablesVerifiedDeployment(t *testing.T) {
	t.Setenv("WENDY_PUSH_SKIP", "0")
	dir, services := newServiceTree(t, 2)
	services["svc01"].DependsOn = []string{"svc00"}
	services["svc01"].Entitlements = []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: 8081}}
	originalBuild := buildServiceImage
	t.Cleanup(func() { buildServiceImage = originalBuild })
	buildServiceImage = func(context.Context, *grpcclient.AgentConnection, int, string, string, string, string, string, string, map[string]string, string, io.Writer, io.Writer) error {
		return nil
	}
	streams := []*verifiedTestStream{
		{messages: []*agentpb.RunContainerLayersResponse{deploymentTestResponse(agentpb.DeploymentState_RUNNING)}},
		{messages: []*agentpb.RunContainerLayersResponse{deploymentTestResponse(agentpb.DeploymentState_READY)}},
	}
	client := &verifiedTestClient{streams: streams}
	conn := &grpcclient.AgentConnection{ContainerService: client, ProvisioningService: &verifiedTestProvisioningClient{}}
	conn.CacheAgentVersion(&agentpb.GetAgentVersionResponse{Os: "linux", CpuArchitecture: "arm64", Featureset: []string{"verified-deployment"}})
	cfg := &appconfig.AppConfig{AppID: "app", Services: services, Entitlements: []appconfig.Entitlement{{Type: appconfig.EntitlementHTTP, Port: 8080}}}
	if err := runWithAgent(context.Background(), conn, dir, cfg, runOptions{detach: true, builder: "docker"}); err != nil {
		t.Fatal(err)
	}
	if len(client.requests) != 2 {
		t.Fatalf("RunContainer calls = %d, want 2", len(client.requests))
	}
	for i, req := range client.requests {
		var serviceCfg appconfig.AppConfig
		if err := json.Unmarshal(req.AppConfig, &serviceCfg); err != nil {
			t.Fatal(err)
		}
		if !req.Verified || req.AppName != fmt.Sprintf("app_svc%02d", i) || serviceCfg.ContainerName() != req.AppName {
			t.Fatalf("invalid service identity or probe scope: %+v %+v", req, serviceCfg)
		}
		if i == 0 && serviceCfg.Readiness.HasProbe() {
			t.Fatal("inherited app HTTP became a worker readiness probe")
		}
		if i == 1 && (!serviceCfg.Readiness.HasProbe() || serviceCfg.Readiness.TCPSocket.Port != 8081) {
			t.Fatalf("service HTTP probe lost: %+v", serviceCfg.Readiness)
		}
	}
}

func TestRunWithAgentComposeUsesServiceReadiness(t *testing.T) {
	t.Setenv("WENDY_PUSH_SKIP", "0")
	dir := t.TempDir()
	compose := "services:\n  web:\n    image: nginx:latest\n    x-wendy:\n      readiness:\n        tcpSocket:\n          port: 8080\n"
	if err := os.WriteFile(filepath.Join(dir, "compose.yaml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "wendy.json"), []byte(`{"appId":"app"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	client := &verifiedTestClient{streams: []*verifiedTestStream{{messages: []*agentpb.RunContainerLayersResponse{deploymentTestResponse(agentpb.DeploymentState_READY)}}}}
	conn := &grpcclient.AgentConnection{ContainerService: client, ProvisioningService: &verifiedTestProvisioningClient{}}
	conn.CacheAgentVersion(&agentpb.GetAgentVersionResponse{Os: "linux", CpuArchitecture: "arm64", Featureset: []string{"verified-deployment"}})
	if err := runWithAgent(context.Background(), conn, dir, &appconfig.AppConfig{AppID: "app"}, runOptions{detach: true, waitReady: true, builder: "docker"}); err != nil {
		t.Fatal(err)
	}
	if len(client.requests) != 1 || !client.requests[0].Verified || client.requests[0].AppName != "app_web" {
		t.Fatalf("lost Compose service readiness: %+v", client.requests)
	}
	var sent appconfig.AppConfig
	if err := json.Unmarshal(client.requests[0].AppConfig, &sent); err != nil {
		t.Fatal(err)
	}
	if !sent.Readiness.HasProbe() || sent.Readiness.TCPSocket.Port != 8080 {
		t.Fatalf("Compose readiness not sent to the agent: %+v", sent.Readiness)
	}
}

func TestVerifiedServiceLogsKeepPrefixesAndJSONClean(t *testing.T) {
	oldJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJSON })
	stream := &verifiedTestStream{messages: []*agentpb.RunContainerLayersResponse{
		{ResponseType: &agentpb.RunContainerLayersResponse_StdoutOutput{StdoutOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{Data: []byte("booting\n")}}},
		deploymentTestResponse(agentpb.DeploymentState_RUNNING),
		{ResponseType: &agentpb.RunContainerLayersResponse_StdoutOutput{StdoutOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{Data: []byte("par")}}},
		{ResponseType: &agentpb.RunContainerLayersResponse_StdoutOutput{StdoutOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{Data: []byte("tial line\n")}}},
		{ResponseType: &agentpb.RunContainerLayersResponse_StderrOutput{StderrOutput: &agentpb.RunContainerLayersResponse_ConsoleOutput{Data: []byte("error without newline")}}},
	}}
	client := &verifiedTestClient{streams: []*verifiedTestStream{stream}}
	cfgs := map[string]*appconfig.AppConfig{"web": {AppID: "app", ServiceName: "web"}}
	requests := map[string]*agentpb.CreateContainerRequest{"web": {AppName: "app_web"}}
	var runErr error
	stdout, stderr := captureBoth(t, func() {
		runErr = runVerifiedServiceGroup(context.Background(), &grpcclient.AgentConnection{ContainerService: client}, []string{"web"}, cfgs, cfgs, requests, nil, runOptions{verifiedDeployment: true, serviceDeployment: true})
	})
	if runErr != nil {
		t.Fatal(runErr)
	}
	var outcome map[string]any
	if err := json.Unmarshal([]byte(stdout), &outcome); err != nil || outcome["state"] != "RUNNING" {
		t.Fatalf("deployment stdout was not one JSON outcome: %q (%v)", stdout, err)
	}
	for _, want := range []string{"[web] booting", "[web] partial line", "[web] error without newline"} {
		if !strings.Contains(stderr, want) {
			t.Fatalf("missing %q in service logs: %q", want, stderr)
		}
	}
}

func TestPostStartHookOutputDoesNotCorruptJSON(t *testing.T) {
	oldJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJSON })
	cfg := &appconfig.AppConfig{AppID: "app", Hooks: &appconfig.HooksConfig{PostStart: &appconfig.HookCommand{CLI: "echo hook-output"}}}
	var hookErr error
	stdout, stderr := captureBoth(t, func() {
		cmd := startPostStartHook(context.Background(), cfg, "localhost", "")
		if cmd == nil {
			hookErr = fmt.Errorf("hook did not start")
			return
		}
		hookErr = cmd.Wait()
	})
	if hookErr != nil || stdout != "" || !strings.Contains(stderr, "hook-output") {
		t.Fatalf("hook output escaped to JSON stdout: stdout=%q stderr=%q err=%v", stdout, stderr, hookErr)
	}
}
