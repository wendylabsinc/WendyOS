package commands

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/internal/shared/wendyconf"
	cloudpb "github.com/wendylabsinc/wendy/proto/gen/cloudpb"
)

func TestProvisioningRequired(t *testing.T) {
	// Empty inputs: no provisioning data, agent download is the only thing
	// that would have happened, and a config-partition write failure can
	// safely degrade to a warning.
	if provisioningRequired(nil, "", nil) {
		t.Error("provisioningRequired(nil, \"\", nil) = true; want false")
	}

	cred := []wendyconf.WifiCredential{{SSID: "Home", Password: "x"}}
	cases := []struct {
		name             string
		creds            []wendyconf.WifiCredential
		deviceName       string
		provisioningJSON []byte
	}{
		{"creds only", cred, "", nil},
		{"deviceName only", nil, "brave-dolphin", nil},
		{"provisioningJSON only", nil, "", []byte(`{"enrolled":true}`)},
		{"all three", cred, "brave-dolphin", []byte(`{"enrolled":true}`)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !provisioningRequired(c.creds, c.deviceName, c.provisioningJSON) {
				t.Errorf("provisioningRequired(%v, %q, %v) = false; want true (user-supplied data must not be silently dropped)", c.creds, c.deviceName, c.provisioningJSON)
			}
		})
	}
}

type fakePreEnrollCertService struct {
	cloudpb.UnimplementedCertificateServiceServer
	orgID     int32
	assetID   int32
	token     string
	certPEM   string
	chainPEM  string
	tokenErr  error
	issueErr  error
	emptyCert bool
}

func (f *fakePreEnrollCertService) CreateAssetEnrollmentToken(_ context.Context, _ *cloudpb.CreateAssetEnrollmentTokenRequest) (*cloudpb.CreateAssetEnrollmentTokenResponse, error) {
	if f.tokenErr != nil {
		return nil, f.tokenErr
	}
	return &cloudpb.CreateAssetEnrollmentTokenResponse{
		OrganizationId:  f.orgID,
		AssetId:         f.assetID,
		EnrollmentToken: f.token,
	}, nil
}

func (f *fakePreEnrollCertService) IssueCertificate(_ context.Context, _ *cloudpb.IssueCertificateRequest) (*cloudpb.IssueCertificateResponse, error) {
	if f.issueErr != nil {
		return nil, f.issueErr
	}
	if f.emptyCert {
		return &cloudpb.IssueCertificateResponse{}, nil
	}
	return &cloudpb.IssueCertificateResponse{
		Certificate: &cloudpb.Certificate{
			PemCertificate:      f.certPEM,
			PemCertificateChain: f.chainPEM,
		},
	}, nil
}

// startPreEnrollFakeServer starts a local gRPC server backed by svc and returns
// a PreEnrollDialer that ignores the addr/opt arguments and connects to it directly.
func startPreEnrollFakeServer(t *testing.T, svc *fakePreEnrollCertService) PreEnrollDialer {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	cloudpb.RegisterCertificateServiceServer(srv, svc)
	go srv.Serve(lis) //nolint:errcheck
	t.Cleanup(func() { srv.GracefulStop(); lis.Close() })

	addr := lis.Addr().String()
	return func(_ context.Context, _ string, _ grpc.DialOption) (*grpc.ClientConn, error) {
		return grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}
}

// generateSelfSignedCert returns a minimal valid PEM cert and key for testing.
func generateSelfSignedCert(t *testing.T) (certPEM, keyPEM string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	keyPEM = string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}))

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	certBytes, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certBytes}))
	return
}

func fakeAuth(t *testing.T) *config.AuthConfig {
	certPEM, keyPEM := generateSelfSignedCert(t)
	return &config.AuthConfig{
		CloudGRPC: "localhost:9999",
		Certificates: []config.CertificateInfo{
			{
				PemCertificate:      certPEM,
				PemCertificateChain: certPEM,
				PemPrivateKey:       keyPEM,
				OrganizationID:      7,
			},
		},
	}
}

func TestPreEnrollDevice_Success(t *testing.T) {
	dialer := startPreEnrollFakeServer(t, &fakePreEnrollCertService{
		orgID:    7,
		assetID:  42,
		token:    "tok",
		certPEM:  "device-cert",
		chainPEM: "ca-chain",
	})

	data, err := preEnrollDevice(context.Background(), fakeAuth(t), "my-device", dialer)
	if err != nil {
		t.Fatalf("preEnrollDevice: %v", err)
	}

	var state preProvisionedState
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !state.Enrolled {
		t.Error("enrolled should be true")
	}
	if state.OrgID != 7 {
		t.Errorf("orgId = %d; want 7", state.OrgID)
	}
	if state.AssetID != 42 {
		t.Errorf("assetId = %d; want 42", state.AssetID)
	}
	if state.CertPEM != "device-cert" {
		t.Errorf("certPem = %q; want device-cert", state.CertPEM)
	}
	if state.ChainPEM != "ca-chain" {
		t.Errorf("chainPem = %q; want ca-chain", state.ChainPEM)
	}
	if state.KeyPEM == "" {
		t.Error("keyPem must not be empty")
	}
	if state.CloudHost == "" {
		t.Error("cloudHost must not be empty")
	}
}

func TestPreEnrollDevice_WritesFile(t *testing.T) {
	dialer := startPreEnrollFakeServer(t, &fakePreEnrollCertService{
		orgID: 1, assetID: 1, token: "t", certPEM: "c", chainPEM: "ch",
	})

	data, err := preEnrollDevice(context.Background(), fakeAuth(t), "", dialer)
	if err != nil {
		t.Fatalf("preEnrollDevice: %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "provisioning.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o; want 0600", info.Mode().Perm())
	}
}

func TestPreEnrollDevice_NoAuthCerts(t *testing.T) {
	auth := &config.AuthConfig{CloudGRPC: "localhost:9999", Certificates: nil}
	_, err := preEnrollDevice(context.Background(), auth, "", nil)
	if err == nil {
		t.Fatal("expected error with no auth certificates")
	}
}

func TestPreEnrollDevice_TokenError(t *testing.T) {
	dialer := startPreEnrollFakeServer(t, &fakePreEnrollCertService{
		tokenErr: fmt.Errorf("token denied"),
	})
	_, err := preEnrollDevice(context.Background(), fakeAuth(t), "", dialer)
	if err == nil {
		t.Fatal("expected error when token creation fails")
	}
}

func TestPreEnrollDevice_IssueError(t *testing.T) {
	dialer := startPreEnrollFakeServer(t, &fakePreEnrollCertService{
		orgID: 1, assetID: 1, token: "t",
		issueErr: fmt.Errorf("issuance rejected"),
	})
	_, err := preEnrollDevice(context.Background(), fakeAuth(t), "", dialer)
	if err == nil {
		t.Fatal("expected error when certificate issuance fails")
	}
}

func TestPreEnrollDevice_EmptyCert(t *testing.T) {
	dialer := startPreEnrollFakeServer(t, &fakePreEnrollCertService{
		orgID: 1, assetID: 1, token: "t",
		emptyCert: true,
	})
	_, err := preEnrollDevice(context.Background(), fakeAuth(t), "", dialer)
	if err == nil {
		t.Fatal("expected error when cloud returns empty certificate")
	}
}
