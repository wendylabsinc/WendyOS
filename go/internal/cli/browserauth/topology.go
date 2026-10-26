package browserauth

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
)

// TopologyObservation describes one authenticated reporter, not global health.
// Expected unsupported/offline results remain distinct from empty healthy graphs.
type TopologyObservation struct {
	Status     string          `json:"status"`
	Topology   json.RawMessage `json:"topology,omitempty"`
	Error      string          `json:"error,omitempty"`
	ObservedAt time.Time       `json:"observedAt"`
}

// TopologyFailure classifies a failed observation without inferring device health.
func TopologyFailure(err error) TopologyObservation {
	state := "error"
	switch status.Code(err) {
	case codes.Unimplemented:
		state = "unsupported"
	case codes.PermissionDenied, codes.Unauthenticated:
		state = "unauthorized"
	case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
		state = "unavailable"
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		state = "unavailable"
	}
	return TopologyObservation{Status: state, Error: err.Error(), ObservedAt: time.Now().UTC()}
}

// MeshTopology uses a separate, bounded connection. It never replaces a live
// dashboard connection, starts a device, changes its clock, or changes mesh policy.
func (s *Session) MeshTopology(ctx context.Context, asset string, dial func(context.Context, string) (net.Conn, error), client *http.Client) TopologyObservation {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	conn, err := s.ConnectDevice(ctx, asset, dial, client)
	if err != nil {
		return TopologyFailure(err)
	}
	defer conn.Close()
	snapshot, err := pb.NewWendyLocalMeshAdminServiceClient(conn.Conn).GetLocalMeshTopology(ctx, &pb.GetLocalMeshTopologyRequest{})
	if err != nil {
		return TopologyFailure(err)
	}
	peer, verified := conn.ObservedServerIdentity()
	if !verified {
		return TopologyFailure(status.Error(codes.Unauthenticated, "topology reporter identity was not verified"))
	}
	if err := ValidateTopologyReporter(snapshot, peer); err != nil {
		return TopologyFailure(err)
	}
	data, err := protojson.Marshal(snapshot)
	if err != nil {
		return TopologyFailure(err)
	}
	return TopologyObservation{Status: "ok", Topology: data, ObservedAt: time.Now().UTC()}
}

// ValidateTopologyReporter binds the claimed local observation to the TLS peer.
func ValidateTopologyReporter(snapshot *pb.LocalMeshTopology, peer certs.WendyIdentity) error {
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
