package mcp

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/internal/shared/version"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"google.golang.org/grpc/status"
)

// ConnectFunc connects to a wendy agent at the given address (host:port).
type ConnectFunc func(ctx context.Context, address string) (*grpcclient.AgentConnection, error)

// mcpServer holds active connection state and implements all MCP tool handlers.
type mcpServer struct {
	cfg          *config.Config
	connectFn    ConnectFunc
	conn         *grpcclient.AgentConnection
	cloudTunnels map[string]*mcpCloudTunnel
	mu           sync.RWMutex
}

// New creates a new mcpServer. connectFn is called by device_connect; pass nil
// to disable dynamic connection (useful in tests that set conn directly).
func New(cfg *config.Config, connectFn ConnectFunc) *mcpServer {
	return &mcpServer{cfg: cfg, connectFn: connectFn, cloudTunnels: make(map[string]*mcpCloudTunnel)}
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
	s.registerCloudTools(srv)
	s.registerContainerMCPTools(ctx, srv)
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

// registerContainerMCPTools scans running containers for mcp_port > 0 and
// registers each container's tools on srv, prefixed with the app name.
// Errors per-container are warnings; they do not prevent the session from starting.
func (s *mcpServer) registerContainerMCPTools(ctx context.Context, srv *server.MCPServer) {
	conn := s.GetConn()
	if conn == nil {
		return
	}

	stream, err := conn.ContainerService.ListContainers(ctx, &agentpb.ListContainersRequest{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: listing containers for MCP tools: %v\n", err)
		return
	}

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: reading container list: %v\n", err)
			return
		}
		c := resp.GetContainer()
		if c == nil || c.GetMcpPort() == 0 || c.GetRunningState() != agentpb.AppRunningState_RUNNING {
			continue
		}
		s.connectContainerMCPTools(ctx, srv, c.GetAppName())
	}
}

// connectContainerMCPTools proxies a single container's MCP server into srv.
// It retries Initialize up to 4 times with exponential backoff (2s, 4s, 8s).
func (s *mcpServer) connectContainerMCPTools(ctx context.Context, srv *server.MCPServer, appName string) {
	conn := s.GetConn()
	addr, closeProxy, err := startMCPProxy(ctx, conn, appName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: MCP proxy for %s: %v\n", appName, err)
		return
	}

	mcpCli, err := mcpclient.NewStreamableHttpClient("http://" + addr)
	if err != nil {
		closeProxy()
		fmt.Fprintf(os.Stderr, "Warning: MCP client for %s: %v\n", appName, err)
		return
	}

	var initErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				closeProxy()
				return
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
		fmt.Fprintf(os.Stderr, "Warning: MCP init for %s: %v\n", appName, initErr)
		return
	}

	result, err := mcpCli.ListTools(ctx, mcpgo.ListToolsRequest{})
	if err != nil {
		closeProxy()
		fmt.Fprintf(os.Stderr, "Warning: listing MCP tools for %s: %v\n", appName, err)
		return
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
			return mcpCli.CallTool(ctx, inner)
		})
	}
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
