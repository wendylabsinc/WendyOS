package services

import (
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

// fakeCertService implements the CertificateService with a canned IssueCertificate response.
type fakeCertService struct {
	cloudpb.UnimplementedCertificateServiceServer
	certPEM  string
	chainPEM string

	mu     sync.Mutex
	gotCSR string // the last CSR received, for SAN assertions
}

func (f *fakeCertService) lastCSR() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gotCSR
}

func (f *fakeCertService) IssueCertificate(_ context.Context, req *cloudpb.IssueCertificateRequest) (*cloudpb.IssueCertificateResponse, error) {
	f.mu.Lock()
	f.gotCSR = req.GetPemCsr()
	f.mu.Unlock()
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

func TestUnprovision_NotProvisioned(t *testing.T) {
	svc, tmpDir := newTestProvisioningService(t)
	defer os.RemoveAll(tmpDir)

	if _, err := svc.Unprovision(context.Background(), &agentpb.UnprovisionRequest{}); err == nil {
		t.Fatal("expected error when unprovisioning a device that is not provisioned")
	}
}

func TestUnprovision_ClearsStateAndFiles(t *testing.T) {
	svc, tmpDir := newTestProvisioningService(t)
	defer os.RemoveAll(tmpDir)

	if _, err := svc.StartProvisioning(context.Background(), &agentpb.StartProvisioningRequest{
		OrganizationId: 7,
		CloudHost:      "unprov.wendy.io",
		AssetId:        70,
	}); err != nil {
		t.Fatalf("StartProvisioning: %v", err)
	}

	// All on-disk artifacts should exist after provisioning.
	stateFiles := []string{"provisioning.json", "device-key.pem", "device.pem", "ca.pem", ".provisioned"}
	for _, f := range stateFiles {
		if _, err := os.Stat(filepath.Join(tmpDir, f)); err != nil {
			t.Fatalf("expected %s to exist after provisioning: %v", f, err)
		}
	}

	// OnUnprovisioned fires asynchronously after the response; capture it.
	called := make(chan struct{}, 1)
	svc.OnUnprovisioned = func() { called <- struct{}{} }

	if _, err := svc.Unprovision(context.Background(), &agentpb.UnprovisionRequest{}); err != nil {
		t.Fatalf("Unprovision: %v", err)
	}

	// All artifacts should be gone.
	for _, f := range stateFiles {
		if _, err := os.Stat(filepath.Join(tmpDir, f)); !os.IsNotExist(err) {
			t.Errorf("expected %s to be removed after unprovision (err=%v)", f, err)
		}
	}

	// In-memory state should report not provisioned.
	resp, err := svc.IsProvisioned(context.Background(), &agentpb.IsProvisionedRequest{})
	if err != nil {
		t.Fatalf("IsProvisioned: %v", err)
	}
	if resp.GetNotProvisioned() == nil {
		t.Error("expected NotProvisioned after unprovision")
	}

	select {
	case <-called:
	case <-time.After(3 * time.Second):
		t.Error("OnUnprovisioned callback was not invoked")
	}

	// A subsequent unprovision should fail (already cleared).
	if _, err := svc.Unprovision(context.Background(), &agentpb.UnprovisionRequest{}); err == nil {
		t.Error("expected error when unprovisioning an already-unprovisioned device")
	}
}

func TestCertificateServiceAddr(t *testing.T) {
	tests := []struct {
		name      string
		cloudHost string
		want      string
	}{
		{
			// WDY-2799: a port-less host used to become <host>:50051, which the
			// old port heuristic then dialled in cleartext.
			name:      "host without port uses the TLS port",
			cloudHost: "test.wendy.io",
			want:      "test.wendy.io:443",
		},
		{
			name:      "cloud run endpoint keeps explicit tls port",
			cloudHost: "wendy-cloud-services-114319063177.us-central1.run.app:443",
			want:      "wendy-cloud-services-114319063177.us-central1.run.app:443",
		},
		{
			name:      "local endpoint keeps explicit port",
			cloudHost: "localhost:50051",
			want:      "localhost:50051",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := certificateServiceAddr(tt.cloudHost); got != tt.want {
				t.Fatalf("certificateServiceAddr(%q) = %q, want %q", tt.cloudHost, got, tt.want)
			}
		})
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
	certPEM, chainPEM, keyData := svc2.ProvisioningCerts()
	if certPEM != "fake-cert-pem" {
		t.Errorf("CertPEM = %q; want fake-cert-pem", certPEM)
	}
	if chainPEM != "fake-chain-pem" {
		t.Errorf("ChainPEM = %q; want fake-chain-pem", chainPEM)
	}
	if len(keyData) == 0 {
		t.Error("KeyPEM should not be empty after provisioning")
	}
}

func TestStartProvisioning_OnProvisionedCallback(t *testing.T) {
	svc, tmpDir := newTestProvisioningService(t)
	defer os.RemoveAll(tmpDir)

	var callbackCert, callbackChain string
	var callbackKey []byte
	svc.OnProvisioned = func(certPEM, chainPEM string, keyData []byte) {
		callbackCert = certPEM
		callbackChain = chainPEM
		callbackKey = keyData
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
	if len(callbackKey) == 0 {
		t.Error("callback keyData should not be empty")
	}
}

// TestDefaultCloudDialerRequiresTLS is the regression guard for WDY-2799: the
// enrollment dial must not fall back to plaintext because of how the address
// is spelled. It points the real DefaultCloudDialer at a plaintext server on a
// non-443 port — the exact shape that used to downgrade — and asserts the RPC
// only succeeds once the operator has explicitly opted out via
// WENDY_CLOUD_INSECURE.
func TestDefaultCloudDialerRequiresTLS(t *testing.T) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer lis.Close()

	srv := grpc.NewServer()
	cloudpb.RegisterCertificateServiceServer(srv, &fakeCertService{
		certPEM:  "fake-cert-pem",
		chainPEM: "fake-chain-pem",
	})
	go srv.Serve(lis)
	defer srv.GracefulStop()

	// issue makes a real RPC so the transport actually handshakes; grpc.NewClient
	// alone connects lazily and would pass regardless of the credentials.
	issue := func(t *testing.T) error {
		t.Helper()
		conn, err := DefaultCloudDialer(context.Background(), lis.Addr().String())
		if err != nil {
			return err
		}
		defer conn.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err = cloudpb.NewCertificateServiceClient(conn).IssueCertificate(ctx,
			&cloudpb.IssueCertificateRequest{})
		return err
	}

	t.Run("plaintext server is refused by default", func(t *testing.T) {
		if err := issue(t); err == nil {
			t.Fatal("DefaultCloudDialer reached a plaintext server without WENDY_CLOUD_INSECURE; " +
				"the enrollment token would be sent in cleartext")
		}
	})

	t.Run("explicit opt-out allows plaintext", func(t *testing.T) {
		t.Setenv(cloudInsecureEnv, "1")
		if err := issue(t); err != nil {
			t.Fatalf("with %s=1 the plaintext dial should succeed, got: %v", cloudInsecureEnv, err)
		}
	})

	t.Run("unset and non-true values keep TLS", func(t *testing.T) {
		for _, v := range []string{"", "0", "false", "no", "maybe"} {
			t.Setenv(cloudInsecureEnv, v)
			if cloudDialInsecure() {
				t.Fatalf("%s=%q must not disable TLS", cloudInsecureEnv, v)
			}
		}
	})
}

// provisioningTokenFor builds an enrollment token shaped like cloud's, with an
// optional tenant_uuid claim (WDY-2584). Only the payload segment is decoded by
// the agent, so header and signature are placeholders.
func provisioningTokenFor(t *testing.T, orgID, assetID int32, tenantUUID string) string {
	t.Helper()
	tenant := ""
	if tenantUUID != "" {
		tenant = fmt.Sprintf(`"tenant_uuid":%q,`, tenantUUID)
	}
	payload := fmt.Sprintf(`{"org_id":%d,"asset_id":%d,%s"type":"asset_enrollment"}`,
		orgID, assetID, tenant)
	return "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".sig"
}

// csrURIs returns the URI SANs and DNS SANs of a PEM-encoded CSR.
func csrURIs(t *testing.T, csrPEM string) (uris, dnsNames []string) {
	t.Helper()
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil {
		t.Fatalf("CSR is not valid PEM: %q", csrPEM)
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificateRequest: %v", err)
	}
	for _, u := range csr.URIs {
		uris = append(uris, u.String())
	}
	return uris, csr.DNSNames
}

// TestStartProvisioningCSRCarriesTenantSPIFFESAN covers the agent half of
// WDY-2498: cloud refuses to sign a relay grant unless the CSR carries the
// tenant SPIFFE principal, and pki-core refuses the mint outright if the CSR
// carries any DNS SAN.
func TestStartProvisioningCSRCarriesTenantSPIFFESAN(t *testing.T) {
	const tenant = "13a72725-dfe3-4425-bd04-b253d2036089"

	newSvcWithFake := func(t *testing.T) (*ProvisioningService, *fakeCertService) {
		t.Helper()
		tmpDir := t.TempDir()

		lis, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		fake := &fakeCertService{certPEM: "fake-cert-pem", chainPEM: "fake-chain-pem"}
		srv := grpc.NewServer()
		cloudpb.RegisterCertificateServiceServer(srv, fake)
		go srv.Serve(lis)
		t.Cleanup(func() { srv.GracefulStop(); lis.Close() })

		svc := NewProvisioningService(zap.NewNop(), tmpDir)
		svc.CloudDialer = func(_ context.Context, _ string) (*grpc.ClientConn, error) {
			return grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
		}
		return svc, fake
	}

	t.Run("token with tenant_uuid adds the SPIFFE SAN alongside the urn", func(t *testing.T) {
		svc, fake := newSvcWithFake(t)
		if _, err := svc.StartProvisioning(context.Background(), &agentpb.StartProvisioningRequest{
			OrganizationId:  7,
			AssetId:         42,
			CloudHost:       "cloud.wendy.io",
			EnrollmentToken: provisioningTokenFor(t, 7, 42, tenant),
		}); err != nil {
			t.Fatalf("StartProvisioning: %v", err)
		}

		uris, dnsNames := csrURIs(t, fake.lastCSR())
		want := []string{
			"urn:wendy:org:7:asset:42",
			"spiffe://wendy.sh/tenant/" + tenant + "/service/asset-42",
		}
		if len(uris) != len(want) {
			t.Fatalf("CSR URI SANs = %q, want %q", uris, want)
		}
		for i := range want {
			if uris[i] != want[i] {
				t.Errorf("CSR URI SAN %d = %q, want %q", i, uris[i], want[i])
			}
		}
		// pki-core's service-identity profile checks every dNSName against the
		// tenant allow-list, so any DNS SAN at all fails the mint.
		if len(dnsNames) != 0 {
			t.Errorf("CSR must be URI-SAN-only, got DNS SANs %q", dnsNames)
		}
	})

	t.Run("token without tenant_uuid keeps the pre-tenant CSR", func(t *testing.T) {
		svc, fake := newSvcWithFake(t)
		if _, err := svc.StartProvisioning(context.Background(), &agentpb.StartProvisioningRequest{
			OrganizationId:  7,
			AssetId:         42,
			CloudHost:       "cloud.wendy.io",
			EnrollmentToken: provisioningTokenFor(t, 7, 42, ""),
		}); err != nil {
			t.Fatalf("StartProvisioning must not fail when the org has no pki tenant: %v", err)
		}

		uris, _ := csrURIs(t, fake.lastCSR())
		if len(uris) != 1 || uris[0] != "urn:wendy:org:7:asset:42" {
			t.Fatalf("CSR URI SANs = %q, want only [urn:wendy:org:7:asset:42]", uris)
		}
	})
}

// TestStartProvisioningWithStagedACMENeverDialsTheRelay is the regression that
// matters most about the two enrollment legs: a staged credential means the
// device enrolls itself against pki-core, and silently falling back to the
// cloud relay would defeat the whole point while still looking like success.
func TestStartProvisioningWithStagedACMENeverDialsTheRelay(t *testing.T) {
	svc, tmpDir := newTestProvisioningService(t)
	defer os.RemoveAll(tmpDir)

	dialed := false
	svc.CloudDialer = func(ctx context.Context, addr string) (*grpc.ClientConn, error) {
		dialed = true
		return nil, fmt.Errorf("the relay must not be dialled for a staged ACME credential")
	}

	// The directory is unreachable, so this fails — that is fine. What is
	// asserted is which path it took, and that the operator is told the
	// credential is gone.
	_, err := svc.StartProvisioning(context.Background(), &agentpb.StartProvisioningRequest{
		Acme: &agentpb.AcmeEnrollment{
			DirectoryUrl: "https://127.0.0.1:1/acme/directory",
			DeviceId:     "box-01",
			EabKeyId:     "11111111-1111-1111-1111-111111111111",
			EabHmacKey:   "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff",
		},
	})
	if err == nil {
		t.Fatal("StartProvisioning succeeded against an unreachable ACME directory")
	}
	if dialed {
		t.Error("the cloud relay was dialled even though an ACME credential was staged")
	}
	if !strings.Contains(err.Error(), "single-use") {
		t.Errorf("error does not tell the operator the credential is spent: %v", err)
	}
	if _, _, _, enrolled := svc.ProvisioningInfo(); enrolled {
		t.Error("a failed ACME enrollment left the agent marked as provisioned")
	}
}
