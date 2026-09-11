package commands

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
)

type pickerV2OrgServer struct {
	pb.UnimplementedOrganizationServiceServer
	t *testing.T
}

func (s *pickerV2OrgServer) GetOrganization(ctx context.Context, req *pb.GetOrganizationRequest) (*pb.Organization, error) {
	if req.Id != testOperatorTenant {
		s.t.Errorf("organization lookup used %q instead of certificate tenant", req.Id)
	}
	return &pb.Organization{Id: testOperatorTenant, Name: "UUID organization"}, nil
}
func (s *pickerV2OrgServer) ListOrganizations(req *pb.ListOrganizationsRequest, stream grpc.ServerStreamingServer[pb.ListOrganizationsResponse]) error {
	return stream.Send(&pb.ListOrganizationsResponse{Organization: &pb.Organization{Id: testOperatorTenant, Name: "UUID organization"}})
}
func TestDevicePickerCloudTabV2OrganizationAndDevices(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	pb.RegisterOrganizationServiceServer(srv, &pickerV2OrgServer{t: t})
	pb.RegisterAssetServiceServer(srv, &discoveryV2Server{t: t, count: 1})
	go srv.Serve(lis)
	defer srv.Stop()
	auth := oidcEnrollmentAuth(t)
	auth.CloudGRPC = lis.Addr().String()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	model := newDevicePickerModel(ctx, tui.NewPicker(), auth, 0)
	model, _ = tabTo(t, model, devicePickerCloudTab)
	updated, _ := model.Update(devicePickerCloudMsg{msg: model.cloud.scanCmd()()})
	model = updated.(devicePickerModel)
	updated, _ = model.Update(model.loadOrgNameCmd()())
	model = updated.(devicePickerModel)
	if model.cloud.err != nil {
		t.Fatal(model.cloud.err)
	}
	view := model.View()
	for _, want := range []string{"UUID organization", "online-device"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view missing %q: %s", want, view)
		}
	}
	if strings.Contains(view, testOperatorTenant) {
		t.Fatal("organization ID displayed instead of resolved name")
	}
	if strings.Contains(view, "org 0") {
		t.Fatal("displayed fake numeric organization")
	}
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyEnter})
	model = updated.(devicePickerModel)
	choice, ok := model.choice()
	if !ok || choice.CloudV2 == nil || choice.Cloud != nil {
		t.Fatal("selected device lost its UUID")
	}
	// Switching uses v2 memberships and matches the selected UUID to its session.
	oldPicker := pickCloudOrgV2
	pickCloudOrgV2 = func(orgs []*pb.Organization, a *config.AuthConfig, cfg *config.Config) (string, error) {
		if len(orgs) != 1 {
			t.Fatal("membership list missing")
		}
		return orgs[0].Id, nil
	}
	defer func() { pickCloudOrgV2 = oldPicker }()
	cfg := &config.Config{Auth: []config.AuthConfig{*auth}, DefaultCloudGRPC: auth.CloudGRPC}
	oldLoad := loadCloudOrgConfig
	loadCloudOrgConfig = func() (*config.Config, error) { return cfg, nil }
	defer func() { loadCloudOrgConfig = oldLoad }()
	selected, _, err := switchCloudOrganization(ctx, cfg)
	if err != nil || selected.Certificates[0].TenantUUID() != testOperatorTenant {
		t.Fatalf("UUID switch failed: %v", err)
	}
	srv.Stop()
	if got := cloudOrganizationName(ctx, auth); got != "UUID organization" {
		t.Fatalf("offline v2 name = %q", got)
	}
}
func TestAuthPickerUUIDKeysAndDefaults(t *testing.T) {
	a := oidcEnrollmentAuth(t)
	b := *a
	b.Certificates = append([]config.CertificateInfo(nil), a.Certificates...)
	b.Certificates[0].PrincipalURI = "spiffe://wendy.sh/tenant/8a53be77-2a69-464f-8f73-83643fe0beaa/operator/other"
	cfg := &config.Config{Auth: []config.AuthConfig{*a, b}}
	items := authPickerItems(cfg, nil)
	if len(items) != 2 || items[0].DedupKey == items[1].DedupKey {
		t.Fatal("UUID organizations collapsed to org zero")
	}
	for _, item := range items {
		if item.Description == "0" || item.Name == "org 0" {
			t.Fatal("fake org ID in auth picker")
		}
	}
	load := seedConfig(t, cfg)
	if err := persistSessionDefault(authSessionKey(&b)); err != nil {
		t.Fatal(err)
	}
	saved := load()
	if saved.DefaultOrgID != 0 || saved.DefaultTenantUUID != b.Certificates[0].TenantUUID() {
		t.Fatal("UUID default was not saved")
	}
	selected, err := config.ResolveAuth(saved, "", nil)
	if err != nil || selected.OrganizationKey() != b.OrganizationKey() {
		t.Fatal("persisted default changed organizations")
	}
}
