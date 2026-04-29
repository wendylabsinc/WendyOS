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
		s.conn.Close()
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

// CallTool invokes a registered tool handler by name. Used in tests.
func (s *mcpServer) CallTool(ctx context.Context, name string, args map[string]any) (*mcpgo.CallToolResult, error) {
	req := mcpgo.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = any(args)
	switch name {
	case "device_list":
		return s.handleDeviceList(ctx, req)
	case "device_connect":
		return s.handleDeviceConnect(ctx, req)
	case "device_disconnect":
		return s.handleDeviceDisconnect(ctx, req)
	case "device_info":
		return s.handleDeviceInfo(ctx, req)
	case "device_set_default":
		return s.handleDeviceSetDefault(ctx, req)
	case "container_list":
		return s.handleContainerList(ctx, req)
	case "container_start":
		return s.handleContainerStart(ctx, req)
	case "container_stop":
		return s.handleContainerStop(ctx, req)
	case "container_delete":
		return s.handleContainerDelete(ctx, req)
	case "container_stats":
		return s.handleContainerStats(ctx, req)
	case "container_attach":
		return s.handleContainerAttach(ctx, req)
	case "telemetry_logs":
		return s.handleTelemetryLogs(ctx, req)
	case "telemetry_metrics":
		return s.handleTelemetryMetrics(ctx, req)
	case "telemetry_traces":
		return s.handleTelemetryTraces(ctx, req)
	case "wifi_list":
		return s.handleWiFiList(ctx, req)
	case "wifi_connect":
		return s.handleWiFiConnect(ctx, req)
	case "wifi_status":
		return s.handleWiFiStatus(ctx, req)
	case "wifi_disconnect":
		return s.handleWiFiDisconnect(ctx, req)
	case "wifi_known_networks":
		return s.handleWiFiKnownNetworks(ctx, req)
	case "bluetooth_scan":
		return s.handleBluetoothScan(ctx, req)
	case "bluetooth_connect":
		return s.handleBluetoothConnect(ctx, req)
	case "bluetooth_disconnect":
		return s.handleBluetoothDisconnect(ctx, req)
	case "hardware_capabilities":
		return s.handleHardwareCapabilities(ctx, req)
	case "filesync_sync":
		return s.handleFileSyncSync(ctx, req)
	case "provisioning_status":
		return s.handleProvisioningStatus(ctx, req)
	case "provisioning_start":
		return s.handleProvisioningStart(ctx, req)
	case "os_update":
		return s.handleOSUpdate(ctx, req)
	default:
		return mcpgo.NewToolResultError("unknown tool: " + name), nil
	}
}
