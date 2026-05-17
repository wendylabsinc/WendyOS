package services

import (
	"context"
	"math"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ProvisioningServiceV2 implements agentpbv2.WendyProvisioningServiceServer by
// delegating to the v1 ProvisioningService.
type ProvisioningServiceV2 struct {
	agentpbv2.UnimplementedWendyProvisioningServiceServer
	v1 *ProvisioningService
}

// NewProvisioningServiceV2 creates a new ProvisioningServiceV2 wrapping the given v1 service.
func NewProvisioningServiceV2(v1 *ProvisioningService) *ProvisioningServiceV2 {
	return &ProvisioningServiceV2{v1: v1}
}

func (s *ProvisioningServiceV2) IsProvisioned(ctx context.Context, _ *agentpbv2.IsProvisionedRequest) (*agentpbv2.IsProvisionedResponse, error) {
	resp, err := s.v1.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
	if err != nil {
		return nil, err
	}
	if resp.GetNotProvisioned() != nil {
		return &agentpbv2.IsProvisionedResponse{
			ResponseType: &agentpbv2.IsProvisionedResponse_NotProvisioned{
				NotProvisioned: &agentpbv2.NotProvisionedResponse{},
			},
		}, nil
	}
	p := resp.GetProvisioned()
	return &agentpbv2.IsProvisionedResponse{
		ResponseType: &agentpbv2.IsProvisionedResponse_Provisioned{
			Provisioned: &agentpbv2.ProvisionedResponse{
				CloudHost:      p.CloudHost,
				OrganizationId: int64(p.OrganizationId),
				AssetId:        int64(p.AssetId),
			},
		},
	}, nil
}

func (s *ProvisioningServiceV2) StartProvisioning(ctx context.Context, req *agentpbv2.StartProvisioningRequest) (*agentpbv2.StartProvisioningResponse, error) {
	// The v1 provisioning path stores these IDs as int32. Reject values outside
	// that range instead of letting the int32() cast below silently truncate
	// them, which would provision the device under a different org/asset.
	if req.OrganizationId < math.MinInt32 || req.OrganizationId > math.MaxInt32 {
		return nil, status.Errorf(codes.InvalidArgument, "organization_id %d is outside the range supported by the agent", req.OrganizationId)
	}
	if req.AssetId < math.MinInt32 || req.AssetId > math.MaxInt32 {
		return nil, status.Errorf(codes.InvalidArgument, "asset_id %d is outside the range supported by the agent", req.AssetId)
	}
	if _, err := s.v1.StartProvisioning(ctx, &agentpb.StartProvisioningRequest{
		OrganizationId:  int32(req.OrganizationId),
		EnrollmentToken: req.EnrollmentToken,
		CloudHost:       req.CloudHost,
		AssetId:         int32(req.AssetId),
	}); err != nil {
		return nil, err
	}
	return &agentpbv2.StartProvisioningResponse{}, nil
}
