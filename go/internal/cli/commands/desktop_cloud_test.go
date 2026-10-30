package commands

import (
	"context"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"testing"
)

func TestDesktopCloudRequiresSelectedAccountTenant(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	cfg := &config.Config{Auth: []config.AuthConfig{
		{Name: "first", CloudGRPC: "cloud.test:443", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/one"}}},
		{Name: "second", CloudGRPC: "cloud.test:443", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/22222222-2222-4222-8222-222222222222/operator/two"}}},
	}}
	if e := config.Save(cfg); e != nil {
		t.Fatal(e)
	}
	if a, e := desktopCloudAuth("first", "11111111-1111-4111-8111-111111111111"); e != nil || a.Name != "first" {
		t.Fatalf("matching session %v %v", a, e)
	}
	if _, e := desktopCloudAuth("first", "22222222-2222-4222-8222-222222222222"); e == nil {
		t.Fatal("silently changed account")
	}
	if _, e := desktopCloudAuth("deleted", ""); e == nil {
		t.Fatal("accepted missing account")
	}
}
func TestDesktopInviteRejectsInvalidInputBeforeNetwork(t *testing.T) {
	ctx := context.Background()
	if _, e := DesktopCreateInvite(ctx, "", "", "owner", 9999999999); e == nil {
		t.Fatal("accepted owner invite")
	}
	if _, e := DesktopCreateInvite(ctx, "", "", "member", 0); e == nil {
		t.Fatal("accepted expired invite")
	}
	if e := DesktopAddMember(ctx, "", "", "invalid email", ""); e == nil {
		t.Fatal("accepted invalid email")
	}
}
