package commands

import (
	"context"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// DesktopTopSampler shares the CLI top command's CPU normalization, counter
// reset handling, and app/service aggregation. Access is serialized by backend.
type DesktopTopSampler struct {
	previous     *topSample
	storage      *pb.DiskPartition
	storageError string
	storageAt    time.Time
}
type DesktopResourceRow struct {
	ID, App, Name     string
	State             pb.AppRunningState
	CPUPercent        float64
	HasCPU, IsService bool
	MemoryBytes       int64
}
type DesktopResourceSnapshot struct {
	Host         *pb.HostStats
	CPUPercent   float64
	HasCPU       bool
	Storage      *pb.DiskPartition
	StorageError string
	Rows         []DesktopResourceRow
}

func (s *DesktopTopSampler) Sample(ctx context.Context, conn *grpcclient.AgentConnection) (*DesktopResourceSnapshot, error) {
	containers, err := listAppContainers(ctx, conn)
	if err != nil {
		return nil, err
	}
	stats, err := sampleResourceStats(ctx, conn)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	current := newTopSample(stats, now.UnixNano())
	result := buildDesktopResources(s.previous, current, containers)
	s.previous = &current
	if s.storageAt.IsZero() || now.Sub(s.storageAt) >= 10*time.Second {
		s.storageAt = now
		storageCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		storage, err := fetchTopStorage(storageCtx, conn)
		cancel()
		if err != nil {
			s.storageError = err.Error()
		} else {
			s.storage, s.storageError = storage, ""
		}
	}
	result.Storage, result.StorageError = s.storage, s.storageError
	return result, nil
}
func buildDesktopResources(previous *topSample, current topSample, containers []*pb.AppContainer) *DesktopResourceSnapshot {
	result := &DesktopResourceSnapshot{Host: current.host}
	cpus := uint32(1)
	if current.host != nil && current.host.GetCpuCount() > 0 {
		cpus = current.host.GetCpuCount()
	}
	cpuByID := map[string]float64{}
	if previous != nil {
		result.HasCPU = previous.host != nil && current.host != nil && current.host.GetCpuTotalJiffies() > previous.host.GetCpuTotalJiffies()
		result.CPUPercent = hostCPUPercent(*previous, current)
		for id := range current.containers {
			cpuByID[id] = containerCPUPercent(*previous, current, id, cpus)
		}
	}
	parent := ""
	for _, row := range buildTopRows(containers, cpuByID, current.mem, true) {
		name := row.displayName
		if !row.isSubrow {
			parent = row.name
			name = strings.TrimSuffix(name, " [group]")
		} else {
			name = strings.TrimPrefix(strings.TrimSpace(name), "↳ ")
		}
		id := parent
		if row.isSubrow {
			id += "/" + name
		}
		result.Rows = append(result.Rows, DesktopResourceRow{ID: id, App: parent, Name: name,
			State: row.state, CPUPercent: row.cpuPercent, HasCPU: previous != nil && row.hasCPU,
			MemoryBytes: row.memBytes, IsService: row.isSubrow})
	}
	return result
}

// DesktopStartApp waits for the agent's started acknowledgement, then detaches.
func DesktopStartApp(ctx context.Context, conn *grpcclient.AgentConnection, app string) error {
	stream, err := conn.ContainerService.StartContainer(ctx, &pb.StartContainerRequest{AppName: app, RestartPolicy: &pb.RestartPolicy{Mode: pb.RestartPolicyMode_UNLESS_STOPPED}})
	if err != nil {
		return err
	}
	return awaitStarted(stream)
}
