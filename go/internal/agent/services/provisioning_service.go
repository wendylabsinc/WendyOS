package services

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/agent/acmeenroll"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/enrolltoken"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

// provisioningState is persisted to disk at configPath/provisioning.json.
// NOTE: KeyPEM is retained only for one-time migration of existing deployments;
// new writes never populate it — the private key lives exclusively in
// device-key.pem (mode 0o400) and is never written to provisioning.json.
type provisioningState struct {
	Enrolled  bool   `json:"enrolled"`
	CloudHost string `json:"cloudHost,omitempty"`
	OrgID     int32  `json:"orgId,omitempty"`
	AssetID   int32  `json:"assetId,omitempty"`
	KeyPEM    string `json:"keyPem,omitempty"` // read-only: migration only; never written
	CertPEM   string `json:"certPem,omitempty"`
	ChainPEM  string `json:"chainPem,omitempty"`
}

type CloudDialer func(ctx context.Context, addr string) (*grpc.ClientConn, error)

// cloudInsecureEnv opts the enrollment dial out of TLS. It exists only so a
// developer can point the agent at a local plaintext pki-core; it must never
// be set on a real device. Anything other than an explicit true value keeps
// the dial on TLS.
const cloudInsecureEnv = "WENDY_CLOUD_INSECURE"

// cloudDialInsecure reports whether the operator explicitly asked for a
// plaintext enrollment dial.
func cloudDialInsecure() bool {
	v, err := strconv.ParseBool(os.Getenv(cloudInsecureEnv))
	return err == nil && v
}

// DefaultCloudDialer connects to the cloud gRPC server over TLS.
//
// TLS is the default for every address. Previously the transport was chosen by
// a port heuristic — ":443" meant TLS, anything else meant plaintext — so a
// cloudHost without a port became "<host>:50051" (see certificateServiceAddr)
// and the enrollment token, a bearer credential, went out in cleartext to a
// public host (WDY-2799). A downgrade now requires WENDY_CLOUD_INSECURE, so it
// can only ever be a deliberate local-dev choice rather than a side effect of
// how the caller happened to spell the address.
func DefaultCloudDialer(ctx context.Context, addr string) (*grpc.ClientConn, error) {
	if cloudDialInsecure() {
		return grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
	return grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})))
}

// defaultCloudPort is appended to a cloudHost that carries no port. It is the
// TLS port: a bare hostname denotes Wendy Cloud, which is TLS-only. It used to
// be the plaintext provisioning port 50051, which is what made a port-less
// host downgrade (WDY-2799). A local pki-core is still reachable by naming its
// port explicitly ("localhost:50051") alongside WENDY_CLOUD_INSECURE.
const defaultCloudPort = "443"

// acmeAccountKeyFileName holds the ACME account key. It is deliberately NOT in
// clearStateFiles: an EAB is single-use, so a surviving account key is what
// lets a re-enroll re-register idempotently instead of needing a fresh
// credential. Deleting it is the unrecoverable direction.
const acmeAccountKeyFileName = "acme-account-key.pem"

func certificateServiceAddr(cloudHost string) string {
	if _, _, err := net.SplitHostPort(cloudHost); err == nil {
		return cloudHost
	}
	return net.JoinHostPort(cloudHost, defaultCloudPort)
}

// OnProvisionedFunc is called when provisioning completes successfully.
// keyData is the raw PEM bytes of the private key; callers should zero it
// when done. certPEM and chainPEM are plain strings (public material).
type OnProvisionedFunc func(certPEM, chainPEM string, keyData []byte)

// OnUnprovisionedFunc is called after the device has been unprovisioned and its
// state has been cleared. It is invoked asynchronously, shortly after the RPC
// response is sent, so implementations can revert the mDNS advertisement and
// restart the agent process. If nil, no post-unprovision action is taken.
type OnUnprovisionedFunc func()

// ProvisioningService implements agentpb.WendyProvisioningServiceServer.
type ProvisioningService struct {
	agentpb.UnimplementedWendyProvisioningServiceServer
	logger          *zap.Logger
	configPath      string
	mu              sync.Mutex
	enrolled        bool
	cloudHost       string
	orgID           int32
	assetID         int32
	keyPEM          []byte // stored as []byte so it can be zeroed on rotation/shutdown
	certPEM         string
	chainPEM        string
	CloudDialer     CloudDialer
	OnProvisioned   OnProvisionedFunc
	OnUnprovisioned OnUnprovisionedFunc
}

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
// The private key is returned as a copy so callers can zero it after use.
// Returns empty cert/chain and nil key if not provisioned.
func (s *ProvisioningService) ProvisioningCerts() (certPEM, chainPEM string, keyData []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.keyPEM) == 0 {
		return s.certPEM, s.chainPEM, nil
	}
	keyData = make([]byte, len(s.keyPEM))
	copy(keyData, s.keyPEM)
	return s.certPEM, s.chainPEM, keyData
}

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
	locked := true
	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()

	if s.enrolled {
		return nil, status.Error(codes.FailedPrecondition, "agent is already provisioned")
	}

	s.logger.Info("Starting provisioning",
		zap.Int32("org_id", req.GetOrganizationId()),
		zap.String("cloud_host", req.GetCloudHost()),
		zap.Int32("asset_id", req.GetAssetId()),
		zap.String("device_id", req.GetAcme().GetDeviceId()),
	)

	// Reuse the device's existing private key if present, otherwise generate a new one.
	keyPEM, err := s.loadOrGenerateKey()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to load or generate key pair: %v", err)
	}

	// Two ways to obtain the leaf, and only the source differs: everything
	// below — persistence, in-memory state, PEM files, the callback — is the
	// same either way. With a staged credential the device enrolls itself
	// against pki-core and no certificate ever passes through the CLI; without
	// one it is the legacy cloud relay, kept working for older callers.
	var certPEM, chainPEM string
	if acmeCred := req.GetAcme(); acmeCred != nil {
		certPEM, chainPEM, err = s.enrollViaACME(ctx, acmeCred, keyPEM)
	} else {
		certPEM, chainPEM, err = s.enrollViaCloud(ctx, req, keyPEM)
	}
	if err != nil {
		return nil, err
	}

	// Build the state struct from the request/cert values WITHOUT first mutating
	// s.* fields. Only apply the state to s.* after saveState succeeds so that a
	// disk-write failure does not leave the agent permanently stuck as "already
	// provisioned". The private key is never written to provisioning.json —
	// it lives only in device-key.pem (written by loadOrGenerateKey).
	state := &provisioningState{
		Enrolled:  true,
		CloudHost: req.GetCloudHost(),
		OrgID:     req.GetOrganizationId(),
		AssetID:   req.GetAssetId(),
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
	// string(keyPEM) creates a temporary copy; filesystem write cannot be avoided.
	if err := s.writePEMFiles(string(keyPEM), certPEM, chainPEM); err != nil {
		s.logger.Error("Failed to write PEM files for registry", zap.Error(err))
		// Non-fatal: provisioning.json is the source of truth.
	}

	s.logger.Info("Provisioning completed successfully",
		zap.Int32("org_id", s.orgID),
		zap.Int32("asset_id", s.assetID),
	)

	// Capture callback data and unlock before invoking to prevent deadlock
	// (callbacks may call back into ProvisioningService) and to pass a copy
	// so callers can safely zero the slice without corrupting stored state.
	cb := s.OnProvisioned
	var cbKeyPEM []byte
	if cb != nil {
		cbKeyPEM = make([]byte, len(keyPEM))
		copy(cbKeyPEM, keyPEM)
	}
	locked = false
	s.mu.Unlock()
	if cb != nil {
		cb(certPEM, chainPEM, cbKeyPEM)
	}
	return &agentpb.StartProvisioningResponse{}, nil
}

// enrollViaACME redeems a staged EAB against pki-core's ACME frontend. The
// device signs its own CSR with a key that never leaves it, and pki-core
// stamps the identity: nothing the device asserts in the CSR is honoured
// beyond the public key.
func (s *ProvisioningService) enrollViaACME(ctx context.Context, cred *agentpb.AcmeEnrollment, keyPEM []byte) (string, string, error) {
	certPEM, chainPEM, err := acmeenroll.Enroll(ctx, acmeenroll.Config{
		DirectoryURL: cred.GetDirectoryUrl(),
		DeviceID:     cred.GetDeviceId(),
		// Both are relayed verbatim from pki-core; acmeenroll hex-decodes the
		// HMAC key itself, so decoding here would break the MAC.
		EABKeyID:   cred.GetEabKeyId(),
		EABHMACKey: cred.GetEabHmacKey(),
	}, filepath.Join(s.configPath, acmeAccountKeyFileName), keyPEM)
	if err != nil {
		// An EAB is single use. Say so on every failure, because the remedy is
		// always a fresh "wendy device enroll" rather than a retry of this RPC.
		return "", "", status.Errorf(codes.Internal,
			"enrolling against pki-core over ACME: %v (the staged credential is single-use and is now spent; run 'wendy device enroll' again for a fresh one)", err)
	}
	return certPEM, chainPEM, nil
}

// enrollViaCloud is the legacy relay: the device sends a CSR to Wendy Cloud's
// CertificateService and is handed a leaf. Superseded by enrollViaACME and
// kept only so an older CLI keeps working (WDY-2943).
func (s *ProvisioningService) enrollViaCloud(ctx context.Context, req *agentpb.StartProvisioningRequest, keyPEM []byte) (string, string, error) {

	// Generate CSR using org and asset as common name. The device identity acts
	// as both a TLS client (to the cloud) and a TLS server (agent gRPC and tunnel
	// endpoints), so request both EKUs.
	commonName := fmt.Sprintf("sh/wendy/%d/%d", req.GetOrganizationId(), req.GetAssetId())
	identityURIs := []string{certs.AssetURN(req.GetOrganizationId(), req.GetAssetId())}
	// Cloud will not sign a relay grant unless the CSR carries the tenant
	// SPIFFE principal too. The tenant comes from the enrollment token, and is
	// absent for orgs with no pki tenant — then we enroll exactly as before.
	if spiffeURI, ok := enrolltoken.TenantSPIFFEURIFromToken(req.GetEnrollmentToken()); ok {
		identityURIs = append(identityURIs, spiffeURI)
	}
	csrPEM, err := certs.GenerateCSR(keyPEM, commonName, identityURIs,
		x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth)
	if err != nil {
		return "", "", status.Errorf(codes.Internal, "failed to generate CSR: %v", err)
	}

	// Connect to the cloud gRPC server.
	cloudAddr := certificateServiceAddr(req.GetCloudHost())
	if cloudDialInsecure() {
		// The enrollment token is a bearer credential. If someone has opted out
		// of TLS, say so loudly and name the host it is being sent to, so a
		// stray environment variable on a real device is visible in the logs.
		s.logger.Warn("Enrollment dial is PLAINTEXT: the enrollment token will be sent in cleartext",
			zap.String("env", cloudInsecureEnv),
			zap.String("addr", cloudAddr),
		)
	}
	cloudConn, err := s.CloudDialer(ctx, cloudAddr)
	if err != nil {
		return "", "", status.Errorf(codes.Internal, "connecting to cloud: %v", err)
	}
	defer cloudConn.Close()

	// Send the CSR to the cloud for certificate issuance.
	certClient := cloudpb.NewCertificateServiceClient(cloudConn)
	issueResp, err := certClient.IssueCertificate(ctx, &cloudpb.IssueCertificateRequest{
		PemCsr:          csrPEM,
		EnrollmentToken: req.GetEnrollmentToken(),
	})
	if err != nil {
		return "", "", status.Errorf(codes.Internal, "issuing certificate from cloud: %v", err)
	}

	// Check for error in the response.
	if issueResp.GetError() != nil {
		certErr := issueResp.GetError()
		return "", "", status.Errorf(codes.Internal, "cloud certificate issuance failed: %s", certErr.GetMessage())
	}

	// Extract certificate material from the response.
	cert := issueResp.GetCertificate()
	if cert == nil {
		return "", "", status.Error(codes.Internal, "cloud returned empty certificate")
	}

	return cert.GetPemCertificate(), cert.GetPemCertificateChain(), nil
}

// Unprovision resets the device to an unprovisioned state. It deletes the
// stored enrollment certificates and provisioning state from disk, clears the
// in-memory state, and (if configured) invokes OnUnprovisioned shortly after
// the response is sent so the agent can revert its mDNS advertisement and
// restart into plaintext mode.
func (s *ProvisioningService) Unprovision(_ context.Context, _ *agentpb.UnprovisionRequest) (*agentpb.UnprovisionResponse, error) {
	s.mu.Lock()
	locked := true
	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()

	if !s.enrolled {
		return nil, status.Error(codes.FailedPrecondition, "agent is not provisioned")
	}

	s.logger.Info("Unprovisioning device",
		zap.Int32("org_id", s.orgID),
		zap.Int32("asset_id", s.assetID),
	)

	if err := s.clearStateFiles(); err != nil {
		s.logger.Error("Failed to delete provisioning state files", zap.Error(err))
		return nil, status.Errorf(codes.Internal, "failed to delete provisioning state: %v", err)
	}

	// Zero the in-memory key before dropping the reference, then clear state.
	for i := range s.keyPEM {
		s.keyPEM[i] = 0
	}
	s.enrolled = false
	s.cloudHost = ""
	s.orgID = 0
	s.assetID = 0
	s.keyPEM = nil
	s.certPEM = ""
	s.chainPEM = ""

	s.logger.Info("Device unprovisioned; agent will restart into unprovisioned mode")

	cb := s.OnUnprovisioned
	locked = false
	s.mu.Unlock()

	if cb != nil {
		// Invoke asynchronously after a short delay so the RPC response is
		// flushed to the client before the agent restarts. Mirrors the agent
		// update and reboot flows.
		go func() {
			time.Sleep(500 * time.Millisecond)
			cb()
		}()
	}

	return &agentpb.UnprovisionResponse{}, nil
}

// clearStateFiles removes all on-disk provisioning artifacts: the state file,
// the device private key, the mounted PEM files, and the .provisioned marker.
// A missing file is not treated as an error.
func (s *ProvisioningService) clearStateFiles() error {
	files := []string{
		s.statePath(),
		filepath.Join(s.configPath, "device-key.pem"),
		filepath.Join(s.configPath, "device.pem"),
		filepath.Join(s.configPath, "ca.pem"),
		filepath.Join(s.configPath, ".provisioned"),
	}
	for _, f := range files {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing %s: %w", f, err)
		}
	}
	return nil
}

func (s *ProvisioningService) statePath() string {
	return filepath.Join(s.configPath, "provisioning.json")
}

// loadState loads provisioning state from disk.
// The private key is always read from device-key.pem (mode 0o400). If that
// file is absent but the legacy provisioning.json contains a KeyPEM entry, the
// key is migrated to device-key.pem and removed from provisioning.json so that
// subsequent reads use the dedicated file.
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
	s.certPEM = state.CertPEM
	s.chainPEM = state.ChainPEM

	// Load the private key from device-key.pem.  Fall back to the legacy
	// KeyPEM field in provisioning.json for one-time migration of existing
	// devices, then immediately rewrite provisioning.json without the key.
	if s.enrolled {
		keyPath := filepath.Join(s.configPath, "device-key.pem")
		if keyData, readErr := os.ReadFile(keyPath); readErr == nil && len(keyData) > 0 {
			s.keyPEM = keyData
		} else if state.KeyPEM != "" {
			s.keyPEM = []byte(state.KeyPEM)
			if writeErr := os.WriteFile(keyPath, s.keyPEM, 0o400); writeErr == nil {
				s.logger.Info("Migrated device key from provisioning.json to device-key.pem")
				// Rewrite provisioning.json without the now-migrated key.
				toSave := state
				toSave.KeyPEM = ""
				if saveData, marshalErr := json.MarshalIndent(toSave, "", "  "); marshalErr == nil {
					_ = os.WriteFile(s.statePath(), saveData, 0o600)
				}
			} else {
				s.logger.Warn("Failed to migrate device key to device-key.pem", zap.Error(writeErr))
			}
		}
	}

	// Ensure PEM files exist on disk (may have been lost during OTA update).
	if s.enrolled && len(s.keyPEM) > 0 && s.certPEM != "" {
		if err := s.writePEMFiles(string(s.keyPEM), s.certPEM, s.chainPEM); err != nil {
			s.logger.Warn("Failed to restore PEM files from provisioning state", zap.Error(err))
		}
	}
}

// loadOrGenerateKey returns the PEM-encoded private key for this device as []byte.
// It reuses the key at {configPath}/device-key.pem if it exists, otherwise
// generates a new one and persists it.
func (s *ProvisioningService) loadOrGenerateKey() ([]byte, error) {
	keyPath := filepath.Join(s.configPath, "device-key.pem")
	if data, err := os.ReadFile(keyPath); err == nil && len(data) > 0 {
		s.logger.Info("Reusing existing device key", zap.String("path", keyPath))
		return data, nil
	}

	keyStr, err := certs.GenerateKeyPair()
	if err != nil {
		return nil, err
	}
	keyPEM := []byte(keyStr)

	// Persist the key so it's reused on future provisioning.
	// 0o400: private key must be read-only after creation.
	if err := os.MkdirAll(s.configPath, 0o700); err == nil {
		_ = os.WriteFile(keyPath, keyPEM, 0o400)
	}

	return keyPEM, nil
}

func (s *ProvisioningService) writePEMFiles(keyPEM, certPEM, chainPEM string) error {
	return WritePEMFiles(s.configPath, keyPEM, certPEM, chainPEM)
}

// saveState writes provisioning state to disk.
// The private key (KeyPEM) is never included in provisioning.json; it lives
// exclusively in device-key.pem so that the JSON file can be shared or
// inspected without exposing key material.
func (s *ProvisioningService) saveState(state *provisioningState) error {
	if err := os.MkdirAll(s.configPath, 0o700); err != nil {
		return fmt.Errorf("creating config directory: %w", err)
	}

	// Shallow copy to ensure we never accidentally serialise a key.
	toSave := *state
	toSave.KeyPEM = ""

	data, err := json.MarshalIndent(toSave, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	return os.WriteFile(s.statePath(), data, 0o600)
}
