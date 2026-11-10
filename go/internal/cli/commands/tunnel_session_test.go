package commands

import (
	"context"
	"net"
	"testing"
	"time"

	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/relaypb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func TestTunnelSessionReusesConnectionButAuthorizesEveryDial(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	type request struct{ address, method string }
	requests := make(chan request, 2)
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		p, _ := peer.FromContext(stream.Context())
		method, _ := grpc.MethodFromServerStream(stream)
		requests <- request{p.Addr.String(), method}
		return status.Error(codes.PermissionDenied, "test refusal")
	}))
	go srv.Serve(lis)
	defer srv.Stop()
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = lis.Addr().String()
	id := "00000000-0000-4000-8000-000000000042"
	d := cloudDiscoveryDevice{v2: &cloudpbv2.Asset{Id: id}, key: id}
	var session TunnelSession
	session.SetActive(true)
	defer session.SetActive(false)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var address string
	for i := 0; i < 2; i++ {
		if _, err := d.openTunnel(session.Context(ctx), nil, auth, 50052); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("dial %d did not preserve authorization refusal: %v", i, err)
		}
		select {
		case req := <-requests:
			if req.method != pb.TunnelAuthorizationService_RequestTunnel_FullMethodName {
				t.Fatalf("unexpected authorization RPC: %s", req.method)
			}
			if i > 0 && req.address != address {
				t.Fatalf("authorization connection was recreated: %s -> %s", address, req.address)
			}
			address = req.address
		case <-ctx.Done():
			t.Fatal("dial did not request fresh authorization")
		}
	}
}

func TestTunnelSessionScopeAndLifetime(t *testing.T) {
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = "127.0.0.1:12345"
	var session TunnelSession
	session.SetActive(true)
	defer session.SetActive(false)
	old := session.Context(t.Context())
	first, release, err := tunnelAuthorizationFor(old, auth)
	if err != nil {
		t.Fatal(err)
	}
	release()
	same, release, err := tunnelAuthorizationFor(old, auth)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if first != same {
		t.Fatal("same credentials not reused")
	}
	changed := *auth
	changed.APIKey += "changed"
	different, release, err := tunnelAuthorizationFor(old, &changed)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if first == different {
		t.Fatal("changed credentials reused old authorization")
	}
	session.SetActive(false)
	if first.conn.GetState().String() != "SHUTDOWN" {
		t.Fatal("transport not closed")
	}
	session.SetActive(true)
	if _, _, err := tunnelAuthorizationFor(old, auth); err != context.Canceled {
		t.Fatalf("old handle resumed: %v", err)
	}
	fresh, release, err := tunnelAuthorizationFor(session.Context(t.Context()), auth)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if fresh == first {
		t.Fatal("new scope reused old resource")
	}
}

func TestTunnelSessionConcurrentReuse(t *testing.T) {
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = "127.0.0.1:12345"
	var session TunnelSession
	session.SetActive(true)
	defer session.SetActive(false)
	ctx := session.Context(t.Context())
	results := make(chan *tunnelAuthorization, 16)
	for range 16 {
		go func() {
			entry, release, err := tunnelAuthorizationFor(ctx, auth)
			if err != nil {
				t.Error(err)
				results <- nil
				return
			}
			defer release()
			results <- entry
		}()
	}
	first := <-results
	for range 15 {
		if got := <-results; got != first {
			t.Error("concurrent dials created separate transports")
		}
	}
}

func TestTunnelSessionPauseCancelsAuthorization(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		close(started)
		<-stream.Context().Done()
		return stream.Context().Err()
	}))
	go srv.Serve(lis)
	defer srv.Stop()
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = lis.Addr().String()
	var session TunnelSession
	session.SetActive(true)
	defer session.SetActive(false)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx = session.Context(ctx)
	id := "00000000-0000-4000-8000-000000000042"
	d := cloudDiscoveryDevice{v2: &cloudpbv2.Asset{Id: id}, key: id}
	done := make(chan error, 1)
	go func() { _, err := d.openTunnel(ctx, nil, auth, 50052); done <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("authorization did not start")
	}
	session.SetActive(false)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("paused authorization succeeded")
		}
	case <-ctx.Done():
		t.Fatal("pause did not cancel authorization")
	}
}

// A retained connection that failed to reconnect refuses requests until its
// backoff expires, long after Cloud is reachable again. A dial must not wait
// for that: it gets a connection that tries now, as an unshared dial would.
func TestTunnelSessionRecoversAsSoonAsCloudReturns(t *testing.T) {
	serve := func(address string) *grpc.Server {
		t.Helper()
		lis, err := net.Listen("tcp", address)
		for range 50 {
			if err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
			lis, err = net.Listen("tcp", address)
		}
		if err != nil {
			t.Fatal(err)
		}
		srv := grpc.NewServer(grpc.UnknownServiceHandler(func(any, grpc.ServerStream) error {
			return status.Error(codes.PermissionDenied, "test refusal")
		}))
		go srv.Serve(lis)
		return srv
	}
	free, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := free.Addr().String()
	free.Close()
	srv := serve(address)
	defer func() { srv.Stop() }()
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = address
	var session TunnelSession
	session.SetActive(true)
	defer session.SetActive(false)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ctx = session.Context(ctx)
	id := "00000000-0000-4000-8000-000000000042"
	d := cloudDiscoveryDevice{v2: &cloudpbv2.Asset{Id: id}, key: id}
	dial := func() codes.Code {
		_, err := d.openTunnel(ctx, nil, auth, 50052)
		return status.Code(err)
	}
	if got := dial(); got != codes.PermissionDenied {
		t.Fatalf("dial before the outage: %v", got)
	}
	srv.Stop()
	// The first dial finds the connection lost; the second finds it in backoff.
	for range 2 {
		if got := dial(); got != codes.Unavailable {
			t.Fatalf("dial during the outage: %v", got)
		}
	}
	srv = serve(address)
	if got := dial(); got != codes.PermissionDenied {
		t.Fatalf("dial after Cloud returned: %v", got)
	}
}
