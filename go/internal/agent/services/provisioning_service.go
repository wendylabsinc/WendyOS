package services

import (
	"bytes"
	"context"
	"crypto/mldsa"
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

	"github.com/wendylabsinc/wendy/go/internal/agent/timesync"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/enrolltoken"
	"github.com/wendylabsinc/wendy/go/internal/shared/unenrollproof"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

// provisioningState is persisted to disk at configPath/provisioning.json.
// NOTE: KeyPEM is retained only for one-time migration of existing deployments;
// new writes never populate it — the private key lives exclusively in
// device-key.pem (mode 0o600) and is never written to provisioning.json.
type provisioningState struct {
	Enrolled         bool               `json:"enrolled"`
	CloudHost        string             `json:"cloudHost,omitempty"`
	OrgID            int32              `json:"orgId,omitempty"`
	AssetID          int32              `json:"assetId,omitempty"`
	KeyPEM           string             `json:"keyPem,omitempty"` // read-only: migration only; never written
	CertPEM          string             `json:"certPem,omitempty"`
	ChainPEM         string             `json:"chainPem,omitempty"`
	PrincipalURI     string             `json:"principalURI,omitempty"`
	ACMEDirectoryURL string             `json:"acmeDirectoryURL,omitempty"`
	Unenrollment     *unenrollmentState `json:"unenrollment,omitempty"`
	LocalReset       *localResetState   `json:"localReset,omitempty"`
}

// Public recovery evidence lives in the existing provisioning state, never
// alongside it in additional persistent files. Pending survives key cleanup;
// completed is written only after all credential removals are durable.
type unenrollmentState struct {
	Status  string `json:"status"`
	Receipt []byte `json:"receipt"`
}

const (
	unenrollmentPending   = "pending"
	unenrollmentCompleted = "completed"
)

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

// BeforeUnprovisionedFunc runs synchronously after enrollment state is cleared
// and before the RPC reports success, so dependent device-local credentials are
// revoked first. An error means credentials are already erased but local cleanup
// is incomplete. The restart callback still runs so startup can retry cleanup.
type BeforeUnprovisionedFunc func() error

// ProvisioningService implements agentpb.WendyProvisioningServiceServer.
type ProvisioningService struct {
	agentpb.UnimplementedWendyProvisioningServiceServer
	logger         *zap.Logger
	configPath     string
	mu             sync.Mutex
	unenrollmentMu sync.Mutex
	// lifecycleMu orders publication of dependent enrollment resources with
	// reset. Callbacks may reenter read APIs, so it must precede mu and must
	// never be acquired while mu is held.
	lifecycleMu         sync.Mutex
	enrolled            bool
	resetRecoveryErr    error
	resetCleanupErr     error
	resetRestartPending bool
	trustedTime         func(context.Context) (timesync.TimeWindow, error)
	verifyCloudReset    func(context.Context, []byte, string, string, string, []byte, *x509.Certificate, time.Time, time.Time) ([]byte, error)
	cloudHost           string
	orgID               int32
	assetID             int32
	keyPEM              []byte // stored as []byte so it can be zeroed on rotation/shutdown
	certPEM             string
	chainPEM            string
	principalURI        string
	acmeDirectoryURL    string
	CloudDialer         CloudDialer
	OnProvisioned       OnProvisionedFunc
	OnUnprovisioned     OnUnprovisionedFunc
	BeforeUnprovisioned BeforeUnprovisionedFunc
}

func NewProvisioningService(logger *zap.Logger, configPath string, clock ...*timesync.Manager) *ProvisioningService {
	svc := &ProvisioningService{
		logger:           logger,
		configPath:       configPath,
		CloudDialer:      DefaultCloudDialer,
		verifyCloudReset: unenrollproof.VerifyCloudReset,
	}
	if len(clock) > 0 && clock[0] != nil {
		svc.trustedTime = clock[0].FreshTime
	}
	svc.resetRecoveryErr = svc.recoverProvisioningReset()
	if svc.resetRecoveryErr == nil {
		svc.loadState()
	} else {
		logger.Error("Enrollment state recovery failed", zap.Error(svc.resetRecoveryErr))
	}
	return svc
}

// SetResetCleanupError gates enrollment and completion receipts while the agent
// removes resources from a completed reset. Clearing a transient cleanup error
// never clears a failure to verify the durable reset authorization itself.
func (s *ProvisioningService) SetResetCleanupError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resetCleanupErr = err
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

// ProvisioningPrincipal returns the PKI identity, empty for legacy enrollment.
func (s *ProvisioningService) ProvisioningPrincipal() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.principalURI
}

// IsProvisioned checks whether the agent is enrolled with a cloud organization.
func (s *ProvisioningService) IsProvisioned(_ context.Context, _ *agentpb.IsProvisionedRequest) (*agentpb.IsProvisionedResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.resetRecoveryErr != nil {
		return nil, status.Error(codes.Unavailable, "authorized reset recovery incomplete")
	}
	if s.resetCleanupErr != nil {
		return nil, status.Error(codes.Unavailable, "reset resource cleanup incomplete; agent recovery is retrying")
	}
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

	if s.resetRecoveryErr != nil {
		return nil, status.Error(codes.FailedPrecondition, "authorized reset recovery must finish before enrollment")
	}
	if s.resetCleanupErr != nil {
		return nil, status.Error(codes.FailedPrecondition, "reset resource cleanup must finish before enrollment")
	}
	if s.resetRestartPending {
		return nil, status.Error(codes.FailedPrecondition, "agent must restart after reset before enrollment")
	}
	if s.enrolled {
		return nil, status.Error(codes.FailedPrecondition, "agent is already provisioned")
	}

	s.logger.Info("Starting provisioning",
		zap.Int32("org_id", req.GetOrganizationId()),
		zap.String("cloud_host", req.GetCloudHost()),
		zap.Int32("asset_id", req.GetAssetId()),
	)

	// Reuse the device's existing private key if present, otherwise generate a
	// new one. The legacy Cloud issuer predates ML-DSA device keys, so this path
	// stays P-256; pki-core enrollments use ML-DSA-65 (provisioning_acme.go).
	keyPEM, err := s.loadOrGenerateKey(false)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to load or generate key pair: %v", err)
	}

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
		return nil, status.Errorf(codes.Internal, "failed to generate CSR: %v", err)
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
	complete, err := s.persistProvisioning(state, keyPEM)
	if err != nil {
		return nil, err
	}
	locked = false
	s.mu.Unlock()
	complete()
	return &agentpb.StartProvisioningResponse{}, nil
}

// persistProvisioning runs with mu held. The returned callback must run after
// releasing mu, and only after the complete state is durable.
func (s *ProvisioningService) persistProvisioning(state *provisioningState, keyPEM []byte) (func(), error) {
	if err := WriteDeviceKey(s.configPath, keyPEM); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to save device key: %v", err)
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
	s.principalURI = state.PrincipalURI
	s.acmeDirectoryURL = state.ACMEDirectoryURL
	s.keyPEM = keyPEM
	s.certPEM = state.CertPEM
	s.chainPEM = state.ChainPEM

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
	return func() {
		if cb != nil {
			s.lifecycleMu.Lock()
			defer s.lifecycleMu.Unlock()
			// A reset may win the gap between persistence and this callback.
			// Never republish resources for an enrollment it already erased.
			s.mu.Lock()
			current := s.enrolled && s.cloudHost == state.CloudHost && s.orgID == state.OrgID &&
				s.assetID == state.AssetID && s.principalURI == state.PrincipalURI &&
				s.certPEM == state.CertPEM && s.chainPEM == state.ChainPEM && bytes.Equal(s.keyPEM, cbKeyPEM)
			s.mu.Unlock()
			if !current {
				clear(cbKeyPEM)
				return
			}
			cb(state.CertPEM, state.ChainPEM, cbKeyPEM)
		}
	}, nil
}

// Unprovision resets the device to an unprovisioned state. It deletes the
// stored enrollment certificates and provisioning state from disk, clears the
// in-memory state, and (if configured) invokes OnUnprovisioned shortly after
// the response is sent so the agent can revert its mDNS advertisement and
// restart into plaintext mode.
func (s *ProvisioningService) Unprovision(_ context.Context, _ *agentpb.UnprovisionRequest) (*agentpb.UnprovisionResponse, error) {
	return s.unprovision("", "")
}

func (s *ProvisioningService) unprovision(expectedPrincipal, expectedFingerprint string) (*agentpb.UnprovisionResponse, error) {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
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

	// A direct PKI identity is erased only under a protected pending record:
	// Cloud-signed unenrollment, or an operator-requested local-only reset.
	localReset := false
	if s.principalURI != "" {
		if expectedPrincipal == "" || expectedPrincipal != s.principalURI {
			return nil, status.Error(codes.FailedPrecondition, "direct PKI reset requires the exact expected principal through v2")
		}
		localReset = s.localResetAuthorizedLocked()
		if expectedFingerprint == "" || expectedFingerprint != s.certificateFingerprintLocked() || !(localReset || s.cloudResetAuthorizedLocked()) {
			return nil, status.Error(codes.FailedPrecondition, "verify Cloud unenrollment and installed-leaf PKI revocation before clearing enrollment keys")
		}
	}

	s.logger.Info("Unprovisioning device",
		zap.Int32("org_id", s.orgID),
		zap.Int32("asset_id", s.assetID),
	)

	var cleanupErr error
	switch {
	case localReset:
		cleanupErr = s.finishLocalReset()
	case s.principalURI != "":
		cleanupErr = s.finishCloudReset()
	default:
		cleanupErr = s.clearStateFiles()
	}
	if err := cleanupErr; err != nil {
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
	s.principalURI = ""
	s.keyPEM = nil
	s.certPEM = ""
	s.chainPEM = ""

	s.logger.Info("Device unprovisioned; agent will restart into unprovisioned mode")

	before, cb := s.BeforeUnprovisioned, s.OnUnprovisioned
	// Keep enrollment closed during the response-flush delay. Otherwise a new
	// enrollment could consume a one-time credential just before process exit.
	s.resetRestartPending = cb != nil
	if before != nil {
		// Gate status and new enrollment before releasing the lock: the durable
		// completion already exists, but its dependent resources are not gone.
		s.resetCleanupErr = fmt.Errorf("reset resource cleanup pending")
	}
	locked = false
	s.mu.Unlock()

	if before != nil {
		cleanupErr = before()
		s.SetResetCleanupError(cleanupErr)
	}

	if cb != nil {
		// Invoke asynchronously after a short delay so the RPC response is
		// flushed to the client before the agent restarts. Mirrors the agent
		// update and reboot flows.
		go func() {
			time.Sleep(500 * time.Millisecond)
			cb()
		}()
	}

	if cleanupErr != nil {
		s.logger.Error("Enrollment credentials erased but resource cleanup failed", zap.Error(cleanupErr))
		return nil, status.Error(codes.Unavailable, "enrollment credentials erased; resource cleanup incomplete, restart will retry; reset completion unconfirmed")
	}
	return &agentpb.UnprovisionResponse{}, nil
}

// clearStateFiles removes all on-disk provisioning artifacts: the state file,
// the device private key, and legacy certificate copies and enrollment marker.
// A missing file is not treated as an error.
func (s *ProvisioningService) clearStateFiles() error {
	if err := os.Remove(s.statePath()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing provisioning state: %w", err)
	}
	return s.clearCredentialFiles()
}

// PKI reset keeps provisioning.json as its durable recovery record while
// removing the same credentials and markers as the numeric legacy path.
func (s *ProvisioningService) clearCredentialFiles() error {
	files := []string{
		filepath.Join(s.configPath, "device-key.pem"),
		filepath.Join(s.configPath, pendingDeviceKeyFile),
		filepath.Join(s.configPath, "device.pem"),
		filepath.Join(s.configPath, "ca.pem"),
		filepath.Join(s.configPath, ".provisioned"),
		filepath.Join(s.configPath, "acme-account-key.pem"),
	}
	for _, f := range files {
		if err := os.Remove(f); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing %s: %w", f, err)
		}
	}
	if err := os.RemoveAll(filepath.Join(s.configPath, "cloud-relay")); err != nil {
		return err
	}
	if err := os.RemoveAll(filepath.Join(s.configPath, "acme-account-key.pem.d")); err != nil {
		return fmt.Errorf("removing scoped ACME account keys: %w", err)
	}
	return nil
}

func (s *ProvisioningService) statePath() string {
	return filepath.Join(s.configPath, "provisioning.json")
}

// loadState loads provisioning state from disk.
// The private key is always read from device-key.pem (mode 0o600). If that
// file is absent but the legacy provisioning.json contains a KeyPEM entry, the
// key is migrated to device-key.pem and removed from provisioning.json so that
// subsequent reads use the dedicated file.
func (s *ProvisioningService) loadState() {
	defer s.finishInterruptedRenewal() // also drops a pending key on an unenrolled device
	data, err := os.ReadFile(s.statePath())
	// Like readProvisioningState, treat an empty file as no enrollment.
	if err != nil || len(data) == 0 {
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
	s.principalURI = state.PrincipalURI
	s.acmeDirectoryURL = state.ACMEDirectoryURL
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
		} else {
			if readErr == nil {
				readErr = fmt.Errorf("device key file is empty")
			}
			s.logger.Error("Stored enrollment has no usable private key; authenticated services cannot start",
				zap.String("path", keyPath), zap.Error(readErr))
		}
		if s.certPEM == "" {
			s.logger.Error("Stored enrollment has no certificate; authenticated services cannot start",
				zap.String("path", s.statePath()))
		}
	}

	if !s.enrolled || len(s.keyPEM) == 0 {
		return
	}
	// Preserve correct files and repair permissions without emitting unused
	// certificate copies. Persist a legacy key before removing it from JSON.
	if err := WriteDeviceKey(s.configPath, s.keyPEM); err != nil {
		s.logger.Warn("Failed to persist device key", zap.Error(err))
		return
	}
	// Keep the original JSON bytes on ordinary startup, including formatting
	// and fields written by a newer agent, while repairing file permissions.
	if err := WriteProvisioningState(s.configPath, data); err != nil {
		s.logger.Warn("Failed to repair provisioning state file", zap.Error(err))
	}
}

// loadOrGenerateKey returns the PEM-encoded private key for this device as []byte.
// It reuses the key at {configPath}/device-key.pem if it exists and is
// ML-DSA-65 exactly when mldsaKey asks for it; otherwise (a leftover of a failed
// enrollment on the other path) it generates a new one and persists it. Only
// unenrolled devices get here, so replacing a key strands no certificate.
func (s *ProvisioningService) loadOrGenerateKey(mldsaKey bool) ([]byte, error) {
	keyPath := filepath.Join(s.configPath, "device-key.pem")
	if data, err := os.ReadFile(keyPath); err == nil && len(data) > 0 {
		if isMLDSA65Key(data) == mldsaKey {
			if err := WriteDeviceKey(s.configPath, data); err != nil {
				return nil, err
			}
			s.logger.Info("Reusing existing device key", zap.String("path", keyPath), zap.Bool("ml_dsa_65", mldsaKey))
			return data, nil
		}
		s.logger.Info("Replacing leftover device key of the other algorithm", zap.String("path", keyPath), zap.Bool("ml_dsa_65", mldsaKey))
	}

	generate := certs.GenerateKeyPair
	if mldsaKey {
		generate = certs.GenerateMLDSAKeyPair
	}
	keyStr, err := generate()
	if err != nil {
		return nil, err
	}
	keyPEM := []byte(keyStr)

	// Do not begin enrollment unless the key has been persisted successfully.
	if err := WriteDeviceKey(s.configPath, keyPEM); err != nil {
		return nil, err
	}

	return keyPEM, nil
}

func isMLDSA65Key(keyPEM []byte) bool {
	signer, err := certs.ParseSigningPrivateKeyPEM(keyPEM)
	key, ok := signer.(*mldsa.PrivateKey)
	return err == nil && ok && key.PublicKey().Parameters() == mldsa.MLDSA65()
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

	return WriteProvisioningState(s.configPath, data)
}
