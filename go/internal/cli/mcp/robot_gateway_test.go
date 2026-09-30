package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func gatewayTestConfig() RobotGatewayConfig {
	return RobotGatewayConfig{
		Robots:       []GatewayRobot{{ID: "alpha", Name: "Alpha", Device: "alpha.local:50052", Apps: []string{"companion"}, AllowCamera: true}, {ID: "beta", Name: "Beta", Device: "beta.local:50052", Apps: []string{"companion"}}},
		Grants:       []GatewayGrant{{Subject: "alice", Robots: []string{"alpha"}, Scopes: robotGatewayScopes}, {Subject: "bob", Robots: []string{"beta"}, Scopes: []string{RobotReadScope}}},
		LocalSubject: "alice",
		HTTP:         &GatewayHTTPConfig{ResourceURL: "http://localhost:8788/mcp", DevelopmentTokens: []GatewayDevelopmentToken{{Subject: "alice", TokenEnv: "ALICE_TOKEN"}, {Subject: "bob", TokenEnv: "BOB_TOKEN"}}},
	}
}

func gatewayTestEnv(key string) string { return strings.Repeat(key, 8) }

func gatewayHTTPClient(t *testing.T, endpoint, token string) *mcpclient.Client {
	t.Helper()
	c, err := mcpclient.NewStreamableHttpClient(endpoint+"/mcp", transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + token}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	var init mcpgo.InitializeRequest
	init.Params.ProtocolVersion = mcpgo.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcpgo.Implementation{Name: "test", Version: "1"}
	if _, err := c.Initialize(ctx, init); err != nil {
		t.Fatal(err)
	}
	return c
}

func gatewayTestHTTP(t *testing.T, g *RobotGateway) *httptest.Server {
	t.Helper()
	h, err := g.httpHandler(http.DefaultClient, gatewayTestEnv)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

func TestRobotGatewayHTTPIsolationAndAuthorization(t *testing.T) {
	var connects atomic.Int32
	g, err := NewRobotGateway(gatewayTestConfig(), func(context.Context, string) (*grpcclient.AgentConnection, error) {
		connects.Add(1)
		return nil, fmt.Errorf("offline secret address")
	})
	if err != nil {
		t.Fatal(err)
	}
	s := gatewayTestHTTP(t, g)
	for _, tc := range []struct{ subject, token, robot string }{{"alice", "ALICE_TOKEN", "alpha"}, {"bob", "BOB_TOKEN", "beta"}} {
		c := gatewayHTTPClient(t, s.URL, gatewayTestEnv(tc.token))
		result, err := c.CallTool(context.Background(), callToolReq("list_robots", map[string]any{}))
		if err != nil || result.IsError {
			t.Fatalf("list: %v %v", result, err)
		}
		b, _ := json.Marshal(result.StructuredContent)
		if !bytes.Contains(b, []byte(tc.robot)) || bytes.Contains(b, []byte(".local")) {
			t.Fatalf("bad catalog %s", b)
		}
		other := "alpha"
		if tc.robot == "alpha" {
			other = "beta"
		}
		if bytes.Contains(b, []byte(other)) {
			t.Fatalf("leaked other robot: %s", b)
		}
		result, err = c.CallTool(context.Background(), callToolReq("inspect_robot", map[string]any{"robot_id": other}))
		if err != nil || !result.IsError {
			t.Fatalf("cross-account call allowed: %v %v", result, err)
		}
		tools, err := c.ListTools(context.Background(), mcpgo.ListToolsRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if tc.subject == "bob" {
			for _, tool := range tools.Tools {
				if !*tool.Annotations.ReadOnlyHint {
					t.Fatal("read-only account advertised a write tool")
				}
			}
		}
	}
	if connects.Load() != 0 {
		t.Fatal("unauthorized request reached a device")
	}
	for _, tc := range []struct {
		token, origin string
		status        int
	}{{"", "", 401}, {"wrong", "", 401}, {gatewayTestEnv("ALICE_TOKEN"), "https://attacker.example", 403}} {
		req, _ := http.NewRequest("POST", s.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		req.Header.Set("Authorization", "Bearer "+tc.token)
		req.Header.Set("Origin", tc.origin)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.status {
			t.Fatalf("got status %d want %d", resp.StatusCode, tc.status)
		}
	}
}

type gatewayFixtureAgent struct {
	agentpb.UnimplementedWendyContainerServiceServer
	name    string
	running atomic.Bool
	starts  atomic.Int32
}

type gatewayFixtureInfo struct {
	agentpb.UnimplementedWendyAgentServiceServer
	fixture *gatewayFixtureAgent
}
type gatewayFixtureVideo struct {
	agentpb.UnimplementedWendyVideoServiceServer
}

func (f *gatewayFixtureInfo) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	return &agentpb.GetAgentVersionResponse{Version: f.fixture.name}, nil
}
func (f *gatewayFixtureVideo) ListVideoDevices(context.Context, *agentpb.ListVideoDevicesRequest) (*agentpb.ListVideoDevicesResponse, error) {
	return &agentpb.ListVideoDevicesResponse{}, nil
}
func (f *gatewayFixtureAgent) ListContainers(_ *agentpb.ListContainersRequest, s grpc.ServerStreamingServer[agentpb.ListContainersResponse]) error {
	state := agentpb.AppRunningState_STOPPED
	if f.running.Load() {
		state = agentpb.AppRunningState_RUNNING
	}
	for _, name := range []string{"companion", "private-app"} {
		if err := s.Send(&agentpb.ListContainersResponse{Container: &agentpb.AppContainer{AppName: name, RunningState: state}}); err != nil {
			return err
		}
	}
	return nil
}
func (f *gatewayFixtureAgent) StartContainer(_ *agentpb.StartContainerRequest, s grpc.ServerStreamingServer[agentpb.RunContainerLayersResponse]) error {
	f.starts.Add(1)
	f.running.Store(true)
	return s.Send(&agentpb.RunContainerLayersResponse{ResponseType: &agentpb.RunContainerLayersResponse_Started_{Started: &agentpb.RunContainerLayersResponse_Started{}}})
}
func (f *gatewayFixtureAgent) StopContainer(context.Context, *agentpb.StopContainerRequest) (*agentpb.StopContainerResponse, error) {
	f.running.Store(false)
	return &agentpb.StopContainerResponse{}, nil
}

func gatewayFixture(t *testing.T, name string) (string, *gatewayFixtureAgent) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &gatewayFixtureAgent{name: name}
	srv := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(srv, &gatewayFixtureInfo{fixture: f})
	agentpb.RegisterWendyContainerServiceServer(srv, f)
	agentpb.RegisterWendyVideoServiceServer(srv, &gatewayFixtureVideo{})
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(srv.Stop)
	return ln.Addr().String(), f
}

func TestRobotGatewayConcurrentDeviceCallsAndVerifiedAppState(t *testing.T) {
	a, agent := gatewayFixture(t, "alpha-version")
	b, _ := gatewayFixture(t, "beta-version")
	g, err := NewRobotGateway(gatewayTestConfig(), func(_ context.Context, device string) (*grpcclient.AgentConnection, error) {
		addr := a
		if strings.HasPrefix(device, "beta") {
			addr = b
		}
		cc, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, err
		}
		return grpcclient.NewFromConn(cc), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s := gatewayTestHTTP(t, g)
	alice := gatewayHTTPClient(t, s.URL, gatewayTestEnv("ALICE_TOKEN"))
	bob := gatewayHTTPClient(t, s.URL, gatewayTestEnv("BOB_TOKEN"))
	var wg sync.WaitGroup
	for n := 0; n < 12; n++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c, id := alice, "alpha"
			if n%2 != 0 {
				c, id = bob, "beta"
			}
			result, err := c.CallTool(context.Background(), callToolReq("inspect_robot", map[string]any{"robot_id": id}))
			if err != nil {
				t.Error(err)
				return
			}
			data, _ := json.Marshal(result.StructuredContent)
			if result.IsError || !bytes.Contains(data, []byte(id+"-version")) || bytes.Contains(data, []byte("private-app")) {
				t.Errorf("wrong device/result %s", data)
			}
		}(n)
	}
	wg.Wait()
	for _, action := range []string{"start", "stop"} {
		result, err := alice.CallTool(context.Background(), callToolReq(action+"_robot_app", map[string]any{"robot_id": "alpha", "app_name": "companion"}))
		if err != nil || result.IsError {
			t.Fatalf("app control: %v %v", result, err)
		}
		data := structuredMap(t, result)
		if data["state_verified"] != true || data["readiness"] != "unknown" {
			t.Fatalf("state not verified: %v", data)
		}
	}
	result, err := alice.CallTool(context.Background(), callToolReq("start_robot_app", map[string]any{"robot_id": "alpha", "app_name": "private-app"}))
	if err != nil || !result.IsError || agent.starts.Load() != 1 {
		t.Fatal("unpermitted app was started")
	}
}

func TestRobotGatewayExportContractAndValidation(t *testing.T) {
	cfg := gatewayTestConfig()
	descriptor := gatewayTool("set_message", "Set the companion message.", mutating(), mcpgo.WithString("message", mcpgo.Required(), mcpgo.MaxLength(50)))
	cfg.Robots[0].Exports = []GatewayExport{{Name: "companion_message", App: "companion", Tool: descriptor}}
	g, err := NewRobotGateway(cfg, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		return &grpcclient.AgentConnection{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	app := &lifecycleAppClient{tools: []mcpgo.Tool{descriptor}}
	g.connectApp = func(context.Context, context.Context, *grpcclient.AgentConnection, string) (appMCPClient, error) {
		return app, nil
	}
	s := gatewayTestHTTP(t, g)
	c := gatewayHTTPClient(t, s.URL, gatewayTestEnv("ALICE_TOKEN"))
	call := func(args map[string]any) *mcpgo.CallToolResult {
		result, err := c.CallTool(context.Background(), callToolReq("companion_message", args))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	valid := map[string]any{"robot_id": "alpha", "arguments": map[string]any{"message": "hello"}}
	if call(valid).IsError || app.calls.Load() != 1 {
		t.Fatal("valid export did not run")
	}
	if !call(map[string]any{"robot_id": "alpha", "arguments": map[string]any{"message": 42}}).IsError || app.calls.Load() != 1 {
		t.Fatal("invalid schema reached app")
	}
	app.tools[0].Description = "Different behavior"
	if !call(valid).IsError || app.calls.Load() != 1 {
		t.Fatal("changed contract reached app")
	}
	if !call(map[string]any{"robot_id": "beta", "arguments": map[string]any{"message": "wrong robot"}}).IsError {
		t.Fatal("export ran on another robot")
	}
}

func TestRobotGatewayExportLocalSchemaReferences(t *testing.T) {
	cfg := gatewayTestConfig()
	descriptor := mcpgo.NewToolWithRawSchema("set_message", "Set message", json.RawMessage(`{"type":"object","properties":{"message":{"$ref":"#/$defs/message"}},"required":["message"],"$defs":{"message":{"type":"string","maxLength":5}}}`))
	descriptor.Annotations = gatewayTool("unused", "unused", mutating()).Annotations
	cfg.Robots[0].Exports = []GatewayExport{{Name: "companion_message", App: "companion", Tool: descriptor}}
	g, err := NewRobotGateway(cfg, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		return &grpcclient.AgentConnection{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	app := &lifecycleAppClient{tools: []mcpgo.Tool{descriptor}}
	g.connectApp = func(context.Context, context.Context, *grpcclient.AgentConnection, string) (appMCPClient, error) {
		return app, nil
	}
	c := gatewayHTTPClient(t, gatewayTestHTTP(t, g).URL, gatewayTestEnv("ALICE_TOKEN"))
	for _, tc := range []struct {
		message string
		isError bool
	}{{"hello", false}, {"too long", true}} {
		result, err := c.CallTool(context.Background(), callToolReq("companion_message", map[string]any{"robot_id": "alpha", "arguments": map[string]any{"message": tc.message}}))
		if err != nil || result.IsError != tc.isError {
			t.Fatalf("nested reference: result=%v error=%v", result, err)
		}
	}
	if app.calls.Load() != 1 {
		t.Fatal("invalid nested arguments reached app")
	}
	for _, reference := range []string{"https://example.com/schema.json", "file:///etc/passwd"} {
		descriptor.RawInputSchema = json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"message":{"$ref":%q}}}`, reference))
		cfg.Robots[0].Exports[0].Tool = descriptor
		if _, err := NewRobotGateway(cfg, g.connect); err == nil {
			t.Fatal("external schema reference was accepted")
		}
	}
}

func TestRobotGatewayStdioAndPanel(t *testing.T) {
	g, err := NewRobotGateway(gatewayTestConfig(), func(context.Context, string) (*grpcclient.AgentConnection, error) { return nil, fmt.Errorf("offline") })
	if err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	defer inR.Close()
	defer outR.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = context.WithValue(ctx, gatewayPrincipalKey{}, gatewayPrincipal{"alice", robotGatewayScopes})
	done := make(chan error, 1)
	go func() { done <- server.NewStdioServer(g.protocol).Listen(ctx, inR, outW); outW.Close() }()
	defer func() { inW.Close(); cancel(); <-done }()
	enc := json.NewEncoder(inW)
	dec := json.NewDecoder(outR)
	for id, method := range []string{"initialize", "tools/list", "resources/read"} {
		params := map[string]any{}
		if method == "initialize" {
			params = map[string]any{"protocolVersion": mcpgo.LATEST_PROTOCOL_VERSION, "capabilities": map[string]any{}, "clientInfo": map[string]string{"name": "test", "version": "1"}}
		}
		if method == "resources/read" {
			params["uri"] = robotPanelURI
		}
		if err := enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id + 1, "method": method, "params": params}); err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := dec.Decode(&result); err != nil {
			t.Fatal(err)
		}
		if result["error"] != nil {
			t.Fatalf("stdio: %v", result)
		}
		if method == "resources/read" {
			raw, _ := json.Marshal(result)
			if !bytes.Contains(raw, []byte("ui/update-model-context")) {
				t.Fatal("missing panel bridge")
			}
		}
	}
}
