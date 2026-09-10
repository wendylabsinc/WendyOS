package services

import (
	"context"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/pkienroll"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// ProvisioningServiceV2 implements agentpbv2.WendyProvisioningServiceServer by
// delegating to the v1 ProvisioningService.
type ProvisioningServiceV2 struct {
	agentpbv2.UnimplementedWendyProvisioningServiceServer
	v1 *ProvisioningService
	// pki owns the device's pki-core identity. nil disables
	// StagePKIEnrollment, which is what a build with no pki identity manager
	// should report rather than panicking.
	pki *PKIEnrollment
	// pkiCtx bounds the renewal loop a successful enrolment starts. It is the
	// agent's lifetime context and deliberately NOT the RPC's: the call
	// returns in seconds and the loop has to outlive it by weeks.
	pkiCtx context.Context
}

func NewProvisioningServiceV2(v1 *ProvisioningService) *ProvisioningServiceV2 {
	return &ProvisioningServiceV2{v1: v1}
}

// WithPKIEnrollment enables StagePKIEnrollment. lifetime bounds the renewal
// loop started after a successful enrolment.
func (s *ProvisioningServiceV2) WithPKIEnrollment(lifetime context.Context, pki *PKIEnrollment) *ProvisioningServiceV2 {
	s.pki = pki
	s.pkiCtx = lifetime
	return s
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
				OrganizationId: p.OrganizationId,
				AssetId:        p.AssetId,
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

// StagePKIEnrollment writes the staged pki-core credential file and redeems it
// at once.
//
// AUTHORISATION. Exactly StartProvisioning's, because it is registered
// alongside it in registerAllServices and therefore served on the same three
// listeners: the plaintext pre-provisioning port (50051, unauthenticated - any
// caller on the local network), the local administrative unix socket, and the
// device mutual Transport Layer Security port (50052, a client certificate the
// device's own chain verifies, plus the org-equality interceptor). That is the
// right pairing and not an oversight: the credential this method carries is
// minted by pki-core against one tenant and one device_id, it is single-use,
// and pki-core - not the agent - decides whether to honour it. An unauthorised
// caller on the plaintext port can therefore only spend a token it already
// holds, on the device that token names. The same argument is what lets
// StartProvisioning live there, and it is the only reason a device with no
// certificate yet can ever get one.
func (s *ProvisioningServiceV2) StagePKIEnrollment(ctx context.Context, req *agentpbv2.StagePKIEnrollmentRequest) (*agentpbv2.StagePKIEnrollmentResponse, error) {
	if s.pki == nil {
		return nil, status.Error(codes.Unimplemented, "this agent has no pki-core identity manager")
	}
	if err := pkienroll.ValidateToken(req.GetToken()); err != nil {
		// The caller's request is wrong and no credential was spent, so this
		// is InvalidArgument and not a FAILED outcome: a FAILED status tells
		// an operator to mint a fresh token, which here would be wasted.
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	stagedPath, outcome, err := s.pki.StageAndApply(ctx, pkienroll.StagedEnrollment{
		Token:       req.GetToken(),
		TenantUUID:  strings.TrimSpace(req.GetTenantUuid()),
		DeviceID:    strings.TrimSpace(req.GetDeviceId()),
		CSREndpoint: strings.TrimSpace(req.GetCsrEndpoint()),
		Environment: strings.TrimSpace(req.GetEnvironment()),
	})
	if err != nil {
		// Staging failed, so nothing was redeemed and the token is still good.
		// An error, not a FAILED status: the caller can fix this and retry
		// with the same credential.
		return nil, status.Errorf(codes.Internal, "staging the pki enrollment credential: %v", err)
	}

	if outcome.Status == PKIEnrollmentEnrolled {
		// A device enrolled between restarts would otherwise hold a leaf that
		// nothing renews until the next boot. On pki-core's 30-day device
		// leaf that is a one-month fuse, so the loop starts here rather than
		// waiting for a restart. pkiCtx, never ctx: this outlives the call.
		s.pki.EnsureRenewer(s.renewalContext())
	}

	resp := &agentpbv2.StagePKIEnrollmentResponse{
		Status:     pkiStatusToProto(outcome.Status),
		StagedPath: stagedPath,
		SpiffeUri:  outcome.SPIFFEURI,
		DeviceName: outcome.DeviceName,
		Reason:     outcome.Reason,
	}
	if !outcome.NotAfter.IsZero() {
		resp.NotAfterUnix = outcome.NotAfter.Unix()
	}
	return resp, nil
}

// pkiStatusToProto maps the internal outcome onto the wire enum. PKIEnrollmentNone
// cannot occur here - StageAndApply has just written the file it reads - so it
// maps to FAILED rather than to a silent zero value.
func pkiStatusToProto(s PKIEnrollmentStatus) agentpbv2.StagePKIEnrollmentResponse_Status {
	switch s {
	case PKIEnrollmentEnrolled:
		return agentpbv2.StagePKIEnrollmentResponse_STATUS_ENROLLED
	case PKIEnrollmentAlreadyEnrolled:
		return agentpbv2.StagePKIEnrollmentResponse_STATUS_ALREADY_ENROLLED
	case PKIEnrollmentDeferred:
		return agentpbv2.StagePKIEnrollmentResponse_STATUS_DEFERRED
	case PKIEnrollmentRefused:
		return agentpbv2.StagePKIEnrollmentResponse_STATUS_REFUSED
	default:
		return agentpbv2.StagePKIEnrollmentResponse_STATUS_FAILED
	}
}

// renewalContext is the agent lifetime context, or context.Background() when a
// caller wired the service without one. Background is the safe default: it
// starts a loop that never stops on its own, which for a process-lifetime
// renewer is the intended behaviour anyway.
func (s *ProvisioningServiceV2) renewalContext() context.Context {
	if s.pkiCtx != nil {
		return s.pkiCtx
	}
	return context.Background()
}
