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

// Every connection accepted by `wendy cloud tunnel` asks Cloud for its own
// grant, and they all ask over one authorization connection.
func TestCloudTunnelForwardReusesAuthorizationConnection(t *testing.T) {
	cloud, err := net.Listen("tcp", "127.0.0.1:0")
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
	go srv.Serve(cloud)
	defer srv.Stop()
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = cloud.Addr().String()
	id := "00000000-0000-4000-8000-000000000042"
	d := cloudDiscoveryDevice{v2: &cloudpbv2.Asset{Id: id}, key: id}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	failures := make(chan error, 2)
	served := make(chan error, 1)
	go func() {
		served <- d.serveTunnelForward(ctx, ln, nil, auth, 50052, func(err error) { failures <- err })
	}()

	var address string
	for i := range 2 {
		local, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer local.Close()
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
			t.Fatalf("connection %d did not request fresh authorization", i)
		}
		select {
		case err := <-failures:
			if status.Code(err) != codes.PermissionDenied {
				t.Fatalf("connection %d did not preserve authorization refusal: %v", i, err)
			}
		case <-ctx.Done():
			t.Fatalf("connection %d did not report its refusal", i)
		}
	}
	cancel()
	if err := <-served; err != nil {
		t.Fatalf("forwarding ended with %v", err)
	}
}
