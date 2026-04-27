package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/wendylabsinc/wendy/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/proto/gen/cloudpb"
)

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

func fakeAuth() *config.AuthConfig {
	return &config.AuthConfig{
		CloudGRPC: "localhost:9999",
		Certificates: []config.CertificateInfo{
			{
				PemCertificate:      "fake-cert",
				PemCertificateChain: "fake-chain",
				PemPrivateKey:       "fake-key",
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

	data, err := preEnrollDevice(context.Background(), fakeAuth(), "my-device", dialer)
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

	data, err := preEnrollDevice(context.Background(), fakeAuth(), "", dialer)
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
	_, err := preEnrollDevice(context.Background(), fakeAuth(), "", dialer)
	if err == nil {
		t.Fatal("expected error when token creation fails")
	}
}

func TestPreEnrollDevice_IssueError(t *testing.T) {
	dialer := startPreEnrollFakeServer(t, &fakePreEnrollCertService{
		orgID: 1, assetID: 1, token: "t",
		issueErr: fmt.Errorf("issuance rejected"),
	})
	_, err := preEnrollDevice(context.Background(), fakeAuth(), "", dialer)
	if err == nil {
		t.Fatal("expected error when certificate issuance fails")
	}
}

func TestPreEnrollDevice_EmptyCert(t *testing.T) {
	dialer := startPreEnrollFakeServer(t, &fakePreEnrollCertService{
		orgID: 1, assetID: 1, token: "t",
		emptyCert: true,
	})
	_, err := preEnrollDevice(context.Background(), fakeAuth(), "", dialer)
	if err == nil {
		t.Fatal("expected error when cloud returns empty certificate")
	}
}
