package services

import (
	"context"
	"os"
	"runtime"
	"strings"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/gpudiscovery"
	"github.com/wendylabsinc/wendy/go/internal/agent/hoststats"
	"github.com/wendylabsinc/wendy/go/internal/shared/version"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type DeviceInfoService struct {
	agentpbv2.UnimplementedWendyDeviceInfoServiceServer
	logger                   *zap.Logger
	hardwareDiscoverer       HardwareDiscoverer
	discoverGPUs             func() []gpudiscovery.Device
	discoverContainerStorage func() (partitionUsage, bool)
}

func NewDeviceInfoService(logger *zap.Logger, hd HardwareDiscoverer) *DeviceInfoService {
	return &DeviceInfoService{logger: logger, hardwareDiscoverer: hd}
}

func (s *DeviceInfoService) GetDeviceInfo(_ context.Context, _ *agentpbv2.GetDeviceInfoRequest) (*agentpbv2.GetDeviceInfoResponse, error) {
	resp := &agentpbv2.GetDeviceInfoResponse{
		Version:         version.Version,
		Os:              detectOS(),
		CpuArchitecture: runtime.GOARCH,
		Featureset:      detectFeatureset(),
	}

	if v, ok := wendyOSVersion(); ok {
		resp.OsVersion = &v
	} else if _, distroVer := detectDistro(); distroVer != "" {
		resp.OsVersion = &distroVer
	}

	if data, err := os.ReadFile("/etc/wendyos/device-type"); err == nil {
		v := strings.TrimSpace(string(data))
		resp.DeviceType = &v
	}

	gpuProbe := s.discoverGPUs
	if gpuProbe == nil {
		gpuProbe = gpudiscovery.Host
	}
	gpuInfo := detectGPUInfoFrom(gpuProbe())
	resp.GpuCapabilities = gpuCapabilitiesV2(gpuInfo.devices)
	resp.HasGpu = &gpuInfo.hasGPU
	if gpuInfo.vendor != "" {
		resp.GpuVendor = &gpuInfo.vendor
	}
	if gpuInfo.jetpackVersion != "" {
		resp.JetpackVersion = &gpuInfo.jetpackVersion
	}
	if gpuInfo.cudaVersion != "" {
		resp.CudaVersion = &gpuInfo.cudaVersion
	}
	if gpuInfo.gpuArch != "" {
		resp.GpuArch = &gpuInfo.gpuArch
	}

	npuInfo := detectNPUInfo()
	resp.HasNpu = &npuInfo.hasNPU
	if npuInfo.vendor != "" {
		resp.NpuVendor = &npuInfo.vendor
	}

	if usage, ok := rootDiskUsage(); ok {
		resp.DiskUsedBytes = &usage.usedBytes
		resp.DiskTotalBytes = &usage.totalBytes
	}

	storageProbe := s.discoverContainerStorage
	if storageProbe == nil {
		storageProbe = containerStorageUsage
	}
	if p, ok := storageProbe(); ok {
		resp.ContainerStorage = &agentpbv2.DiskPartition{Mountpoint: p.mountpoint, Filesystem: p.filesystem, Device: p.device, UsedBytes: p.usedBytes, TotalBytes: p.totalBytes}
	}

	resp.MemTotalBytes, resp.CpuCount = hostMemAndCPUCount()

	for _, p := range listDiskPartitions() {
		resp.Partitions = append(resp.Partitions, &agentpbv2.DiskPartition{
			Mountpoint: p.mountpoint,
			Filesystem: p.filesystem,
			Device:     p.device,
			UsedBytes:  p.usedBytes,
			TotalBytes: p.totalBytes,
		})
	}

	return resp, nil
}

// hostMemAndCPUCount reads the device's total RAM and online logical CPU core
// count from /proc for the device-info response. Values are zero when
// unreadable (non-Linux hosts) — a real host never has 0 RAM or 0 CPUs, so
// consumers treat zero as "unknown".
// REFACTOR: if zero ever needs to be distinguishable from unreadable, make
// mem_total_bytes/cpu_count `optional` the next time device_info_service.proto
// is touched, and return pointers here.
func hostMemAndCPUCount() (memTotal int64, cpuCount uint32) {
	if mem, err := hoststats.ReadMemory(); err == nil {
		memTotal = mem.TotalBytes
	}
	if cpu, err := hoststats.ReadCPU(); err == nil {
		cpuCount = cpu.CPUCount
	}
	return memTotal, cpuCount
}

func (s *DeviceInfoService) ListHardwareCapabilities(ctx context.Context, req *agentpbv2.ListHardwareCapabilitiesRequest) (*agentpbv2.ListHardwareCapabilitiesResponse, error) {
	caps, err := s.hardwareDiscoverer.Discover(ctx, req.GetCategoryFilter())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "hardware discovery failed: %v", err)
	}
	v2caps := make([]*agentpbv2.ListHardwareCapabilitiesResponse_HardwareCapability, len(caps))
	for i, c := range caps {
		v2caps[i] = &agentpbv2.ListHardwareCapabilitiesResponse_HardwareCapability{
			Category:    c.Category,
			DevicePath:  c.DevicePath,
			Description: c.Description,
			Properties:  c.Properties,
		}
	}
	return &agentpbv2.ListHardwareCapabilitiesResponse{Capabilities: v2caps}, nil
}

// gpuCapabilitiesV2 is the v2 twin of gpuCapabilitiesV1.
func gpuCapabilitiesV2(devices []gpudiscovery.Device) []*agentpbv2.GpuCapabilities {
	out := make([]*agentpbv2.GpuCapabilities, 0, len(devices))
	for _, d := range devices {
		out = append(out, &agentpbv2.GpuCapabilities{
			Vendor:          d.Vendor,
			Path:            d.Path,
			ComputeBackends: append([]string{}, d.ComputeBackends...),
		})
	}
	return out
}
