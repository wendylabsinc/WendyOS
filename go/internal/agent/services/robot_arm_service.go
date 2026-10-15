package services

import (
	"context"
	"errors"
	"strings"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/agent/robotarm"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type RobotArmService struct {
	pb.UnimplementedWendyRobotArmServiceServer
	store    *robotarm.Store
	discover func(context.Context) ([]*pb.RobotArm, error)
	mu       sync.Mutex
}

func NewRobotArmService(store *robotarm.Store, discover func(context.Context) ([]*pb.RobotArm, error)) *RobotArmService {
	return &RobotArmService{store: store, discover: discover}
}
func (s *RobotArmService) list(ctx context.Context) ([]*pb.RobotArm, error) {
	live, err := s.discover(ctx)
	if errors.Is(err, robotarm.ErrUnsupported) {
		return nil, status.Error(codes.Unimplemented, err.Error())
	}
	if err != nil {
		return nil, status.Error(codes.Unavailable, "USB serial inventory unavailable on target Agent")
	}
	saved, err := s.store.List()
	if err != nil {
		return nil, status.Error(codes.Internal, "cannot read robot arm bindings on target Agent")
	}
	return robotarm.Merge(live, saved), nil
}
func (s *RobotArmService) ListRobotArms(ctx context.Context, req *pb.ListRobotArmsRequest) (*pb.ListRobotArmsResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ds, err := s.list(ctx)
	if err != nil {
		return nil, err
	}
	return &pb.ListRobotArmsResponse{Devices: ds}, nil
}
func (s *RobotArmService) BindRobotArm(ctx context.Context, req *pb.BindRobotArmRequest) (*pb.BindRobotArmResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Id == "" || req.Model != "so101" || len(req.Name) > 128 {
		return nil, status.Error(codes.InvalidArgument, "select a USB endpoint, model so101 and a name of at most 128 bytes")
	}
	ds, err := s.list(ctx)
	if err != nil {
		return nil, err
	}
	for _, d := range ds {
		if d.Id != req.Id {
			continue
		}
		if !d.Connected {
			return nil, status.Error(codes.FailedPrecondition, "USB identity is disconnected or ambiguous; rescan on target Agent")
		}
		if d.IdentityKind == "usb_location" && !req.AllowLocationBinding {
			return nil, status.Error(codes.FailedPrecondition, "no USB serial ID; explicitly acknowledge a location binding that follows the USB port")
		}
		d.Configured, d.Model, d.Name, d.Verified = true, "so101", strings.TrimSpace(req.Name), false
		if d.Name == "" {
			d.Name = "SO-101"
		}
		if err = s.store.Put(d); err != nil {
			return nil, status.Error(codes.Internal, "cannot save robot arm binding on target Agent")
		}
		return &pb.BindRobotArmResponse{Device: d}, nil
	}
	return nil, status.Error(codes.NotFound, "USB endpoint is no longer present on target Agent; rescan")
}
func (s *RobotArmService) ForgetRobotArm(ctx context.Context, req *pb.ForgetRobotArmRequest) (*pb.ForgetRobotArmResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.Id == "" {
		return nil, status.Error(codes.InvalidArgument, "robot arm binding ID is required")
	}
	if err := s.store.Forget(req.Id); err != nil {
		return nil, status.Error(codes.Internal, "cannot forget robot arm binding on target Agent")
	}
	return &pb.ForgetRobotArmResponse{}, nil
}
