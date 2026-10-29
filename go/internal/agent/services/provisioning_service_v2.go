package services

import (
	"context"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// assetUUID is this certificate's device UUID under the sole-ID contract.
// Reporting one's own identity requires no Cloud inventory or network access.
func (s *ProvisioningServiceV2) assetUUID(principal string) string {
	device, err := meshid.DeviceFromPrincipal(principal)
	if err != nil {
		return ""
	}
	return device.UUID().String()
}

func (s *ProvisioningServiceV2) IsProvisioned(ctx context.Context, _ *agentpbv2.IsProvisionedRequest) (*agentpbv2.IsProvisionedResponse, error) {
	s.v1.mu.Lock()
	defer s.v1.mu.Unlock()
	if s.v1.resetRecoveryErr != nil {
		return nil, status.Error(codes.Unavailable, "reset recovery blocked; explicit local recovery required")
	}
	if s.v1.resetCleanupErr != nil {
		return nil, status.Error(codes.Unavailable, "reset resource cleanup incomplete; agent recovery is retrying")
	}
	if !s.v1.enrolled {
		completion, err := s.v1.cloudCompletion()
		if err != nil {
			return nil, status.Error(codes.Unavailable, "completion evidence unavailable")
		}
		supported := true
		return &agentpbv2.IsProvisionedResponse{
			ResponseType: &agentpbv2.IsProvisionedResponse_NotProvisioned{
				NotProvisioned: &agentpbv2.NotProvisionedResponse{AcmeEnrollmentSupported: &supported, UnenrollmentCompletion: completion},
			},
		}, nil
	}
	return &agentpbv2.IsProvisionedResponse{
		ResponseType: &agentpbv2.IsProvisionedResponse_Provisioned{
			Provisioned: &agentpbv2.ProvisionedResponse{
				CloudHost:      s.v1.cloudHost,
				AssetUuid:      s.assetUUID(s.v1.principalURI),
				OrganizationId: s.v1.orgID,
				AssetId:        s.v1.assetID,
				PrincipalUri:   s.v1.principalURI,
				// SECURITY: prerelease CLIs trusted CloudUnenrollmentSupported and
				// sent caller-serialized deletion fields; keep it false so they stop
				// before Cloud mutation. Only the authenticated-reset bit gates Cloud reset.
				CloudUnenrollmentSupported:       false,
				CloudAuthenticatedResetSupported: supportsTrustedResetStorage && s.v1.trustedTime != nil,
				LocalResetSupported:              supportsTrustedResetStorage && s.v1.principalURI != "",
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

func (s *ProvisioningServiceV2) Unprovision(ctx context.Context, req *agentpbv2.UnprovisionRequest) (*agentpbv2.UnprovisionResponse, error) {
	if req.GetLocalOnly() {
		return s.localUnprovision(ctx, req)
	}
	s.v1.mu.Lock()
	directPKI := s.v1.principalURI != ""
	s.v1.mu.Unlock()
	if directPKI {
		return s.cloudUnprovision(ctx, req)
	}
	if _, err := s.v1.unprovision(req.GetExpectedPrincipalUri(), req.GetExpectedCertificateSha256()); err != nil {
		return nil, err
	}
	return &agentpbv2.UnprovisionResponse{}, nil
}
