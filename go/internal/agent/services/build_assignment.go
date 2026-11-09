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

func (s *BuildService) verifyDeliveryAssignments(ctx context.Context, spec *agentpbv2.BuildSpec, targets []*agentpbv2.PushTarget) (context.Context, error) {
	// Standalone test services have no enrollment. Production always supplies
	// this source, including when enrollment is missing or unavailable.
	if s.deliverySource == nil {
		return ctx, nil
	}
	snapshot, err := s.deliverySource()
	if err != nil || len(snapshot.CertificateChain) == 0 {
		return ctx, status.Error(codes.FailedPrecondition, "build delivery identity unavailable")
	}
	builder, err := x509.ParseCertificate(snapshot.CertificateChain[0])
	if err != nil {
		return ctx, status.Error(codes.FailedPrecondition, "invalid build host certificate")
	}
	input, err := buildassignment.InputSHA256(spec)
	if err != nil {
		return ctx, status.Error(codes.InvalidArgument, err.Error())
	}
	assigned := map[string]DeliveryTarget{}
	ctx = context.WithValue(ctx, assignedTargetsKey{}, assigned)
	for _, target := range targets {
		device, err := meshid.ParseDevice(snapshot.Target.Tenant(), target.GetAssetUuid())
		if err != nil {
			return ctx, status.Error(codes.FailedPrecondition, err.Error())
		}
		claims, err := builddelivery.VerifyBuildBinding(target.GetDeliveryAuthorizationJws(), target.GetDeliverySession(), input, snapshot.TrustAnchors, device, builder, time.Now())
		if err != nil || claims.Reference != target.GetRepository() {
			return ctx, status.Error(codes.PermissionDenied, "missing or mismatched operator build delivery authorization")
		}
		assigned[target.GetAssetUuid()] = DeliveryTarget{Device: device, AssetUUID: target.GetAssetUuid()}
		// A claimed iat cannot establish timely admission. Ask the target for
		// its durable record using our real device credential before building.
		bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
		cc, err := s.dialTarget(bounded, target)
		if err == nil {
			_, err = deliveryAlreadyComplete(bounded, cc, target)
			cc.Close()
		}
		cancel()
		if err != nil {
			return ctx, status.Errorf(codes.FailedPrecondition, "checking admitted delivery for %s: %v", target.GetAssetUuid(), err)
		}
	}
	return ctx, nil
}

type assignedTargetsKey struct{}

// Verified operator authority supplies the exact target without a Cloud roster.
// Discovery remains a locator; both delivery transports still pin target mTLS.
func resolveBuildDeliveryTarget(ctx context.Context, fallback func(string) (DeliveryTarget, error), id string) (DeliveryTarget, error) {
	if assigned, ok := ctx.Value(assignedTargetsKey{}).(map[string]DeliveryTarget); ok {
		if target, found := assigned[id]; found {
			return target, nil
		}
		return DeliveryTarget{}, fmt.Errorf("device has no verified build assignment")
	}
	return fallback(id)
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
