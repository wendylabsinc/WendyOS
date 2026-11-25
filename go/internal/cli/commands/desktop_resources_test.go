package commands

import (
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"testing"
)

func TestDesktopResourceSamples(t *testing.T) {
	apps := []*pb.AppContainer{{AppName: "example", RunningState: pb.AppRunningState_RUNNING, Services: []*pb.ServiceEntry{{Name: "api", RunningState: pb.AppRunningState_RUNNING}, {Name: "db", RunningState: pb.AppRunningState_CRASH_LOOPING}}}}
	sample := func(total, idle, cpu uint64, at int64) topSample {
		return newTopSample(&pb.GetResourceStatsResponse{Host: &pb.HostStats{CpuTotalJiffies: total, CpuIdleJiffies: idle, CpuCount: 4}, Containers: []*pb.ResourceContainerStats{{AppName: "example_api", CpuUsageNanos: cpu, MemoryBytes: 128}}}, at)
	}
	first := sample(100, 50, 1_000_000_000, 1_000_000_000)
	got := buildDesktopResources(nil, first, apps)
	if got.HasCPU || got.Rows[0].HasCPU {
		t.Fatal("first sample must not claim a CPU reading")
	}
	next := sample(200, 100, 2_000_000_000, 2_000_000_000)
	got = buildDesktopResources(&first, next, apps)
	if !got.HasCPU || got.CPUPercent != 50 || len(got.Rows) != 3 {
		t.Fatalf("bad host or grouping: %+v", got)
	}
	if got.Rows[0].CPUPercent != 25 || got.Rows[0].MemoryBytes != 128 {
		t.Fatalf("bad aggregate: %+v", got.Rows[0])
	}
	if got.Rows[1].ID != "example/api" || !got.Rows[1].IsService || got.Rows[2].State != pb.AppRunningState_CRASH_LOOPING || got.Rows[2].HasCPU {
		t.Fatalf("bad services: %+v", got.Rows)
	}
	reset := sample(10, 5, 1, 3_000_000_000)
	if got := buildDesktopResources(&next, reset, apps); got.HasCPU || got.Rows[0].CPUPercent != 0 {
		t.Fatalf("counter reset produced CPU spike: %+v", got)
	}
}
