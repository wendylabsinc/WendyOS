package services

import (
	"context"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/gpudiscovery"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestContainerStorageMountIdentity(t *testing.T) {
	info := `24 1 8:1 / / rw - ext4 /dev/sda1 rw
25 24 259:1 / /data rw - ext4 /dev/nvme0n1p1 rw
26 24 259:1 /containerd /var/lib/containerd rw - ext4 /dev/disk/by-uuid/data rw
27 24 8:2 / /var/lib/containerd-other rw - ext4 /dev/sda2 rw
`
	usage := func(path string) (diskUsage, bool) {
		if path != "/var/lib/containerd" {
			t.Fatalf("statfs queried %q", path)
		}
		return diskUsage{usedBytes: 150, totalBytes: 1000}, true
	}
	p, ok := discoverContainerStorage(info, "/var/lib/containerd", "259:1", usage)
	if !ok || p.mountpoint != "/data" || p.device != "/dev/nvme0n1p1" || p.totalBytes != 1000 {
		t.Fatalf("storage = %+v, %v", p, ok)
	}
	if _, ok := discoverContainerStorage(info, "/var/lib/containerd", "8:2", usage); ok {
		t.Fatal("matched a path prefix without containment")
	}
	if _, ok := discoverContainerStorage(info, "/var/lib/containerd", "259:1", func(string) (diskUsage, bool) { return diskUsage{}, false }); ok {
		t.Fatal("reported failed statfs")
	}
}

func TestDeviceMetadataStorageAndGPUParity(t *testing.T) {
	storage := func() (partitionUsage, bool) {
		return partitionUsage{mountpoint: "/data", device: "/dev/nvme0n1p1", totalBytes: 1000, usedBytes: 150}, true
	}
	gpu := func() []gpudiscovery.Device { return []gpudiscovery.Device{{Vendor: "broadcom"}} }
	v1, err := (&AgentService{discoverContainerStorage: storage, discoverGPUs: gpu}).GetAgentVersion(context.Background(), &agentpb.GetAgentVersionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := (&DeviceInfoService{discoverContainerStorage: storage, discoverGPUs: gpu}).GetDeviceInfo(context.Background(), &agentpbv2.GetDeviceInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if v1.GetContainerStorage().GetTotalBytes() != v2.GetContainerStorage().GetTotalBytes() || v1.GetContainerStorage().GetMountpoint() != "/data" {
		t.Fatal("storage metadata mismatch")
	}
	if !v1.GetHasGpu() || v1.GetGpuVendor() != "broadcom" || v1.GpuCapabilities == nil || len(v1.GpuCapabilities.ComputeBackends) != 0 || v2.GpuCapabilities == nil {
		t.Fatal("hardware presence confused with compute support")
	}
}
