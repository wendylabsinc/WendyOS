package litetopology

import (
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ValidateReporter binds an observation to the independently authenticated peer.
func ValidateReporter(snapshot *pb.LocalMeshTopology, peer certs.WendyIdentity) error {
	if snapshot == nil {
		return status.Error(codes.DataLoss, "agent returned no topology snapshot")
	}
	id, err := certs.ParsePrincipal(snapshot.GetSelfPrincipal())
	device, meshErr := meshid.DeviceFromPrincipal(snapshot.GetSelfPrincipal())
	if err != nil || meshErr != nil || !id.SameEntity(peer) || snapshot.GetSelfTenantId() != device.Tenant().UUID().String() || snapshot.GetSelfDeviceId() != device.UUID().String() || snapshot.GetSelfMeshAddress() != device.MeshAddress().String() {
		return status.Error(codes.PermissionDenied, "topology reporter does not match the authenticated device")
	}
	if len(snapshot.GetPeers()) > 1024 || len(snapshot.GetRoutes()) > 1024 || len(snapshot.GetAnnouncements()) > 1024 {
		return status.Error(codes.ResourceExhausted, "agent topology exceeds the supported observation limit")
	}
	return nil
}
