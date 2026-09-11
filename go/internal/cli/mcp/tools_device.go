package mcp

import (
	"context"
	"fmt"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func (s *mcpServer) registerDeviceTools(srv *server.MCPServer) {
	listOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("List wendy devices from config and known addresses. Pass scan=true to also run a live 3-second mDNS scan for devices on the local network."),
		mcpgo.WithBoolean("scan", mcpgo.Description("If true, run a live mDNS scan (3 s) in addition to returning configured devices")),
		mcpgo.WithNumber("max_bytes", mcpgo.Description("Maximum output size in bytes before the result is truncated (default 100000)")),
	}
	listOpts = append(listOpts, readOnly()...)
	listOpts = append(listOpts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("device_list", listOpts...), s.handleDeviceList)

	connectOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Connect to a wendy device by address (host:port)"),
		mcpgo.WithString("address", mcpgo.Required(), mcpgo.Description("Device address, e.g. mydevice.local:50051 or 192.168.1.10:50051")),
	}
	connectOpts = append(connectOpts, mutating()...)
	connectOpts = append(connectOpts, idempotent()...)
	connectOpts = append(connectOpts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("device_connect", connectOpts...), s.handleDeviceConnect)

	disconnectOpts := []mcpgo.ToolOption{mcpgo.WithDescription("Disconnect from the currently connected device")}
	disconnectOpts = append(disconnectOpts, mutating()...)
	disconnectOpts = append(disconnectOpts, idempotent()...)
	disconnectOpts = append(disconnectOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("device_disconnect", disconnectOpts...), s.handleDeviceDisconnect)

	infoOpts := []mcpgo.ToolOption{mcpgo.WithDescription("Get agent version, OS, CPU architecture, GPU info, and feature set of connected device")}
	infoOpts = append(infoOpts, readOnly()...)
	infoOpts = append(infoOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("device_info", infoOpts...), s.handleDeviceInfo)

	setDefaultOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Save an address as the default device in ~/.wendy/config.json"),
		mcpgo.WithString("address", mcpgo.Required(), mcpgo.Description("Device address to save as default, e.g. mydevice.local:50051")),
	}
	setDefaultOpts = append(setDefaultOpts, mutating()...)
	setDefaultOpts = append(setDefaultOpts, idempotent()...)
	setDefaultOpts = append(setDefaultOpts, localOnly()...)
	srv.AddTool(mcpgo.NewTool("device_set_default", setDefaultOpts...), s.handleDeviceSetDefault)
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

	return okListBounded("devices", devices, intParam(req, "max_bytes", 100000)), nil
}

func (s *mcpServer) handleDeviceConnect(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	address := stringParam(req, "address")
	if address == "" {
		return errResult(errCodeInvalidArgument, "address is required"), nil
	}
	if err := s.ConnectTo(ctx, address); err != nil {
		return errResultf(errCodeDeviceUnreachable, "connecting to %s: %s", address, err.Error()), nil
	}
	s.SetConnType("direct")
	return okText(fmt.Sprintf("connected to %s", address)), nil
}

func (s *mcpServer) handleDeviceDisconnect(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return okText("not connected"), nil
	}
	s.SetConn(nil)
	return okText("disconnected"), nil
}

func (s *mcpServer) handleDeviceInfo(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	if conn == nil {
		return errNotConnected(), nil
	}
	resp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		return errResult(codeFromGRPC(err), grpcErrString(err)), nil
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
	if resp.DiskUsedBytes != nil && resp.DiskTotalBytes != nil {
		info["disk_used_bytes"] = resp.GetDiskUsedBytes()
		info["disk_total_bytes"] = resp.GetDiskTotalBytes()
	}
	if len(resp.GetPartitions()) > 0 {
		parts := make([]map[string]any, len(resp.GetPartitions()))
		for i, p := range resp.GetPartitions() {
			parts[i] = map[string]any{
				"mountpoint":  p.GetMountpoint(),
				"filesystem":  p.GetFilesystem(),
				"device":      p.GetDevice(),
				"used_bytes":  p.GetUsedBytes(),
				"total_bytes": p.GetTotalBytes(),
			}
		}
		info["partitions"] = parts
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
	if resp.GpuArch != nil {
		info["gpu_arch"] = resp.GetGpuArch()
	}
	if resp.HasNpu != nil {
		info["has_npu"] = resp.GetHasNpu()
	}
	if resp.NpuVendor != nil {
		info["npu_vendor"] = resp.GetNpuVendor()
	}
	return okResult(info), nil
}

func (s *mcpServer) handleDeviceSetDefault(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	address := stringParam(req, "address")
	if address == "" {
		return errResult(errCodeInvalidArgument, "address is required"), nil
	}
	s.cfg.DefaultDevice = address
	if err := config.Save(s.cfg); err != nil {
		return errResultf(errCodeInternal, "saving config: %s", err.Error()), nil
	}
	return okText(fmt.Sprintf("default device set to %s", address)), nil
}
