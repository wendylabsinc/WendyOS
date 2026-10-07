package cloudenroll

import (
	"testing"

	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/protobuf/proto"
)

func TestHardwareFromDeviceInfo(t *testing.T) {
	s := proto.String
	thor := &agentpbv2.GetDeviceInfoResponse{
		Os: "linux", OsVersion: s("0.20.0"), CpuArchitecture: "arm64",
		BoardModel:    s("NVIDIA Jetson AGX Thor Developer Kit"),
		SocCompatible: []string{"nvidia,p3971-0080+p3834-0008", "nvidia,p3834-0008", "nvidia,tegra264"},
		SerialNumber:  s("1424325030133"), KernelVersion: s("6.8.12-tegra"), L4TVersion: s("38.2.0"),
		GpuArch: s("sm_110"), CpuCount: 14, MemTotalBytes: 131_000_000_000, DiskTotalBytes: proto.Int64(1_000_000_000_000),
		// Reported by GetDeviceInfo but not part of the enrollment record.
		UptimeSeconds: proto.Uint64(42), PrimaryMac: s("aa:bb:cc:dd:ee:ff"),
	}
	want := &cloudpbv2.DeviceHardware{
		BoardModel: s("NVIDIA Jetson AGX Thor Developer Kit"), SocCompatible: s("nvidia,tegra264"),
		SerialNumber: s("1424325030133"), KernelVersion: s("6.8.12-tegra"), L4TVersion: s("38.2.0"),
		CpuCount: proto.Uint32(14), MemTotalBytes: proto.Int64(131_000_000_000), DiskTotalBytes: proto.Int64(1_000_000_000_000),
		Os: s("linux"), OsVersion: s("0.20.0"), Architecture: s("arm64"), GpuArch: s("sm_110"),
	}
	if got := hardwareFromDeviceInfo(thor); !proto.Equal(got, want) {
		t.Fatalf("got %v\nwant %v", got, want)
	}

	// An x86 host or an older agent: nothing read means nothing sent, never
	// an empty string or a zero count.
	empty := hardwareFromDeviceInfo(&agentpbv2.GetDeviceInfoResponse{OsVersion: s(""), DiskTotalBytes: proto.Int64(0)})
	if !proto.Equal(empty, &cloudpbv2.DeviceHardware{}) {
		t.Fatalf("unread fields were sent: %v", empty)
	}
}
