package services

import (
	"context"
	"crypto"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/unenrollproof"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func validateResetBinding(record *unenrollproof.Completion) error {
	identity, err := certs.ParsePrincipal(record.Principal)
	if err != nil || identity.EntityType != certs.EntityAsset {
		return fmt.Errorf("invalid reset identity")
	}
	if record.Cloud == "" || len(record.CloudDeletion) == 0 || len(record.CloudDeletion) > 4096 {
		return fmt.Errorf("invalid Cloud deletion evidence size/endpoint")
	}
	id, err := uuid.Parse(record.AssetID)
	if err != nil || id.String() != record.AssetID {
		return fmt.Errorf("invalid reset asset UUID")
	}
	var deleted cloudpbv2.DeletedAsset
	if err := proto.Unmarshal(record.CloudDeletion, &deleted); err != nil {
		return err
	}
	// SECURITY: Agent wall-clock skew intentionally fails closed here. Correct
	// the clock before retrying; do not relax the exact binding or proof checks.
	if deleted.GetId() == "" || deleted.GetId() != record.AssetID || deleted.GetOrganizationId() != identity.TenantUUID || deleted.GetDeviceId() != identity.EntityID || deleted.GetDeletedAt() == nil || deleted.GetDeletedAt().CheckValid() != nil || deleted.GetDeletedAt().AsTime().After(time.Unix(record.AuthorizedAt, 0).Add(time.Second)) {
		return fmt.Errorf("invalid exact Cloud deletion binding")
	}
	return nil
}
func (s *ProvisioningService) readProvisioningState() (*provisioningState, error) {
	raw, err := os.ReadFile(s.statePath())
	if err != nil {
		return nil, err
	}
	var state provisioningState
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

// SECURITY: This validates only the Agent's root-owned 0600 provisioning state,
// written after operator mTLS and exact installed-leaf checks. Pending evidence
// is re-bound to that installed identity below. It is not a verifier of remote
// device identity: CLI receipt recovery separately anchors the historical leaf
// to its configured roots before trusting a public status response.
func validateUnenrollmentState(state *provisioningState) (*unenrollproof.Completion, error) {
	if state.Unenrollment == nil {
		return nil, fmt.Errorf("reset authorization absent")
	}
	record, _, err := unenrollproof.ReadCompletion(state.Unenrollment.Receipt)
	if err != nil {
		return nil, err
	}
	if err := validateResetBinding(record); err != nil {
		return nil, err
	}
	switch state.Unenrollment.Status {
	case unenrollmentPending:
		if !state.Enrolled || state.PrincipalURI != record.Principal || state.CloudHost != record.Cloud || state.KeyPEM != "" {
			return nil, fmt.Errorf("reset authorization disagrees with enrollment state")
		}
		leaves, _ := certs.ParseCertsFromPEM([]byte(state.CertPEM))
		if len(leaves) == 0 || unenrollproof.Fingerprint(leaves[0]) != record.Fingerprint {
			return nil, fmt.Errorf("reset authorization certificate changed")
		}
	case unenrollmentCompleted:
		if state.Enrolled || state.CloudHost != "" || state.OrgID != 0 || state.AssetID != 0 || state.PrincipalURI != "" || state.KeyPEM != "" || state.CertPEM != "" || state.ChainPEM != "" || state.ACMEDirectoryURL != "" {
			return nil, fmt.Errorf("completed reset retains enrollment state")
		}
	default:
		return nil, fmt.Errorf("unknown reset recovery status")
	}
	return record, nil
}

func (s *ProvisioningService) pendingCloudReset() (*unenrollproof.Completion, error) {
	state, err := s.readProvisioningState()
	if err != nil {
		return nil, err
	}
	record, err := validateUnenrollmentState(state)
	if err != nil {
		return nil, err
	}
	if state.Unenrollment.Status != unenrollmentPending {
		return nil, fmt.Errorf("reset authorization is not pending")
	}
	return record, nil
}
func syncResetDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (s *ProvisioningService) finishCloudReset() error {
	// provisioning.json must survive every partial credential removal. Only
	// after cleanup and directory sync do we replace it with minimal public
	// completed state; a crash earlier leaves the signed pending authorization.
	state, err := s.readProvisioningState()
	if err != nil {
		return err
	}
	if _, err := validateUnenrollmentState(state); err != nil {
		return err
	}
	if state.Unenrollment.Status != unenrollmentPending {
		return fmt.Errorf("reset authorization is not pending")
	}
	if err := syncResetDirectory(s.configPath); err != nil {
		return err
	}
	if err := s.clearCredentialFiles(); err != nil {
		return err
	}
	if err := syncResetDirectory(s.configPath); err != nil {
		return err
	}
	return s.saveState(&provisioningState{Unenrollment: &unenrollmentState{
		Status: unenrollmentCompleted, Receipt: state.Unenrollment.Receipt,
	}})
}
func (s *ProvisioningService) recoverCloudReset() error {
	state, err := s.readProvisioningState()
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if state.Unenrollment == nil {
		return nil
	}
	if _, err := validateUnenrollmentState(state); err != nil {
		return err
	}
	if state.Unenrollment.Status == unenrollmentPending {
		return s.finishCloudReset()
	}
	return syncResetDirectory(s.configPath)
}
func (s *ProvisioningService) cloudCompletion() ([]byte, error) {
	state, err := s.readProvisioningState()
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if state.Unenrollment == nil {
		return nil, nil
	}
	if _, err := validateUnenrollmentState(state); err != nil {
		return nil, err
	}
	if state.Unenrollment.Status != unenrollmentCompleted {
		return nil, fmt.Errorf("reset recovery is still pending")
	}
	return state.Unenrollment.Receipt, nil
}
func (s *ProvisioningServiceV2) cloudUnprovision(ctx context.Context, req *agentpbv2.UnprovisionRequest) (*agentpbv2.UnprovisionResponse, error) {
	svc := s.v1
	svc.unenrollmentMu.Lock() // serialization only; no ACME calls
	defer svc.unenrollmentMu.Unlock()
	svc.mu.Lock()
	if err := svc.requirePKIOperator(ctx); err != nil {
		svc.mu.Unlock()
		return nil, err
	}
	if !svc.enrolled || req.GetExpectedPrincipalUri() != svc.principalURI || req.GetExpectedCertificateSha256() != svc.certificateFingerprintLocked() {
		svc.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "exact installed identity required; keys retained")
	}
	leaves, _ := certs.ParseCertsFromPEM([]byte(svc.certPEM))
	chain, _ := certs.ParseCertsFromPEM([]byte(svc.chainPEM))
	if len(leaves) == 0 {
		svc.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "installed certificate unavailable")
	}
	issuer, err := unenrollproof.Issuer(leaves[0], chain)
	if err == nil {
		err = unenrollproof.VerifyRevocation(req.GetRevocationProof(), leaves[0], issuer, time.Now())
	}
	if err != nil {
		svc.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "authenticated installed-leaf revocation evidence required; keys retained")
	}
	var deleted cloudpbv2.DeletedAsset
	if err := proto.Unmarshal(req.GetCloudDeletion(), &deleted); err != nil {
		svc.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "typed Cloud deletion evidence required")
	}
	record := unenrollproof.Completion{Principal: svc.principalURI, Cloud: svc.cloudHost, AssetID: deleted.GetId(), Fingerprint: req.GetExpectedCertificateSha256(), AuthorizedAt: time.Now().Unix(), Certificate: leaves[0].Raw, Chain: svc.chainPEM, Revocation: req.GetRevocationProof(), CloudDeletion: req.GetCloudDeletion()}
	if err := validateResetBinding(&record); err != nil {
		svc.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "exact Cloud deletion binding required; keys retained")
	}
	pair, err := certs.TLSKeyPair(svc.certPEM, svc.chainPEM, string(svc.keyPEM))
	if err != nil {
		svc.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "completion signing key unavailable; keys retained")
	}
	signer, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		svc.mu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "completion signing key unsupported")
	}
	receipt, err := unenrollproof.SignCompletion(record, signer)
	if err == nil {
		var state *provisioningState
		state, err = svc.readProvisioningState()
		if err == nil {
			state.KeyPEM = ""
			state.Unenrollment = &unenrollmentState{Status: unenrollmentPending, Receipt: receipt}
			if _, err = validateUnenrollmentState(state); err == nil {
				err = svc.saveState(state)
			}
		}
	}
	svc.mu.Unlock()
	if err != nil {
		return nil, status.Error(codes.Internal, "durable reset authorization failed; keys retained")
	}
	if _, err := svc.unprovision(req.GetExpectedPrincipalUri(), req.GetExpectedCertificateSha256()); err != nil {
		return nil, err
	}
	return &agentpbv2.UnprovisionResponse{UnenrollmentCompletion: receipt}, nil
}
