package cloudenroll

import (
	"context"
	"fmt"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
)

// ReadHardware asks the connected agent for its GetDeviceInfo report and
// returns the part EnrollDevice records on the asset row (WDY-3544). The
// report is descriptive and optional in the contract, so callers enroll
// without it when this fails rather than abandoning the enrollment.
func ReadHardware(ctx context.Context, conn grpc.ClientConnInterface) (*cloudpbv2.DeviceHardware, error) {
	info, err := agentpbv2.NewWendyDeviceInfoServiceClient(conn).GetDeviceInfo(ctx, &agentpbv2.GetDeviceInfoRequest{})
	if err != nil {
		return nil, fmt.Errorf("reading device hardware: %w", err)
	}
	return hardwareFromDeviceInfo(info), nil
}

// hardwareFromDeviceInfo copies the fields DeviceHardware carries. The
// contract says absent means "could not read", never zero or empty, so the
// agent's empty strings and zero counts become unset fields.
func hardwareFromDeviceInfo(info *agentpbv2.GetDeviceInfoResponse) *cloudpbv2.DeviceHardware {
	str := func(s string) *string {
		if s == "" {
			return nil
		}
		return &s
	}
	hw := &cloudpbv2.DeviceHardware{
		BoardModel:    str(info.GetBoardModel()),
		SerialNumber:  str(info.GetSerialNumber()),
		KernelVersion: str(info.GetKernelVersion()),
		L4TVersion:    str(info.GetL4TVersion()),
		Os:            str(info.GetOs()),
		OsVersion:     str(info.GetOsVersion()),
		Architecture:  str(info.GetCpuArchitecture()),
		GpuArch:       str(info.GetGpuArch()),
	}
	// Device-tree compatible lists run most specific first, so the SoC is the
	// last entry ("nvidia,tegra264" on Jetson Thor).
	if c := info.GetSocCompatible(); len(c) > 0 {
		hw.SocCompatible = str(c[len(c)-1])
	}
	if n := info.GetCpuCount(); n > 0 {
		hw.CpuCount = &n
	}
	if n := info.GetMemTotalBytes(); n > 0 {
		hw.MemTotalBytes = &n
	}
	if n := info.GetDiskTotalBytes(); n > 0 {
		hw.DiskTotalBytes = &n
	}
	return hw
}
