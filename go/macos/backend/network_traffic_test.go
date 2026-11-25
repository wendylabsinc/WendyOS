package backend

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type networkStatsAgent struct {
	pb.UnimplementedWendyContainerServiceServer
	samples atomic.Uint64
	fail    atomic.Bool
}

func (s *networkStatsAgent) GetNetworkStats(context.Context, *pb.GetNetworkStatsRequest) (*pb.NetworkStats, error) {
	if s.fail.Load() {
		return nil, status.Error(codes.Unimplemented, "old agent")
	}
	n := s.samples.Add(1)
	return &pb.NetworkStats{BootId: "boot", SampledAtBoottimeNs: n * 1e9, Agent: &pb.AgentNetworkStats{AccountingId: "agent", SentBytes: n * 100}}, nil
}

func TestNetworkTrafficBackendSessionAndFailureReset(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	fake := &networkStatsAgent{}
	pb.RegisterWendyContainerServiceServer(server, fake)
	go server.Serve(listener)
	conn, err := grpc.NewClient("passthrough:///network-stats", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	b := New()
	b.conn, b.session = grpcclient.NewFromConn(conn), 7
	b.life, b.stop = context.WithCancel(context.Background())
	t.Cleanup(func() { b.Shutdown(); server.Stop(); listener.Close() })
	first, err := b.NetworkTraffic(7)
	if err != nil || first.Agent.SendBytesPerSecond != nil {
		t.Fatalf("first sample: %+v, %v", first, err)
	}
	second, err := b.NetworkTraffic(7)
	if err != nil || second.Agent.SendBytesPerSecond == nil || *second.Agent.SendBytesPerSecond != 100 {
		t.Fatalf("second sample: %+v, %v", second, err)
	}
	b.networkSampleAt = time.Now().Add(-time.Minute)
	resumed, err := b.NetworkTraffic(7)
	if err != nil || resumed.Agent.SendBytesPerSecond != nil {
		t.Fatal("resumed screen reused old rate")
	}
	b.session = 8
	if _, err := b.NetworkTraffic(7); err == nil {
		t.Fatal("accepted stale device session")
	}
	changed, err := b.NetworkTraffic(8)
	if err != nil || changed.Agent.SendBytesPerSecond != nil {
		t.Fatal("new session reused old rate")
	}
	fake.fail.Store(true)
	if _, err := b.NetworkTraffic(8); err == nil || status.Code(err) != codes.Unimplemented {
		t.Fatalf("old agent error: %v", err)
	}
	fake.fail.Store(false)
	recovered, err := b.NetworkTraffic(8)
	if err != nil || recovered.Agent.SendBytesPerSecond != nil {
		t.Fatal("recovery reused failed sample")
	}
}
