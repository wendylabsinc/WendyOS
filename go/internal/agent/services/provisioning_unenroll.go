package services

import (
	"context"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/unenrollproof"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// The prerelease ACME revocation RPCs are retired, not alternate orchestrators.
func (s *ProvisioningServiceV2) RevokeACMECertificate(context.Context, *agentpbv2.RevokeACMECertificateRequest) (*agentpbv2.RevokeACMECertificateResponse, error) {
	return nil, status.Error(codes.Unimplemented, "Cloud owns device unenrollment and certificate revocation")
}
func (s *ProvisioningServiceV2) CheckACMERevocation(context.Context, *agentpbv2.RevokeACMECertificateRequest) (*agentpbv2.RevokeACMECertificateResponse, error) {
	return nil, status.Error(codes.Unimplemented, "use Cloud binding preflight; no Agent ACME revocation path")
}

func (s *ProvisioningService) requirePKIOperator(ctx context.Context) error {
	identity, err := certs.ParsePrincipal(s.principalURI)
	if err != nil {
		return status.Error(codes.FailedPrecondition, "device has no direct PKI identity")
	}
	if err := interceptor.CheckMTLS(ctx, s.logger, identity.Scope(), interceptor.OrgModeStrict); err != nil {
		return err
	}
	p, ok := peer.FromContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "operator mTLS required")
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || !tlsInfo.State.HandshakeComplete || len(tlsInfo.State.PeerCertificates) == 0 {
		return status.Error(codes.Unauthenticated, "operator mTLS required")
	}
	principal, ok := certs.TenantPrincipalFromCert(tlsInfo.State.PeerCertificates[0])
	actor, err := certs.ParsePrincipal(principal)
	if !ok || err != nil || actor.EntityType != certs.EntityUser || actor.TenantUUID != identity.TenantUUID {
		return status.Error(codes.PermissionDenied, "same-tenant operator certificate required")
	}
	return nil
}

func (s *ProvisioningService) certificateFingerprintLocked() string {
	leaves, _ := certs.ParseCertsFromPEM([]byte(s.certPEM))
	if len(leaves) == 0 {
		return ""
	}
	return unenrollproof.Fingerprint(leaves[0])
}

// This is only reset authorization from verified Cloud/PKI evidence, never a
// revocation performed by Agent and never the old provisioning-state marker.
func (s *ProvisioningService) cloudResetAuthorizedLocked() bool {
	receipt, err := s.pendingCloudReset()
	return err == nil && receipt.Principal == s.principalURI && receipt.Fingerprint == s.certificateFingerprintLocked() && strings.TrimSpace(receipt.Cloud) == s.cloudHost
}
