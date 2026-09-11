package services

import (
	"bytes"
	"context"
	"os"
	"path/filepath"

	"github.com/wendylabsinc/wendy/go/internal/agent/acmeenroll"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// StartACMEProvisioning is separate from the legacy Cloud RPC so older agents
// reject an unsupported method before spending any EAB credentials.
func (s *ProvisioningServiceV2) StartACMEProvisioning(ctx context.Context, req *agentpbv2.StartACMEProvisioningRequest) (*agentpbv2.StartACMEProvisioningResponse, error) {
	cfg := acmeenroll.Config{
		DirectoryURL: req.GetDirectoryUrl(), DeviceID: req.GetDeviceId(),
		EABKeyID: req.GetEabKeyId(), EABHMACKey: req.GetEabHmacKey(),
	}
	if err := cfg.Validate(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid ACME enrollment config: %v", err)
	}
	if req.GetCloudHost() == "" {
		return nil, status.Error(codes.InvalidArgument, "cloud host is required")
	}
	svc := s.v1
	svc.mu.Lock()
	locked := true
	defer func() {
		if locked {
			svc.mu.Unlock()
		}
	}()
	if svc.enrolled {
		return nil, status.Error(codes.FailedPrecondition, "agent is already provisioned")
	}
	keyPEM, err := svc.loadOrGenerateKey()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "loading device key: %v", err)
	}
	// A lost key makes the newly issued certificate unusable. Check persistence
	// before ACME can consume the one-time EAB.
	stored, err := os.ReadFile(filepath.Join(svc.configPath, "device-key.pem"))
	if err != nil || !bytes.Equal(stored, keyPEM) {
		return nil, status.Error(codes.Internal, "device key could not be persisted; enrollment has not started")
	}
	leaf, chain, err := acmeEnrollDevice(ctx, cfg, filepath.Join(svc.configPath, "acme-account-key.pem"), keyPEM)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "enrolling with PKI: %v", err)
	}
	if chain == "" {
		return nil, status.Error(codes.Internal, "PKI returned no certificate chain for device mTLS")
	}
	principal, _ := cfg.PrincipalURI()
	state := &provisioningState{
		Enrolled: true, CloudHost: req.GetCloudHost(), CertPEM: leaf, ChainPEM: chain,
		PrincipalURI: principal, ACMEDirectoryURL: cfg.DirectoryURL,
	}
	complete, err := svc.persistProvisioning(state, keyPEM)
	if err != nil {
		return nil, err
	}
	locked = false
	svc.mu.Unlock()
	complete()
	return &agentpbv2.StartACMEProvisioningResponse{PrincipalUri: principal}, nil
}

var acmeEnrollDevice = acmeenroll.Enroll
