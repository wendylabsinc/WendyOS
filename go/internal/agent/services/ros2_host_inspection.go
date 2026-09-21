package services

import (
	"context"

	"github.com/wendylabsinc/wendy/go/internal/shared/ros2inspection"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// ROS2HostRuntime is optional so older/custom runtimes continue to support app
// inspection. Host inspection never falls back to an app's isolated graph.
type ROS2HostRuntime interface {
	EnsureHostROS2Sidecar(context.Context, ros2inspection.HostOptions) (ROS2Sidecar, error)
}

func requestedROS2Scope(ctx context.Context) (string, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	scopes := md.Get(ros2inspection.ScopeMetadata)
	if len(scopes) > 1 {
		return "", status.Error(codes.InvalidArgument, "ROS 2 inspection scope must have one value")
	}
	scope := ros2inspection.AppScope
	if len(scopes) == 1 {
		scope = scopes[0]
	}
	if scope != ros2inspection.AppScope && scope != ros2inspection.HostScope {
		return "", status.Error(codes.InvalidArgument, "ROS 2 inspection scope must be app or host")
	}
	return scope, nil
}

// Only the four bounded sensor-inspection RPCs call this resolver. Mutations and
// raw Exec keep resolveSidecars, which rejects host scope before provisioning.
func (s *ROS2Service) resolveInspectionSidecars(ctx context.Context, domain *int32) ([]ros2SC, error) {
	scope, err := requestedROS2Scope(ctx)
	if err != nil {
		return nil, err
	}
	if scope == ros2inspection.AppScope {
		return s.resolveSidecars(ctx, domain)
	}
	if domain == nil {
		return nil, status.Error(codes.InvalidArgument, "host ROS 2 inspection requires an explicit domain ID")
	}
	opts := ros2inspection.HostOptions{DomainID: int(*domain)}
	if err := opts.Validate(); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	runtime, ok := s.runtime.(ROS2HostRuntime)
	if !ok {
		return nil, status.Error(codes.Unimplemented, "this runtime does not support standalone host ROS 2 inspection")
	}
	// Send the acknowledgment before setup or subscription can block. Clients
	// require it so old agents cannot silently interpret host requests as app ones.
	if err := grpc.SendHeader(ctx, metadata.Pairs(ros2inspection.ScopeMetadata, ros2inspection.HostScope)); err != nil {
		return nil, status.Errorf(codes.Internal, "acknowledging host ROS 2 inspection: %v", err)
	}
	sidecar, err := runtime.EnsureHostROS2Sidecar(ctx, opts)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	return []ros2SC{{name: sidecar.Name, rmw: sidecar.RMW, domainID: opts.DomainID}}, nil
}
