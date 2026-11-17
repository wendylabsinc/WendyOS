//go:build darwin || linux || windows

package commands

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// This peer exercises real setup-listener RPC framing and upload bytes. It does
// not emulate module activation; that requires the real driver VM/physical gate.
type firstBootDriverPeer struct {
	agentpb.UnimplementedWendyAgentServiceServer
	pb.UnimplementedWendyDriverServiceServer
	hostname, board, kernel string
	writes                  atomic.Int32
	mu                      sync.Mutex
	spec                    *pb.DriverSpec
	payload                 []byte
	chunks                  int
}

func (p *firstBootDriverPeer) GetAgentVersion(context.Context, *agentpb.GetAgentVersionRequest) (*agentpb.GetAgentVersionResponse, error) {
	return &agentpb.GetAgentVersionResponse{Hostname: p.hostname, DeviceType: &p.board}, nil
}
func (p *firstBootDriverPeer) ListDrivers(context.Context, *pb.ListDriversRequest) (*pb.ListDriversResponse, error) {
	return &pb.ListDriversResponse{KernelVersion: p.kernel}, nil
}
func (p *firstBootDriverPeer) InstallDriver(stream pb.WendyDriverService_InstallDriverServer) error {
	p.writes.Add(1)
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.GetSpec() == nil {
		return fmt.Errorf("missing initial specification")
	}
	spec := proto.Clone(first.GetSpec()).(*pb.DriverSpec)
	var payload []byte
	chunks := 0
	for {
		msg, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if msg.GetChunk() == nil {
			return fmt.Errorf("unexpected repeated specification")
		}
		data := msg.GetChunk().GetData()
		if len(data) == 0 || len(data) > driverChunkSize || len(payload)+len(data) > 2*driverChunkSize {
			return fmt.Errorf("unbounded upload")
		}
		payload = append(payload, data...)
		chunks++
	}
	p.mu.Lock()
	p.spec = spec
	p.payload = payload
	p.chunks = chunks
	p.mu.Unlock()
	return stream.Send(&pb.DriverApplyResponse{ResponseType: &pb.DriverApplyResponse_Completed_{Completed: &pb.DriverApplyResponse_Completed{Name: spec.GetName()}}})
}
func startFirstBootDriverPeer(t *testing.T, p *firstBootDriverPeer) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rpc := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(rpc, p)
	pb.RegisterWendyDriverServiceServer(rpc, p)
	done := make(chan struct{})
	go func() { defer close(done); _ = rpc.Serve(listener) }()
	t.Cleanup(func() {
		rpc.Stop()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("first-boot RPC server did not drain")
		}
	})
	return listener.Addr().String()
}
func TestDriverBootReconnectRPC(t *testing.T) {
	const host = "wendyos-driver-boot"
	const board = "jetson-orin-nano"
	const kernel = "fixture-pinned-kernel"
	// Keep ownership of the old endpoint while dropping every accepted transport,
	// as the recovery/setup service does during boot. Never probe an unowned port.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	disconnected := listener.Addr().String()
	var dropped atomic.Int32
	disconnectedDone := make(chan struct{})
	go func() {
		defer close(disconnectedDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			dropped.Add(1)
			_ = conn.Close()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		select {
		case <-disconnectedDone:
		case <-time.After(time.Second):
			t.Error("disconnected endpoint did not drain")
		}
	})
	peers := []*firstBootDriverPeer{
		{hostname: "wendyos-old-name", board: board, kernel: kernel},
		{hostname: host, board: "raspberry-pi-5", kernel: kernel},
		{hostname: host, board: board, kernel: "old-kernel"},
		{hostname: host, board: board, kernel: kernel},
	}
	addresses := []string{disconnected}
	for _, peer := range peers {
		addresses = append(addresses, startFirstBootDriverPeer(t, peer))
	}
	var dialed []string
	var connections []*grpcclient.AgentConnection
	previous := dialInstallDriverDevice
	t.Cleanup(func() {
		dialInstallDriverDevice = previous
		for _, conn := range connections {
			_ = conn.Close()
		}
	})
	// Only name resolution is controlled. Every probe and upload crosses real TCP
	// and gRPC, using a fresh endpoint; no fake AgentConnection or RPC client.
	dialInstallDriverDevice = func(ctx context.Context, address string) (*grpcclient.AgentConnection, error) {
		if address != host+".local:50051" {
			return nil, fmt.Errorf("unexpected target %s", address)
		}
		index := len(dialed)
		if index >= len(addresses) {
			index = len(addresses) - 1
		}
		endpoint := addresses[index]
		dialed = append(dialed, endpoint)
		conn, err := grpcclient.Connect(ctx, endpoint)
		if err == nil {
			connections = append(connections, conn)
		}
		return conn, err
	}
	payload := bytes.Repeat([]byte("boot-driver-payload"), 70000)
	digest := sha256.Sum256(payload)
	spec := &pb.DriverSpec{Name: "wendyos-hello", Version: "1.0.0", KernelVersion: kernel, Sha256: hex.EncodeToString(digest[:]), Signature: []byte{1, 2, 3}, ModulesLoad: []string{"wendyos_hello"}}
	path := filepath.Join(t.TempDir(), "driver.raw")
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	prepared := &osInstallDrivers{deviceType: board, kernel: kernel, drivers: []preparedInstallDriver{{spec: spec, path: path}}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := prepared.installAfterBoot(ctx, "driver-boot"); err != nil {
		t.Fatal(err)
	}
	for _, peer := range peers[:3] {
		if peer.writes.Load() != 0 {
			t.Fatalf("driver upload reached a rejected boot identity: host=%s board=%s kernel=%s", peer.hostname, peer.board, peer.kernel)
		}
	}
	final := peers[3]
	final.mu.Lock()
	defer final.mu.Unlock()
	if dropped.Load() == 0 || len(dialed) != 5 || dialed[0] == dialed[4] || final.writes.Load() != 1 || final.chunks < 2 || !proto.Equal(final.spec, spec) || !bytes.Equal(final.payload, payload) {
		t.Fatalf("first-boot transfer did not preserve one pinned multipart upload after address change: attempts=%d writes=%d chunks=%d", len(dialed), final.writes.Load(), final.chunks)
	}
	// Completed is the remote protocol verdict, not proof of kernel activation.
	t.Log("FIRST_BOOT_DRIVER_TRANSPORT_VERIFIED disconnected, wrong-name, wrong-board and old-kernel peers precede the sole complete upload")
}
func TestDriverBootCancelledRPC(t *testing.T) {
	peer := &firstBootDriverPeer{hostname: "wendyos-cancel", board: "jetson-orin-nano", kernel: "old-kernel"}
	endpoint := startFirstBootDriverPeer(t, peer)
	previous := dialInstallDriverDevice
	t.Cleanup(func() { dialInstallDriverDevice = previous })
	dialInstallDriverDevice = func(ctx context.Context, _ string) (*grpcclient.AgentConnection, error) {
		return grpcclient.Connect(ctx, endpoint)
	}
	p := &osInstallDrivers{deviceType: peer.board, kernel: "new-kernel"}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	err := p.installAfterBoot(ctx, "cancel")
	if !errors.Is(err, context.DeadlineExceeded) || peer.writes.Load() != 0 {
		t.Fatalf("cancelled first boot lost incomplete/cause evidence: %v writes=%d", err, peer.writes.Load())
	}
}
