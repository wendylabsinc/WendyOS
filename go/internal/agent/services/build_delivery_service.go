package services

import (
	"context"
	"github.com/containerd/errdefs"

	"github.com/wendylabsinc/wendy/go/internal/agent/delivery"
	"github.com/wendylabsinc/wendy/go/internal/shared/builddelivery"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type DeliveryImageResolver interface {
	ImageDigest(context.Context, string) (string, error)
}
type BuildDeliveryService struct {
	agentpbv2.UnimplementedWendyBuildDeliveryServiceServer
	source delivery.Source
	images DeliveryImageResolver
}

func NewBuildDeliveryService(source delivery.Source, images DeliveryImageResolver) *BuildDeliveryService {
	return &BuildDeliveryService{source: source, images: images}
}
func (s *BuildDeliveryService) snapshot() (delivery.Snapshot, error) {
	if s.source == nil {
		return delivery.Snapshot{}, status.Error(codes.FailedPrecondition, "operator delivery unavailable")
	}
	snapshot, err := s.source()
	if err != nil || snapshot.Store == nil {
		return snapshot, status.Error(codes.FailedPrecondition, "operator delivery requires a provisioned target and durable authorization store")
	}
	return snapshot, nil
}
func (s *BuildDeliveryService) GetDeliveryCapabilities(ctx context.Context, _ *agentpbv2.GetDeliveryCapabilitiesRequest) (*agentpbv2.GetDeliveryCapabilitiesResponse, error) {
	snapshot, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	return &agentpbv2.GetDeliveryCapabilitiesResponse{ContractVersion: builddelivery.Version, CertificateChain: snapshot.CertificateChain, TargetPrincipal: snapshot.Target.Principal()}, nil
}
func (s *BuildDeliveryService) GetDeliveryReference(ctx context.Context, req *agentpbv2.GetDeliveryReferenceRequest) (*agentpbv2.GetDeliveryReferenceResponse, error) {
	// Keep this policy in the handler too: a future registration must not turn
	// inventory lookup into a new device capability.
	if err := authorizeBuildSubmission(ctx); err != nil {
		return nil, err
	}
	if err := builddelivery.ValidateReference(req.GetReference()); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	snapshot, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	if s.images == nil {
		return nil, status.Error(codes.Unimplemented, "image references unavailable")
	}
	digest, err := s.images.ImageDigest(ctx, snapshot.ImagePrefix+req.GetReference())
	if errdefs.IsNotFound(err) {
		digest = ""
		err = nil
	}
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &agentpbv2.GetDeliveryReferenceResponse{Digest: digest}, nil
}
func (s *BuildDeliveryService) AdmitDelivery(ctx context.Context, req *agentpbv2.AdmitDeliveryRequest) (*agentpbv2.AdmitDeliveryResponse, error) {
	snapshot, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	session, err := snapshot.Store.Admit(req.GetAuthorizationJws(), delivery.Leaf(ctx))
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	return &agentpbv2.AdmitDeliveryResponse{Session: session}, nil
}

func (s *BuildDeliveryService) GetDeliveryStatus(ctx context.Context, req *agentpbv2.GetDeliveryStatusRequest) (*agentpbv2.GetDeliveryStatusResponse, error) {
	snapshot, err := s.snapshot()
	if err != nil {
		return nil, err
	}
	leaf := delivery.Leaf(ctx)
	claims, err := snapshot.Store.Policy(req.GetSession(), leaf)
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	output, completed, err := snapshot.Store.CompletedOutput(req.GetSession(), leaf)
	if err != nil {
		return nil, status.Error(codes.PermissionDenied, err.Error())
	}
	if completed {
		if s.images == nil {
			return nil, status.Error(codes.Unimplemented, "image references unavailable")
		}
		current, err := s.images.ImageDigest(ctx, snapshot.ImagePrefix+claims.Reference)
		if err != nil {
			return nil, status.Error(codes.FailedPrecondition, "completed delivery reference is unavailable")
		}
		if current != output {
			return nil, status.Error(codes.FailedPrecondition, "completed delivery reference was superseded")
		}
	}
	return &agentpbv2.GetDeliveryStatusResponse{Reference: claims.Reference, OutputDigest: output, Completed: completed}, nil
}
