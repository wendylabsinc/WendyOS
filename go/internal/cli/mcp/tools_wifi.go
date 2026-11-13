package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// wifiDeviceDescription documents the Wi-Fi tools' optional device argument.
const wifiDeviceDescription = "Optional device selector from device_list. Opens a connection for this call only, " +
	"instead of using the session connection. Use it for first-boot Wi-Fi setup of an unenrolled device that " +
	"device_connect refuses for lack of an expected identity; a device with no saved binding that reports it is " +
	"unprovisioned is reached without authentication."

func wifiDeviceOption() mcpgo.ToolOption {
	return mcpgo.WithString("device", mcpgo.Description(wifiDeviceDescription))
}

func (s *mcpServer) registerWiFiTools(srv *server.MCPServer) {
	listOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("List available WiFi networks visible to the connected device"),
		mcpgo.WithNumber("max_bytes", mcpgo.Description("Maximum output size in bytes before the result is truncated (default 100000)")),
		wifiDeviceOption(),
	}
	listOpts = append(listOpts, readOnly()...)
	listOpts = append(listOpts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("wifi_list", listOpts...), s.handleWiFiList)

	connectOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Connect the device to a WiFi network. Drops its current network, so the device may become unreachable. " +
			"Over an unauthenticated first-boot setup connection the password is sent unencrypted to whichever host answered " +
			"the selector, so the call is refused with " + string(errCodeConfirmationRequired) + " until the user agrees and " +
			"allow_unverified_device is true."),
		mcpgo.WithString("ssid",
			mcpgo.Required(),
			mcpgo.Description("WiFi network SSID"),
		),
		mcpgo.WithString("password",
			mcpgo.Description("WiFi password (leave empty for open networks)"),
		),
		wifiDeviceOption(),
		mcpgo.WithBoolean("allow_unverified_device",
			mcpgo.Description("Send the password to an unenrolled device whose identity cannot be verified. Set only after the user has agreed; default false."),
		),
	}
	connectOpts = append(connectOpts, destructive()...)
	connectOpts = append(connectOpts, openWorld()...)
	connectOpts = append(connectOpts, idempotent()...)
	srv.AddTool(mcpgo.NewTool("wifi_connect", connectOpts...), s.handleWiFiConnect)

	statusOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Get the current WiFi connection status of the connected device"),
		wifiDeviceOption(),
	}
	statusOpts = append(statusOpts, readOnly()...)
	statusOpts = append(statusOpts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("wifi_status", statusOpts...), s.handleWiFiStatus)

	disconnectOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Disconnect the device from WiFi. This may also lose the connection carrying the request. acknowledged=true means the device committed to attempting it even if contact is lost; it does not confirm completion. Report the device as disconnected only when completion_confirmed and success are both true. If completion is unconfirmed, use wifi_status after reconnecting through another working path; do not treat a received request as completed."),
		wifiDeviceOption(),
	}
	disconnectOpts = append(disconnectOpts, destructive()...)
	disconnectOpts = append(disconnectOpts, openWorld()...)
	disconnectOpts = append(disconnectOpts, idempotent()...)
	srv.AddTool(mcpgo.NewTool("wifi_disconnect", disconnectOpts...), s.handleWiFiDisconnect)

	knownOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("List WiFi networks with saved profiles on the connected device"),
		wifiDeviceOption(),
	}
	knownOpts = append(knownOpts, readOnly()...)
	knownOpts = append(knownOpts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("wifi_known_networks", knownOpts...), s.handleWiFiKnownNetworks)
}

// wifiConn returns the connection a Wi-Fi tool call uses: the session's, or a
// connection to the device argument that lasts this call. release must be
// called when the call is done; failure is set when no connection is usable.
func (s *mcpServer) wifiConn(ctx context.Context, req mcpgo.CallToolRequest) (conn *grpcclient.AgentConnection, unverified bool, release func(), failure *mcpgo.CallToolResult) {
	device := strings.TrimSpace(stringParam(req, "device"))
	if device == "" {
		conn := s.GetConn()
		if conn == nil {
			return nil, false, nil, errNotConnected()
		}
		return conn, false, func() {}, nil
	}
	// A relay client's reads skip confirmation, and a device argument would
	// let them reach any host on this network (see hostTools).
	if s.bindingPolicy() != nil {
		return nil, false, nil, errResult(errCodeDenied, "the device argument is not available to relay clients; use the session's connected device")
	}
	if inDeviceContainer() && !isLocalSelector(device) {
		return nil, false, nil, errResultf(errCodeUnsupported, "inside a device container only the local device is reachable; omit device or use %q", localDeviceSelector)
	}
	s.mu.RLock()
	connect := s.wifiSetupConnectFn
	s.mu.RUnlock()
	if connect == nil {
		return nil, false, nil, errResult(errCodeUnsupported, "this server cannot open a per-call Wi-Fi connection; use device_connect")
	}
	conn, unverified, err := connect(ctx, device)
	if err != nil {
		return nil, false, nil, errResultf(errCodeDeviceUnreachable, "connecting to %s: %s", device, err.Error())
	}
	return conn, unverified, func() { conn.Close() }, nil
}

func (s *mcpServer) handleWiFiList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn, _, release, failure := s.wifiConn(ctx, req)
	if failure != nil {
		return failure, nil
	}
	defer release()
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
	ssid := stringParam(req, "ssid")
	if ssid == "" {
		return errResult(errCodeInvalidArgument, "ssid is required"), nil
	}
	conn, unverified, release, failure := s.wifiConn(ctx, req)
	if failure != nil {
		return failure, nil
	}
	defer release()
	password := stringParam(req, "password")
	if unverified && password != "" && !req.GetBool("allow_unverified_device", false) {
		return errResultf(errCodeConfirmationRequired,
			"%s answered as an unenrolled device, but its identity cannot be verified and the Wi-Fi password would be sent unencrypted to whichever host answered; ask the user, then retry with allow_unverified_device=true if they agree",
			stringParam(req, "device")), nil
	}
	resp, err := conn.AgentService.ConnectToWiFi(ctx, &agentpb.ConnectToWiFiRequest{
		Ssid:     ssid,
		Password: password,
	})
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
	}
	if !resp.GetSuccess() {
		return errResultf(errCodeInternal, "connect failed: %s", resp.GetErrorMessage()), nil
	}
	return okText(fmt.Sprintf("connected to %s", ssid)), nil
}

func (s *mcpServer) handleWiFiStatus(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn, _, release, failure := s.wifiConn(ctx, req)
	if failure != nil {
		return failure, nil
	}
	defer release()
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

func (s *mcpServer) handleWiFiDisconnect(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn, _, release, failure := s.wifiConn(ctx, req)
	if failure != nil {
		return failure, nil
	}
	defer release()
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

func (s *mcpServer) handleWiFiKnownNetworks(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn, _, release, failure := s.wifiConn(ctx, req)
	if failure != nil {
		return failure, nil
	}
	defer release()
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
