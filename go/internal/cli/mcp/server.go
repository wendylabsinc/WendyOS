package mcp

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"sync"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc/status"
)

// ConnectFunc connects to a wendy agent at the given address (host:port).
type ConnectFunc func(ctx context.Context, address string) (*grpcclient.AgentConnection, error)

type configLoadFunc func() (*config.Config, error)
type configSaveFunc func(*config.Config) error

type mcpServer struct {
	loadConfigFn     configLoadFunc
	saveConfigFn     configSaveFunc
	connectFn        ConnectFunc
	startupConnectFn func(context.Context)
	conn             *grpcclient.AgentConnection
	connRevision     uint64
	connType         string
	cloudTunnels     map[string]*mcpCloudTunnel
	discoverLANFn    func(ctx context.Context, timeout time.Duration) ([]models.LANDevice, error)
	mu               sync.RWMutex
	proxyDiag        []proxyDiagEntry
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

// New constructs an MCP server whose configuration is always read from and
// written to ~/.wendy/config.json. The server is long-lived, while CLI commands
// may update that file in other processes, so retaining the startup snapshot
// would make device pins, auth sessions, and defaults disagree between the two
// surfaces and could overwrite newer state on the next MCP write.
func New(connectFn ConnectFunc) *mcpServer {
	return newMCPServer(config.Load, config.Save, connectFn)
}

// newMCPServer accepts config functions so tests can use an isolated store
// without touching the user's real Wendy configuration.
func newMCPServer(loadConfigFn configLoadFunc, saveConfigFn configSaveFunc, connectFn ConnectFunc) *mcpServer {
	return &mcpServer{
		loadConfigFn:  loadConfigFn,
		saveConfigFn:  saveConfigFn,
		connectFn:     connectFn,
		cloudTunnels:  make(map[string]*mcpCloudTunnel),
		discoverLANFn: discovery.DiscoverLAN,
	}
}

func (s *mcpServer) loadConfig() (*config.Config, error) {
	if s.loadConfigFn == nil {
		return nil, fmt.Errorf("no config loader configured")
	}
	return s.loadConfigFn()
}

func (s *mcpServer) saveConfig(cfg *config.Config) error {
	if s.saveConfigFn == nil {
		return fmt.Errorf("no config saver configured")
	}
	return s.saveConfigFn(cfg)
}

func (s *mcpServer) GetConn() *grpcclient.AgentConnection {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.conn
}

// SetConn replaces the active connection, closing the previous one.
func (s *mcpServer) SetConn(conn *grpcclient.AgentConnection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.conn = conn
	s.connRevision++
	if conn == nil {
		s.connType = ""
	}
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

	startupCtx, cancelStartup := context.WithCancel(ctx)
	defer cancelStartup()
	go s.runStartupConnect(startupCtx, srv)

	return serveStdio(srv)
}

// serveStdio is replaceable in tests so startup ordering can be verified
// without taking over the test process's stdin and stdout.
var serveStdio = func(srv *server.MCPServer) error {
	return server.ServeStdio(srv)
}

func (s *mcpServer) runStartupConnect(ctx context.Context, srv *server.MCPServer) {
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

	// MCPServer.AddTool is concurrency-safe and sends tools/list_changed to
	// initialized clients, so container tools may be discovered after the host
	// has completed its handshake with Wendy.
	cleanups := s.registerContainerMCPTools(ctx, srv)
	defer func() {
		for _, cleanup := range cleanups {
			cleanup()
		}
	}()
	<-ctx.Done()
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

// registerContainerMCPTools scans running containers for mcp_port > 0 and
// registers each container's tools on srv, prefixed with the app name.
// Errors per-container are warnings; they do not prevent the session from starting.
func (s *mcpServer) registerContainerMCPTools(ctx context.Context, srv *server.MCPServer) []func() {
	conn := s.GetConn()
	if conn == nil {
		return nil
	}

	stream, err := conn.ContainerService.ListContainers(ctx, &agentpb.ListContainersRequest{})
	if err != nil {
		s.recordProxyDiag("", "list-containers", err)
		fmt.Fprintf(os.Stderr, "Warning: listing containers for MCP tools: %v\n", err)
		return nil
	}

	var cleanups []func()
	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.recordProxyDiag("", "read-container-list", err)
			fmt.Fprintf(os.Stderr, "Warning: reading container list: %v\n", err)
			return cleanups
		}
		c := resp.GetContainer()
		if c == nil || c.GetMcpPort() == 0 || c.GetRunningState() != agentpb.AppRunningState_RUNNING {
			continue
		}
		if cleanup := s.connectContainerMCPTools(ctx, srv, conn, c.GetAppName()); cleanup != nil {
			cleanups = append(cleanups, cleanup)
		}
	}
	return cleanups
}

// connectContainerMCPTools proxies a single container's MCP server into srv.
// It retries Initialize up to 4 times with exponential backoff (2s, 4s, 8s).
// On success it returns a cleanup function that closes the proxy; on failure it
// returns nil (after cleaning up internally).
func (s *mcpServer) connectContainerMCPTools(ctx context.Context, srv *server.MCPServer, conn *grpcclient.AgentConnection, appName string) func() {
	addr, closeProxy, err := startMCPProxy(ctx, conn, appName)
	if err != nil {
		s.recordProxyDiag(appName, "proxy", err)
		fmt.Fprintf(os.Stderr, "Warning: MCP proxy for %s: %v\n", appName, err)
		return nil
	}

	mcpCli, err := mcpclient.NewStreamableHttpClient("http://" + addr)
	if err != nil {
		closeProxy()
		s.recordProxyDiag(appName, "client", err)
		fmt.Fprintf(os.Stderr, "Warning: MCP client for %s: %v\n", appName, err)
		return nil
	}

	var initErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				closeProxy()
				return nil
			case <-time.After(time.Duration(1<<attempt) * time.Second):
			}
		}
		_, initErr = mcpCli.Initialize(ctx, mcpgo.InitializeRequest{})
		if initErr == nil {
			break
		}
	}
	if initErr != nil {
		closeProxy()
		s.recordProxyDiag(appName, "initialize", initErr)
		fmt.Fprintf(os.Stderr, "Warning: MCP init for %s: %v\n", appName, initErr)
		return nil
	}

	result, err := mcpCli.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		closeProxy()
		s.recordProxyDiag(appName, "list-tools", err)
		fmt.Fprintf(os.Stderr, "Warning: listing MCP tools for %s: %v\n", appName, err)
		return nil
	}

	prefix := sanitizeMCPPrefix(appName)
	for _, tool := range result.Tools {
		proxied := tool
		proxied.Name = prefix + "__" + tool.Name
		originalName := tool.Name
		srv.AddTool(proxied, func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			inner := mcpgo.CallToolRequest{}
			inner.Params.Name = originalName
			inner.Params.Arguments = req.Params.Arguments
			result, err := mcpCli.CallTool(ctx, inner)
			if err != nil {
				return result, err
			}
			// Container-supplied tools are not held to the same output
			// discipline as wendy's own tools; cap the result the same way
			// okResultBounded/okTextBounded cap native ones (see results.go).
			return capProxiedResult(result, defaultProxyMaxBytes), nil
		})
	}
	return closeProxy
}

// sanitizeMCPPrefix converts an app name to a valid MCP tool name prefix
// by replacing non-alphanumeric characters with underscores.
func sanitizeMCPPrefix(appName string) string {
	b := make([]byte, len(appName))
	for i := range appName {
		c := appName[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_' {
			b[i] = c
		} else {
			b[i] = '_'
		}
	}
	return string(b)
}
