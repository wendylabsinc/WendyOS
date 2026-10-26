package browserauth

import (
	"context"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestTopologyReporterMustMatchAuthenticatedPeer(t *testing.T) {
	const principal = "spiffe://wendy.sh/tenant/12345678-1234-4234-8234-123456789abc/device/00000000-0000-4000-8000-000000000001"
	peer, err := certs.ParsePrincipal(principal)
	if err != nil {
		t.Fatal(err)
	}
	device, err := meshid.DeviceFromPrincipal(principal)
	if err != nil {
		t.Fatal(err)
	}
	valid := &pb.LocalMeshTopology{SelfPrincipal: principal, SelfTenantId: device.Tenant().UUID().String(), SelfDeviceId: device.UUID().String(), SelfMeshAddress: device.MeshAddress().String()}
	if err := ValidateTopologyReporter(valid, peer); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*pb.LocalMeshTopology){
		func(p *pb.LocalMeshTopology) {
			p.SelfPrincipal = "spiffe://wendy.sh/tenant/12345678-1234-4234-8234-123456789abc/device/00000000-0000-4000-8000-000000000002"
		},
		func(p *pb.LocalMeshTopology) { p.SelfTenantId = "another-tenant" },
		func(p *pb.LocalMeshTopology) { p.SelfDeviceId = "another-device" },
		func(p *pb.LocalMeshTopology) { p.SelfMeshAddress = "::1" },
	} {
		bad := proto.Clone(valid).(*pb.LocalMeshTopology)
		mutate(bad)
		if err := ValidateTopologyReporter(bad, peer); status.Code(err) != codes.PermissionDenied {
			t.Fatalf("accepted mismatched reporter: %v", err)
		}
	}
	if err := ValidateTopologyReporter(nil, peer); status.Code(err) != codes.DataLoss {
		t.Fatalf("accepted empty snapshot: %v", err)
	}
	valid.Peers = make([]*pb.LocalMeshPeerLink, 1025)
	if err := ValidateTopologyReporter(valid, peer); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("accepted unbounded snapshot: %v", err)
	}
}

func TestTopologyFailuresDoNotLookLikeEmptyHealthyNetworks(t *testing.T) {
	for _, test := range []struct {
		err   error
		state string
	}{
		{status.Error(codes.Unimplemented, "older agent"), "unsupported"},
		{status.Error(codes.PermissionDenied, "another tenant"), "unauthorized"},
		{status.Error(codes.Unauthenticated, "expired credential"), "unauthorized"},
		{status.Error(codes.Unavailable, "no route"), "unavailable"},
		{context.DeadlineExceeded, "unavailable"},
		{status.Error(codes.Internal, "snapshot failed"), "error"},
	} {
		got := TopologyFailure(test.err)
		if got.Status != test.state || len(got.Topology) != 0 || len(got.Error) == 0 || got.ObservedAt.IsZero() {
			t.Fatalf("incorrect failure observation: %+v", got)
		}
	}
}
