package mcp

import (
	"context"
	"fmt"
	"math"
	"strings"
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

// commandTarget is the public, credential-free connection information a local
// CLI child needs to reconnect to the same device independently of this session.
// A zero value means this transport cannot be recreated from CLI arguments.
type commandTarget struct {
	Device    string `json:"device"`
	Transport string `json:"transport"`
	CloudGRPC string `json:"cloud_grpc,omitempty"`
	BrokerURL string `json:"broker_url,omitempty"`
}

type mcpServer struct {
	cfg              *config.Config
	connectFn        ConnectFunc
	startupConnectFn func(context.Context)
	conn             *grpcclient.AgentConnection
	connRevision     uint64
	connType         string
	commandTarget    commandTarget
	cloudTunnels     map[string]*mcpCloudTunnel
	discoverLANFn    func(ctx context.Context, timeout time.Duration) ([]models.LANDevice, error)
	mu               sync.RWMutex
	proxyDiag        []proxyDiagEntry
	containerMCP     *containerMCPManager
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
		cfg:           cfg,
		connectFn:     connectFn,
		cloudTunnels:  make(map[string]*mcpCloudTunnel),
		discoverLANFn: discovery.DiscoverLAN,
	}
}

func (s *mcpServer) GetConn() *grpcclient.AgentConnection {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.conn
}

// SetConn replaces the active connection, closing the previous one.
func (s *mcpServer) SetConn(conn *grpcclient.AgentConnection) {
	s.setConnection(conn, "direct", directCommandTarget(conn, ""))
}

// setConnection publishes the connection and all of its routing metadata in
// one update so callers never combine an old target with a new connection.
func (s *mcpServer) setConnection(conn *grpcclient.AgentConnection, connType string, target commandTarget) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setConnectionLocked(conn, connType, target)
}

func (s *mcpServer) setConnectionLocked(conn *grpcclient.AgentConnection, connType string, target commandTarget) {
	if s.conn != nil && s.conn != conn {
		_ = s.conn.Close()
	}
	s.conn = conn
	s.connType = connType
	s.commandTarget = target
	s.connRevision++
	if s.containerMCP != nil {
		s.containerMCP.invalidateLocked()
	}
	if conn == nil {
		s.connType = ""
		s.commandTarget = commandTarget{}
	}
}

func directCommandTarget(conn *grpcclient.AgentConnection, address string) commandTarget {
	if conn == nil || strings.HasPrefix(conn.Host, "unix:") {
		return commandTarget{}
	}
	if conn.SimulatorName != "" {
		// The named alias retains the VM's identity when its forwarded port changes.
		address = "vm:" + conn.SimulatorName
	} else if address == "" {
		// Host alone loses custom ports; a prebuilt connection with no Addr
		// cannot safely be replayed by guessing a default endpoint.
		address = conn.Addr
	}
	if address == "" {
		return commandTarget{}
	}
	return commandTarget{Device: address, Transport: "direct"}
}

func (s *mcpServer) connectionSnapshot() (*grpcclient.AgentConnection, string, commandTarget) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.conn, s.connType, s.commandTarget
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
	if s.conn == nil {
		return
	}
	s.connType = t
	if s.commandTarget.Transport != t {
		// Legacy callers that only know the transport cannot reconstruct a
		// cloud target from its local tunnel address.
		s.commandTarget = commandTarget{}
	}
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
	s.setConnection(conn, "direct", directCommandTarget(conn, address))
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
	s.setConnectionLocked(conn, "direct", directCommandTarget(conn, address))
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
	s.registerROS2Tools(srv)
	s.registerWiFiTools(srv)
	s.registerBluetoothTools(srv)
	s.registerHardwareTools(srv)
	s.registerCameraTools(srv)
	s.registerProvisioningTools(srv)
	s.registerOSTools(srv)
	s.registerCloudTools(srv)

	startupCtx, cancelStartup := context.WithCancel(ctx)
	defer cancelStartup()
	stopContainerMCP := s.startContainerMCP(startupCtx, srv)
	defer stopContainerMCP()
	go s.runStartupConnect(startupCtx)

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
