package services

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/agent/hoststats"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func networkStats(ctx context.Context) *pb.NetworkStats {
	sample := hoststats.ReadNetwork(ctx)
	result := &pb.NetworkStats{BootId: sample.BootID, SampledAtBoottimeNs: sample.BootTimeNS,
		InterfacesUnavailableReason: sample.InterfacesError, AgentUnavailableReason: sample.AgentError}
	for _, iface := range sample.Interfaces {
		result.Interfaces = append(result.Interfaces, &pb.NetworkInterfaceStats{Name: iface.Name, Index: iface.Index,
			ReceivedBytes: iface.ReceivedBytes, SentBytes: iface.SentBytes, Virtual: iface.Virtual, Loopback: iface.Loopback})
	}
	if agent := sample.Agent; agent != nil {
		result.Agent = &pb.AgentNetworkStats{ReceivedBytes: agent.ReceivedBytes, SentBytes: agent.SentBytes, AccountingId: agent.AccountingID}
	}
	return result
}

func (s *ContainerService) GetNetworkStats(ctx context.Context, _ *pb.GetNetworkStatsRequest) (*pb.NetworkStats, error) {
	return networkStats(ctx), nil
}
