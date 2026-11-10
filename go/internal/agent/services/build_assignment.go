package services

import (
	"context"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/buildassignment"
	"github.com/wendylabsinc/wendy/go/internal/shared/builddelivery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *BuildService) verifyDeliveryAssignments(spec *agentpbv2.BuildSpec, targets []*agentpbv2.PushTarget) error {
	// Standalone test services have no enrollment. Production always supplies
	// this source, including when enrollment is missing or unavailable.
	if s.deliverySource == nil {
		return nil
	}
	snapshot, err := s.deliverySource()
	if err != nil || len(snapshot.CertificateChain) == 0 {
		return status.Error(codes.FailedPrecondition, "build delivery identity unavailable")
	}
	builder, err := x509.ParseCertificate(snapshot.CertificateChain[0])
	if err != nil {
		return status.Error(codes.FailedPrecondition, "invalid build host certificate")
	}
	input, err := buildassignment.InputSHA256(spec)
	if err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	for _, target := range targets {
		device, err := meshid.ParseDevice(snapshot.Target.Tenant(), target.GetAssetUuid())
		if err != nil {
			return status.Error(codes.FailedPrecondition, err.Error())
		}
		claims, err := builddelivery.VerifyBuildBinding(target.GetDeliveryAuthorizationJws(), target.GetDeliverySession(), input, snapshot.TrustAnchors, device, builder, time.Now())
		if err != nil || claims.Reference != target.GetRepository() {
			return status.Error(codes.PermissionDenied, "missing or mismatched operator build delivery authorization")
		}
	}
	return nil
}

func deliveryAlreadyComplete(ctx context.Context, cc grpc.ClientConnInterface, target *agentpbv2.PushTarget) (bool, error) {
	response, err := agentpbv2.NewWendyBuildDeliveryServiceClient(cc).GetDeliveryStatus(ctx, &agentpbv2.GetDeliveryStatusRequest{Session: target.GetDeliverySession()})
	if err != nil {
		return false, fmt.Errorf("checking delivery admission: %w", err)
	}
	if response.GetReference() != target.GetRepository() {
		return false, fmt.Errorf("delivery admission addresses a different image reference")
	}
	return response.GetCompleted(), nil
}

func (s *BuildService) confirmDelivery(ctx context.Context, target *agentpbv2.PushTarget) error {
	if target.GetDeliverySession() == "" {
		return nil
	}
	cc, err := s.dialTarget(ctx, target)
	if err != nil {
		return err
	}
	defer cc.Close()
	done, err := deliveryAlreadyComplete(ctx, cc, target)
	if err != nil {
		return err
	}
	if !done {
		return fmt.Errorf("image transfer finished without completing its authorized publication")
	}
	return nil
}
