package mcp

import (
	"context"
	"sort"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type cloudTunnelInfo struct {
	ID         string    `json:"id"`
	Protocol   string    `json:"protocol"`
	LocalAddr  string    `json:"local_addr"`
	LocalPort  int       `json:"local_port"`
	RemotePort int       `json:"remote_port"`
	Device     string    `json:"device"`
	DeviceName string    `json:"device_name"`
	DeviceID   string    `json:"device_id"`
	CloudGRPC  string    `json:"cloud_grpc"`
	CreatedAt  time.Time `json:"created_at"`
}

func (s *mcpServer) registerCloudTunnelManagementTools(srv *server.MCPServer) {
	list := []mcpgo.ToolOption{
		mcpgo.WithDescription("List port forwards owned by this MCP server. Includes tunnel IDs, ports and canonical device selectors; excludes other CLI processes."),
		mcpgo.WithInteger("max_bytes", mcpgo.Min(1024), mcpgo.Max(1000000), mcpgo.DefaultNumber(16384)),
	}
	list = append(list, readOnly()...)
	list = append(list, localOnly()...)
	srv.AddTool(mcpgo.NewTool("cloud_tunnel_list", list...), s.handleCloudTunnelList)
	close := []mcpgo.ToolOption{
		mcpgo.WithDescription("Close one port forward owned by this MCP server, including active connections. Repeating a close is safe."),
		mcpgo.WithString("id", mcpgo.Required(), mcpgo.Description("Tunnel ID from cloud_tunnel or cloud_tunnel_list")),
	}
	close = append(close, destructive()...)
	close = append(close, idempotent()...)
	close = append(close, localOnly()...)
	srv.AddTool(mcpgo.NewTool("cloud_tunnel_close", close...), s.handleCloudTunnelClose)
}

func (s *mcpServer) handleCloudTunnelList(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	limit, err := ros2Int(req, "max_bytes", 16384, 1024, 1000000)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	s.mu.RLock()
	rows := make([]cloudTunnelInfo, 0, len(s.cloudTunnels))
	for _, tunnel := range s.cloudTunnels {
		rows = append(rows, tunnel.info)
	}
	s.mu.RUnlock()
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	return okRowsBounded("tunnels", rows, map[string]any{"scope": "mcp_session"}, limit, len(rows)), nil
}

func (s *mcpServer) handleCloudTunnelClose(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	id := strings.TrimSpace(stringParam(req, "id"))
	if id == "" {
		return errResult(errCodeInvalidArgument, "id is required"), nil
	}
	s.mu.Lock()
	tunnel := s.cloudTunnels[id]
	delete(s.cloudTunnels, id)
	s.mu.Unlock()
	if tunnel == nil {
		return okResult(map[string]any{"id": id, "status": "already_closed"}), nil
	}
	if err := tunnel.Close(); err != nil {
		return errResult(errCodeInternal, err.Error()), nil
	}
	return okResult(map[string]any{"id": id, "status": "closed"}), nil
}

// A terminated forwarding loop must not leave a stale entry or remove a newer
// tunnel that replaced it. Network closes happen outside the session lock.
func (s *mcpServer) removeCloudTunnel(id string, tunnel *mcpCloudTunnel) {
	s.mu.Lock()
	if s.cloudTunnels[id] == tunnel {
		delete(s.cloudTunnels, id)
	}
	s.mu.Unlock()
	_ = tunnel.Close()
}

func (s *mcpServer) closeCloudTunnels() {
	s.mu.Lock()
	s.tunnelsClosed = true
	tunnels := s.cloudTunnels
	s.cloudTunnels = make(map[string]*mcpCloudTunnel)
	s.mu.Unlock()
	for _, tunnel := range tunnels {
		_ = tunnel.Close()
	}
}

func (s *mcpServer) addCloudTunnel(id string, tunnel *mcpCloudTunnel) bool {
	s.mu.Lock()
	closed := s.tunnelsClosed
	if !closed {
		s.cloudTunnels[id] = tunnel
	}
	s.mu.Unlock()
	if closed {
		_ = tunnel.Close()
	}
	return !closed
}
