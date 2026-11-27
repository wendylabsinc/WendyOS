package backend

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

func TestEnrollmentReplacementKeepsAppRPCsOnTheLiveConnection(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	fake := &fakeAgent{controls: make(chan string, 2)}
	pb.RegisterWendyContainerServiceServer(server, fake)
	go server.Serve(listener)
	defer server.Stop()
	defer listener.Close()
	dial := func() *grpc.ClientConn {
		t.Helper()
		conn, err := grpc.NewClient("passthrough:///fixture", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
		if err != nil {
			t.Fatal(err)
		}
		return conn
	}
	previous, next := dial(), dial()
	b := New()
	b.conn = grpcclient.NewFromConn(previous)
	b.life, b.stop = context.WithCancel(context.Background())
	b.session = 5
	b.publishDeviceRPCSession()
	defer b.Shutdown()
	if _, err := b.Apps(); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.replaceEnrolledConnection(grpcclient.NewFromConn(next))
	b.mu.Unlock()
	if previous.GetState() != connectivity.Shutdown {
		t.Fatal("enrollment did not close its previous transport")
	}
	apps, err := b.Apps()
	if err != nil || len(apps) != 1 {
		t.Fatalf("inventory after enrollment = %v, %v", apps, err)
	}
	if err := b.StopApp("test-app", 5); err != nil {
		t.Fatalf("stop after enrollment: %v", err)
	}
	if got := <-fake.controls; got != "stop:test-app" {
		t.Fatal(got)
	}
}

func TestAppControlsAndRefreshBypassBusyDeviceOperation(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	fake := &fakeAgent{controls: make(chan string, 2)}
	pb.RegisterWendyContainerServiceServer(server, fake)
	go server.Serve(listener)
	defer server.Stop()
	defer listener.Close()
	conn, err := grpc.NewClient("passthrough:///fixture", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	b := New()
	b.conn = grpcclient.NewFromConn(conn)
	b.life, b.stop = context.WithCancel(context.Background())
	b.session = 5
	b.publishDeviceRPCSession()
	defer b.Shutdown()
	_, releaseBusyOperation := b.begin()
	finished := make(chan error, 1)
	go func() {
		if err := b.StopApp("test-app", 5); err != nil {
			finished <- err
			return
		}
		_, err := b.Apps()
		finished <- err
	}()
	select {
	case err := <-finished:
		releaseBusyOperation()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		releaseBusyOperation()
		t.Fatal("Stop or its status refresh waited behind an unrelated device operation")
	}
	if got := <-fake.controls; got != "stop:test-app" {
		t.Fatal(got)
	}
	if err := b.StopApp("test-app", 4); err == nil {
		t.Fatal("stale session could stop an app")
	}
	ctx, _, cancel, err := b.deviceRPC(5)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if err := b.Disconnect(); err != nil {
		t.Fatal(err)
	}
	if ctx.Err() == nil {
		t.Fatal("disconnect did not cancel app requests")
	}
	if _, _, _, err := b.deviceRPC(5); err == nil {
		t.Fatal("disconnected session accepted app request")
	}
}
