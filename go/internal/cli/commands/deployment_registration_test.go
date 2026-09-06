package commands

import (
	"context"
	"errors"
	"net"
	"reflect"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type deploymentProvisioningStub struct {
	agentpb.WendyProvisioningServiceClient
	response *agentpb.IsProvisionedResponse
	err      error
}

func (s deploymentProvisioningStub) IsProvisioned(context.Context, *agentpb.IsProvisionedRequest, ...grpc.CallOption) (*agentpb.IsProvisionedResponse, error) {
	return s.response, s.err
}

func registrationDevice() *agentpb.ProvisionedResponse {
	return &agentpb.ProvisionedResponse{CloudHost: "cloud.example:443", OrganizationId: 42, AssetId: 9}
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
	for _, modify := range []func(*agentpb.ProvisionedResponse){
		func(d *agentpb.ProvisionedResponse) { d.CloudHost = "attacker.example:443" },
		func(d *agentpb.ProvisionedResponse) { d.OrganizationId = 100 },
		func(d *agentpb.ProvisionedResponse) { d.AssetId = 0 },
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
	provisioning := deploymentProvisioningStub{response: &agentpb.IsProvisionedResponse{
		Response: &agentpb.IsProvisionedResponse_NotProvisioned{NotProvisioned: &agentpb.NotProvisionedResponse{}},
	}}
	err := registerDeviceApps(context.Background(), provisioning, []string{"app"}, func() (*config.Config, error) {
		t.Fatal("local deployment loaded Cloud credentials")
		return nil, nil
	}, func(context.Context, *config.AuthConfig, *agentpb.ProvisionedResponse, []string) error {
		t.Fatal("local deployment contacted Cloud")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRegistrationDeduplicatesAppsAndPropagatesCloudFailure(t *testing.T) {
	device := registrationDevice()
	provisioning := deploymentProvisioningStub{response: &agentpb.IsProvisionedResponse{
		Response: &agentpb.IsProvisionedResponse_Provisioned{Provisioned: device},
	}}
	denied := status.Error(codes.PermissionDenied, "viewer cannot register deployments")
	called := false
	err := registerDeviceApps(context.Background(), provisioning, []string{"app", "app", "campaign:people"}, func() (*config.Config, error) {
		return registrationConfig(), nil
	}, func(_ context.Context, auth *config.AuthConfig, got *agentpb.ProvisionedResponse, apps []string) error {
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
	conn := &grpcclient.AgentConnection{ProvisioningService: deploymentProvisioningStub{err: status.Error(codes.Unavailable, "offline")}}
	err := runWithAgent(context.Background(), conn, t.TempDir(), &appconfig.AppConfig{AppID: "app"}, runOptions{})
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
	requests []*cloudpb.RegisterAppDeploymentRequest
	err      error
}

func (s *deploymentCloudServer) RegisterAppDeployment(ctx context.Context, request *cloudpb.RegisterAppDeploymentRequest) (*cloudpb.App, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if len(md.Get("x-wendy-client-cert")) == 0 {
		return nil, status.Error(codes.Unauthenticated, "missing operator metadata")
	}
	s.requests = append(s.requests, request)
	if s.err != nil {
		return nil, s.err
	}
	return &cloudpb.App{Id: request.GetId(), OrganizationId: request.GetOrganizationId()}, nil
}

func TestCloudRegistrationSendsAppAndDeviceIdentity(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	implementation := &deploymentCloudServer{}
	cloudpb.RegisterAppServiceServer(server, implementation)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	cfg := registrationConfig()
	auth := cfg.Auth[0]
	auth.CloudGRPC = listener.Addr().String()
	auth.Certificates = auth.Certificates[1:]
	device := registrationDevice()
	device.CloudHost = auth.CloudGRPC
	for _, id := range []string{"app", "campaign:people"} {
		if err := registerAppsWithCloud(context.Background(), &auth, device, []string{id}); err != nil {
			t.Fatal(err)
		}
	}
	if len(implementation.requests) != 2 {
		t.Fatalf("requests: %d", len(implementation.requests))
	}
	for i, req := range implementation.requests {
		if req.GetOrganizationId() != 42 || req.GetAssetId() != 9 || req.GetId() != []string{"app", "campaign:people"}[i] {
			t.Fatalf("wrong Cloud registration: %v", req)
		}
	}
	// An older Cloud must fail explicitly, rather than ignore an optional field
	// and report a registration that never created a device assignment.
	implementation.err = status.Error(codes.Unimplemented, "upgrade Cloud")
	if err := registerAppsWithCloud(context.Background(), &auth, device, []string{"app"}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("old Cloud failure hidden: %v", err)
	}
}
