package services

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/internal/shared/certs"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	cloudpb "github.com/wendylabsinc/wendy/proto/gen/cloudpb"
)

// provisioningState is persisted to disk at configPath/provisioning.json.
type provisioningState struct {
	Enrolled  bool   `json:"enrolled"`
	CloudHost string `json:"cloudHost,omitempty"`
	OrgID     int32  `json:"orgId,omitempty"`
	AssetID   int32  `json:"assetId,omitempty"`
	KeyPEM    string `json:"keyPem,omitempty"`
	CertPEM   string `json:"certPem,omitempty"`
	ChainPEM  string `json:"chainPem,omitempty"`
}

// CloudDialer is a function that creates a gRPC client connection to the cloud.
// It can be replaced in tests to avoid real network calls.
type CloudDialer func(ctx context.Context, addr string) (*grpc.ClientConn, error)

// DefaultCloudDialer connects to the cloud gRPC server with plaintext transport.
func DefaultCloudDialer(ctx context.Context, addr string) (*grpc.ClientConn, error) {
	if strings.HasSuffix(addr, ":443") {
		return grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})))
	}
	return grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
}

func certificateServiceAddr(cloudHost string) string {
	if _, _, err := net.SplitHostPort(cloudHost); err == nil {
		return cloudHost
	}
	return net.JoinHostPort(cloudHost, "50051")
}

// OnProvisionedFunc is called when provisioning completes successfully.
// It receives the provisioned certificate PEM, chain PEM, and private key PEM.
type OnProvisionedFunc func(certPEM, chainPEM, keyPEM string)

// ProvisioningService implements agentpb.WendyProvisioningServiceServer.
type ProvisioningService struct {
	agentpb.UnimplementedWendyProvisioningServiceServer
	logger        *zap.Logger
	configPath    string
	mu            sync.Mutex
	enrolled      bool
	cloudHost     string
	orgID         int32
	assetID       int32
	keyPEM        string
	certPEM       string
	chainPEM      string
	CloudDialer   CloudDialer
	OnProvisioned OnProvisionedFunc
}

// NewProvisioningService creates a new ProvisioningService.
// configPath is the directory where provisioning state is stored (e.g., /etc/wendy).
func NewProvisioningService(logger *zap.Logger, configPath string) *ProvisioningService {
	svc := &ProvisioningService{
		logger:      logger,
		configPath:  configPath,
		CloudDialer: DefaultCloudDialer,
	}
	svc.loadState()
	return svc
}

// ProvisioningCerts returns the stored certificate material if the agent is provisioned.
// Returns empty strings if not provisioned.
func (s *ProvisioningService) ProvisioningCerts() (certPEM, chainPEM, keyPEM string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.certPEM, s.chainPEM, s.keyPEM
}

// ProvisioningInfo returns the cloud host, org ID, and asset ID if the agent is provisioned.
func (s *ProvisioningService) ProvisioningInfo() (cloudHost string, orgID, assetID int32, enrolled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cloudHost, s.orgID, s.assetID, s.enrolled
}

// IsProvisioned checks whether the agent is enrolled with a cloud organization.
func (s *ProvisioningService) IsProvisioned(_ context.Context, _ *agentpb.IsProvisionedRequest) (*agentpb.IsProvisionedResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.enrolled {
		return &agentpb.IsProvisionedResponse{
			Response: &agentpb.IsProvisionedResponse_Provisioned{
				Provisioned: &agentpb.ProvisionedResponse{
					CloudHost:      s.cloudHost,
					OrganizationId: s.orgID,
					AssetId:        s.assetID,
				},
			},
		}, nil
	}

	return &agentpb.IsProvisionedResponse{
		Response: &agentpb.IsProvisionedResponse_NotProvisioned{
			NotProvisioned: &agentpb.NotProvisionedResponse{},
		},
	}, nil
}

// StartProvisioning generates a CSR, exchanges with the cloud, and stores certificates.
func (s *ProvisioningService) StartProvisioning(ctx context.Context, req *agentpb.StartProvisioningRequest) (*agentpb.StartProvisioningResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.enrolled {
		return nil, status.Error(codes.FailedPrecondition, "agent is already provisioned")
	}

	s.logger.Info("Starting provisioning",
		zap.Int32("org_id", req.GetOrganizationId()),
		zap.String("cloud_host", req.GetCloudHost()),
		zap.Int32("asset_id", req.GetAssetId()),
	)

	// Reuse the device's existing private key if present, otherwise generate a new one.
	keyPEM, err := s.loadOrGenerateKey()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to load or generate key pair: %v", err)
	}

	// Generate CSR using org and asset as common name.
	commonName := fmt.Sprintf("sh/wendy/%d/%d", req.GetOrganizationId(), req.GetAssetId())
	csrPEM, err := certs.GenerateCSR(keyPEM, commonName)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to generate CSR: %v", err)
	}

	// Connect to the cloud gRPC server.
	cloudAddr := certificateServiceAddr(req.GetCloudHost())
	cloudConn, err := s.CloudDialer(ctx, cloudAddr)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "connecting to cloud: %v", err)
	}
	defer cloudConn.Close()

	// Send the CSR to the cloud for certificate issuance.
	certClient := cloudpb.NewCertificateServiceClient(cloudConn)
	issueResp, err := certClient.IssueCertificate(ctx, &cloudpb.IssueCertificateRequest{
		PemCsr:          csrPEM,
		EnrollmentToken: req.GetEnrollmentToken(),
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "issuing certificate from cloud: %v", err)
	}

	// Check for error in the response.
	if issueResp.GetError() != nil {
		certErr := issueResp.GetError()
		return nil, status.Errorf(codes.Internal, "cloud certificate issuance failed: %s", certErr.GetMessage())
	}

	// Extract certificate material from the response.
	cert := issueResp.GetCertificate()
	if cert == nil {
		return nil, status.Error(codes.Internal, "cloud returned empty certificate")
	}

	certPEM := cert.GetPemCertificate()
	chainPEM := cert.GetPemCertificateChain()

	// Build the state struct from the request/cert values WITHOUT first mutating
	// s.* fields. Only apply the state to s.* after saveState succeeds so that a
	// disk-write failure does not leave the agent permanently stuck as "already
	// provisioned".
	state := &provisioningState{
		Enrolled:  true,
		CloudHost: req.GetCloudHost(),
		OrgID:     req.GetOrganizationId(),
		AssetID:   req.GetAssetId(),
		KeyPEM:    keyPEM,
		CertPEM:   certPEM,
		ChainPEM:  chainPEM,
	}
	if err := s.saveState(state); err != nil {
		s.logger.Error("Failed to persist provisioning state", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "failed to save provisioning state: %v", err)
	}

	// Persist succeeded — now it is safe to update in-memory state.
	s.enrolled = true
	s.cloudHost = state.CloudHost
	s.orgID = state.OrgID
	s.assetID = state.AssetID
	s.keyPEM = keyPEM
	s.certPEM = certPEM
	s.chainPEM = chainPEM

	// Write individual PEM files so the container registry can mount and use them.
	if err := s.writePEMFiles(keyPEM, certPEM, chainPEM); err != nil {
		s.logger.Error("Failed to write PEM files for registry", zap.Error(err))
		// Non-fatal: provisioning.json is the source of truth.
	}

	s.logger.Info("Provisioning completed successfully",
		zap.Int32("org_id", s.orgID),
		zap.Int32("asset_id", s.assetID),
	)

	// Capture the callback and invoke it without manually unlocking/re-locking
	// the mutex here, to avoid interfering with any deferred Unlock.
	cb := s.OnProvisioned
	if cb != nil {
		cb(certPEM, chainPEM, keyPEM)
	}

	return &agentpb.StartProvisioningResponse{}, nil
}

// statePath returns the path to the provisioning state file.
func (s *ProvisioningService) statePath() string {
	return filepath.Join(s.configPath, "provisioning.json")
}

// loadState loads provisioning state from disk.
func (s *ProvisioningService) loadState() {
	data, err := os.ReadFile(s.statePath())
	if err != nil {
		return
	}

	var state provisioningState
	if err := json.Unmarshal(data, &state); err != nil {
		s.logger.Warn("Failed to parse provisioning state", zap.Error(err))
		return
	}

	s.enrolled = state.Enrolled
	s.cloudHost = state.CloudHost
	s.orgID = state.OrgID
	s.assetID = state.AssetID
	s.keyPEM = state.KeyPEM
	s.certPEM = state.CertPEM
	s.chainPEM = state.ChainPEM

	// Ensure PEM files exist on disk (may have been lost during OTA update).
	if s.enrolled && s.keyPEM != "" && s.certPEM != "" {
		if err := s.writePEMFiles(s.keyPEM, s.certPEM, s.chainPEM); err != nil {
			s.logger.Warn("Failed to restore PEM files from provisioning state", zap.Error(err))
		}
	}
}

// loadOrGenerateKey returns the PEM-encoded private key for this device.
// It reuses the key at {configPath}/device-key.pem if it exists, otherwise
// generates a new one and persists it.
func (s *ProvisioningService) loadOrGenerateKey() (string, error) {
	keyPath := filepath.Join(s.configPath, "device-key.pem")
	if data, err := os.ReadFile(keyPath); err == nil && len(data) > 0 {
		s.logger.Info("Reusing existing device key", zap.String("path", keyPath))
		return string(data), nil
	}

	keyPEM, err := certs.GenerateKeyPair()
	if err != nil {
		return "", err
	}

	// Persist the key so it's reused on future provisioning.
	if err := os.MkdirAll(s.configPath, 0o700); err == nil {
		_ = os.WriteFile(keyPath, []byte(keyPEM), 0o600)
	}

	return keyPEM, nil
}

// writePEMFiles writes individual PEM files for the container registry and
// other services that read certs from the filesystem.
func (s *ProvisioningService) writePEMFiles(keyPEM, certPEM, chainPEM string) error {
	return WritePEMFiles(s.configPath, keyPEM, certPEM, chainPEM)
}

// saveState writes provisioning state to disk.
func (s *ProvisioningService) saveState(state *provisioningState) error {
	if err := os.MkdirAll(s.configPath, 0o700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	return os.WriteFile(s.statePath(), data, 0o600)
}
