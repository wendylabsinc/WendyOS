package mcp

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const testV2Tenant = "2558fd76-afc7-466e-9613-6b715296a526"

// fakeAssetV2Server streams a fixed asset set from the v2 AssetService. It
// returns the online set under OnlineOnly, the full set otherwise, so the
// offline re-query path can be exercised.
type fakeAssetV2Server struct {
	cloudpbv2.UnimplementedAssetServiceServer
	online []*cloudpbv2.Asset
	all    []*cloudpbv2.Asset
}

func (s *fakeAssetV2Server) ListAssets(req *cloudpbv2.ListAssetsRequest, stream cloudpbv2.AssetService_ListAssetsServer) error {
	set := s.all
	if req.GetOnlineOnly() {
		set = s.online
	}
	for _, a := range set {
		if err := stream.Send(&cloudpbv2.ListAssetsResponse{Asset: a, Total: int32(len(set))}); err != nil {
			return err
		}
	}
	return nil
}

// v2TestAuth builds an operator session whose SPIFFE principal resolves to a
// tenant UUID (so isV2Session is true) with a real ML-DSA credential, which the
// cloud dial's signer and the tunnel principal signer both need.
func v2TestAuth(t *testing.T, cloudGRPC string) *config.AuthConfig {
	t.Helper()
	keyPEM, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	principal, err := url.Parse("spiffe://wendy.sh/tenant/" + testV2Tenant + "/operator/op-1")
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "operator"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		URIs:         []*url.URL{principal},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return &config.AuthConfig{
		CloudGRPC: cloudGRPC,
		APIKey:    "test-access-token",
		Certificates: []config.CertificateInfo{{
			PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
			PemPrivateKey:  keyPEM,
			PrincipalURI:   principal.String(),
		}},
	}
}

func startFakeAssetV2Server(t *testing.T, svc *fakeAssetV2Server) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	cloudpbv2.RegisterAssetServiceServer(srv, svc)
	go srv.Serve(lis) //nolint:errcheck
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func TestResolveCloudDeviceV2UsesUUID(t *testing.T) {
	const deviceUUID = "11111111-1111-4111-8111-111111111111"
	addr := startFakeAssetV2Server(t, &fakeAssetV2Server{
		online: []*cloudpbv2.Asset{{Id: deviceUUID, Name: "edge-one", IsComputeDevice: true}},
	})
	auth := v2TestAuth(t, addr)
	s := New(&config.Config{Auth: []config.AuthConfig{*auth}}, nil)

	device, err := s.resolveCloudDevice(context.Background(), auth, "edge-one")
	if err != nil {
		t.Fatalf("resolveCloudDevice: %v", err)
	}
	if !device.isV2 {
		t.Fatal("v2 session did not resolve on the v2 arm")
	}
	if device.key != deviceUUID {
		t.Fatalf("device key = %q, want the asset UUID %q", device.key, deviceUUID)
	}
	if device.legacyID != 0 {
		t.Fatalf("v2 device carried an int32 asset id %d", device.legacyID)
	}
	if device.GetName() != "edge-one" {
		t.Fatalf("device name = %q", device.GetName())
	}
}

// TestCloudTunnelV2UsesLiveRelay proves the v2 tunnel dials the live
// wendycloud.tunnel.v2 relay, never the retired wendycloud.v2.TunnelBrokerService
// (WDY-3119). It captures the first dialed method against an unknown-service
// handler; the RPC itself failing afterward is fine.
func TestCloudTunnelV2UsesLiveRelay(t *testing.T) {
	for _, protocol := range []string{"tcp", "udp"} {
		t.Run(protocol, func(t *testing.T) { testCloudTunnelV2UsesLiveRelay(t, protocol) })
	}
}
func testCloudTunnelV2UsesLiveRelay(t *testing.T, protocol string) {
	var gotMethod string
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer(grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
		gotMethod, _ = grpc.MethodFromServerStream(stream)
		return nil
	}))
	go srv.Serve(lis) //nolint:errcheck
	t.Cleanup(srv.Stop)

	auth := v2TestAuth(t, lis.Addr().String())
	device := mcpCloudDevice{name: "edge-one", key: "11111111-1111-4111-8111-111111111111", isV2: true}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if protocol == "tcp" {
		_, _ = device.openTunnel(ctx, nil, auth, 8080)
	} else {
		_, _ = device.openDatagram(ctx, nil, auth, 5353)
	} // error is expected; we only inspect the dialed method

	if !strings.HasPrefix(gotMethod, "/wendycloud.tunnel.v2.") {
		t.Fatalf("v2 tunnel dialed %q, want the live wendycloud.tunnel.v2 relay", gotMethod)
	}
	if strings.Contains(gotMethod, "TunnelBrokerService") {
		t.Fatalf("v2 tunnel dialed the retired relay: %q", gotMethod)
	}
}

// TestAgentRPCPingSessionMeasuresRTT proves a v2 ping measures RTT with one
// GetAgentVersion round-trip per echo: every send yields a reply.
func TestAgentRPCPingSessionMeasuresRTT(t *testing.T) {
	conn, _ := startFakeAgentServer(t, &fakeAgentServer{versionResp: &agentpb.GetAgentVersionResponse{Version: "test"}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stats := mcpRunPingLoop(ctx, newMCPAgentRPCPingSession(ctx, conn), "edge-one", 3, 10*time.Millisecond, io.Discard)
	if stats.Sent != 3 || stats.Received != 3 {
		t.Fatalf("sent/received = %d/%d, want 3/3 (err=%v)", stats.Sent, stats.Received, stats.Err)
	}
	if got := mcpPingResult(stats, "edge-one"); got.IsError {
		t.Fatalf("healthy ping mapped to an error result: %+v", got)
	}
}

// TestAgentRPCPingSessionSurfacesAgentError proves an unreachable agent yields
// zero replies and the transport error is surfaced (not the silent-device hint).
func TestAgentRPCPingSessionSurfacesAgentError(t *testing.T) {
	conn, _ := startFakeAgentServer(t, &fakeAgentServer{versionErr: status.Error(codes.PermissionDenied, "denied")})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stats := mcpRunPingLoop(ctx, newMCPAgentRPCPingSession(ctx, conn), "edge-one", 3, 10*time.Millisecond, io.Discard)
	if stats.Received != 0 || stats.Err == nil {
		t.Fatalf("received=%d err=%v, want 0 replies with a transport error", stats.Received, stats.Err)
	}
	res := mcpPingResult(stats, "edge-one")
	if !res.IsError || !strings.Contains(toolResultText(t, res), "denied") {
		t.Fatalf("agent error should surface in the result; got %+v", res)
	}
}

func TestMCPTunnelServiceMapping(t *testing.T) {
	for _, tc := range []struct {
		port    uint32
		want    string
		wantErr bool
	}{
		{50052, "wendy-agent", false},
		{22, "ssh", false},
		{8080, "tcp-8080", false},
		{65535, "tcp-65535", false},
		{0, "", true},
		{65536, "", true},
	} {
		got, err := mcpTunnelService(tc.port)
		if tc.wantErr {
			if err == nil {
				t.Errorf("port %d should be refused", tc.port)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("mcpTunnelService(%d) = %q, %v; want %q", tc.port, got, err, tc.want)
		}
	}
}
