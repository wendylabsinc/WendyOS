package commands

import (
	"context"
	"errors"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func mixedOrgContexts(t *testing.T) *config.Config {
	t.Helper()
	a := oidcEnrollmentAuth(t)
	a.Name = "wendy"
	a.CloudGRPC = "api.dev.example:443"
	b := *a
	b.Name = "other"
	b.Certificates = append([]config.CertificateInfo(nil), a.Certificates...)
	b.Certificates[0].PrincipalURI = "spiffe://wendy.sh/tenant/8a53be77-2a69-464f-8f73-83643fe0beaa/operator/other"
	legacy := pickerAuth(2)
	legacy.Name = "production"
	legacy.CloudGRPC = "api.production.example:443"
	return &config.Config{CurrentContext: a.Name, Auth: []config.AuthConfig{*legacy, *a, b}}
}

func stubOrgChoiceQueries(t *testing.T, fail bool) {
	t.Helper()
	old := listCloudOrganizationsV2
	listCloudOrganizationsV2 = func(_ context.Context, a *config.AuthConfig) ([]*pb.Organization, error) {
		if fail {
			return nil, errors.New("offline")
		}
		return []*pb.Organization{{Id: a.OrganizationKey(), Name: a.Name}, {Id: "additional-membership", Name: "Not signed in"}}, nil
	}
	t.Cleanup(func() { listCloudOrganizationsV2 = old })
	if fail {
		stubListOrgs(t, nil, errors.New("offline"))
	} else {
		stubListOrgs(t, []*cloudpb.Organization{makeOrg(2, "Production")}, nil)
	}
}

func TestCloudOrgChoicesIncludeAllStoredContexts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := mixedOrgContexts(t)
	stubOrgChoiceQueries(t, false)
	choices, failures := cloudOrgChoices(context.Background(), cfg)
	if len(failures) != 0 || len(choices) != 4 {
		t.Fatalf("choices=%+v failures=%v", choices, failures)
	}
	keys := map[string]bool{}
	for _, item := range cloudOrgChoiceItems(choices) {
		keys[item.Value.(string)] = true
	}
	for i := range cfg.Auth {
		if !keys[authSessionKey(&cfg.Auth[i])] {
			t.Fatalf("missing context %s", cfg.Auth[i].Name)
		}
	}
}

func TestCloudOrgChoicesKeepStoredContextsOffline(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := mixedOrgContexts(t)
	stubOrgChoiceQueries(t, true)
	choices, failures := cloudOrgChoices(context.Background(), cfg)
	if len(choices) != 3 || len(failures) != 3 {
		t.Fatalf("choices=%d failures=%d", len(choices), len(failures))
	}
}

func TestCloudOrgChoicesScopeSameTenantByEndpoint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := mixedOrgContexts(t)
	stubOrgChoiceQueries(t, false)
	other := cfg.Auth[1]
	other.Name = "other-environment"
	other.CloudGRPC = "api.other.example:443"
	cfg.Auth = append(cfg.Auth, other)
	choices, _ := cloudOrgChoices(context.Background(), cfg)
	key := authSessionKey(&other)
	found := false
	for _, choice := range choices {
		if choice.key == key {
			found = true
		}
	}
	if !found || cloudAuthForSessionKey(cfg, key).Name != other.Name {
		t.Fatal("same tenant in different environments collapsed")
	}
}

func TestSwitchCloudOrganizationAcrossStoredContexts(t *testing.T) {
	for _, name := range []string{"production", "wendy", "other"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			cfg := mixedOrgContexts(t)
			stubOrgChoiceQueries(t, false)
			stubCloudOrgReload(t, cfg, nil)
			var want *config.AuthConfig
			for i := range cfg.Auth {
				if cfg.Auth[i].Name == name {
					want = &cfg.Auth[i]
				}
			}
			old := pickCloudOrgChoice
			pickCloudOrgChoice = func(_ *config.Config, choices []cloudOrgChoice) (string, error) {
				if len(choices) != 4 {
					t.Fatalf("picker has %d rows", len(choices))
				}
				return authSessionKey(want), nil
			}
			t.Cleanup(func() { pickCloudOrgChoice = old })
			stubCloudOrgLogin(t, func(context.Context, string, string) error {
				t.Fatal("stored context must not log in again")
				return nil
			})
			got, _, err := switchCloudOrganization(context.Background(), cfg)
			if err != nil || got != want {
				t.Fatalf("auth=%+v error=%v", got, err)
			}
		})
	}
}

func TestCloudOrgChoicePrefersOperatorCredentials(t *testing.T) {
	cfg := mixedOrgContexts(t)
	operator := cfg.Auth[1]
	legacy := operator
	legacy.OAuthIssuer = ""
	cfg.Auth = []config.AuthConfig{legacy, operator}
	if got := cloudAuthForSessionKey(cfg, authSessionKey(&operator)); got != &cfg.Auth[1] {
		t.Fatal("selected legacy credentials over OIDC")
	}
}
