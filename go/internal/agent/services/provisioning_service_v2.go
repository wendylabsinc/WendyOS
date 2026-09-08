package services

import (
	"context"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// ProvisioningServiceV2 implements agentpbv2.WendyProvisioningServiceServer by
// delegating to the v1 ProvisioningService.
type ProvisioningServiceV2 struct {
	agentpbv2.UnimplementedWendyProvisioningServiceServer
	v1 *ProvisioningService
}

func NewProvisioningServiceV2(v1 *ProvisioningService) *ProvisioningServiceV2 {
	return &ProvisioningServiceV2{v1: v1}
}

func (s *ProvisioningServiceV2) IsProvisioned(ctx context.Context, _ *agentpbv2.IsProvisionedRequest) (*agentpbv2.IsProvisionedResponse, error) {
	s.v1.mu.Lock()
	defer s.v1.mu.Unlock()
	if !s.v1.enrolled {
		return &agentpbv2.IsProvisionedResponse{
			ResponseType: &agentpbv2.IsProvisionedResponse_NotProvisioned{
				NotProvisioned: &agentpbv2.NotProvisionedResponse{},
			},
		}, nil
	}
	return &agentpbv2.IsProvisionedResponse{
		ResponseType: &agentpbv2.IsProvisionedResponse_Provisioned{
			Provisioned: &agentpbv2.ProvisionedResponse{
				CloudHost:      s.v1.cloudHost,
				OrganizationId: s.v1.orgID,
				AssetId:        s.v1.assetID,
				PrincipalUri:   s.v1.principalURI,
			},
		},
	}, nil
}

func (s *ProvisioningServiceV2) StartProvisioning(ctx context.Context, req *agentpbv2.StartProvisioningRequest) (*agentpbv2.StartProvisioningResponse, error) {
	if _, err := s.v1.StartProvisioning(ctx, &agentpb.StartProvisioningRequest{
		OrganizationId:  req.OrganizationId,
		EnrollmentToken: req.EnrollmentToken,
		CloudHost:       req.CloudHost,
		AssetId:         req.AssetId,
	}); err != nil {
		return nil, err
	}
	return &agentpbv2.StartProvisioningResponse{}, nil
}

func (s *ProvisioningServiceV2) Unprovision(ctx context.Context, _ *agentpbv2.UnprovisionRequest) (*agentpbv2.UnprovisionResponse, error) {
	if _, err := s.v1.Unprovision(ctx, &agentpb.UnprovisionRequest{}); err != nil {
		return nil, err
	}
	return &agentpbv2.UnprovisionResponse{}, nil
}
