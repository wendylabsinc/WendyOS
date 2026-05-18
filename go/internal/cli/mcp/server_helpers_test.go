package mcp

import (
	"context"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
)

// callToolReq builds a minimal CallToolRequest for tests.
func callToolReq(name string, args map[string]any) mcpgo.CallToolRequest {
	req := mcpgo.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = any(args)
	return req
}

// toolResultText extracts the text from the first content item of a CallToolResult.
func toolResultText(t *testing.T, result *mcpgo.CallToolResult) string {
	t.Helper()
	if len(result.Content) == 0 {
		t.Fatal("result has no content")
	}
	tc, ok := result.Content[0].(mcpgo.TextContent)
	if !ok {
		t.Fatalf("expected TextContent, got %T", result.Content[0])
	}
	return tc.Text
}

// callTool invokes a registered tool handler by name. Used in tests only.
func (s *mcpServer) callTool(ctx context.Context, name string, args map[string]any) (*mcpgo.CallToolResult, error) {
	req := mcpgo.CallToolRequest{}
	req.Params.Name = name
	req.Params.Arguments = any(args)
	switch name {
	case "wendy_status":
		return s.handleWendyStatus(ctx, req)
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
	case "cloud_discover":
		return s.handleCloudDiscover(ctx, req)
	case "cloud_connect":
		return s.handleCloudConnect(ctx, req)
	case "cloud_device_connect":
		return s.handleCloudConnect(ctx, req)
	case "cloud_enroll_device":
		return s.handleCloudEnrollDevice(ctx, req)
	case "cloud_tunnel":
		return s.handleCloudTunnel(ctx, req)
	case "run":
		return s.handleRun(ctx, req)
	case "cloud_run":
		return s.handleRun(ctx, req)
	default:
		return mcpgo.NewToolResultError("unknown tool: " + name), nil
	}
}
