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

func TestCloudTunnelDialerReusesConnectionButAuthorizesEveryDial(t *testing.T) {
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
	dial, closeDialer, err := d.tunnelDialer(nil, auth, 50052)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDialer()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var address string
	for i := 0; i < 2; i++ {
		if _, err := dial(ctx); status.Code(err) != codes.PermissionDenied {
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
