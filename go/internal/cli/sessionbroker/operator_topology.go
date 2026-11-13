package sessionbroker

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshview"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const operatorTopologyMethod = "/wendy.sessionbroker.TopologyV1/Get"
const operatorTopologyBytes = 256 * 1024

// ReadOperatorTopology reads only an existing private broker's actual local
// participant. It never starts/enables mesh, discovers radios, logs in, or opens
// a target TLS connection. Absence is reported through ErrUnavailable.
// The reply is binary protobuf; all uint64 values remain exact.
func ReadOperatorTopology(ctx context.Context) (*pb.LocalMeshTopology, error) {
	client, err := connectedOperatorControl(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer client.Close()
	request, reply := rawMessage{}, rawMessage{}
	if err = client.Conn.Invoke(ctx, operatorTopologyMethod, &request, &reply, grpc.ForceCodec(rawCodec{}), grpc.MaxCallRecvMsgSize(operatorTopologyBytes)); err != nil {
		return nil, err
	}
	view := new(pb.LocalMeshTopology)
	if err = proto.Unmarshal(reply, view); err != nil {
		return nil, err
	}
	operator, err := meshoperator.ParsePrincipal(view.SelfPrincipal)
	if client.CertInfo == nil {
		return nil, status.Error(codes.Unavailable, "broker has no selected operator identity")
	}
	expected, expectedErr := operatorPrincipal(*client.CertInfo)
	if err != nil || expectedErr != nil || expected.Principal() != view.SelfPrincipal {
		return nil, status.Error(codes.Unavailable, "broker topology is not an operator participant")
	}
	instance, err := uuid.Parse(view.SelfDeviceId)
	if err != nil || instance == uuid.Nil || instance.String() != view.SelfDeviceId || !view.RuntimeAvailable || view.SelfTenantId != operator.Tenant().String() || (meshoperator.Endpoint{Operator: operator, Instance: meshoperator.InstanceID(instance)}).Address().String() != view.SelfMeshAddress {
		return nil, status.Error(codes.Unavailable, "broker topology has no bound local installation")
	}
	return view, nil
}

func (r *operatorTargets) localTopology(stream grpc.ServerStream, a *activity) (bool, error) {
	method, _ := grpc.MethodFromServerStream(stream)
	if method != operatorTopologyMethod {
		return false, nil
	}
	a.activeRPCs.Add(1)
	a.touch()
	defer func() { a.activeRPCs.Add(-1); a.touch() }()
	var request rawMessage
	if err := stream.RecvMsg(&request); err != nil {
		return true, err
	}
	if len(request) != 0 {
		return true, status.Error(codes.InvalidArgument, "local topology request has no payload")
	}
	if r.localSnapshot == nil {
		return true, status.Error(codes.Unavailable, "broker has no local mesh participant")
	}
	snapshot := r.localSnapshot()
	if !snapshot.Self.IsOperator() || snapshot.CapturedAt.IsZero() {
		return true, status.Error(codes.Unavailable, "local operator participant is starting")
	}
	view := meshview.Topology(snapshot.Self, snapshot, snapshot.CapturedAt)
	data, err := proto.Marshal(view)
	if err != nil {
		return true, err
	}
	if len(data) > operatorTopologyBytes {
		return true, status.Error(codes.ResourceExhausted, "local topology exceeds its bounded reply")
	}
	reply := rawMessage(data)
	return true, stream.SendMsg(&reply)
}
