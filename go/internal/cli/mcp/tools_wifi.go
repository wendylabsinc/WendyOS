package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func (s *mcpServer) registerWiFiTools(srv *server.MCPServer) {
	srv.AddTool(mcpgo.NewTool("wifi_list",
		mcpgo.WithDescription("List available WiFi networks visible to the connected device"),
	), s.handleWiFiList)

	srv.AddTool(mcpgo.NewTool("wifi_connect",
		mcpgo.WithDescription("Connect the device to a WiFi network"),
		mcpgo.WithString("ssid",
			mcpgo.Required(),
			mcpgo.Description("WiFi network SSID"),
		),
		mcpgo.WithString("password",
			mcpgo.Description("WiFi password (leave empty for open networks)"),
		),
	), s.handleWiFiConnect)

	srv.AddTool(mcpgo.NewTool("wifi_status",
		mcpgo.WithDescription("Get the current WiFi connection status of the connected device"),
	), s.handleWiFiStatus)

	srv.AddTool(mcpgo.NewTool("wifi_disconnect",
		mcpgo.WithDescription("Disconnect the device from its current WiFi network"),
	), s.handleWiFiDisconnect)

	srv.AddTool(mcpgo.NewTool("wifi_known_networks",
		mcpgo.WithDescription("List WiFi networks with saved profiles on the connected device"),
	), s.handleWiFiKnownNetworks)
}

func (s *mcpServer) handleWiFiList(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	resp, err := conn.AgentService.ListWiFiNetworks(ctx, &agentpb.ListWiFiNetworksRequest{})
	if err != nil {
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
	}
	var networks []map[string]any
	for _, n := range resp.GetNetworks() {
		networks = append(networks, map[string]any{
			"ssid":         n.GetSsid(),
			"signal":       n.GetSignalStrength(),
			"rssi_dbm":     n.GetRssiDbm(),
			"security":     n.GetSecurity().String(),
			"is_known":     n.GetIsKnown(),
			"is_connected": n.GetIsConnected(),
			"priority":     n.GetPriority(),
		})
	}
	if networks == nil {
		networks = []map[string]any{}
	}
	b, _ := json.MarshalIndent(networks, "", "  ")
	return mcpgo.NewToolResultText(string(b)), nil
}

func (s *mcpServer) handleWiFiConnect(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	ssid := stringParam(req, "ssid")
	if ssid == "" {
		return mcpgo.NewToolResultError("ssid is required"), nil
	}
	resp, err := conn.AgentService.ConnectToWiFi(ctx, &agentpb.ConnectToWiFiRequest{
		Ssid:     ssid,
		Password: stringParam(req, "password"),
	})
	if err != nil {
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
	}
	if !resp.GetSuccess() {
		return mcpgo.NewToolResultError(fmt.Sprintf("connect failed: %s", resp.GetErrorMessage())), nil
	}
	return mcpgo.NewToolResultText(fmt.Sprintf("connected to %s", ssid)), nil
}

func (s *mcpServer) handleWiFiStatus(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	resp, err := conn.AgentService.GetWiFiStatus(ctx, &agentpb.GetWiFiStatusRequest{})
	if err != nil {
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
	}
	status := map[string]any{
		"connected": resp.GetConnected(),
		"ssid":      resp.GetSsid(),
	}
	if msg := resp.GetErrorMessage(); msg != "" {
		status["error"] = msg
	}
	b, _ := json.MarshalIndent(status, "", "  ")
	return mcpgo.NewToolResultText(string(b)), nil
}

func (s *mcpServer) handleWiFiDisconnect(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	resp, err := conn.AgentService.DisconnectWiFi(ctx, &agentpb.DisconnectWiFiRequest{})
	if err != nil {
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
	}
	if !resp.GetSuccess() {
		return mcpgo.NewToolResultError(fmt.Sprintf("disconnect failed: %s", resp.GetErrorMessage())), nil
	}
	return mcpgo.NewToolResultText("disconnected from WiFi"), nil
}

func (s *mcpServer) handleWiFiKnownNetworks(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	resp, err := conn.AgentService.ListKnownWiFiNetworks(ctx, &agentpb.ListKnownWiFiNetworksRequest{})
	if err != nil {
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
	}
	var networks []map[string]any
	for _, n := range resp.GetNetworks() {
		networks = append(networks, map[string]any{
			"ssid":     n.GetSsid(),
			"uuid":     n.GetUuid(),
			"priority": n.GetPriority(),
			"security": n.GetSecurity().String(),
		})
	}
	if networks == nil {
		networks = []map[string]any{}
	}
	b, _ := json.MarshalIndent(networks, "", "  ")
	return mcpgo.NewToolResultText(string(b)), nil
}
