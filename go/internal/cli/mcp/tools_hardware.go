package mcp

import (
	"context"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func (s *mcpServer) registerHardwareTools(srv *server.MCPServer) {
	capsOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("List hardware capabilities (GPUs, cameras, I2C buses, USB devices, etc.) on the connected device"),
		mcpgo.WithString("category",
			mcpgo.Description("Filter by category, e.g. gpu, usb, i2c, gpio, camera (optional)"),
		),
		mcpgo.WithNumber("max_bytes", mcpgo.Description("Maximum output size in bytes before the result is truncated (default 100000)")),
	}
	capsOpts = append(capsOpts, readOnly()...)
	capsOpts = append(capsOpts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("hardware_capabilities", capsOpts...), s.handleHardwareCapabilities)
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
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
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
	return okListBounded("capabilities", caps, intParam(req, "max_bytes", 100000)), nil
}
