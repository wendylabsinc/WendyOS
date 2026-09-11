//go:build darwin || linux || windows

package commands

import (
	"fmt"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestAuthPickerSeparatesOIDCTenantsWithZeroLegacyOrgID(t *testing.T) {
	cfg := &config.Config{}
	for i, tenant := range []string{testOperatorTenant, "11111111-1111-4111-8111-111111111111"} {
		cfg.AddAuth(config.AuthConfig{
			CloudGRPC: "cloud:443", OAuthIssuer: fmt.Sprintf("https://auth.example/realms/%d", i),
			Certificates: []config.CertificateInfo{{
				PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/test",
			}},
		})
	}
	items := authPickerItems(cfg, nil)
	if len(items) != 2 || items[0].DedupKey == items[1].DedupKey {
		t.Fatalf("OIDC tenants collapsed into one session: %+v", items)
	}
	load := seedConfig(t, cfg)
	if err := persistSessionDefault(items[1].Value.(string)); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []string{"", "cloud:443"} {
		selected, err := config.ResolveAuth(load(), endpoint, nil)
		if err != nil {
			t.Fatal(err)
		}
		if authSessionKey(selected) != items[1].DedupKey {
			t.Fatal("persisted selection resolved the wrong OIDC realm")
		}
	}
}

func TestAuthSessionLabel(t *testing.T) {
	withOrg := &config.AuthConfig{CloudGRPC: "prod:443", Certificates: []config.CertificateInfo{{OrganizationID: 7}}}
	if got := authSessionLabel(withOrg); got != "org 7 — prod:443" {
		t.Fatalf("got %q", got)
	}
	noCerts := &config.AuthConfig{CloudGRPC: "local:50051"}
	if got := authSessionLabel(noCerts); got != "local:50051" {
		t.Fatalf("got %q", got)
	}
}

func TestAuthPickerItems(t *testing.T) {
	cfg := &config.Config{Auth: []config.AuthConfig{
		{CloudDashboard: "https://cloud.wendy.dev", CloudGRPC: "prod:443", Certificates: []config.CertificateInfo{{OrganizationID: 7}}},
		{CloudGRPC: "local:50051", Certificates: []config.CertificateInfo{{OrganizationID: 1}}},
	}}

	// With org names resolved: Name shows the org name, Description shows the org ID.
	withNames := map[string]string{"prod:443::7": "Acme Corp", "local:50051::1": "Dev Env"}
	items := authPickerItems(cfg, withNames)
	if len(items) != 2 {
		t.Fatalf("want 2 items, got %d", len(items))
	}
	if items[0].Name != "Acme Corp" {
		t.Errorf("item 0 name = %q, want Acme Corp", items[0].Name)
	}
	if items[0].Description != "7" {
		t.Errorf("item 0 description = %q, want 7", items[0].Description)
	}
	// Environment column carries the dashboard URL.
	if items[0].Type != "https://cloud.wendy.dev" {
		t.Errorf("item 0 env = %q, want https://cloud.wendy.dev", items[0].Type)
	}
	// DedupKey and Value include the org ID so two orgs on the same endpoint
	// are represented as separate rows.
	if items[0].Value.(string) != "prod:443::7" || items[0].DedupKey != "prod:443::7" {
		t.Errorf("item 0 value/dedup wrong: %+v", items[0])
	}

	// Without org names: falls back to "org N".
	noNames := map[string]string{}
	items2 := authPickerItems(cfg, noNames)
	if items2[0].Name != "org 7" {
		t.Errorf("item 0 fallback name = %q, want org 7", items2[0].Name)
	}
	// Session with no dashboard: environment falls back to the gRPC endpoint.
	if items2[1].Type != "local:50051" {
		t.Errorf("item 1 env = %q, want local:50051", items2[1].Type)
	}
}

func TestAuthPickerItemsDeduplicatesLegacyAndOperatorSessions(t *testing.T) {
	cfg := &config.Config{Auth: []config.AuthConfig{
		{CloudDashboard: "https://cloud.dev.wendy.sh", CloudGRPC: "api.dev.wendy.sh:443", Certificates: []config.CertificateInfo{{OrganizationID: 0}}},
		{CloudDashboard: "https://cloud.dev.wendy.sh", CloudGRPC: "api.dev.wendy.sh:443", OAuthIssuer: "https://auth.dev.wendy.sh/realms/acme", Certificates: []config.CertificateInfo{{OrganizationID: 0}}},
	}}

	items := authPickerItems(cfg, nil)
	if len(items) != 1 {
		t.Fatalf("legacy/operator duplicate produced %d picker rows, want 1", len(items))
	}
	if got := items[0].Value; got != "api.dev.wendy.sh:443::0" {
		t.Fatalf("picker key = %v", got)
	}
}
