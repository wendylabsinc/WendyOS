package mcp

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	"google.golang.org/grpc/status"
)

// ConnectFunc connects to a wendy agent at the given address (host:port).
type ConnectFunc func(ctx context.Context, address string) (*grpcclient.AgentConnection, error)

type mcpServer struct {
	cfg              *config.Config
	connectFn        ConnectFunc
	startupConnectFn func(context.Context)
	conn             *grpcclient.AgentConnection
	connRevision     uint64
	connType         string
	cloudTunnels     map[string]*mcpCloudTunnel
	discoverLANFn    func(ctx context.Context, timeout time.Duration) ([]models.LANDevice, error)
	mu               sync.RWMutex
	proxyDiag        []proxyDiagEntry

	// srv is the MCP server tools are registered on, kept so a connection
	// change can drop the previous device's app tools before returning rather
	// than waiting for the next reconcile pass. Nil until Start runs.
	srv *server.MCPServer

	// Proxied app tools, reconciled against the device rather than registered
	// once at startup -- see app_tools.go. appMu guards the two maps; it is
	// separate from mu because a reconcile pass does network I/O and must never
	// hold the lock the tool handlers take.
	appMu       sync.Mutex
	appTools    map[string]*appToolSet
	appRetry    map[string]appRetryState
	appToolsRev uint64
	reconcileCh chan struct{}

	// Reconcile timing, zero meaning the package defaults. Tests set these to
	// separate "the trigger fired" from "the ticker came round".
	rescanInterval  time.Duration
	retryBackoffMin time.Duration
}

// SetStartupConnect configures the optional device connection attempted after
// the MCP stdio server starts accepting requests. Keeping this work off the
// protocol startup path prevents an offline default device from delaying the
// initialize handshake until the MCP host times out.
func (s *mcpServer) SetStartupConnect(fn func(context.Context)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startupConnectFn = fn
}

func New(cfg *config.Config, connectFn ConnectFunc) *mcpServer {
	return &mcpServer{
		cfg:          cfg,
		connectFn:    connectFn,
		cloudTunnels: make(map[string]*mcpCloudTunnel),
		// Buffered so a connection change never blocks on the reconciler, and
		// depth 1 because a second pending wake-up would do the same work.
		reconcileCh:   make(chan struct{}, 1),
		discoverLANFn: discovery.DiscoverLAN,
	}
}

func (s *mcpServer) GetConn() *grpcclient.AgentConnection {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.conn
}

// SetConn replaces the active connection, closing the previous one.
//
// Every connection change -- device_connect, cloud_connect, device_disconnect
// -- lands here, which is why this is where the app-tool reconcile is kicked:
// registering from the startup path alone left a client that connected by tool
// call with no app tools at all.
func (s *mcpServer) SetConn(conn *grpcclient.AgentConnection) {
	s.mu.Lock()
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.conn = conn
	s.connRevision++
	if conn == nil {
		s.connType = ""
	}
	srv := s.srv
	s.mu.Unlock()

	// Outside the lock: both of these read the connection back through it.
	// Dropping the previous device's tools synchronously matters because a
	// client calls tools/list straight after device_connect -- leaving it to
	// the reconciler would show it the device it just left.
	s.unregisterAllAppTools(srv)
	s.triggerAppToolReconcile()
}

// SetLANDiscoverer replaces the function device_list (and any other LAN
// discovery tool) uses to collect LAN devices. New wires this to
// discovery.DiscoverLAN by default; the CLI's mcp serve command overrides it
// with the cache+probe collector (discovery.CollectLAN + the CLI's shared
// StreamOptions) so MCP clients get the same instant-discovery acceleration
// as the rest of the CLI. Tests can substitute a fixture the same way.
func (s *mcpServer) SetLANDiscoverer(fn func(ctx context.Context, timeout time.Duration) ([]models.LANDevice, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discoverLANFn = fn
}

// SetConnType records the transport type of the active connection ("direct" or "cloud").
func (s *mcpServer) SetConnType(t string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.connType = t
}

func (s *mcpServer) GetConnType() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.connType
}

// ConnectTo connects to address and stores the result as the active connection.
func (s *mcpServer) ConnectTo(ctx context.Context, address string) error {
	if s.connectFn == nil {
		return fmt.Errorf("no connect function configured")
	}
	conn, err := s.connectFn(ctx, address)
	if err != nil {
		return err
	}
	s.SetConn(conn)
	return nil
}

// ConnectToOnStartup connects only if no explicit connection change occurred
// while the attempt was in flight. Once stdio is serving, a device_connect,
// cloud_connect, or device_disconnect request must take precedence over the
// automatic default-device connection.
func (s *mcpServer) ConnectToOnStartup(ctx context.Context, address string) error {
	if s.connectFn == nil {
		return fmt.Errorf("no connect function configured")
	}

	s.mu.RLock()
	revision := s.connRevision
	alreadyConnected := s.conn != nil
	s.mu.RUnlock()
	if alreadyConnected {
		return nil
	}

	conn, err := s.connectFn(ctx, address)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		_ = conn.Close()
		return err
	}

	s.mu.Lock()
	if s.connRevision != revision || s.conn != nil {
		s.mu.Unlock()
		_ = conn.Close()
		return nil
	}
	s.conn = conn
	s.connRevision++
	s.mu.Unlock()
	s.triggerAppToolReconcile()
	return nil
}

// Start registers all tools and begins serving MCP over stdio. Blocks until
// the client closes the connection.
func (s *mcpServer) Start(ctx context.Context) error {
	srv := server.NewMCPServer("wendy", version.Version,
		server.WithToolCapabilities(true),
		server.WithResourceCapabilities(true, false),
		server.WithPromptCapabilities(false),
	)
	s.registerStatusTools(srv)
	s.registerGuideResource(srv)
	s.registerDiagnosticsResource(srv)
	s.registerPrompts(srv)
	s.registerDeviceTools(srv)
	s.registerContainerTools(srv)
	s.registerTelemetryTools(srv)
	s.registerWiFiTools(srv)
	s.registerBluetoothTools(srv)
	s.registerHardwareTools(srv)
	s.registerCameraTools(srv)
	s.registerProvisioningTools(srv)
	s.registerOSTools(srv)
	s.registerCloudTools(srv)

	s.mu.Lock()
	s.srv = srv
	s.mu.Unlock()

	startupCtx, cancelStartup := context.WithCancel(ctx)
	defer cancelStartup()
	go s.runStartupConnect(startupCtx)
	// Unconditional: a server started with no --device and no default still has
	// to pick up app tools when the client later calls device_connect.
	go s.runAppToolReconciler(startupCtx, srv)

	return serveStdio(srv)
}

// serveStdio is replaceable in tests so startup ordering can be verified
// without taking over the test process's stdin and stdout.
var serveStdio = func(srv *server.MCPServer) error {
	return server.ServeStdio(srv)
}

func (s *mcpServer) runStartupConnect(ctx context.Context) {
	s.mu.RLock()
	connect := s.startupConnectFn
	s.mu.RUnlock()
	if connect == nil {
		return
	}

	connect(ctx)
	if ctx.Err() != nil {
		return
	}

	// The reconciler owns registration from here. MCPServer.AddTool is
	// concurrency-safe and sends tools/list_changed to initialized clients, so
	// app tools may arrive after the host has finished its handshake with
	// wendy -- and, now, at any point after that.
	s.triggerAppToolReconcile()
}

func errNotConnected() *mcpgo.CallToolResult {
	return errResult(errCodeNotConnected, "no device connected — use device_connect first")
}

// grpcErrString unwraps a gRPC status error into a human-readable string.
func grpcErrString(err error) string {
	if s, ok := status.FromError(err); ok {
		return s.Message()
	}
	return err.Error()
}

// stringParam extracts a string argument from an MCP tool request.
func stringParam(req mcpgo.CallToolRequest, name string) string {
	return req.GetString(name, "")
}

// intParam extracts an integer argument, falling back to defaultVal.
func intParam(req mcpgo.CallToolRequest, name string, defaultVal int) int {
	return req.GetInt(name, defaultVal)
}

// intParamAlias reads primary, falling back to alias, then defaultVal.
func intParamAlias(req mcpgo.CallToolRequest, primary, alias string, defaultVal int) int {
	// math.MinInt is a sentinel no realistic caller supplies; using it (rather
	// than an int-overflowing constant like -1<<62) keeps this portable across
	// 32- and 64-bit build targets.
	if v := req.GetInt(primary, math.MinInt); v != math.MinInt {
		return v
	}
	return req.GetInt(alias, defaultVal)
}
