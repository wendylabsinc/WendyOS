package mcp

import (
	"context"
	"fmt"
	"sync"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/internal/shared/version"
	"google.golang.org/grpc/status"
)

// ConnectFunc connects to a wendy agent at the given address (host:port).
type ConnectFunc func(ctx context.Context, address string) (*grpcclient.AgentConnection, error)

// mcpServer holds active connection state and implements all MCP tool handlers.
type mcpServer struct {
	cfg       *config.Config
	connectFn ConnectFunc
	conn      *grpcclient.AgentConnection
	mu        sync.RWMutex
}

// New creates a new mcpServer. connectFn is called by device_connect; pass nil
// to disable dynamic connection (useful in tests that set conn directly).
func New(cfg *config.Config, connectFn ConnectFunc) *mcpServer {
	return &mcpServer{cfg: cfg, connectFn: connectFn}
}

// GetConn returns the current active connection (nil if not connected).
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

// Start registers all tools and begins serving MCP over stdio. Blocks until
// the client closes the connection.
func (s *mcpServer) Start(ctx context.Context) error {
	srv := server.NewMCPServer("wendy", version.Version,
		server.WithToolCapabilities(true),
	)
	s.registerDeviceTools(srv)
	s.registerContainerTools(srv)
	s.registerTelemetryTools(srv)
	s.registerWiFiTools(srv)
	s.registerBluetoothTools(srv)
	s.registerHardwareTools(srv)
	s.registerFileSyncTools(srv)
	s.registerProvisioningTools(srv)
	s.registerOSTools(srv)
	return server.ServeStdio(srv)
}

// errNotConnected returns a tool error result when no device is connected.
func errNotConnected() *mcpgo.CallToolResult {
	return mcpgo.NewToolResultError("no device connected — use device_connect first")
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

