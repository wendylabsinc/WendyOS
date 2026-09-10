//go:build darwin || linux || windows

package commands

import (
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

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
