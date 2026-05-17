package services

import (
	"context"
	"os"
	"runtime"
	"strings"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/internal/shared/version"
	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
)

type DeviceInfoService struct {
	agentpbv2.UnimplementedWendyDeviceInfoServiceServer
	logger             *zap.Logger
	hardwareDiscoverer HardwareDiscoverer
}

func NewDeviceInfoService(logger *zap.Logger, hd HardwareDiscoverer) *DeviceInfoService {
	return &DeviceInfoService{logger: logger, hardwareDiscoverer: hd}
}

func (s *DeviceInfoService) GetDeviceInfo(_ context.Context, _ *agentpbv2.GetDeviceInfoRequest) (*agentpbv2.GetDeviceInfoResponse, error) {
	resp := &agentpbv2.GetDeviceInfoResponse{
		Version:         version.Version,
		Os:              runtime.GOOS,
		CpuArchitecture: runtime.GOARCH,
		Featureset:      detectFeatureset(),
	}

	if data, err := os.ReadFile("/etc/wendy/version.txt"); err == nil {
		v := strings.TrimSpace(string(data))
		resp.OsVersion = &v
	}

	if data, err := os.ReadFile("/etc/wendyos/device-type"); err == nil {
		deviceType, storageMedium := parseDeviceType(string(data))
		if deviceType != "" {
			resp.DeviceType = &deviceType
		}
		if storageMedium != "" {
			resp.StorageMedium = &storageMedium
		}
	}

	gpuInfo := detectGPUInfo()
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

	return resp, nil
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
