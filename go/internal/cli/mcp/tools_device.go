package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

func (s *mcpServer) registerDeviceTools(srv *server.MCPServer) {
	srv.AddTool(mcpgo.NewTool("device_list",
		mcpgo.WithDescription("List wendy devices from config and known addresses. Pass scan=true to also run a live 3-second mDNS scan for devices on the local network."),
		mcpgo.WithBoolean("scan",
			mcpgo.Description("If true, run a live mDNS scan (3 s) in addition to returning configured devices"),
		),
	), s.handleDeviceList)

	srv.AddTool(mcpgo.NewTool("device_connect",
		mcpgo.WithDescription("Connect to a wendy device by address (host:port)"),
		mcpgo.WithString("address",
			mcpgo.Required(),
			mcpgo.Description("Device address, e.g. mydevice.local:50051 or 192.168.1.10:50051"),
		),
	), s.handleDeviceConnect)

	srv.AddTool(mcpgo.NewTool("device_disconnect",
		mcpgo.WithDescription("Disconnect from the currently connected device"),
	), s.handleDeviceDisconnect)

	srv.AddTool(mcpgo.NewTool("device_info",
		mcpgo.WithDescription("Get agent version, OS, CPU architecture, and feature set of connected device"),
	), s.handleDeviceInfo)

	srv.AddTool(mcpgo.NewTool("device_set_default",
		mcpgo.WithDescription("Save an address as the default device in ~/.wendy/config.json"),
		mcpgo.WithString("address",
			mcpgo.Required(),
			mcpgo.Description("Device address to save as default"),
		),
	), s.handleDeviceSetDefault)
}

func (s *mcpServer) handleDeviceList(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	scan := req.GetBool("scan", false)

	var devices []map[string]any
	for _, auth := range s.cfg.Auth {
		if auth.CloudGRPC != "" {
			devices = append(devices, map[string]any{
				"address": auth.CloudGRPC,
				"type":    "cloud",
				"source":  "config",
			})
		}
	}
	if s.cfg.DefaultDevice != "" {
		devices = append(devices, map[string]any{
			"address": s.cfg.DefaultDevice,
			"type":    "default",
			"source":  "config",
		})
	}

	if scan {
		found, err := s.discoverLANFn(ctx, 3*time.Second)
		if err == nil {
			for _, d := range found {
				addr := d.Hostname
				if d.IPAddress != "" {
					addr = d.IPAddress
				}
				if d.Port > 0 {
					addr = fmt.Sprintf("%s:%d", addr, d.Port)
				}
				entry := map[string]any{
					"address": addr,
					"type":    "lan",
					"source":  "scan",
				}
				if d.DisplayName != "" {
					entry["name"] = d.DisplayName
				}
				if d.AgentVersion != "" {
					entry["agent_version"] = d.AgentVersion
				}
				devices = append(devices, entry)
			}
		}
	}

	if len(devices) == 0 {
		devices = []map[string]any{}
	}
	b, _ := json.MarshalIndent(devices, "", "  ")
	return mcpgo.NewToolResultText(string(b)), nil
}

func (s *mcpServer) handleDeviceConnect(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	address := stringParam(req, "address")
	if address == "" {
		return mcpgo.NewToolResultError("address is required"), nil
	}
	if err := s.ConnectTo(ctx, address); err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("connecting to %s: %s", address, err.Error())), nil
	}
	s.SetConnType("direct")
	return mcpgo.NewToolResultText(fmt.Sprintf("connected to %s", address)), nil
}

func (s *mcpServer) handleDeviceDisconnect(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return mcpgo.NewToolResultText("not connected"), nil
	}
	s.SetConn(nil)
	return mcpgo.NewToolResultText("disconnected"), nil
}

func (s *mcpServer) handleDeviceInfo(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	resp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		return mcpgo.NewToolResultError(grpcErrString(err)), nil
	}
	info := map[string]any{
		"version":          resp.GetVersion(),
		"os":               resp.GetOs(),
		"cpu_architecture": resp.GetCpuArchitecture(),
		"featureset":       resp.GetFeatureset(),
	}
	if resp.OsVersion != nil {
		info["os_version"] = resp.GetOsVersion()
	}
	if resp.DeviceType != nil {
		info["device_type"] = resp.GetDeviceType()
	}
	if resp.StorageMedium != nil {
		info["storage_medium"] = resp.GetStorageMedium()
	}
	if resp.HasGpu != nil {
		info["has_gpu"] = resp.GetHasGpu()
	}
	if resp.GpuVendor != nil {
		info["gpu_vendor"] = resp.GetGpuVendor()
	}
	if resp.JetpackVersion != nil {
		info["jetpack_version"] = resp.GetJetpackVersion()
	}
	if resp.CudaVersion != nil {
		info["cuda_version"] = resp.GetCudaVersion()
	}
	b, _ := json.MarshalIndent(info, "", "  ")
	return mcpgo.NewToolResultText(string(b)), nil
}

func (s *mcpServer) handleDeviceSetDefault(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	address := stringParam(req, "address")
	if address == "" {
		return mcpgo.NewToolResultError("address is required"), nil
	}
	s.cfg.DefaultDevice = address
	if err := config.Save(s.cfg); err != nil {
		return mcpgo.NewToolResultError(fmt.Sprintf("saving config: %s", err.Error())), nil
	}
	return mcpgo.NewToolResultText(fmt.Sprintf("default device set to %s", address)), nil
}
