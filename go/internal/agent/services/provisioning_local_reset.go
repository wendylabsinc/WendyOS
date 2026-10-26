package services

import (
	"context"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/unenrollproof"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// localResetState is authority only inside the protected provisioning.json
// written after an authenticated operator requested exact local credential
// erasure. It carries no claim about Cloud deletion or certificate revocation.
// A completed record keeps no identity; it only tells startup recovery that a
// reset's dependent resources may still need reclaiming.
type localResetState struct {
	Version     int    `json:"version"`
	Status      string `json:"status"`
	Principal   string `json:"principal,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
}

func validateLocalResetState(state *provisioningState) error {
	record := state.LocalReset
	if record == nil || record.Version != 1 || state.Unenrollment != nil {
		return fmt.Errorf("invalid local reset authorization")
	}
	switch record.Status {
	case unenrollmentPending:
		identity, err := certs.ParsePrincipal(record.Principal)
		if err != nil || identity.EntityType != certs.EntityAsset || !state.Enrolled || state.PrincipalURI != record.Principal || state.KeyPEM != "" {
			return fmt.Errorf("local reset authorization disagrees with enrollment state")
		}
		leaves, err := certs.ParseCertsFromPEM([]byte(state.CertPEM))
		if err != nil || len(leaves) == 0 || unenrollproof.Fingerprint(leaves[0]) != record.Fingerprint {
			return fmt.Errorf("local reset authorization certificate changed")
		}
	case unenrollmentCompleted:
		if record.Principal != "" || record.Fingerprint != "" || state.Enrolled || state.CloudHost != "" || state.OrgID != 0 || state.AssetID != 0 || state.PrincipalURI != "" || state.KeyPEM != "" || state.CertPEM != "" || state.ChainPEM != "" || state.ACMEDirectoryURL != "" {
			return fmt.Errorf("completed local reset retains enrollment state")
		}
	default:
		return fmt.Errorf("unknown local reset status")
	}
	return nil
}

func (s *ProvisioningService) pendingLocalReset() (*localResetState, error) {
	state, err := s.readProvisioningState()
	if err != nil {
		return nil, err
	}
	if err := validateLocalResetState(state); err != nil {
		return nil, err
	}
	if state.LocalReset.Status != unenrollmentPending {
		return nil, fmt.Errorf("local reset is not pending")
	}
	return state.LocalReset, nil
}

func (s *ProvisioningService) localResetAuthorizedLocked() bool {
	record, err := s.pendingLocalReset()
	return err == nil && record.Principal == s.principalURI && record.Fingerprint == s.certificateFingerprintLocked()
}

// LocalResetCompleted reports a verified completed local-only reset. Startup
// uses it, like a Cloud completion receipt, to reclaim dependent resources.
func (s *ProvisioningService) LocalResetCompleted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.enrolled {
		return false
	}
	state, err := s.readProvisioningState()
	return err == nil && state.LocalReset != nil && validateLocalResetState(state) == nil && state.LocalReset.Status == unenrollmentCompleted
}

func (s *ProvisioningService) finishLocalReset() error {
	if _, err := s.pendingLocalReset(); err != nil {
		return err
	}
	// Keep the protected pending record until every credential removal is
	// durable. Startup can resume after a partial cleanup without network I/O.
	if err := syncResetDirectory(s.configPath); err != nil {
		return err
	}
	if err := s.clearCredentialFiles(); err != nil {
		return err
	}
	if err := syncResetDirectory(s.configPath); err != nil {
		return err
	}
	return s.saveState(&provisioningState{LocalReset: &localResetState{Version: 1, Status: unenrollmentCompleted}})
}

func (s *ProvisioningServiceV2) localUnprovision(ctx context.Context, req *agentpbv2.UnprovisionRequest) (*agentpbv2.UnprovisionResponse, error) {
	svc := s.v1
	svc.unenrollmentMu.Lock() // serialization only; no network I/O
	defer svc.unenrollmentMu.Unlock()
	svc.mu.Lock()
	locked := true
	defer func() {
		if locked {
			svc.mu.Unlock()
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	if !supportsTrustedResetStorage {
		return nil, status.Error(codes.FailedPrecondition, "protected local reset storage unavailable; keys retained")
	}
	if err := svc.requirePKIOperator(ctx); err != nil {
		return nil, err
	}
	if !svc.enrolled || req.GetExpectedPrincipalUri() != svc.principalURI || req.GetExpectedCertificateSha256() == "" || req.GetExpectedCertificateSha256() != svc.certificateFingerprintLocked() {
		return nil, status.Error(codes.FailedPrecondition, "exact installed identity required for local reset; keys retained")
	}
	if len(req.GetRevocationProof()) != 0 || len(req.GetCloudDeletion()) != 0 || len(req.GetCloudResetAuthorization()) != 0 {
		return nil, status.Error(codes.InvalidArgument, "local-only reset must not include Cloud unenrollment evidence")
	}
	state, err := svc.readProvisioningState()
	if err != nil {
		return nil, status.Error(codes.Internal, "reading local reset state failed; keys retained")
	}
	if state.Unenrollment != nil {
		return nil, status.Error(codes.FailedPrecondition, "Cloud reset is already pending; resume that operation")
	}
	previous := *state
	state.KeyPEM = ""
	state.LocalReset = &localResetState{Version: 1, Status: unenrollmentPending, Principal: svc.principalURI, Fingerprint: req.GetExpectedCertificateSha256()}
	if err := validateLocalResetState(state); err != nil {
		return nil, status.Error(codes.FailedPrecondition, "stored identity changed before local reset; keys retained")
	}
	if err := svc.saveState(state); err != nil {
		return nil, status.Error(codes.Internal, "durable local reset authorization failed; keys retained")
	}
	// Recovery honours a pending record only from protected storage. If the
	// saved record would be refused (for example, a group-writable config
	// directory), restore the enrolled state rather than block the next start.
	if _, err := svc.pendingLocalReset(); err != nil {
		if rollbackErr := svc.saveState(&previous); rollbackErr != nil {
			svc.logger.Error("Restoring state after an unusable local reset record failed", zap.Error(rollbackErr))
		}
		return nil, status.Error(codes.FailedPrecondition, "provisioning state is not in protected local storage; keys retained")
	}
	caller, _ := peer.FromContext(ctx)
	operator, _ := certs.TenantPrincipalFromCert(caller.AuthInfo.(credentials.TLSInfo).State.PeerCertificates[0])
	svc.logger.Warn("Operator forced local enrollment reset without Cloud unenrollment", zap.String("principal", svc.principalURI), zap.String("operator", operator))
	// unprovision rechecks the exact identity against the pending record under
	// its own locks, then shares Cloud reset's cleanup, hooks and restart.
	locked = false
	svc.mu.Unlock()
	if _, err := svc.unprovision(req.GetExpectedPrincipalUri(), req.GetExpectedCertificateSha256()); err != nil {
		return nil, err
	}
	return &agentpbv2.UnprovisionResponse{LocalResetCompleted: true}, nil
}
