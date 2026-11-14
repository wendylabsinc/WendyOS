package commands

import (
	"context"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func TestDesktopCloudRouteIdentityUsesTrustedDeviceBinding(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const asset = "22222222-2222-4222-8222-222222222222"
	const device = "33333333-3333-4333-8333-333333333333"
	selector := "cloud://api.wendy.dev/tenant/" + tenant + "/asset/" + asset
	cfg := &config.Config{Auth: []config.AuthConfig{{CloudGRPC: "api.wendy.dev", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/44444444-4444-4444-8444-444444444444"}}}}}
	binding := device
	calls := 0
	result, err := desktopCloudRouteIdentities(context.Background(), cfg, []string{selector, selector}, func(context.Context, *config.AuthConfig) ([]*cloudpbv2.Asset, error) {
		calls++
		return []*cloudpbv2.Asset{{Id: asset, OrganizationId: tenant, Name: "jetson", PkiDeviceName: &binding}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || result[selector].Name != "jetson" || result[selector].Principal != "spiffe://wendy.sh/tenant/"+tenant+"/device/"+device {
		t.Fatalf("calls=%d result=%+v", calls, result)
	}
}

func TestDesktopCloudRouteIdentityRejectsForeignInventory(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	const asset = "22222222-2222-4222-8222-222222222222"
	selector := "cloud://api.wendy.dev/tenant/" + tenant + "/asset/" + asset
	cfg := &config.Config{Auth: []config.AuthConfig{{CloudGRPC: "api.wendy.dev", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/44444444-4444-4444-8444-444444444444"}}}}}
	if _, err := desktopCloudRouteIdentities(context.Background(), cfg, []string{selector}, func(context.Context, *config.AuthConfig) ([]*cloudpbv2.Asset, error) {
		return []*cloudpbv2.Asset{{Id: asset, OrganizationId: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", Name: "jetson"}}, nil
	}); err == nil {
		t.Fatal("accepted foreign-tenant inventory")
	}
}
