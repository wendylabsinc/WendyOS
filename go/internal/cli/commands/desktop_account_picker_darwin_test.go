//go:build darwin && !ios

package commands

import (
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"testing"
)

func TestDesktopAccountPickerSelectsExactSavedContext(t *testing.T) {
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	firstOrg := "11111111-1111-4111-8111-111111111111"
	secondOrg := "22222222-2222-4222-8222-222222222222"
	cfg := &config.Config{CurrentContext: "first", DefaultDevice: "local-device", Auth: []config.AuthConfig{
		{Name: "first", CloudGRPC: "cloud.test:443", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + firstOrg + "/operator/one"}}},
		{Name: "second", CloudGRPC: "cloud.test:443", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + secondOrg + "/operator/two"}}},
		{Name: "incomplete", CloudGRPC: "other.test:443"},
	}}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	config.SetOrganizationName("cloud.test:443", secondOrg, "Robotics Lab")
	choices, err := DesktopAccountChoices()
	if err != nil {
		t.Fatal(err)
	}
	if len(choices) != 3 || !choices[0].Active || choices[1].OrganizationID != secondOrg || choices[1].OrganizationName != "Robotics Lab" {
		t.Fatalf("unexpected choices: %+v", choices)
	}
	for _, invalid := range []string{"cloud.test", secondOrg, "incomplete", "missing"} {
		if err := DesktopUseAccount(invalid); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
	saved, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.CurrentContext != "first" {
		t.Fatal("failed selection changed context")
	}
	if err := DesktopUseAccount("second"); err != nil {
		t.Fatal(err)
	}
	saved, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved.CurrentContext != "second" || saved.DefaultDevice != "local-device" || len(saved.Auth) != 3 {
		t.Fatal("selection changed unrelated configuration")
	}
}
