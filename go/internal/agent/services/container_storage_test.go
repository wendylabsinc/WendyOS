package services

import (
	"context"
	"reflect"
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
	gpu := func() []gpudiscovery.Device {
		return []gpudiscovery.Device{{Path: "/dev/dri/card0", Vendor: "broadcom"}}
	}
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
	if !v1.GetHasGpu() || v1.GetGpuVendor() != "broadcom" {
		t.Fatalf("v1 has_gpu=%v vendor=%q; want a broadcom GPU", v1.GetHasGpu(), v1.GetGpuVendor())
	}
	// A GPU without a supported backend is still listed, with an empty backend
	// list, so callers can tell "no compute" from "older agent".
	v1GPUs, v2GPUs := v1.GetGpuCapabilities(), v2.GetGpuCapabilities()
	if len(v1GPUs) != 1 || v1GPUs[0].GetVendor() != "broadcom" || v1GPUs[0].GetPath() != "/dev/dri/card0" || len(v1GPUs[0].GetComputeBackends()) != 0 {
		t.Fatalf("v1 gpu entries = %+v; want one broadcom entry without backends", v1GPUs)
	}
	if len(v2GPUs) != 1 || v2GPUs[0].GetVendor() != "broadcom" || v2GPUs[0].GetPath() != "/dev/dri/card0" || len(v2GPUs[0].GetComputeBackends()) != 0 {
		t.Fatalf("v2 gpu entries = %+v; want one broadcom entry without backends", v2GPUs)
	}
}

func TestDeviceMetadataListsEveryGPU(t *testing.T) {
	gpu := func() []gpudiscovery.Device {
		return []gpudiscovery.Device{
			{Path: "/dev/dri/card0", Vendor: "amd", ComputeBackends: []string{"rocm"}},
			{Path: "/dev/dri/card1", Vendor: "nvidia", ComputeBackends: []string{"cuda"}},
		}
	}
	v1, err := (&AgentService{discoverGPUs: gpu}).GetAgentVersion(context.Background(), &agentpb.GetAgentVersionRequest{})
	if err != nil {
		t.Fatal(err)
	}
	v2, err := (&DeviceInfoService{discoverGPUs: gpu}).GetDeviceInfo(context.Background(), &agentpbv2.GetDeviceInfoRequest{})
	if err != nil {
		t.Fatal(err)
	}
	want := []struct{ vendor, path, backend string }{
		{"amd", "/dev/dri/card0", "rocm"},
		{"nvidia", "/dev/dri/card1", "cuda"},
	}
	v1GPUs, v2GPUs := v1.GetGpuCapabilities(), v2.GetGpuCapabilities()
	if len(v1GPUs) != len(want) || len(v2GPUs) != len(want) {
		t.Fatalf("v1 %d entries, v2 %d entries; want %d each", len(v1GPUs), len(v2GPUs), len(want))
	}
	for i, w := range want {
		if v1GPUs[i].GetVendor() != w.vendor || v1GPUs[i].GetPath() != w.path || !reflect.DeepEqual(v1GPUs[i].GetComputeBackends(), []string{w.backend}) {
			t.Fatalf("v1 entry %d = %+v; want %+v", i, v1GPUs[i], w)
		}
		if v2GPUs[i].GetVendor() != w.vendor || v2GPUs[i].GetPath() != w.path || !reflect.DeepEqual(v2GPUs[i].GetComputeBackends(), []string{w.backend}) {
			t.Fatalf("v2 entry %d = %+v; want %+v", i, v2GPUs[i], w)
		}
	}
	if !v1.GetHasGpu() || v1.GetGpuVendor() == "" {
		t.Fatalf("legacy singular fields dropped: has_gpu=%v vendor=%q", v1.GetHasGpu(), v1.GetGpuVendor())
	}
}
