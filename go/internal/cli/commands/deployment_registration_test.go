package commands

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func registrationDevice() *agentpbv2.ProvisionedResponse {
	return &agentpbv2.ProvisionedResponse{CloudHost: "cloud.example:443", OrganizationId: 42, AssetId: 9}
}

// A PKI-enrolled device: tenant in principal_uri, numeric IDs left at 0.
func registrationTenantDevice() *agentpbv2.ProvisionedResponse {
	return &agentpbv2.ProvisionedResponse{
		CloudHost:    "cloud.example:443",
		PrincipalUri: "spiffe://wendy.sh/tenant/" + testOperatorTenant + "/device/thor",
	}
}

func registrationProvisioned(device *agentpbv2.ProvisionedResponse) *agentpbv2.IsProvisionedResponse {
	return &agentpbv2.IsProvisionedResponse{ResponseType: &agentpbv2.IsProvisionedResponse_Provisioned{Provisioned: device}}
}

func registrationConfig() *config.Config {
	return &config.Config{Auth: []config.AuthConfig{{CloudGRPC: "cloud.example:443", Certificates: []config.CertificateInfo{
		{OrganizationID: 99, UserID: "other"},
		{OrganizationID: 42, UserID: "operator"},
	}}}}
}

func TestDeploymentAuthUsesOnlyDeviceOrganizationAndTrustedHost(t *testing.T) {
	auth, err := deploymentAuth(registrationConfig(), registrationDevice())
	if err != nil || len(auth.Certificates) != 1 || auth.Certificates[0].UserID != "operator" {
		t.Fatalf("wrong operator selection: %v, %v", auth, err)
	}
	for _, modify := range []func(*agentpbv2.ProvisionedResponse){
		func(d *agentpbv2.ProvisionedResponse) { d.CloudHost = "attacker.example:443" },
		func(d *agentpbv2.ProvisionedResponse) { d.OrganizationId = 100 },
		func(d *agentpbv2.ProvisionedResponse) { d.AssetId = 0 },
	} {
		device := registrationDevice()
		modify(device)
		if _, err := deploymentAuth(registrationConfig(), device); err == nil {
			t.Fatal("accepted unmatched or incomplete enrollment")
		}
	}
	cfg := registrationConfig()
	cfg.Auth[0].Certificates = []config.CertificateInfo{{OrganizationID: 42, AssetID: 9}}
	if _, err := deploymentAuth(cfg, registrationDevice()); err == nil {
		t.Fatal("used device credentials for operator registration")
	}
}

func TestRegistrationSkipsUnenrolledDevicesWithoutLoadingCredentials(t *testing.T) {
	unenrolled := &agentpbv2.IsProvisionedResponse{ResponseType: &agentpbv2.IsProvisionedResponse_NotProvisioned{
		NotProvisioned: &agentpbv2.NotProvisionedResponse{},
	}}
	err := registerDeviceApps(context.Background(), unenrolled, []string{"app"}, func() (*config.Config, error) {
		t.Fatal("local deployment loaded Cloud credentials")
		return nil, nil
	}, func(context.Context, *config.AuthConfig, *agentpbv2.ProvisionedResponse, []string) error {
		t.Fatal("local deployment contacted Cloud")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRegistrationDeduplicatesAppsAndPropagatesCloudFailure(t *testing.T) {
	device := registrationDevice()
	denied := status.Error(codes.PermissionDenied, "viewer cannot register deployments")
	called := false
	err := registerDeviceApps(context.Background(), registrationProvisioned(device), []string{"app", "app", "campaign:people"}, func() (*config.Config, error) {
		return registrationConfig(), nil
	}, func(_ context.Context, auth *config.AuthConfig, got *agentpbv2.ProvisionedResponse, apps []string) error {
		called = true
		if got != device || auth.Certificates[0].OrganizationID != 42 || !reflect.DeepEqual(apps, []string{"app", "campaign:people"}) {
			t.Fatalf("wrong registration: %v %v", got, apps)
		}
		return denied
	})
	if !called || !errors.Is(err, denied) {
		t.Fatalf("failure hidden: %v", err)
	}
}

func TestRunStopsBeforeBuildingWhenRegistrationFails(t *testing.T) {
	// Nothing listens on a closed listener's port, so the enrollment check fails.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener.Close()
	agent, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.Close() })
	conn := &grpcclient.AgentConnection{Conn: agent}
	err = runWithAgent(context.Background(), conn, t.TempDir(), &appconfig.AppConfig{AppID: "app"}, runOptions{})
	if err == nil || !strings.Contains(err.Error(), "registering deployment with Cloud") || !strings.Contains(err.Error(), "--skip-cloud-registration") {
		t.Fatalf("registration did not stop deployment: %v", err)
	}
}

func TestSkipCloudRegistrationDoesNotContactDevice(t *testing.T) {
	if err := registerCloudApps(context.Background(), nil, []string{"app"}, true); err != nil {
		t.Fatal(err)
	}
	if newRunCmd().Flags().Lookup("skip-cloud-registration") == nil || newDataCampaignDeployCmd().Flags().Lookup("skip-cloud-registration") == nil {
		t.Fatal("offline option missing")
	}
}

type deploymentCloudServer struct {
	cloudpb.UnimplementedAppServiceServer
	mu       sync.Mutex
	requests []*cloudpb.UpsertAppRequest
	apps     map[string]*cloudpb.App
	err      error
}

func (s *deploymentCloudServer) GetApp(ctx context.Context, request *cloudpb.GetAppRequest) (*cloudpb.App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	md, _ := metadata.FromIncomingContext(ctx)
	if len(md.Get("x-wendy-client-cert")) == 0 {
		return nil, status.Error(codes.Unauthenticated, "missing operator metadata")
	}
	if s.err != nil {
		return nil, s.err
	}
	if app := s.apps[request.GetId()]; app != nil {
		return app, nil
	}
	return nil, status.Error(codes.NotFound, "app not found")
}

func (s *deploymentCloudServer) UpsertApp(ctx context.Context, request *cloudpb.UpsertAppRequest) (*cloudpb.App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	md, _ := metadata.FromIncomingContext(ctx)
	if len(md.Get("x-wendy-client-cert")) == 0 {
		return nil, status.Error(codes.Unauthenticated, "missing operator metadata")
	}
	s.requests = append(s.requests, request)
	app := &cloudpb.App{Id: request.GetId(), OrganizationId: request.GetOrganizationId(), Name: request.GetName()}
	s.apps[request.GetId()] = app
	return app, nil
}

func TestCloudRegistrationUsesExistingCatalogRPCs(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	implementation := &deploymentCloudServer{apps: make(map[string]*cloudpb.App)}
	cloudpb.RegisterAppServiceServer(server, implementation)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	cfg := registrationConfig()
	auth := cfg.Auth[0]
	auth.CloudGRPC = listener.Addr().String()
	auth.Certificates = auth.Certificates[1:]
	device := registrationDevice()
	device.CloudHost = auth.CloudGRPC
	for _, id := range []string{"app", "campaign:people", "app", "campaign:people"} {
		if err := registerAppsWithCloud(context.Background(), &auth, device, []string{id}); err != nil {
			t.Fatal(err)
		}
	}
	implementation.mu.Lock()
	if len(implementation.requests) != 2 {
		t.Fatalf("upsert calls = %d; existing apps should be preserved", len(implementation.requests))
	}
	for i, req := range implementation.requests {
		if req.GetOrganizationId() != 42 || req.GetId() != []string{"app", "campaign:people"}[i] || req.GetName() != []string{"app", "people"}[i] {
			t.Fatalf("wrong Cloud catalog registration: %v", req)
		}
	}
	implementation.err = status.Error(codes.PermissionDenied, "not an organization member")
	implementation.mu.Unlock()
	if err := registerAppsWithCloud(context.Background(), &auth, device, []string{"new-app"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("catalog access failure hidden: %v", err)
	}
	implementation.mu.Lock()
	defer implementation.mu.Unlock()
	if len(implementation.requests) != 2 {
		t.Fatal("upsert attempted after a catalog access denial")
	}
}

// tenantOperatorSession is a PKI operator session in testOperatorTenant: an
// ML-DSA leaf and key, no numeric IDs and no UserID, as OIDC login stores it.
func tenantOperatorSession(t *testing.T, host string) config.AuthConfig {
	t.Helper()
	keyPEM, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		t.Fatal(err)
	}
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
	if err != nil {
		t.Fatal(err)
	}
	return config.AuthConfig{CloudGRPC: host, Certificates: []config.CertificateInfo{{
		PemCertificate: testLeafPEM(t, key), PemPrivateKey: keyPEM,
		PrincipalURI: "spiffe://wendy.sh/tenant/" + testOperatorTenant + "/operator/" + testOperatorSubject,
	}}}
}

func TestDeploymentAuthMatchesTenantDeviceToOperatorSession(t *testing.T) {
	device := registrationTenantDevice()
	deviceCert := config.CertificateInfo{PrincipalURI: device.GetPrincipalUri()}
	otherTenant := config.CertificateInfo{PrincipalURI: "spiffe://wendy.sh/tenant/11111111-2222-4333-8444-555555555555/operator/x"}
	operator := tenantOperatorSession(t, device.GetCloudHost())
	operator.Certificates = append([]config.CertificateInfo{deviceCert, otherTenant}, operator.Certificates...)
	cfg := &config.Config{Auth: []config.AuthConfig{operator}}
	auth, err := deploymentAuth(cfg, device)
	if err != nil || len(auth.Certificates) != 1 || auth.OrganizationKey() != testOperatorTenant || !strings.Contains(auth.Certificates[0].PrincipalURI, "/operator/") {
		t.Fatalf("wrong operator selection: %v, %v", auth, err)
	}
	moved := registrationTenantDevice()
	moved.CloudHost = "attacker.example:443"
	if _, err := deploymentAuth(cfg, moved); err == nil {
		t.Fatal("device chose the Cloud host for an operator session")
	}
}

func TestRegistrationOfTenantDeviceWithoutSessionNamesLogin(t *testing.T) {
	cfg := registrationConfig() // numeric sessions only: none can serve a tenant device
	err := registerDeviceApps(context.Background(), registrationProvisioned(registrationTenantDevice()), []string{"app"},
		func() (*config.Config, error) { return cfg, nil },
		func(context.Context, *config.AuthConfig, *agentpbv2.ProvisionedResponse, []string) error {
			t.Fatal("registered without an operator session")
			return nil
		})
	if err == nil || !strings.Contains(err.Error(), testOperatorTenant) || !strings.Contains(err.Error(), "cloud.example:443") || !strings.Contains(err.Error(), "wendy auth login") {
		t.Fatalf("missing login hint: %v", err)
	}
}

type tenantCloudServer struct {
	cloudpbv2.UnimplementedAppServiceServer
	mu      sync.Mutex
	upserts []*cloudpbv2.UpsertAppRequest
	gets    []*cloudpbv2.GetAppRequest
	apps    map[string]*cloudpbv2.App
}

func (s *tenantCloudServer) GetApp(_ context.Context, request *cloudpbv2.GetAppRequest) (*cloudpbv2.App, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets = append(s.gets, request)
	if app := s.apps[request.GetId()]; app != nil {
		return app, nil
	}
	return nil, status.Error(codes.NotFound, "app not found")
}

func (s *tenantCloudServer) UpsertApp(_ context.Context, signed *cloudpbv2.SignedRequest) (*cloudpbv2.App, error) {
	if signed.GetPayloadType() != "wendycloud.v2.UpsertAppRequest" || len(signed.GetSignature()) == 0 {
		return nil, status.Error(codes.PermissionDenied, "unsigned upsert")
	}
	request := &cloudpbv2.UpsertAppRequest{}
	if err := proto.Unmarshal(signed.GetPayload(), request); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	// Cloud's AppServiceHandler checks this exact operation and resource.
	var claims struct {
		Operation string `json:"operation"`
		Target    struct {
			Resource string `json:"resource"`
		} `json:"target"`
	}
	parts := strings.Split(string(signed.GetSignature()), ".")
	raw, _ := base64.RawURLEncoding.DecodeString(parts[min(1, len(parts)-1)])
	if json.Unmarshal(raw, &claims) != nil || claims.Operation != "wendycloud.v2.AppService/UpsertApp" ||
		claims.Target.Resource != "org/"+request.GetOrganizationId()+"/app/"+request.GetId() {
		return nil, status.Error(codes.PermissionDenied, "signature does not cover this upsert")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upserts = append(s.upserts, request)
	app := &cloudpbv2.App{Id: request.GetId(), OrganizationId: request.GetOrganizationId(), Name: request.GetName()}
	s.apps[request.GetId()] = app
	return app, nil
}

func TestTenantDeviceRegistersWithCloudV2(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	cloud := &tenantCloudServer{apps: make(map[string]*cloudpbv2.App)}
	cloudpbv2.RegisterAppServiceServer(server, cloud)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	device := registrationTenantDevice()
	device.CloudHost = listener.Addr().String()
	cfg := &config.Config{Auth: []config.AuthConfig{tenantOperatorSession(t, device.CloudHost)}}
	for range 2 {
		if err := registerDeviceApps(context.Background(), registrationProvisioned(device), []string{"campaign:people"},
			func() (*config.Config, error) { return cfg, nil }, registerAppsWithCloud); err != nil {
			t.Fatal(err)
		}
	}
	cloud.mu.Lock()
	defer cloud.mu.Unlock()
	if len(cloud.upserts) != 1 || len(cloud.gets) != 2 {
		t.Fatalf("upserts = %d, gets = %d; an existing app must be kept", len(cloud.upserts), len(cloud.gets))
	}
	if req := cloud.upserts[0]; req.GetOrganizationId() != testOperatorTenant || req.GetId() != "campaign:people" || req.GetName() != "people" {
		t.Fatalf("wrong Cloud catalog registration: %v", req)
	}
	if cloud.gets[1].GetOrganizationId() != testOperatorTenant {
		t.Fatalf("GetApp organization = %q", cloud.gets[1].GetOrganizationId())
	}
}
