package mcp

import (
	"context"
	"encoding/json"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func (s *mcpServer) registerHardwareTools(srv *server.MCPServer) {
	srv.AddTool(mcpgo.NewTool("hardware_capabilities",
		mcpgo.WithDescription("List hardware capabilities (GPUs, cameras, I2C buses, USB devices, etc.) on the connected device"),
		mcpgo.WithString("category",
			mcpgo.Description("Filter by category, e.g. gpu, usb, i2c, gpio, camera (optional)"),
		),
	), s.handleHardwareCapabilities)
}

func (s *mcpServer) handleHardwareCapabilities(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	hwReq := &agentpb.ListHardwareCapabilitiesRequest{}
	if v := stringParam(req, "category"); v != "" {
		hwReq.CategoryFilter = &v
	}
	resp, err := conn.AgentService.ListHardwareCapabilities(ctx, hwReq)
	if err != nil {
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
	}
	var caps []map[string]any
	for _, c := range resp.GetCapabilities() {
		entry := map[string]any{
			"category":    c.GetCategory(),
			"device_path": c.GetDevicePath(),
			"description": c.GetDescription(),
		}
		if props := c.GetProperties(); len(props) > 0 {
			entry["properties"] = props
		}
		caps = append(caps, entry)
	}
	if caps == nil {
		caps = []map[string]any{}
	}
	b, _ := json.MarshalIndent(caps, "", "  ")
	return mcpgo.NewToolResultText(string(b)), nil
}
