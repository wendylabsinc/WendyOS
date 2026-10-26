package commands

import (
	"context"
	"crypto/mldsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	legacyagentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

const deploymentTenant = "2558fd76-afc7-466e-9613-6b715296a526"

func deploymentV2Device() *agentpb.ProvisionedResponse {
	// Numeric IDs and asset UUID are intentionally unset: neither is part of
	// v2 app registration, and the roster may not have synced yet.
	return &agentpb.ProvisionedResponse{CloudHost: "cloud.example:443", PrincipalUri: "spiffe://wendy.sh/tenant/" + deploymentTenant + "/device/019535d9-3df7-79fb-b466-fa907fa17f9e"}
}

func TestDeploymentAuthV2ScopesOperatorToDeviceTenantAndHost(t *testing.T) {
	operator := "spiffe://wendy.sh/tenant/" + deploymentTenant + "/operator/test"
	for _, principal := range []string{operator, "spiffe://wendy.sh/tenant/" + deploymentTenant + "/service/user-test"} {
		cfg := &config.Config{Auth: []config.AuthConfig{
			{CloudGRPC: "other.example:443", Certificates: []config.CertificateInfo{{PrincipalURI: principal}}},
			{CloudGRPC: "cloud.example:443", Certificates: []config.CertificateInfo{
				{UserID: "legacy", OrganizationID: 0},
				{PrincipalURI: "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/other"},
				{PrincipalURI: deploymentV2Device().PrincipalUri},
				{PrincipalURI: principal},
			}},
		}}
		auth, err := deploymentAuth(cfg, deploymentV2Device())
		if err != nil || auth.CloudGRPC != "cloud.example:443" || len(auth.Certificates) != 1 || auth.Certificates[0].PrincipalURI != principal {
			t.Fatalf("wrong operator selection: %v, %v", auth, err)
		}
		cfg.Auth[1].Certificates = cfg.Auth[1].Certificates[:3]
		if _, err := deploymentAuth(cfg, deploymentV2Device()); err == nil {
			t.Fatal("accepted wrong endpoint, wrong tenant, device, or zero-ID legacy session")
		}
	}
	for _, principal := range []string{
		"", "invalid", operator, "urn:wendy:org:42:asset:9",
		"spiffe://wendy.sh/tenant/not-a-uuid/device/test",
		"spiffe://wendy.sh/tenant//device/test",
	} {
		device := deploymentV2Device()
		device.PrincipalUri = principal
		if _, err := deploymentAuth(registrationConfig(), device); err == nil {
			t.Fatalf("accepted incomplete or non-device enrollment %q", principal)
		}
	}
	// Imported operator sessions can recover their principal from the leaf SAN.
	key := testKey(t)
	leaf := liteLeafPEM(t, key, operator, time.Now().Add(time.Hour), false)
	cfg := &config.Config{Auth: []config.AuthConfig{{CloudGRPC: "cloud.example:443", Certificates: []config.CertificateInfo{{PemCertificate: leaf}}}}}
	auth, err := deploymentAuth(cfg, deploymentV2Device())
	if err != nil || auth.Certificates[0].PrincipalURI != operator {
		t.Fatalf("failed to recover stored operator principal: %v %v", auth, err)
	}
}

// Legacy fixture also used by build-host deployment tests.
type deploymentProvisioningStub struct {
	legacyagentpb.WendyProvisioningServiceClient
	response *legacyagentpb.IsProvisionedResponse
	err      error
}

func (s deploymentProvisioningStub) IsProvisioned(context.Context, *legacyagentpb.IsProvisionedRequest, ...grpc.CallOption) (*legacyagentpb.IsProvisionedResponse, error) {
	return s.response, s.err
}

type deploymentLegacyProvisioningStub struct {
	legacyagentpb.WendyProvisioningServiceClient
	response *legacyagentpb.IsProvisionedResponse
	err      error
	calls    int
}

func (s *deploymentLegacyProvisioningStub) IsProvisioned(context.Context, *legacyagentpb.IsProvisionedRequest, ...grpc.CallOption) (*legacyagentpb.IsProvisionedResponse, error) {
	s.calls++
	return s.response, s.err
}

type deploymentV2Server struct {
	agentpb.UnimplementedWendyProvisioningServiceServer
	cloudpb.UnimplementedAppServiceServer
	mu        sync.Mutex
	device    *agentpb.ProvisionedResponse
	stateErr  error
	appErr    error
	wrongApp  bool
	key       *mldsa.PublicKey
	apps      map[string]*cloudpb.App
	mutations int
}

func (s *deploymentV2Server) IsProvisioned(context.Context, *agentpb.IsProvisionedRequest) (*agentpb.IsProvisionedResponse, error) {
	return &agentpb.IsProvisionedResponse{ResponseType: &agentpb.IsProvisionedResponse_Provisioned{Provisioned: s.device}}, s.stateErr
}

func (s *deploymentV2Server) GetApp(_ context.Context, req *cloudpb.GetAppRequest) (*cloudpb.App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if req.GetOrganizationId() != deploymentTenant {
		return nil, status.Error(codes.PermissionDenied, "wrong tenant")
	}
	if s.appErr != nil {
		return nil, s.appErr
	}
	if s.wrongApp {
		return &cloudpb.App{Id: req.GetId(), OrganizationId: "11111111-1111-4111-8111-111111111111"}, nil
	}
	if app := s.apps[req.GetId()]; app != nil {
		return app, nil
	}
	return nil, status.Error(codes.NotFound, "missing")
}

func (s *deploymentV2Server) UpsertApp(_ context.Context, envelope *cloudpb.SignedRequest) (*cloudpb.App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var req cloudpb.UpsertAppRequest
	if envelope.GetPayloadType() != "wendycloud.v2.UpsertAppRequest" || proto.Unmarshal(envelope.GetPayload(), &req) != nil {
		return nil, status.Error(codes.InvalidArgument, "wrong signed payload")
	}
	parts := strings.Split(string(envelope.GetSignature()), ".")
	if len(parts) != 3 {
		return nil, status.Error(codes.Unauthenticated, "missing JWS")
	}
	sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
	if err := mldsa.Verify(s.key, []byte(parts[0]+"."+parts[1]), sig, nil); err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid operator signature")
	}
	raw, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		Operation  string                            `json:"operation"`
		BodyDigest string                            `json:"body_sha256"`
		Target     struct{ Tenant, Resource string } `json:"target"`
	}
	digest := sha256.Sum256(envelope.GetPayload())
	if err := json.Unmarshal(raw, &claims); err != nil ||
		claims.Operation != "wendycloud.v2.AppService/UpsertApp" ||
		claims.Target.Tenant != deploymentTenant || req.GetOrganizationId() != deploymentTenant ||
		claims.Target.Resource != "org/"+deploymentTenant+"/app/"+req.GetId() ||
		claims.BodyDigest != base64.RawURLEncoding.EncodeToString(digest[:]) {
		return nil, status.Error(codes.PermissionDenied, "wrong signature binding")
	}
	s.mutations++
	app := &cloudpb.App{Id: req.GetId(), OrganizationId: req.GetOrganizationId(), Name: req.GetName()}
	s.apps[req.GetId()] = app
	return app, nil
}

func deploymentTestConnection(t *testing.T, srv *deploymentV2Server, legacy *deploymentLegacyProvisioningStub) *grpcclient.AgentConnection {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	agentpb.RegisterWendyProvisioningServiceServer(server, srv)
	cloudpb.RegisterAppServiceServer(server, srv)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	conn, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if srv.device != nil {
		srv.device.CloudHost = listener.Addr().String()
	}
	return &grpcclient.AgentConnection{Conn: conn, ProvisioningService: legacy}
}

func TestCloudRegistrationV2BeforeRosterSyncUsesSignedOperatorRequest(t *testing.T) {
	key := testKey(t)
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	principal := "spiffe://wendy.sh/tenant/" + deploymentTenant + "/operator/test"
	cert := config.CertificateInfo{PrincipalURI: principal,
		PemCertificate: liteLeafPEM(t, key, principal, time.Now().Add(time.Hour), false),
		PemPrivateKey:  string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})),
	}
	legacy := &deploymentLegacyProvisioningStub{err: fmt.Errorf("must not use v1")}
	srv := &deploymentV2Server{device: deploymentV2Device(), key: key.Public().(*mldsa.PublicKey), apps: map[string]*cloudpb.App{
		"existing": {Id: "existing", OrganizationId: deploymentTenant, Name: "operator metadata", CanSendNotifications: true},
	}}
	conn := deploymentTLSConnection(t, srv, legacy, srv.device.PrincipalUri)
	conn.CertInfo = &cert
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{{CloudGRPC: srv.device.CloudHost, Certificates: []config.CertificateInfo{cert}}}})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := registerCloudApps(ctx, conn, []string{"app", "campaign:people", "app", "existing"}, false); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	if srv.mutations != 2 || legacy.calls != 0 || srv.apps["campaign:people"].Name != "people" || srv.apps["existing"].Name != "operator metadata" || !srv.apps["existing"].CanSendNotifications {
		t.Fatal("wrong mutation count, fallback, campaign name, or existing app metadata/grant changed")
	}
	srv.appErr = status.Error(codes.PermissionDenied, "viewer")
	srv.mu.Unlock()
	if err := registerCloudApps(ctx, conn, []string{"denied"}, false); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("catalog denial hidden: %v", err)
	}
	srv.mu.Lock()
	srv.appErr = nil
	srv.wrongApp = true
	srv.mu.Unlock()
	if err := registerCloudApps(ctx, conn, []string{"wrong"}, false); err == nil || !strings.Contains(err.Error(), "different app identity") {
		t.Fatalf("accepted cross-tenant catalog result: %v", err)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.mutations != 2 {
		t.Fatal("mutated Cloud after catalog denial or identity mismatch")
	}
}

func TestDeploymentProvisioningFallsBackOnlyForUnimplementedV2(t *testing.T) {
	for _, code := range []codes.Code{codes.Unimplemented, codes.Unavailable, codes.OK} {
		t.Run(code.String(), func(t *testing.T) {
			legacy := &deploymentLegacyProvisioningStub{response: &legacyagentpb.IsProvisionedResponse{Response: &legacyagentpb.IsProvisionedResponse_Provisioned{
				Provisioned: &legacyagentpb.ProvisionedResponse{CloudHost: "legacy", OrganizationId: 42, AssetId: 9},
			}}}
			srv := &deploymentV2Server{stateErr: status.Error(code, "test"), device: deploymentV2Device()}
			conn := deploymentTestConnection(t, srv, legacy)
			if code == codes.OK {
				conn = deploymentTLSConnection(t, srv, legacy, srv.device.PrincipalUri)
			}
			if code == codes.Unimplemented {
				conn = deploymentTLSConnection(t, srv, legacy, "urn:wendy:org:42:asset:9")
			}
			response, err := (deploymentProvisioningService{conn}).IsProvisioned(context.Background(), &agentpb.IsProvisionedRequest{})
			if code == codes.Unimplemented {
				if err != nil || legacy.calls != 1 || response.GetProvisioned().GetAssetId() != 9 {
					t.Fatalf("legacy fallback failed: %v %v", response, err)
				}
			} else if legacy.calls != 0 || status.Code(err) != code {
				t.Fatalf("unexpected fallback or failure: calls=%d err=%v", legacy.calls, err)
			}
		})
	}
}
