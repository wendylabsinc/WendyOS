package services

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/acmeenroll"
	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const acmeRevocationFile = "acme-revocation.json"

type acmeRevocationRecord struct {
	Principal   string `json:"principal"`
	Fingerprint string `json:"certificateSHA256"`
	Serial      string `json:"certificateSerial"`
}

var revokeACMECertificate = acmeenroll.Revoke
var checkACMERevocationAccount = acmeenroll.CheckRevocationAccount

// requirePKIOperator also protects the plaintext server and direct handler
// callers: tenant membership alone must not let another device revoke this one.
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
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return status.Error(codes.Unauthenticated, "operator mTLS required")
	}
	principal, ok := certs.TenantPrincipalFromCert(tlsInfo.State.PeerCertificates[0])
	if !ok || !strings.HasPrefix(principal, "spiffe://wendy.sh/tenant/"+identity.TenantUUID+"/operator/") {
		return status.Error(codes.PermissionDenied, "same-tenant operator certificate required")
	}
	return nil
}

func (s *ProvisioningServiceV2) RevokeACMECertificate(ctx context.Context, req *agentpbv2.RevokeACMECertificateRequest) (*agentpbv2.RevokeACMECertificateResponse, error) {
	return s.acmeRevocation(ctx, req, false)
}

func (s *ProvisioningServiceV2) CheckACMERevocation(ctx context.Context, req *agentpbv2.RevokeACMECertificateRequest) (*agentpbv2.RevokeACMECertificateResponse, error) {
	return s.acmeRevocation(ctx, req, true)
}

func (s *ProvisioningServiceV2) acmeRevocation(ctx context.Context, req *agentpbv2.RevokeACMECertificateRequest, checkOnly bool) (*agentpbv2.RevokeACMECertificateResponse, error) {
	svc := s.v1
	// Serialize account operations, not unrelated provisioning RPCs.
	svc.acmeRevocationMu.Lock()
	defer svc.acmeRevocationMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, status.FromContextError(err).Err()
	}
	svc.mu.Lock()
	locked := true
	defer func() {
		if locked {
			svc.mu.Unlock()
		}
	}()
	if err := svc.requirePKIOperator(ctx); err != nil {
		return nil, err
	}
	if !svc.enrolled || req.GetExpectedPrincipalUri() == "" || req.GetExpectedPrincipalUri() != svc.principalURI {
		return nil, status.Error(codes.FailedPrecondition, "enrolled identity does not match the requested revocation")
	}
	data, err := os.ReadFile(svc.statePath())
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "enrollment metadata unavailable; keys retained")
	}
	var state provisioningState
	if json.Unmarshal(data, &state) != nil || state.PrincipalURI != svc.principalURI || state.CertPEM != svc.certPEM {
		return nil, status.Error(codes.FailedPrecondition, "enrollment metadata changed; keys retained")
	}
	identity, _ := certs.ParsePrincipal(svc.principalURI)
	cfg := acmeenroll.Config{DirectoryURL: state.ACMEDirectoryURL, DeviceID: identity.EntityID}
	principal, fingerprint, serial, _, err := acmeenroll.CertificateIdentity(cfg, svc.certPEM)
	if err != nil || principal != svc.principalURI || fingerprint != req.GetExpectedCertificateSha256() {
		return nil, status.Error(codes.FailedPrecondition, "installed ACME certificate binding is invalid; keys retained")
	}
	record := acmeRevocationRecord{Principal: principal, Fingerprint: fingerprint, Serial: serial}
	if checkOnly || !svc.revocationConfirmedLocked() {
		certificate := svc.certPEM
		accountPath := filepath.Join(svc.configPath, "acme-account-key.pem")
		svc.mu.Unlock()
		locked = false
		bounded, cancel := context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
		operation := revokeACMECertificate
		if checkOnly {
			operation = checkACMERevocationAccount
		}
		if err := operation(bounded, cfg, accountPath, certificate); err != nil {
			return nil, safeACMERevocationStatus(err)
		}
		svc.mu.Lock()
		locked = true
		current, err := os.ReadFile(svc.statePath())
		if err != nil || !svc.enrolled || svc.principalURI != principal || svc.certPEM != certificate || !bytes.Equal(data, current) {
			return nil, status.Error(codes.FailedPrecondition, "enrollment changed during ACME operation; no acknowledgement written; reconcile before cleanup")
		}
		if checkOnly {
			// Do not write an acknowledgement or change enrollment state.
			return &agentpbv2.RevokeACMECertificateResponse{PrincipalUri: principal, CertificateSha256: fingerprint, CertificateSerial: serial}, nil
		}
	}
	// Re-sync even a retry's existing acknowledgement before confirming it;
	// a prior rename may have succeeded while directory sync failed. This also
	// imports matching legacy development evidence without deleting that file.
	if err := writeACMERevocationRecord(svc.configPath, record); err != nil {
		return nil, status.Error(codes.Internal, "certificate revoked but durable acknowledgement failed; keys retained; retry reconciliation before cleanup")
	}
	return &agentpbv2.RevokeACMECertificateResponse{PrincipalUri: principal, CertificateSha256: fingerprint, CertificateSerial: serial}, nil
}

func safeACMERevocationStatus(err error) error {
	var diagnostic *acmeenroll.RevocationError
	if errors.As(err, &diagnostic) {
		return status.Error(codes.FailedPrecondition, diagnostic.Error())
	}
	return status.Error(codes.FailedPrecondition, "ACME revocation failed or is uncertain; enrollment and keys retained")
}

func (s *ProvisioningService) certificateFingerprintLocked() string {
	block, _ := pem.Decode([]byte(s.certPEM))
	if block == nil {
		return ""
	}
	digest := sha256.Sum256(block.Bytes)
	return hex.EncodeToString(digest[:])
}

func (s *ProvisioningService) revocationConfirmedLocked() bool {
	data, err := os.ReadFile(s.statePath())
	if err != nil {
		return false
	}
	var state provisioningState
	if json.Unmarshal(data, &state) != nil || state.PrincipalURI != s.principalURI || state.CertPEM != s.certPEM {
		return false
	}
	var record acmeRevocationRecord
	if state.ACMERevocation != nil {
		record = *state.ACMERevocation
	} else {
		// Read-only compatibility with earlier development builds. Never create
		// another standalone acknowledgement or move/delete existing evidence.
		data, err = os.ReadFile(filepath.Join(s.configPath, acmeRevocationFile))
		if err != nil || json.Unmarshal(data, &record) != nil {
			return false
		}
	}
	if record.Principal != s.principalURI || record.Serial == "" {
		return false
	}
	block, _ := pem.Decode([]byte(s.certPEM))
	if block == nil {
		return false
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil || record.Serial != leaf.SerialNumber.Text(16) {
		return false
	}
	digest := sha256.Sum256(block.Bytes)
	return record.Fingerprint == hex.EncodeToString(digest[:])
}

func writeACMERevocationRecord(dir string, record acmeRevocationRecord) error {
	path := filepath.Join(dir, "provisioning.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var state provisioningState
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	block, _ := pem.Decode([]byte(state.CertPEM))
	if !state.Enrolled || state.PrincipalURI != record.Principal || block == nil {
		return errors.New("enrollment changed before acknowledgement")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(block.Bytes)
	if hex.EncodeToString(digest[:]) != record.Fingerprint || record.Serial != leaf.SerialNumber.Text(16) {
		return errors.New("certificate changed before acknowledgement")
	}
	state.ACMERevocation = &record
	state.KeyPEM = ""
	data, err = json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return writeProvisioningState(path, data)
}
