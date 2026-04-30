// local-pki-test exercises the end-to-end enrollment flow against a local
// pki-core wendy frontend running on :50051.
//
// Usage:
//
//	go run ./cmd/local-pki-test/ [--agent=:50053] [--cloud=localhost:50051] [--api-key=dev-secret-change-me]
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"log"
	"net"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"

	"github.com/wendylabsinc/wendy/internal/shared/certs"
	agentpb "github.com/wendylabsinc/wendy/proto/gen/agentpb"
	cloudpb "github.com/wendylabsinc/wendy/proto/gen/cloudpb"
)

func main() {
	agentAddr := flag.String("agent", "localhost:50053", "wendy-agent gRPC address")
	cloudAddr := flag.String("cloud", "localhost:50051", "pki-core wendy frontend address")
	apiKey := flag.String("api-key", "dev-secret-change-me", "Bearer API key for CreateAssetEnrollmentToken")
	deviceName := flag.String("name", "test-device", "device name")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// ── Step 1: create enrollment token ──────────────────────────────────────
	fmt.Printf("Connecting to pki-core wendy frontend at %s ...\n", *cloudAddr)
	cloudConn, err := grpc.NewClient(*cloudAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial cloud: %v", err)
	}
	defer cloudConn.Close()

	authCtx := metadata.NewOutgoingContext(ctx,
		metadata.Pairs("authorization", "Bearer "+*apiKey))

	certClient := cloudpb.NewCertificateServiceClient(cloudConn)
	tokenResp, err := certClient.CreateAssetEnrollmentToken(authCtx, &cloudpb.CreateAssetEnrollmentTokenRequest{
		OrganizationId: 1,
		Name:           *deviceName,
		TtlSeconds:     600,
	})
	if err != nil {
		log.Fatalf("CreateAssetEnrollmentToken: %v", err)
	}
	fmt.Printf("✓ enrollment token: %s (jti=%s, expires=%s)\n",
		tokenResp.GetEnrollmentToken()[:8]+"...",
		tokenResp.GetJti(),
		tokenResp.GetExpiresAt().AsTime().Format(time.RFC3339))

	// ── Step 2: provision the agent ──────────────────────────────────────────
	fmt.Printf("\nConnecting to wendy-agent at %s ...\n", *agentAddr)
	agentConn, err := grpc.NewClient(*agentAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		log.Fatalf("dial agent: %v", err)
	}
	defer agentConn.Close()

	// The agent's StartProvisioning dials cloudHost:50051, so pass only the host.
	cloudHost, _, err := net.SplitHostPort(*cloudAddr)
	if err != nil {
		cloudHost = *cloudAddr // no port — use as-is
	}

	provClient := agentpb.NewWendyProvisioningServiceClient(agentConn)
	_, err = provClient.StartProvisioning(ctx, &agentpb.StartProvisioningRequest{
		OrganizationId:  tokenResp.GetOrganizationId(),
		AssetId:         tokenResp.GetAssetId(),
		EnrollmentToken: tokenResp.GetEnrollmentToken(),
		CloudHost:       cloudHost,
	})
	if err != nil {
		log.Fatalf("StartProvisioning: %v", err)
	}
	fmt.Println("✓ provisioning complete")

	// ── Step 3: check IsProvisioned ───────────────────────────────────────────
	isProvResp, err := provClient.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
	if err != nil {
		log.Fatalf("IsProvisioned: %v", err)
	}

	switch r := isProvResp.GetResponse().(type) {
	case *agentpb.IsProvisionedResponse_Provisioned:
		fmt.Printf("✓ agent is provisioned (org=%d asset=%d cloud=%s)\n",
			r.Provisioned.GetOrganizationId(),
			r.Provisioned.GetAssetId(),
			r.Provisioned.GetCloudHost())
	case *agentpb.IsProvisionedResponse_NotProvisioned:
		fmt.Println("✗ agent reports not provisioned")
	}

	// ── Step 4: fetch CA bundle ───────────────────────────────────────────────
	bundleResp, err := certClient.GetCaBundle(ctx, &cloudpb.GetCaBundleRequest{})
	if err != nil {
		log.Fatalf("GetCaBundle: %v", err)
	}
	pemLen := len(bundleResp.GetPemBundle())
	fmt.Printf("✓ CA bundle received (%d bytes)\n", pemLen)

	// ── Step 5: issue a test-client certificate ───────────────────────────────
	// The mTLS server requires a client cert signed by the same cloud CA.
	// Issue one for the test runner using a fresh enrollment token.
	fmt.Println("\nIssuing test-client certificate for mTLS connection...")
	clientTokenResp, err := certClient.CreateAssetEnrollmentToken(authCtx, &cloudpb.CreateAssetEnrollmentTokenRequest{
		OrganizationId: 1,
		Name:           *deviceName + "-test-client",
		TtlSeconds:     600,
	})
	if err != nil {
		log.Fatalf("CreateAssetEnrollmentToken for test client: %v", err)
	}

	clientKeyPEM, err := certs.GenerateKeyPair()
	if err != nil {
		log.Fatalf("GenerateKeyPair: %v", err)
	}

	clientCSRPEM, err := certs.GenerateCSR(clientKeyPEM, "sh/wendy/test-client")
	if err != nil {
		log.Fatalf("GenerateCSR: %v", err)
	}

	issueResp, err := certClient.IssueCertificate(ctx, &cloudpb.IssueCertificateRequest{
		PemCsr:          clientCSRPEM,
		EnrollmentToken: clientTokenResp.GetEnrollmentToken(),
	})
	if err != nil {
		log.Fatalf("IssueCertificate for test client: %v", err)
	}
	if issueResp.GetError() != nil {
		log.Fatalf("IssueCertificate error: %s", issueResp.GetError().GetMessage())
	}
	clientCert := issueResp.GetCertificate()
	fmt.Println("✓ test-client certificate issued")

	// ── Step 6: connect to the mTLS port (agentPort + 1) ─────────────────────
	agentHost, agentPortStr, err := net.SplitHostPort(*agentAddr)
	if err != nil {
		log.Fatalf("parsing agent address: %v", err)
	}
	agentPortNum, err := strconv.Atoi(agentPortStr)
	if err != nil {
		log.Fatalf("parsing agent port: %v", err)
	}
	mtlsAddr := net.JoinHostPort(agentHost, strconv.Itoa(agentPortNum+1))
	fmt.Printf("\nConnecting to agent mTLS port at %s ...\n", mtlsAddr)

	tlsCfg, err := certs.LoadTLSConfig(
		clientCert.GetPemCertificate(),
		clientCert.GetPemCertificateChain(),
		clientKeyPEM,
		bundleResp.GetPemBundle(),
	)
	if err != nil {
		log.Fatalf("LoadTLSConfig: %v", err)
	}
	// Agent certs use sh/wendy/<org>/<asset> as CN, not the hostname.
	tlsCfg.InsecureSkipVerify = true //nolint:gosec
	tlsCfg.MinVersion = tls.VersionTLS12

	mtlsConn, err := grpc.NewClient(mtlsAddr, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg)))
	if err != nil {
		log.Fatalf("dial mTLS agent: %v", err)
	}
	defer mtlsConn.Close()

	// ── Step 7: verify IsProvisioned over mTLS ────────────────────────────────
	mtlsProvClient := agentpb.NewWendyProvisioningServiceClient(mtlsConn)
	mtlsProvResp, err := mtlsProvClient.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
	if err != nil {
		log.Fatalf("IsProvisioned over mTLS: %v", err)
	}
	switch r := mtlsProvResp.GetResponse().(type) {
	case *agentpb.IsProvisionedResponse_Provisioned:
		fmt.Printf("✓ mTLS: agent is provisioned (org=%d asset=%d cloud=%s)\n",
			r.Provisioned.GetOrganizationId(),
			r.Provisioned.GetAssetId(),
			r.Provisioned.GetCloudHost())
	case *agentpb.IsProvisionedResponse_NotProvisioned:
		log.Fatalf("✗ mTLS: agent reports not provisioned")
	}

	fmt.Println("\nEnd-to-end enrollment + mTLS flow succeeded.")
}
