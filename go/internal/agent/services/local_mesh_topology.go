package services

import (
	"context"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshview"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const maxTopologyEntries = 1024
const maxTopologyServiceBytes = 256 * 1024

// SetTopologySource installs a read-only coherent node snapshot source. A nil
// source means the runtime is absent, which is distinct from an empty mesh.
func (s *LocalMeshAdminService) SetTopologySource(source func() localmesh.NodeSnapshot) {
	s.mu.Lock()
	s.topology = source
	s.mu.Unlock()
}

func (s *LocalMeshAdminService) GetLocalMeshTopology(ctx context.Context, request *pb.GetLocalMeshTopologyRequest) (*pb.LocalMeshTopology, error) {
	device, err := s.authorizeRead(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	source := s.topology
	sharing, carriers, err := s.read(device)
	var runtimeStatus *pb.LocalMeshStatus
	if err == nil {
		runtimeStatus = s.response(sharing, carriers)
	}
	s.mu.Unlock()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "reading local net configuration: %v", err)
	}
	var snapshot localmesh.NodeSnapshot
	if source != nil {
		snapshot = source()
	}
	// Enrollment may have changed while the source was read. Never label an old
	// tenant's snapshot with the new identity or return it to a new tenant caller.
	current, err := s.authorizeRead(ctx)
	if err != nil {
		return nil, err
	}
	if current != device {
		return nil, status.Error(codes.Aborted, "device enrollment changed; retry topology")
	}
	out := projectLocalMeshTopology(device, snapshot, time.Now(), request.GetIncludeServices())
	out.Status = runtimeStatus
	return out, nil
}

func projectLocalMeshTopology(self meshid.Device, snapshot localmesh.NodeSnapshot, now time.Time, includeServices ...bool) *pb.LocalMeshTopology {
	return meshview.Topology(self, snapshot, now, includeServices...)
}
