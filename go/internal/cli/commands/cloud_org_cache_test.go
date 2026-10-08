package commands

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

func TestCloudOrganizationNameCacheRefreshAndOffline(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	auth := &config.AuthConfig{CloudGRPC: "prod:443", OAuthIssuer: "https://must-not-refresh.invalid", APIKey: "current-token", OAuthExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339), Certificates: []config.CertificateInfo{{OrganizationID: 7}}}
	dir, err := config.ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.json")
	original := []byte(`{"sentinel":"credentials must not be rewritten"}`)
	if err := os.WriteFile(configPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	old := listOrgsFromCloud
	t.Cleanup(func() { listOrgsFromCloud = old })
	name := "Robotics"
	var lookupErr error
	listOrgsFromCloud = func(_ context.Context, lookup *config.AuthConfig) ([]*pb.Organization, error) {
		// The issuer must survive: it is what keeps a cnf-bound token on DPoP
		// instead of going out as a Bearer that Cloud rejects (WDY-3524).
		if lookup.OAuthIssuer != auth.OAuthIssuer || lookup.APIKey != "current-token" {
			t.Fatal("display lookup dropped its DPoP binding or its current access token")
		}
		return []*pb.Organization{{Id: 7, Name: name}}, lookupErr
	}
	ctx := context.Background()
	if got := cloudOrganizationName(ctx, auth); got != name {
		t.Fatalf("lookup = %q", got)
	}
	// Reads always load the file, proving the name survives the next invocation.
	if got := cachedCloudOrganizationName(auth); got != name {
		t.Fatalf("persisted name = %q", got)
	}
	name = "Renamed Robotics"
	if got := cloudOrganizationName(ctx, auth); got != name {
		t.Fatalf("refreshed name = %q", got)
	}
	lookupErr = errors.New("offline")
	if got := cloudOrganizationName(ctx, auth); got != name {
		t.Fatalf("offline name = %q", got)
	}
	lookupErr, name = nil, ""
	if got := cloudOrganizationName(ctx, auth); got != "Renamed Robotics" {
		t.Fatalf("empty response erased cached name: %q", got)
	}
	// An access token that would need a refresh is never refreshed by a
	// display lookup: the last known name stands.
	stale := *auth
	stale.OAuthExpiresAt = time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	name = "Must not be fetched"
	if got := cloudOrganizationName(ctx, &stale); got != "Renamed Robotics" {
		t.Fatalf("stale-token lookup = %q, want cached name", got)
	}
	if auth.OAuthIssuer != "https://must-not-refresh.invalid" || auth.APIKey != "current-token" {
		t.Fatal("lookup mutated caller's auth session")
	}
	data, err := os.ReadFile(configPath)
	if err != nil || string(data) != string(original) {
		t.Fatal("name cache modified credentials config")
	}
	info, err := os.Stat(filepath.Join(dir, "organization-names.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("name cache was not persisted privately")
	}
}

func TestCloudOrganizationNameCacheIdentityScopeAndImmediateLabels(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := oidcEnrollmentAuth(t)
	b := *a
	b.CloudGRPC = "other-environment:443"
	c := *a
	c.Certificates = []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/8a53be77-2a69-464f-8f73-83643fe0beaa/operator/other"}}
	legacy := config.AuthConfig{CloudGRPC: a.CloudGRPC, Certificates: []config.CertificateInfo{{OrganizationID: 7}}}
	otherLegacy := legacy
	otherLegacy.CloudGRPC = b.CloudGRPC
	auths := []config.AuthConfig{*a, b, c, legacy, otherLegacy}
	names := []string{"Robotics", "Development", "Another tenant", "Legacy production", "Legacy development"}
	for i := range auths {
		cacheCloudOrganizationName(auths[i].CloudGRPC, auths[i].OrganizationKey(), names[i])
	}
	items := authPickerItems(&config.Config{Auth: auths}, nil)
	for i := range auths {
		if got := cachedCloudOrganizationName(&auths[i]); got != names[i] {
			t.Fatalf("cache scope %d = %q, want %q", i, got, names[i])
		}
		if items[i].Name != names[i] {
			t.Fatalf("picker name %d = %q", i, items[i].Name)
		}
		if !strings.HasPrefix(authSessionLabel(&auths[i]), names[i]+" — ") {
			t.Fatalf("session label ignored cached name: %s", authSessionLabel(&auths[i]))
		}
	}
	ctx := context.Background()
	device := newDevicePickerModel(ctx, tui.NewPicker(), a, 0, false, devicePickerLocalTab)
	if device.cloudOrg != names[0] {
		t.Fatal("device picker waits for network before showing cached name")
	}
	discover := newDiscoverTabsModel(ctx, newDiscoverModel(ctx, defaultOpts(), true), a, 0, devicePickerCloudTab)
	if discover.cloudOrg != names[0] {
		t.Fatal("discovery waits for network before showing cached name")
	}
	updated, _ := device.Update(devicePickerOrgMsg{})
	if updated.(devicePickerModel).cloudOrg != names[0] {
		t.Fatal("empty refresh cleared device name")
	}
	updated, _ = discover.Update(discoverTabsOrgMsg{})
	if updated.(discoverTabsModel).cloudOrg != names[0] {
		t.Fatal("empty refresh cleared discovery name")
	}
	if got := deviceCloudOrgLabel(a, names[0], 0); got != "Organization: Robotics  (o switch)" {
		t.Fatalf("header = %q", got)
	}
}

func TestCloudOrganizationNameCacheCorruption(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir, err := config.ConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	auth := &config.AuthConfig{CloudGRPC: "prod:443", Certificates: []config.CertificateInfo{{OrganizationID: 7}}}
	for _, bad := range []string{"not json", `{"version":99,"names":{"prod:443":{"7":"wrong version"}}}`} {
		if err := os.WriteFile(filepath.Join(dir, "organization-names.json"), []byte(bad), 0600); err != nil {
			t.Fatal(err)
		}
		if cachedCloudOrganizationName(auth) != "" {
			t.Fatal("used corrupt cache")
		}
		cacheCloudOrganizationName(auth.CloudGRPC, auth.OrganizationKey(), "Recovered")
		if cachedCloudOrganizationName(auth) != "Recovered" {
			t.Fatal("could not recover corrupt cache")
		}
	}
}
