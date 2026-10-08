package mcp

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func (s *mcpServer) registerDeviceTools(srv *server.MCPServer) {
	listOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("List configured and online cloud devices; scan=true adds LAN discovery. Pass a returned device selector to device_connect or run. Cloud failures appear as warnings. On Linux, a USB-C-tethered device the host can't reach until a person approves a one-time setup appears as a usb warning: relay its instructions to the user."),
		mcpgo.WithBoolean("scan", mcpgo.Description("If true, run a live mDNS scan (3 s) in addition to returning configured devices")),
		mcpgo.WithString("cloud_grpc", mcpgo.Description("Cloud gRPC endpoint to use (optional when a default auth session is selected via 'wendy auth use')")),
		mcpgo.WithInteger("max_bytes", mcpgo.Min(1), mcpgo.Max(1000000), mcpgo.DefaultNumber(16384), mcpgo.Description("JSON byte limit; complete devices retained with omitted count")),
	}
	listOpts = append(listOpts, readOnly()...)
	listOpts = append(listOpts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("device_list", listOpts...), s.handleDeviceList)

	connectOpts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Connect to a LAN, simulator, or cloud device using its discovery selector."),
		mcpgo.WithString("device", mcpgo.Required(), mcpgo.Description("device from device_list, a host:port, or vm:name")),
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

	infoOpts := []mcpgo.ToolOption{mcpgo.WithDescription("Get device versions, hardware, storage, and battery percentage/charge state. Missing battery fields mean unavailable readings or estimates. No ROS app required.")}
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
	maxBytes, err := ros2Int(req, "max_bytes", 16384, 1, 1000000)
	if err != nil {
		return errResult(errCodeInvalidArgument, err.Error()), nil
	}
	scan := req.GetBool("scan", false)

	// Run cloud discovery alongside the optional LAN scan, so an unreachable
	// cloud cannot extend local discovery by an unbounded amount of time.
	type cloudResult struct {
		devices []map[string]any
		err     error
	}
	cloudResults := make(chan cloudResult, 1)
	go func() {
		devices, err := s.listCloudDevices(ctx, stringParam(req, "cloud_grpc"))
		cloudResults <- cloudResult{devices: devices, err: err}
	}()

	var devices []map[string]any
	if s.cfg.DefaultDevice != "" {
		devices = append(devices, map[string]any{
			"device":  s.cfg.DefaultDevice,
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
					addr = net.JoinHostPort(addr, strconv.Itoa(d.Port))
				}
				entry := map[string]any{
					"device":  addr,
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

	cloud := <-cloudResults
	devices = append(devices, cloud.devices...)
	out := map[string]any{}
	var warnings []map[string]any
	if cloud.err != nil {
		warnings = append(warnings, map[string]any{
			"source":  "cloud",
			"message": fmt.Sprintf("Cloud discovery unavailable: %s", cloud.err),
		})
	}
	if s.usbSetupNoticeFn != nil {
		if msg := s.usbSetupNoticeFn(); msg != "" {
			warnings = append(warnings, map[string]any{"source": "usb", "message": msg})
		}
	}
	if len(warnings) > 0 {
		out["warnings"] = warnings
	}
	return okRowsBounded("devices", devices, out, maxBytes, len(devices)), nil
}

func (s *mcpServer) listCloudDevices(ctx context.Context, cloudGRPC string) ([]map[string]any, error) {
	if len(s.currentConfig().Auth) == 0 && cloudGRPC == "" {
		return nil, nil // Local-only installations do not require cloud login.
	}
	auth, err := s.cloudAuthEntry(cloudGRPC)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	devices, err := discoverCloudDevices(ctx, auth, "", true)
	if err != nil {
		return nil, err
	}
	for _, entry := range devices {
		entry["type"] = "cloud"
		entry["source"] = "cloud"
		entry["cloud_grpc"] = auth.CloudGRPC
		entry["online"] = true // ListAssets requested active tunnel broker presence.
	}
	return devices, nil
}

func (s *mcpServer) handleDeviceConnect(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	device, address := stringParam(req, "device"), stringParam(req, "address")
	if device != "" && address != "" && device != address {
		return errResult(errCodeInvalidArgument, "device and legacy address must not conflict"), nil
	}
	if device != "" {
		address = device
	}
	if strings.TrimSpace(address) == "" {
		return errResult(errCodeInvalidArgument, "device is required (legacy address is also accepted)"), nil
	}
	if err := s.ConnectTo(ctx, address); err != nil {
		return errResultf(errCodeDeviceUnreachable, "connecting to %s: %s", address, err.Error()), nil
	}
	return okText(fmt.Sprintf("connected to %s", address)), nil
}

func (s *mcpServer) handleDeviceDisconnect(_ context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	conn := s.GetConn()
	// Even when disconnected, invalidate any automatic connection still in flight.
	s.SetConn(nil)
	if conn == nil {
		return okText("not connected"), nil
	}
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
	if p := resp.GetContainerStorage(); p != nil {
		info["container_storage"] = map[string]any{"mountpoint": p.GetMountpoint(), "filesystem": p.GetFilesystem(), "device": p.GetDevice(), "used_bytes": p.GetUsedBytes(), "total_bytes": p.GetTotalBytes()}
	}
	if gpus := resp.GetGpuCapabilities(); len(gpus) > 0 {
		entries := make([]map[string]any, 0, len(gpus))
		for _, gpu := range gpus {
			entries = append(entries, map[string]any{"vendor": gpu.GetVendor(), "path": gpu.GetPath(), "compute_backends": append([]string{}, gpu.GetComputeBackends()...)})
		}
		info["gpu_capabilities"] = entries
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
	if backends := resp.GetNpuBackends(); len(backends) > 0 {
		info["npu_backends"] = append([]string{}, backends...)
	}
	if battery := resp.GetBattery(); battery != nil {
		entry := map[string]any{
			"percent": battery.GetPercent(),
			"state":   battery.GetState().String(),
		}
		if battery.SecondsRemaining != nil {
			entry["seconds_remaining"] = battery.GetSecondsRemaining()
		}
		info["battery"] = entry
	}
	return okResult(info), nil
}

func (s *mcpServer) handleDeviceSetDefault(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	address := stringParam(req, "address")
	if address == "" {
		return errResult(errCodeInvalidArgument, "address is required"), nil
	}
	// Change only this field of the config as it is on disk now. Saving the
	// startup snapshot would undo any login, pin or default another wendy
	// process wrote since this server started.
	if err := config.Update(func(cfg *config.Config) (bool, error) {
		cfg.DefaultDevice = address
		return true, nil
	}); err != nil {
		return errResultf(errCodeInternal, "saving config: %s", err.Error()), nil
	}
	s.cfg.DefaultDevice = address
	return okText(fmt.Sprintf("default device set to %s", address)), nil
}
