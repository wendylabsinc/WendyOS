package services

import (
	"context"
	"net"
	"os"
	"testing"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	cloudpb "github.com/wendylabsinc/wendy/proto/gen/cloudpb"
)

// fakeCertService implements the CertificateService with a canned IssueCertificate response.
type fakeCertService struct {
	cloudpb.UnimplementedCertificateServiceServer
	certPEM  string
	chainPEM string
}

func (f *fakeCertService) IssueCertificate(_ context.Context, _ *cloudpb.IssueCertificateRequest) (*cloudpb.IssueCertificateResponse, error) {
	return &cloudpb.IssueCertificateResponse{
		Certificate: &cloudpb.Certificate{
			PemCertificate:      f.certPEM,
			PemCertificateChain: f.chainPEM,
		},
	}, nil
}

// startFakeCloudServer starts a gRPC server with the fake CertificateService and returns
// a CloudDialer that connects to it.
func startFakeCloudServer(t *testing.T, certPEM, chainPEM string) (CloudDialer, func()) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	srv := grpc.NewServer()
	cloudpb.RegisterCertificateServiceServer(srv, &fakeCertService{
		certPEM:  certPEM,
		chainPEM: chainPEM,
	})

	go srv.Serve(lis)

	dialer := func(_ context.Context, _ string) (*grpc.ClientConn, error) {
		return grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	cleanup := func() {
		srv.GracefulStop()
		lis.Close()
	}

	return dialer, cleanup
}

func newTestProvisioningService(t *testing.T) (*ProvisioningService, string) {
	t.Helper()
	tmpDir, err := os.MkdirTemp("", "wendy-prov-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}

	logger := zap.NewNop()
	svc := NewProvisioningService(logger, tmpDir)

	dialer, cleanup := startFakeCloudServer(t, "fake-cert-pem", "fake-chain-pem")
	t.Cleanup(cleanup)
	svc.CloudDialer = dialer

	return svc, tmpDir
}

func TestIsProvisioned_NotProvisioned(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "wendy-prov-test-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	logger := zap.NewNop()
	svc := NewProvisioningService(logger, tmpDir)

	resp, err := svc.IsProvisioned(context.Background(), &agentpb.IsProvisionedRequest{})
	if err != nil {
		t.Fatalf("IsProvisioned: %v", err)
	}

	np := resp.GetNotProvisioned()
	if np == nil {
		t.Fatal("expected NotProvisioned response")
	}
}

func TestIsProvisioned_Provisioned(t *testing.T) {
	svc, tmpDir := newTestProvisioningService(t)
	defer os.RemoveAll(tmpDir)

	// Provision first.
	_, err := svc.StartProvisioning(context.Background(), &agentpb.StartProvisioningRequest{
		OrganizationId: 42,
		CloudHost:      "cloud.wendy.io",
		AssetId:        100,
	})
	if err != nil {
		t.Fatalf("StartProvisioning: %v", err)
	}

	// Now check provisioned state.
	resp, err := svc.IsProvisioned(context.Background(), &agentpb.IsProvisionedRequest{})
	if err != nil {
		t.Fatalf("IsProvisioned: %v", err)
	}

	prov := resp.GetProvisioned()
	if prov == nil {
		t.Fatal("expected Provisioned response")
	}
	if prov.CloudHost != "cloud.wendy.io" {
		t.Errorf("CloudHost = %q; want cloud.wendy.io", prov.CloudHost)
	}
	if prov.OrganizationId != 42 {
		t.Errorf("OrgID = %d; want 42", prov.OrganizationId)
	}
	if prov.AssetId != 100 {
		t.Errorf("AssetID = %d; want 100", prov.AssetId)
	}
}

func TestStartProvisioning(t *testing.T) {
	svc, tmpDir := newTestProvisioningService(t)
	defer os.RemoveAll(tmpDir)

	_, err := svc.StartProvisioning(context.Background(), &agentpb.StartProvisioningRequest{
		OrganizationId: 1,
		CloudHost:      "test.wendy.io",
		AssetId:        10,
	})
	if err != nil {
		t.Fatalf("StartProvisioning: %v", err)
	}

	// Provisioning again should fail.
	_, err = svc.StartProvisioning(context.Background(), &agentpb.StartProvisioningRequest{
		OrganizationId: 2,
		CloudHost:      "test2.wendy.io",
		AssetId:        20,
	})
	if err == nil {
		t.Fatal("expected error when already provisioned")
	}
}

func TestStartProvisioning_PersistAndReload(t *testing.T) {
	svc, tmpDir := newTestProvisioningService(t)
	defer os.RemoveAll(tmpDir)

	// Provision.
	_, err := svc.StartProvisioning(context.Background(), &agentpb.StartProvisioningRequest{
		OrganizationId: 5,
		CloudHost:      "persist.wendy.io",
		AssetId:        55,
	})
	if err != nil {
		t.Fatalf("StartProvisioning: %v", err)
	}

	// Create a new service instance that loads from disk.
	logger := zap.NewNop()
	svc2 := NewProvisioningService(logger, tmpDir)
	resp, err := svc2.IsProvisioned(context.Background(), &agentpb.IsProvisionedRequest{})
	if err != nil {
		t.Fatalf("IsProvisioned: %v", err)
	}

	prov := resp.GetProvisioned()
	if prov == nil {
		t.Fatal("expected provisioned after reload")
	}
	if prov.OrganizationId != 5 {
		t.Errorf("OrgID = %d; want 5", prov.OrganizationId)
	}
	if prov.AssetId != 55 {
		t.Errorf("AssetID = %d; want 55", prov.AssetId)
	}

	// Verify certs were persisted and reloaded.
	certPEM, chainPEM, keyPEM := svc2.ProvisioningCerts()
	if certPEM != "fake-cert-pem" {
		t.Errorf("CertPEM = %q; want fake-cert-pem", certPEM)
	}
	if chainPEM != "fake-chain-pem" {
		t.Errorf("ChainPEM = %q; want fake-chain-pem", chainPEM)
	}
	if keyPEM == "" {
		t.Error("KeyPEM should not be empty after provisioning")
	}
}

func TestStartProvisioning_OnProvisionedCallback(t *testing.T) {
	svc, tmpDir := newTestProvisioningService(t)
	defer os.RemoveAll(tmpDir)

	var callbackCert, callbackChain, callbackKey string
	svc.OnProvisioned = func(certPEM, chainPEM, keyPEM string) {
		callbackCert = certPEM
		callbackChain = chainPEM
		callbackKey = keyPEM
	}

	_, err := svc.StartProvisioning(context.Background(), &agentpb.StartProvisioningRequest{
		OrganizationId: 1,
		CloudHost:      "callback.wendy.io",
		AssetId:        10,
	})
	if err != nil {
		t.Fatalf("StartProvisioning: %v", err)
	}

	if callbackCert != "fake-cert-pem" {
		t.Errorf("callback certPEM = %q; want fake-cert-pem", callbackCert)
	}
	if callbackChain != "fake-chain-pem" {
		t.Errorf("callback chainPEM = %q; want fake-chain-pem", callbackChain)
	}
	if callbackKey == "" {
		t.Error("callback keyPEM should not be empty")
	}
}
