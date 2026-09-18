package commands

import (
	"context"
	"fmt"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/robotprobe"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// agentHostFacts adapts the agent's device-info RPC to robotprobe.HostFactsSource. It is
// the only place that knows the agent's wire format, which is what keeps the probes
// themselves free of proto types and testable with a literal.
//
// The fetch is cached: four probes read these facts, and a report should interrogate the
// device once rather than once per section.
type agentHostFacts struct {
	conn *grpcclient.AgentConnection

	once  sync.Once
	facts *robotprobe.HostFacts
	err   error
}

func newAgentHostFacts(conn *grpcclient.AgentConnection) *agentHostFacts {
	return &agentHostFacts{conn: conn}
}

func (a *agentHostFacts) HostFacts(ctx context.Context) (*robotprobe.HostFacts, error) {
	a.once.Do(func() {
		resp, err := a.conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
		if err != nil {
			a.err = fmt.Errorf("reading device info: %w", err)
			return
		}
		a.facts = hostFactsFromAgent(resp)
	})
	return a.facts, a.err
}

// hostFactsFromAgent flattens the agent's response. Optional fields arrive as pointers,
// and an absent one stays absent rather than becoming a zero that reads as a measurement.
func hostFactsFromAgent(resp *agentpb.GetAgentVersionResponse) *robotprobe.HostFacts {
	facts := &robotprobe.HostFacts{
		Hostname:         resp.GetHostname(),
		DeviceType:       resp.GetDeviceType(),
		CPUArchitecture:  resp.GetCpuArchitecture(),
		CPUCount:         int(resp.GetCpuCount()),
		MemoryTotalBytes: resp.GetMemTotalBytes(),
		OS:               resp.GetOs(),
		OSVersion:        resp.GetOsVersion(),
		AgentVersion:     resp.GetVersion(),
		HasGPU:           resp.GetHasGpu(),
		GPUVendor:        resp.GetGpuVendor(),
		GPUArch:          resp.GetGpuArch(),
		CUDAVersion:      resp.GetCudaVersion(),
		JetpackVersion:   resp.GetJetpackVersion(),
		HasNPU:           resp.GetHasNpu(),
		NPUVendor:        resp.GetNpuVendor(),
		DiskTotalBytes:   resp.GetDiskTotalBytes(),
		DiskUsedBytes:    resp.GetDiskUsedBytes(),
		StorageMedium:    resp.GetStorageMedium(),
	}

	for _, capability := range resp.GetGpuCapabilities() {
		facts.ComputeBackends = append(facts.ComputeBackends, capability.GetComputeBackends()...)
	}
	if cs := resp.GetContainerStorage(); cs != nil {
		facts.ContainerStorage = &robotprobe.Partition{
			Mountpoint: cs.GetMountpoint(),
			TotalBytes: cs.GetTotalBytes(),
			UsedBytes:  cs.GetUsedBytes(),
		}
	}
	for _, iface := range resp.GetNetworkInterfaces() {
		facts.Interfaces = append(facts.Interfaces, robotprobe.HostInterface{
			Name:      iface.GetName(),
			Addresses: iface.GetIpAddresses(),
		})
	}
	if battery := resp.GetBattery(); battery != nil {
		facts.Battery = &robotprobe.BatteryFacts{
			Percent: battery.GetPercent(),
			State:   battery.GetState().String(),
		}
	}
	return facts
}
