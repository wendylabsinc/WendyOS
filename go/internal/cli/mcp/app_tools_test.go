package mcp

import (
	"context"
	"io"
	"net"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// --- harness ----------------------------------------------------------------

// fakeAppAgent is an agent that both lists containers and relays StreamMCP to a
// real in-process MCP server per app, so these tests drive the whole path the
// reconciler uses: list -> proxy -> initialize -> tools/list -> call.
type fakeAppAgent struct {
	agentpb.UnimplementedWendyContainerServiceServer
	mu         sync.Mutex
	containers []*agentpb.AppContainer
	backends   map[string]string // app name -> host:port of its MCP server
}

func (f *fakeAppAgent) setApps(containers []*agentpb.AppContainer, backends map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.containers = containers
	f.backends = backends
}

func (f *fakeAppAgent) ListContainers(_ *agentpb.ListContainersRequest, stream agentpb.WendyContainerService_ListContainersServer) error {
	f.mu.Lock()
	containers := append([]*agentpb.AppContainer(nil), f.containers...)
	f.mu.Unlock()
	for _, c := range containers {
		if err := stream.Send(&agentpb.ListContainersResponse{Container: c}); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeAppAgent) StreamMCP(stream grpc.BidiStreamingServer[agentpb.MCPChunk, agentpb.MCPChunk]) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	names := md.Get("app-name")
	if len(names) == 0 {
		return nil
	}
	f.mu.Lock()
	addr := f.backends[names[0]]
	f.mu.Unlock()
	if addr == "" {
		return nil
	}

	backend, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer backend.Close()

	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 32*1024)
		for {
			n, readErr := backend.Read(buf)
			if n > 0 {
				if sendErr := stream.Send(&agentpb.MCPChunk{Data: buf[:n]}); sendErr != nil {
					return
				}
			}
			if readErr != nil {
				return
			}
		}
	}()

	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			break
		}
		if _, err := backend.Write(chunk.GetData()); err != nil {
			break
		}
	}
	backend.Close()
	<-done
	return nil
}

func startFakeAppAgent(t *testing.T, fake *fakeAppAgent) *grpcclient.AgentConnection {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	g := grpc.NewServer()
	agentpb.RegisterWendyContainerServiceServer(g, fake)
	go func() { _ = g.Serve(ln) }()
	t.Cleanup(g.Stop)

	conn, err := grpc.NewClient(ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return &grpcclient.AgentConnection{
		Conn:             conn,
		ContainerService: agentpb.NewWendyContainerServiceClient(conn),
	}
}

// newAppMCPServer runs a real MCP server exposing toolNames and returns its
// host:port. Each tool answers with its own name so a proxied call can be
// traced back to the app that served it.
func newAppMCPServer(t *testing.T, toolNames ...string) (string, func()) {
	t.Helper()
	app := server.NewMCPServer("app", "1.0.0", server.WithToolCapabilities(true))
	for _, n := range toolNames {
		name := n
		app.AddTool(mcpgo.NewTool(name, mcpgo.WithDescription(name+" tool")),
			func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
				return mcpgo.NewToolResultText("answered by " + name), nil
			})
	}
	ts := httptest.NewServer(server.NewStreamableHTTPServer(app))
	t.Cleanup(ts.Close)
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	return u.Host, ts.Close
}

func runningMCPApp(name string, port uint32) *agentpb.AppContainer {
	return &agentpb.AppContainer{
		AppName:      name,
		RunningState: agentpb.AppRunningState_RUNNING,
		McpPort:      port,
	}
}

// proxiedToolNames lists the registered tools that came from an app, i.e. every
// name carrying the app/tool separator.
func proxiedToolNames(srv *server.MCPServer) []string {
	var names []string
	for name := range srv.ListTools() {
		if strings.Contains(name, mcpToolSeparator) {
			names = append(names, name)
		}
	}
	return names
}

func hasTool(srv *server.MCPServer, name string) bool {
	_, ok := srv.ListTools()[name]
	return ok
}

// --- §4: prefixes and the name-length guard ---------------------------------

func TestShortMCPPrefix_UsesLastSegment(t *testing.T) {
	if got := shortMCPPrefix("sh.wendy.demos.go2-patrol"); got != "go2_patrol" {
		t.Errorf("shortMCPPrefix = %q, want %q", got, "go2_patrol")
	}
	// No dots: the whole name is the segment.
	if got := shortMCPPrefix("coke-can-detector"); got != "coke_can_detector" {
		t.Errorf("shortMCPPrefix = %q, want %q", got, "coke_can_detector")
	}
	// A trailing dot has no segment after it; fall back rather than emit "".
	if got := shortMCPPrefix("a.b."); got != "a_b_" {
		t.Errorf("shortMCPPrefix = %q, want %q", got, "a_b_")
	}
}

func TestMCPToolPrefixes_CollisionFallsBackToFullID(t *testing.T) {
	prefixes := mcpToolPrefixes([]string{"org.a.status", "org.b.status", "org.c.unique"})
	if got := prefixes["org.a.status"]; got != "org_a_status" {
		t.Errorf("colliding app a got %q, want the full id", got)
	}
	if got := prefixes["org.b.status"]; got != "org_b_status" {
		t.Errorf("colliding app b got %q, want the full id", got)
	}
	// The app that does not collide keeps its short prefix.
	if got := prefixes["org.c.unique"]; got != "unique" {
		t.Errorf("non-colliding app got %q, want %q", got, "unique")
	}
}

func TestAddProxiedTools_SkipsOverlongNameAndDiagnoses(t *testing.T) {
	srv := server.NewMCPServer("t", "0")
	s := New(&config.Config{}, nil)

	longTool := strings.Repeat("x", 70)
	names := s.addProxiedTools(srv, nil, "demo", "demo", []mcpgo.Tool{
		{Name: "ok"},
		{Name: longTool},
	})

	if len(names) != 1 || names[0] != "demo__ok" {
		t.Fatalf("registered names = %v, want only demo__ok", names)
	}
	if hasTool(srv, "demo"+mcpToolSeparator+longTool) {
		t.Error("an over-length tool name must not be registered")
	}
	diags := s.proxyDiagnostics()
	var found bool
	for _, d := range diags {
		if d.AppName == "demo" && d.Stage == "tool-name-too-long" {
			found = true
		}
	}
	if !found {
		t.Errorf("skipping an over-length name must be diagnosed; got %+v", diags)
	}
}

// --- §1: rescan while running ------------------------------------------------

func TestReconcile_AppAppearingAfterStartIsRegistered(t *testing.T) {
	fake := &fakeAppAgent{}
	conn := startFakeAppAgent(t, fake)
	srv := server.NewMCPServer("t", "0")
	s := New(&config.Config{}, nil)
	s.SetConn(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Nothing deployed yet.
	s.reconcileAppTools(ctx, srv)
	if got := proxiedToolNames(srv); len(got) != 0 {
		t.Fatalf("expected no app tools before deploy, got %v", got)
	}

	// The app is deployed after the server is already running.
	addr, _ := newAppMCPServer(t, "ping")
	fake.setApps([]*agentpb.AppContainer{runningMCPApp("sh.wendy.demos.mcp-example", 3000)},
		map[string]string{"sh.wendy.demos.mcp-example": addr})

	s.reconcileAppTools(ctx, srv)
	if !hasTool(srv, "mcp_example__ping") {
		t.Fatalf("app deployed after start should be registered; tools: %v", proxiedToolNames(srv))
	}
}

func TestReconcile_StoppedAppLosesItsTools(t *testing.T) {
	addr, _ := newAppMCPServer(t, "ping")
	fake := &fakeAppAgent{}
	fake.setApps([]*agentpb.AppContainer{runningMCPApp("demo.pinger", 3000)},
		map[string]string{"demo.pinger": addr})
	conn := startFakeAppAgent(t, fake)
	srv := server.NewMCPServer("t", "0")
	s := New(&config.Config{}, nil)
	s.SetConn(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s.reconcileAppTools(ctx, srv)
	if !hasTool(srv, "pinger__ping") {
		t.Fatalf("expected pinger__ping registered; got %v", proxiedToolNames(srv))
	}

	// The app stops: still listed, no longer RUNNING.
	fake.setApps([]*agentpb.AppContainer{{
		AppName:      "demo.pinger",
		RunningState: agentpb.AppRunningState_STOPPED,
		McpPort:      3000,
	}}, map[string]string{"demo.pinger": addr})

	s.reconcileAppTools(ctx, srv)
	if hasTool(srv, "pinger__ping") {
		t.Error("a stopped app must lose its tools")
	}
	// The client sees a missing tool, which mcp-go answers with an error, not
	// a call that hangs against a dead proxy.
	if got := proxiedToolNames(srv); len(got) != 0 {
		t.Errorf("expected no app tools after stop, got %v", got)
	}
}

func TestReconcile_ChangedToolListReregistersWithoutDuplicates(t *testing.T) {
	first, stopFirst := newAppMCPServer(t, "ping")
	fake := &fakeAppAgent{}
	fake.setApps([]*agentpb.AppContainer{runningMCPApp("demo.pinger", 3000)},
		map[string]string{"demo.pinger": first})
	conn := startFakeAppAgent(t, fake)
	srv := server.NewMCPServer("t", "0")
	s := New(&config.Config{}, nil)
	s.SetConn(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s.reconcileAppTools(ctx, srv)
	if !hasTool(srv, "pinger__ping") {
		t.Fatalf("expected pinger__ping; got %v", proxiedToolNames(srv))
	}

	// The app is redeployed with a different tool surface. A redeploy replaces
	// the container, so the old MCP server goes away with it -- without that,
	// the client's keep-alive connection would keep answering from the old one
	// and the test would prove nothing about a real restart.
	stopFirst()
	second, _ := newAppMCPServer(t, "pong", "status")
	fake.setApps([]*agentpb.AppContainer{runningMCPApp("demo.pinger", 3000)},
		map[string]string{"demo.pinger": second})

	s.reconcileAppTools(ctx, srv)
	if hasTool(srv, "pinger__ping") {
		t.Error("a tool the app no longer serves must be removed")
	}
	for _, want := range []string{"pinger__pong", "pinger__status"} {
		if !hasTool(srv, want) {
			t.Errorf("missing %s after redeploy; got %v", want, proxiedToolNames(srv))
		}
	}
	if got := proxiedToolNames(srv); len(got) != 2 {
		t.Errorf("expected exactly 2 app tools after redeploy, got %v", got)
	}
}

func TestReconciler_TriggerRegistersWithoutWaitingForTick(t *testing.T) {
	addr, _ := newAppMCPServer(t, "ping")
	fake := &fakeAppAgent{}
	fake.setApps([]*agentpb.AppContainer{runningMCPApp("demo.pinger", 3000)},
		map[string]string{"demo.pinger": addr})
	conn := startFakeAppAgent(t, fake)
	srv := server.NewMCPServer("t", "0")
	s := New(&config.Config{}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.runAppToolReconciler(ctx, srv)

	// SetConn triggers a pass; the rescan interval is far longer than this
	// deadline, so passing proves the trigger and not the ticker.
	s.SetConn(conn)
	deadline := time.Now().Add(20 * time.Second)
	for !hasTool(srv, "pinger__ping") {
		if time.Now().After(deadline) {
			t.Fatalf("tools not registered via trigger; got %v", proxiedToolNames(srv))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// --- §2: register after any connect, unregister on switch --------------------

func TestReconcile_RegistersAfterDeviceConnectNotOnlyStartup(t *testing.T) {
	addr, _ := newAppMCPServer(t, "ping")
	fake := &fakeAppAgent{}
	fake.setApps([]*agentpb.AppContainer{runningMCPApp("sh.wendy.demos.mcp-example", 3000)},
		map[string]string{"sh.wendy.demos.mcp-example": addr})
	conn := startFakeAppAgent(t, fake)

	srv := server.NewMCPServer("t", "0")
	// Started bare: no --device, no default device, so no startup connect ever
	// runs. This is the shape a client that connects by tool call has.
	s := New(&config.Config{}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s.reconcileAppTools(ctx, srv)
	if got := proxiedToolNames(srv); len(got) != 0 {
		t.Fatalf("no connection should mean no app tools, got %v", got)
	}

	// The equivalent of the device_connect tool: it stores the connection here.
	s.SetConn(conn)
	s.reconcileAppTools(ctx, srv)

	if !hasTool(srv, "mcp_example__ping") {
		t.Fatalf("connecting by tool call must register app tools; got %v", proxiedToolNames(srv))
	}
}

func TestReconcile_SwitchingDeviceReplacesAppTools(t *testing.T) {
	firstAddr, _ := newAppMCPServer(t, "ping")
	firstAgent := &fakeAppAgent{}
	firstAgent.setApps([]*agentpb.AppContainer{runningMCPApp("demo.first", 3000)},
		map[string]string{"demo.first": firstAddr})
	firstConn := startFakeAppAgent(t, firstAgent)

	secondAddr, _ := newAppMCPServer(t, "scan")
	secondAgent := &fakeAppAgent{}
	secondAgent.setApps([]*agentpb.AppContainer{runningMCPApp("demo.second", 3000)},
		map[string]string{"demo.second": secondAddr})
	secondConn := startFakeAppAgent(t, secondAgent)

	srv := server.NewMCPServer("t", "0")
	s := New(&config.Config{}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s.SetConn(firstConn)
	s.reconcileAppTools(ctx, srv)
	if !hasTool(srv, "first__ping") {
		t.Fatalf("expected first__ping; got %v", proxiedToolNames(srv))
	}

	// Connecting to a second device must not leave the first device's tools
	// behind: the tool list has to describe the device being talked to.
	s.SetConn(secondConn)
	s.reconcileAppTools(ctx, srv)

	if hasTool(srv, "first__ping") {
		t.Error("the previous device's tools must be removed on switch")
	}
	if !hasTool(srv, "second__scan") {
		t.Errorf("the new device's tools must be registered; got %v", proxiedToolNames(srv))
	}
}

func TestReconcile_DisconnectLeavesOnlyBuiltInTools(t *testing.T) {
	addr, _ := newAppMCPServer(t, "ping")
	fake := &fakeAppAgent{}
	fake.setApps([]*agentpb.AppContainer{runningMCPApp("demo.pinger", 3000)},
		map[string]string{"demo.pinger": addr})
	conn := startFakeAppAgent(t, fake)

	srv := server.NewMCPServer("t", "0")
	s := New(&config.Config{}, nil)
	s.registerContainerTools(srv)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	s.SetConn(conn)
	s.reconcileAppTools(ctx, srv)
	if !hasTool(srv, "pinger__ping") {
		t.Fatalf("expected pinger__ping; got %v", proxiedToolNames(srv))
	}

	// device_disconnect stores a nil connection.
	s.SetConn(nil)
	s.reconcileAppTools(ctx, srv)

	if got := proxiedToolNames(srv); len(got) != 0 {
		t.Errorf("disconnect must leave only built-in tools, still have %v", got)
	}
	if !hasTool(srv, "container_list") {
		t.Error("built-in tools must survive a disconnect")
	}
}
