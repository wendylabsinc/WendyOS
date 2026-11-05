package mcp

import (
	"context"
	"errors"
	"fmt"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func (s *mcpServer) registerWiFiTools(srv *server.MCPServer) {
	listOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("List available WiFi networks visible to the connected device"),
		mcpgo.WithNumber("max_bytes", mcpgo.Description("Maximum output size in bytes before the result is truncated (default 100000)")),
	}
	listOpts = append(listOpts, readOnly()...)
	listOpts = append(listOpts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("wifi_list", listOpts...), s.handleWiFiList)

	connectOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Connect the device to a WiFi network. Drops its current network, so the device may become unreachable."),
		mcpgo.WithString("ssid",
			mcpgo.Required(),
			mcpgo.Description("WiFi network SSID"),
		),
		mcpgo.WithString("password",
			mcpgo.Description("WiFi password (leave empty for open networks)"),
		),
	}
	connectOpts = append(connectOpts, destructive()...)
	connectOpts = append(connectOpts, openWorld()...)
	connectOpts = append(connectOpts, idempotent()...)
	srv.AddTool(mcpgo.NewTool("wifi_connect", connectOpts...), s.handleWiFiConnect)

	statusOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Get the current WiFi connection status of the connected device"),
	}
	statusOpts = append(statusOpts, readOnly()...)
	statusOpts = append(statusOpts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("wifi_status", statusOpts...), s.handleWiFiStatus)

	disconnectOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Disconnect the device from WiFi. This may also lose the connection carrying the request. acknowledged=true means the device committed to attempting it even if contact is lost; it does not confirm completion. Report the device as disconnected only when completion_confirmed and success are both true. If completion is unconfirmed, use wifi_status after reconnecting through another working path; do not treat a received request as completed."),
	}
	disconnectOpts = append(disconnectOpts, destructive()...)
	disconnectOpts = append(disconnectOpts, openWorld()...)
	disconnectOpts = append(disconnectOpts, idempotent()...)
	srv.AddTool(mcpgo.NewTool("wifi_disconnect", disconnectOpts...), s.handleWiFiDisconnect)

	knownOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("List WiFi networks with saved profiles on the connected device"),
	}
	knownOpts = append(knownOpts, readOnly()...)
	knownOpts = append(knownOpts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("wifi_known_networks", knownOpts...), s.handleWiFiKnownNetworks)
}

func (s *mcpServer) handleWiFiList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	resp, err := conn.AgentService.ListWiFiNetworks(ctx, &agentpb.ListWiFiNetworksRequest{})
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
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
	return okListBounded("networks", networks, intParam(req, "max_bytes", 100000)), nil
}

func (s *mcpServer) handleWiFiConnect(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	ssid := stringParam(req, "ssid")
	if ssid == "" {
		return errResult(errCodeInvalidArgument, "ssid is required"), nil
	}
	resp, err := conn.AgentService.ConnectToWiFi(ctx, &agentpb.ConnectToWiFiRequest{
		Ssid:     ssid,
		Password: stringParam(req, "password"),
	})
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	if !resp.GetSuccess() {
		return errResultf(errCodeInternal, "connect failed: %s", resp.GetErrorMessage()), nil
	}
	return okText(fmt.Sprintf("connected to %s", ssid)), nil
}

func (s *mcpServer) handleWiFiStatus(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	resp, err := conn.AgentService.GetWiFiStatus(ctx, &agentpb.GetWiFiStatusRequest{})
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	status := map[string]any{
		"connected": resp.GetConnected(),
		"ssid":      resp.GetSsid(),
	}
	if msg := resp.GetErrorMessage(); msg != "" {
		status["error"] = msg
	}
	return okResult(status), nil
}

func (s *mcpServer) handleWiFiDisconnect(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	result, err := conn.DisconnectWiFi(ctx)
	if err != nil {
		code := codeFromGRPC(err)
		if errors.Is(err, context.Canceled) {
			code = errCodeCancelled
		} else if errors.Is(err, context.DeadlineExceeded) {
			code = errCodeTimeout
		}
		response := mcpgo.NewToolResultStructured(map[string]any{
			"error_code": string(code),
			"message":    err.Error(),
			"result":     result,
		}, fmt.Sprintf("[%s] %s", code, err))
		response.IsError = true
		return response, nil
	}
	return okResult(result), nil
}

func (s *mcpServer) handleWiFiKnownNetworks(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	resp, err := conn.AgentService.ListKnownWiFiNetworks(ctx, &agentpb.ListKnownWiFiNetworksRequest{})
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
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
	return okList("networks", networks), nil
}
