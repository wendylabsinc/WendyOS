package backend

import (
	"context"
	"errors"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"time"
)

func (b *Backend) DeviceInfo(session uint64) (*pb.GetDeviceInfoResponse, error) {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return nil, errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return nil, err
	}
	return pb.NewWendyDeviceInfoServiceClient(conn.Conn).GetDeviceInfo(ctx, &pb.GetDeviceInfoRequest{})
}

// All network release lookups and artifact downloads are RPCs on the agent.
func (b *Backend) OnlineUpdate(session uint64, action int32, target string) (*pb.OnlineUpdateStatus, error) {
	ctx, done := b.begin()
	defer done()
	if session == 0 {
		return nil, errors.New("select a device first")
	}
	conn, err := b.connection(session)
	if err != nil {
		return nil, err
	}
	timeout := 35 * time.Second
	if action == 0 {
		timeout = 4 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	client := pb.NewWendyAgentUpdateServiceClient(conn.Conn)
	var value *pb.OnlineUpdateStatus
	switch action {
	case 0:
		value, err = client.GetOnlineUpdateStatus(ctx, &pb.GetOnlineUpdateStatusRequest{})
	case 1:
		value, err = client.CheckOnlineUpdate(ctx, &pb.CheckOnlineUpdateRequest{})
	case 2:
		if target == "" {
			return nil, errors.New("check for updates first")
		}
		value, err = client.StartOnlineUpdate(ctx, &pb.StartOnlineUpdateRequest{Version: target})
	default:
		return nil, errors.New("unknown update action")
	}
	if status.Code(err) == codes.Unimplemented {
		return nil, errors.New("this agent does not support direct online updates yet")
	}
	return value, err
}
