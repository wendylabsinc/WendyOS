package commands

import (
	"context"
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"strings"
	"testing"
)

func TestDesktopManagementRejectsInvalidInputBeforeRPC(t *testing.T) {
	for _, name := range []string{"", "Uppercase", "-bad", "bad-", "a.local", strings.Repeat("a", 64)} {
		if err := DesktopManageDevice(context.Background(), nil, "account", "org", "device", name, false); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	if err := DesktopManageDevice(context.Background(), nil, "", "org", "device", "", true); err == nil {
		t.Fatal("accepted missing operator account")
	}
}

func TestDesktopManagementRejectsChangedIdentityAndCloud(t *testing.T) {
	const device = "fad6089a-06e8-4c7b-8c09-4bcbef67f387"
	for _, tc := range []struct{ name, principal, host, account string }{
		{"different device", "spiffe://wendy.sh/tenant/" + testOperatorTenant + "/device/11111111-1111-4111-8111-111111111111", "cloud.test:443", "selected"},
		{"operator is not device", "spiffe://wendy.sh/tenant/" + testOperatorTenant + "/operator/alice", "cloud.test:443", "selected"},
		{"different Cloud", "spiffe://wendy.sh/tenant/" + testOperatorTenant + "/device/" + device, "other.test:443", "selected"},
		{"deleted account", "spiffe://wendy.sh/tenant/" + testOperatorTenant + "/device/" + device, "cloud.test:443", "missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seedConfig(t, &config.Config{Auth: []config.AuthConfig{{Name: "selected", CloudGRPC: "cloud.test:443", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + testOperatorTenant + "/operator/alice"}}}}})
			startUDSAgent(t, func(s *grpc.Server) {
				pb.RegisterWendyProvisioningServiceServer(s, infoProvisioningV2Server{response: &pb.IsProvisionedResponse{ResponseType: &pb.IsProvisionedResponse_Provisioned{Provisioned: &pb.ProvisionedResponse{PrincipalUri: tc.principal, CloudHost: tc.host}}}})
			})
			conn, err := DesktopConnect(context.Background(), "test")
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			for _, remove := range []bool{false, true} {
				if err := DesktopManageDevice(context.Background(), conn, tc.account, testOperatorTenant, device, "renamed", remove); err == nil {
					t.Fatal("unsafe management request accepted")
				}
			}
		})
	}
}

func TestDesktopManagementRenameSignedCloudFirst(t *testing.T) {
	for _, refused := range []bool{false, true} {
		t.Run(fmt.Sprint(refused), func(t *testing.T) {
			log := &renameLog{}
			cloud := &renameCloudV2{log: log, asset: &cloudpb.Asset{Id: renameAsset, OrganizationId: testOperatorTenant, PkiDeviceName: proto.String(renameDevice)}}
			if refused {
				cloud.updateErr = status.Error(codes.AlreadyExists, "duplicate name")
			}
			startV2RenameDevice(t, cloud, renamePrincipal)
			cfg, err := config.Load()
			if err != nil {
				t.Fatal(err)
			}
			account := cfg.Auth[0].Name
			if account == "" {
				cfg.Auth[0].Name = "selected"
				account = "selected"
				if err := config.Save(cfg); err != nil {
					t.Fatal(err)
				}
			}
			conn, err := DesktopConnect(context.Background(), "test")
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			err = DesktopManageDevice(context.Background(), conn, account, testOperatorTenant, renameDevice, "living-room", false)
			if refused {
				if err == nil || strings.Contains(log.String(), "SetHostname") {
					t.Fatalf("Cloud refusal changed hostname: %v, %s", err, log)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				want := "cloud v2 GetAsset org=" + testOperatorTenant + " device=" + renameDevice + "\ncloud v2 UpdateAsset id=" + renameAsset + " name=living-room op=wendycloud.v2.AssetService/UpdateAsset resource=asset/" + renameAsset + "\nagent SetHostname living-room"
				if log.String() != want {
					t.Fatalf("wrong signed operation order: %s", log)
				}
			}
		})
	}
}
