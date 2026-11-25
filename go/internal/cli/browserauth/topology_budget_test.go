package browserauth

import (
	"context"
	cloud "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

type slowTopologyAssets struct {
	cloud.UnimplementedAssetServiceServer
	started   chan struct{}
	release   chan struct{}
	slowCalls atomic.Uint32
}

func (s *slowTopologyAssets) GetAsset(ctx context.Context, q *cloud.GetAssetRequest) (*cloud.Asset, error) {
	if q.Id == "00000000-0000-4000-8000-000000000001" {
		s.slowCalls.Add(1)
		select {
		case s.started <- struct{}{}:
		default:
		}
		select {
		case <-s.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, status.Error(codes.Unavailable, "offline reporter")
}
func TestBrowserReporterReadsProgressBesideOneSlowReporter(t *testing.T) {
	session, _ := setup(t, nil)
	if _, err := session.Complete(context.Background(), "valid-code", session.state, testIssuer); err != nil {
		t.Fatal(err)
	}
	service := &slowTopologyAssets{started: make(chan struct{}, 2), release: make(chan struct{})}
	listener := bufconn.Listen(1 << 20)
	defer listener.Close()
	server := grpc.NewServer()
	defer server.Stop()
	cloud.RegisterAssetServiceServer(server, service)
	go server.Serve(listener)
	dial := func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	first := make(chan TopologyObservation, 1)
	duplicate := make(chan TopologyObservation, 1)
	go func() { first <- session.MeshTopology(ctx, "00000000-0000-4000-8000-000000000001", dial, nil) }()
	select {
	case <-service.started:
	case <-ctx.Done():
		t.Fatal("slow reporter never started")
	}
	go func() { duplicate <- session.MeshTopology(ctx, "00000000-0000-4000-8000-000000000001", dial, nil) }()
	fastCtx, stopFast := context.WithTimeout(ctx, 500*time.Millisecond)
	defer stopFast()
	fast := session.MeshTopology(fastCtx, "00000000-0000-4000-8000-000000000002", dial, nil)
	if fast.Status != "unavailable" || fast.Error != "Looking up device certificate identity: rpc error: code = Unavailable desc = offline reporter" {
		t.Fatal("independent reporter waited behind slow read", fast)
	}
	close(service.release)
	for _, result := range []<-chan TopologyObservation{first, duplicate} {
		select {
		case got := <-result:
			if got.Status != "unavailable" {
				t.Fatal(got)
			}
		case <-ctx.Done():
			t.Fatal("coalesced reporter did not finish")
		}
	}
	if service.slowCalls.Load() != 1 {
		t.Fatal("same reporter opened parallel sessions", service.slowCalls.Load())
	}
}

func TestReporterIdentityWaitRespectsShortDeadline(t *testing.T) {
	previous := browserTopologyTimeout
	browserTopologyTimeout = 20 * time.Millisecond
	defer func() { browserTopologyTimeout = previous }()
	session := &Session{}
	session.mu.Lock()
	defer session.mu.Unlock()
	done := make(chan TopologyObservation, 1)
	go func() {
		done <- session.MeshTopology(context.Background(), "00000000-0000-4000-8000-000000000001", nil, nil)
	}()
	select {
	case got := <-done:
		if got.Status != "unavailable" {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("identity lock defeated short reporter deadline")
	}
}
