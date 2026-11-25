package main

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type peer struct {
	agentpb.UnimplementedWendyAgentServiceServer
	response *agentpb.GetAgentVersionResponse
	calls    atomic.Int32
}

func (p *peer) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	p.calls.Add(1)
	return p.response, nil
}

type networkPeer struct {
	agentpb.UnimplementedWendyContainerServiceServer
}

func (p *networkPeer) GetNetworkStats(context.Context, *agentpb.GetNetworkStatsRequest) (*agentpb.NetworkStats, error) {
	return &agentpb.NetworkStats{BootId: "11111111-2222-4333-8444-555555555555", SampledAtBoottimeNs: 1000000000}, nil
}

func TestReadOnlyLoopbackIdentity(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &peer{response: &agentpb.GetAgentVersionResponse{Hostname: "owned-vm", Version: "candidate-agent", Os: "wendyos", OsVersion: proto.String("WendyOS-0.20.2"), CpuArchitecture: "arm64", DeviceType: proto.String("vm-arm64")}}
	server := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(server, p)
	agentpb.RegisterWendyContainerServiceServer(server, &networkPeer{})
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		server.Stop()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("owned gRPC server did not drain")
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	nonce := "0123456789abcdef0123456789abcdef"
	value, err := observe(ctx, listener.Addr().String(), nonce)
	if err != nil || value.Hostname != "owned-vm" || value.BootID != "11111111-2222-4333-8444-555555555555" || value.BootTimeNS != 1000000000 || value.Address != listener.Addr().String() || value.Nonce != nonce || value.OSVersion != "WendyOS-0.20.2" {
		t.Fatalf("actual RPC identity=%+v error=%v", value, err)
	}
	if p.calls.Load() != 1 {
		t.Fatal("unexpected calls", p.calls.Load())
	}
	for _, address := range []string{"localhost:50051", "192.0.2.1:50051", "127.0.0.1:80", "127.0.0.1:65535", "unix:///tmp/not-used"} {
		if _, err := observe(ctx, address, nonce); err == nil {
			t.Fatal("unowned destination accepted", address)
		}
	}
	if _, err := observe(ctx, listener.Addr().String(), "bad"); err == nil {
		t.Fatal("invalid nonce accepted")
	}
	if p.calls.Load() != 1 {
		t.Fatal("invalid inputs reached network")
	}
}
