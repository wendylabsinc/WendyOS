package mcp

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type lifecycleAppClient struct {
	mu      sync.Mutex
	tools   []mcpgo.Tool
	listErr error
	calls   atomic.Int32
	closed  atomic.Int32
	call    func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)
}

func (c *lifecycleAppClient) ListTools(context.Context, mcpgo.ListToolsRequest) (*mcpgo.ListToolsResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return &mcpgo.ListToolsResult{Tools: append([]mcpgo.Tool(nil), c.tools...)}, c.listErr
}

func (c *lifecycleAppClient) CallTool(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	c.calls.Add(1)
	if c.call != nil {
		return c.call(ctx, req)
	}
	return mcpgo.NewToolResultText(req.Params.Name), nil
}

func (c *lifecycleAppClient) Close() error {
	c.closed.Add(1)
	return nil
}

func waitContainerMCP(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for app MCP lifecycle")
		}
		time.Sleep(time.Millisecond)
	}
}

func runningMCPApp(name string) *agentpb.AppContainer {
	return &agentpb.AppContainer{AppName: name, AppVersion: "v1", McpPort: 8000, RunningState: agentpb.AppRunningState_RUNNING}
}

func TestContainerMCP_ConnectAfterStartupRefreshesSchemasAndCleansUp(t *testing.T) {
	s := New(&config.Config{}, nil)
	srv := server.NewMCPServer("test", "0", server.WithToolCapabilities(true))
	m := newContainerMCPManager(s, srv)
	m.interval = time.Hour
	m.listApps = func(context.Context, *grpcclient.AgentConnection) ([]*agentpb.AppContainer, error) {
		return []*agentpb.AppContainer{runningMCPApp("robot")}, nil
	}
	client := &lifecycleAppClient{tools: []mcpgo.Tool{mcpgo.NewTool("camera")}}
	var connects atomic.Int32
	m.connectApp = func(context.Context, context.Context, *grpcclient.AgentConnection, string) (appMCPClient, error) {
		connects.Add(1)
		return client, nil
	}
	stop := s.startContainerMCPManager(context.Background(), m)
	t.Cleanup(stop)
	if len(srv.ListTools()) != 0 {
		t.Fatal("tools registered without a device")
	}
	s.SetConn(&grpcclient.AgentConnection{})
	waitContainerMCP(t, func() bool { return srv.GetTool("robot__camera") != nil })
	result, err := srv.GetTool("robot__camera").Handler(context.Background(), mcpgo.CallToolRequest{})
	if err != nil || result.Content[0].(mcpgo.TextContent).Text != "camera" {
		t.Fatalf("original tool name was not forwarded: result=%+v, error=%v", result, err)
	}
	client.mu.Lock()
	client.tools = []mcpgo.Tool{mcpgo.NewTool("robot_status")}
	client.mu.Unlock()
	s.refreshContainerMCPTools()
	waitContainerMCP(t, func() bool { return srv.GetTool("robot__robot_status") != nil })
	if srv.GetTool("robot__camera") != nil || connects.Load() != 1 {
		t.Fatal("schema refresh should remove old tools and retain the app connection")
	}
	stop()
	if len(srv.ListTools()) != 0 || client.closed.Load() != 1 {
		t.Fatal("shutdown did not remove tools and close the app exactly once")
	}
}

func TestContainerMCP_StartupConnectionTriggersDiscovery(t *testing.T) {
	conn := &grpcclient.AgentConnection{}
	s := New(&config.Config{}, func(context.Context, string) (*grpcclient.AgentConnection, error) { return conn, nil })
	srv := server.NewMCPServer("test", "0")
	m := newContainerMCPManager(s, srv)
	m.interval = time.Hour
	m.listApps = func(context.Context, *grpcclient.AgentConnection) ([]*agentpb.AppContainer, error) {
		return []*agentpb.AppContainer{runningMCPApp("robot")}, nil
	}
	m.connectApp = func(context.Context, context.Context, *grpcclient.AgentConnection, string) (appMCPClient, error) {
		return &lifecycleAppClient{tools: []mcpgo.Tool{mcpgo.NewTool("status")}}, nil
	}
	t.Cleanup(s.startContainerMCPManager(context.Background(), m))
	if err := s.ConnectToOnStartup(context.Background(), "robot.local"); err != nil {
		t.Fatal(err)
	}
	waitContainerMCP(t, func() bool { return srv.GetTool("robot__status") != nil })
}

func TestContainerMCP_StoppedAppRemovedAndTransientRestartRetried(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.SetConn(&grpcclient.AgentConnection{})
	srv := server.NewMCPServer("test", "0")
	m := newContainerMCPManager(s, srv)
	m.interval = 10 * time.Millisecond
	var running atomic.Bool
	running.Store(true)
	m.listApps = func(context.Context, *grpcclient.AgentConnection) ([]*agentpb.AppContainer, error) {
		app := runningMCPApp("robot")
		if !running.Load() {
			app.RunningState = agentpb.AppRunningState_STOPPED
		}
		return []*agentpb.AppContainer{app}, nil
	}
	first := &lifecycleAppClient{tools: []mcpgo.Tool{mcpgo.NewTool("status")}}
	second := &lifecycleAppClient{tools: []mcpgo.Tool{mcpgo.NewTool("status")}}
	var attempts atomic.Int32
	m.connectApp = func(context.Context, context.Context, *grpcclient.AgentConnection, string) (appMCPClient, error) {
		switch attempts.Add(1) {
		case 1:
			return first, nil
		case 2:
			return nil, errors.New("app is still starting")
		default:
			return second, nil
		}
	}
	t.Cleanup(s.startContainerMCPManager(context.Background(), m))
	waitContainerMCP(t, func() bool { return srv.GetTool("robot__status") != nil })
	old := srv.GetTool("robot__status")
	running.Store(false)
	waitContainerMCP(t, func() bool { return first.closed.Load() == 1 && srv.GetTool("robot__status") == nil })
	result, err := old.Handler(context.Background(), mcpgo.CallToolRequest{})
	if err != nil || !result.IsError || first.calls.Load() != 0 {
		t.Fatal("stopped app's cached tool handler remained callable")
	}
	running.Store(true)
	waitContainerMCP(t, func() bool { return attempts.Load() >= 3 && srv.GetTool("robot__status") != nil })
	if len(s.proxyDiagnostics()) == 0 {
		t.Fatal("transient failure was not recorded")
	}
}

func TestContainerMCP_DeviceSwitchDuringDiscoveryDiscardsOldTools(t *testing.T) {
	oldConn, newConn := &grpcclient.AgentConnection{}, &grpcclient.AgentConnection{}
	s := New(&config.Config{}, nil)
	s.SetConn(oldConn)
	srv := server.NewMCPServer("test", "0")
	m := newContainerMCPManager(s, srv)
	m.interval = time.Hour
	m.listApps = func(_ context.Context, conn *grpcclient.AgentConnection) ([]*agentpb.AppContainer, error) {
		if conn == oldConn {
			return []*agentpb.AppContainer{runningMCPApp("old")}, nil
		}
		return []*agentpb.AppContainer{runningMCPApp("new")}, nil
	}
	started, release := make(chan struct{}), make(chan struct{})
	old := &lifecycleAppClient{tools: []mcpgo.Tool{mcpgo.NewTool("status")}}
	m.connectApp = func(_ context.Context, _ context.Context, conn *grpcclient.AgentConnection, _ string) (appMCPClient, error) {
		if conn == oldConn {
			close(started)
			// Simulate a transport returning a completed handshake after cancellation.
			<-release
			return old, nil
		}
		return &lifecycleAppClient{tools: []mcpgo.Tool{mcpgo.NewTool("status")}}, nil
	}
	t.Cleanup(s.startContainerMCPManager(context.Background(), m))
	<-started
	s.SetConn(newConn)
	close(release)
	waitContainerMCP(t, func() bool { return srv.GetTool("new__status") != nil })
	if srv.GetTool("old__status") != nil || old.closed.Load() != 1 {
		t.Fatal("old device discovery was published or its proxy leaked")
	}
}

func TestContainerMCP_DeviceSwitchInvalidatesInFlightCallsAndCachedHandlers(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.SetConn(&grpcclient.AgentConnection{})
	srv := server.NewMCPServer("test", "0")
	m := newContainerMCPManager(s, srv)
	m.interval = time.Hour
	m.listApps = func(context.Context, *grpcclient.AgentConnection) ([]*agentpb.AppContainer, error) {
		return []*agentpb.AppContainer{runningMCPApp("robot")}, nil
	}
	callStarted := make(chan struct{})
	client := &lifecycleAppClient{tools: []mcpgo.Tool{mcpgo.NewTool("move")}}
	client.call = func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		close(callStarted)
		<-ctx.Done()
		// A completed response from the old connection must still be rejected.
		return mcpgo.NewToolResultText("old success"), nil
	}
	m.connectApp = func(context.Context, context.Context, *grpcclient.AgentConnection, string) (appMCPClient, error) {
		return client, nil
	}
	t.Cleanup(s.startContainerMCPManager(context.Background(), m))
	waitContainerMCP(t, func() bool { return srv.GetTool("robot__move") != nil })
	cached := srv.GetTool("robot__move")
	results := make(chan *mcpgo.CallToolResult, 1)
	go func() { result, _ := cached.Handler(context.Background(), mcpgo.CallToolRequest{}); results <- result }()
	<-callStarted
	s.SetConn(nil)
	if srv.GetTool("robot__move") != nil {
		t.Fatal("disconnect did not synchronously remove old descriptors")
	}
	select {
	case result := <-results:
		if !result.IsError {
			t.Fatal("accepted an old connection's in-flight result")
		}
	case <-time.After(time.Second):
		t.Fatal("connection switch did not cancel the active call")
	}
	result, err := cached.Handler(context.Background(), mcpgo.CallToolRequest{})
	if err != nil || !result.IsError || client.calls.Load() != 1 {
		t.Fatal("cached handler invoked the old app after disconnect")
	}
}

func TestContainerMCP_DiscoveryHasDeadlineAndShutdownCancels(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.SetConn(&grpcclient.AgentConnection{})
	m := newContainerMCPManager(s, server.NewMCPServer("test", "0"))
	m.timeout = 10 * time.Millisecond
	m.interval = time.Hour
	started := make(chan struct{}, 2)
	m.listApps = func(ctx context.Context, _ *grpcclient.AgentConnection) ([]*agentpb.AppContainer, error) {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("container discovery has no deadline")
		}
		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	stop := s.startContainerMCPManager(context.Background(), m)
	t.Cleanup(stop)
	<-started
	waitContainerMCP(t, func() bool { return len(s.proxyDiagnostics()) > 0 })
	s.refreshContainerMCPTools()
	<-started
	stopped := make(chan struct{})
	go func() { stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("shutdown failed to cancel active discovery")
	}
}

func TestContainerMCP_RedeployReplacesProxyAndDeletedAppDisappears(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.SetConn(&grpcclient.AgentConnection{})
	srv := server.NewMCPServer("test", "0")
	m := newContainerMCPManager(s, srv)
	m.interval = time.Hour
	var version atomic.Int32
	version.Store(1)
	m.listApps = func(context.Context, *grpcclient.AgentConnection) ([]*agentpb.AppContainer, error) {
		app := runningMCPApp("robot")
		switch version.Load() {
		case 0:
			return nil, nil
		case 2:
			app.AppVersion = "v2"
		}
		return []*agentpb.AppContainer{app}, nil
	}
	first := &lifecycleAppClient{tools: []mcpgo.Tool{mcpgo.NewTool("status")}}
	second := &lifecycleAppClient{tools: []mcpgo.Tool{mcpgo.NewTool("status", mcpgo.WithDescription("new schema"))}}
	m.connectApp = func(context.Context, context.Context, *grpcclient.AgentConnection, string) (appMCPClient, error) {
		if version.Load() == 1 {
			return first, nil
		}
		return second, nil
	}
	t.Cleanup(s.startContainerMCPManager(context.Background(), m))
	waitContainerMCP(t, func() bool { return srv.GetTool("robot__status") != nil })
	cached := srv.GetTool("robot__status")
	version.Store(2)
	s.refreshContainerMCPTools()
	waitContainerMCP(t, func() bool {
		tool := srv.GetTool("robot__status")
		return tool != nil && tool.Tool.Description == "new schema"
	})
	result, err := cached.Handler(context.Background(), mcpgo.CallToolRequest{})
	if err != nil || !result.IsError || first.closed.Load() != 1 {
		t.Fatal("redeploy left the old app connected or callable")
	}
	version.Store(0)
	s.refreshContainerMCPTools()
	waitContainerMCP(t, func() bool { return srv.GetTool("robot__status") == nil && second.closed.Load() == 1 })
}

func TestContainerMCP_RejectsCollidingAppPrefixes(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.SetConn(&grpcclient.AgentConnection{})
	srv := server.NewMCPServer("test", "0")
	m := newContainerMCPManager(s, srv)
	m.interval = time.Hour
	m.listApps = func(context.Context, *grpcclient.AgentConnection) ([]*agentpb.AppContainer, error) {
		return []*agentpb.AppContainer{runningMCPApp("my-robot"), runningMCPApp("my_robot")}, nil
	}
	var connects atomic.Int32
	m.connectApp = func(context.Context, context.Context, *grpcclient.AgentConnection, string) (appMCPClient, error) {
		connects.Add(1)
		return &lifecycleAppClient{tools: []mcpgo.Tool{mcpgo.NewTool("status")}}, nil
	}
	t.Cleanup(s.startContainerMCPManager(context.Background(), m))
	waitContainerMCP(t, func() bool { return len(s.proxyDiagnostics()) == 2 })
	if connects.Load() != 0 || len(srv.ListTools()) != 0 {
		t.Fatal("colliding app prefixes should not publish ambiguous tool destinations")
	}
}

func TestContainerMCP_SchemaRefreshInvalidatesCachedAndInFlightCalls(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.SetConn(&grpcclient.AgentConnection{})
	srv := server.NewMCPServer("test", "0")
	m := newContainerMCPManager(s, srv)
	m.interval = time.Hour
	m.listApps = func(context.Context, *grpcclient.AgentConnection) ([]*agentpb.AppContainer, error) {
		return []*agentpb.AppContainer{runningMCPApp("robot")}, nil
	}
	started := make(chan struct{})
	client := &lifecycleAppClient{tools: []mcpgo.Tool{mcpgo.NewTool("move")}}
	client.call = func(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		close(started)
		<-ctx.Done()
		return mcpgo.NewToolResultText("obsolete result"), nil
	}
	m.connectApp = func(context.Context, context.Context, *grpcclient.AgentConnection, string) (appMCPClient, error) {
		return client, nil
	}
	t.Cleanup(s.startContainerMCPManager(context.Background(), m))
	waitContainerMCP(t, func() bool { return srv.GetTool("robot__move") != nil })
	cached := srv.GetTool("robot__move")
	results := make(chan *mcpgo.CallToolResult, 1)
	go func() { result, _ := cached.Handler(context.Background(), mcpgo.CallToolRequest{}); results <- result }()
	<-started
	client.mu.Lock()
	client.tools = []mcpgo.Tool{mcpgo.NewTool("status")}
	client.mu.Unlock()
	s.refreshContainerMCPTools()
	waitContainerMCP(t, func() bool { return srv.GetTool("robot__status") != nil })
	select {
	case result := <-results:
		if !result.IsError {
			t.Fatal("schema refresh accepted an obsolete in-flight result")
		}
	case <-time.After(time.Second):
		t.Fatal("schema refresh did not cancel the old call")
	}
	result, err := cached.Handler(context.Background(), mcpgo.CallToolRequest{})
	if err != nil || !result.IsError || client.calls.Load() != 1 || client.closed.Load() != 0 {
		t.Fatal("schema refresh should invalidate cached handlers while keeping the client")
	}
}

func TestContainerMCP_FullNameCollisionKeepsOwnerAndRecovers(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.SetConn(&grpcclient.AgentConnection{})
	srv := server.NewMCPServer("test", "0")
	m := newContainerMCPManager(s, srv)
	m.interval = time.Hour
	var removeFirst atomic.Bool
	m.listApps = func(context.Context, *grpcclient.AgentConnection) ([]*agentpb.AppContainer, error) {
		apps := []*agentpb.AppContainer{runningMCPApp("a__b")}
		if !removeFirst.Load() {
			apps = append(apps, runningMCPApp("a"))
		}
		return apps, nil
	}
	m.connectApp = func(_ context.Context, _ context.Context, _ *grpcclient.AgentConnection, name string) (appMCPClient, error) {
		toolName := "c"
		if name == "a" {
			toolName = "b__c"
		}
		return &lifecycleAppClient{tools: []mcpgo.Tool{mcpgo.NewTool(toolName)}, call: func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			return mcpgo.NewToolResultText(name), nil
		}}, nil
	}
	t.Cleanup(s.startContainerMCPManager(context.Background(), m))
	waitContainerMCP(t, func() bool { return len(s.proxyDiagnostics()) > 0 })
	tool := srv.GetTool("a__b__c")
	if tool == nil {
		t.Fatal("first owner lost its tool")
	}
	result, err := tool.Handler(context.Background(), mcpgo.CallToolRequest{})
	if err != nil || result.Content[0].(mcpgo.TextContent).Text != "a" {
		t.Fatal("colliding name was routed to a different app")
	}
	removeFirst.Store(true)
	s.refreshContainerMCPTools()
	waitContainerMCP(t, func() bool {
		tool := srv.GetTool("a__b__c")
		if tool == nil {
			return false
		}
		result, err := tool.Handler(context.Background(), mcpgo.CallToolRequest{})
		return err == nil && !result.IsError && result.Content[0].(mcpgo.TextContent).Text == "a__b"
	})
}

func TestContainerMCP_FullNameCollisionPreservesNativeTool(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.SetConn(&grpcclient.AgentConnection{})
	srv := server.NewMCPServer("test", "0")
	srv.AddTool(mcpgo.NewTool("robot__status"), func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("native"), nil
	})
	m := newContainerMCPManager(s, srv)
	m.interval = time.Hour
	m.listApps = func(context.Context, *grpcclient.AgentConnection) ([]*agentpb.AppContainer, error) {
		return []*agentpb.AppContainer{runningMCPApp("robot")}, nil
	}
	m.connectApp = func(context.Context, context.Context, *grpcclient.AgentConnection, string) (appMCPClient, error) {
		return &lifecycleAppClient{tools: []mcpgo.Tool{mcpgo.NewTool("status")}}, nil
	}
	stop := s.startContainerMCPManager(context.Background(), m)
	t.Cleanup(stop)
	waitContainerMCP(t, func() bool { return len(s.proxyDiagnostics()) > 0 })
	stop()
	tool := srv.GetTool("robot__status")
	if tool == nil {
		t.Fatal("app cleanup deleted a native tool")
	}
	result, err := tool.Handler(context.Background(), mcpgo.CallToolRequest{})
	if err != nil || result.Content[0].(mcpgo.TextContent).Text != "native" {
		t.Fatal("app collision replaced native tool")
	}
}
