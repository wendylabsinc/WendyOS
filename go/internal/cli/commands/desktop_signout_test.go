package commands

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

const (
	signOutTenantA = "11111111-1111-4111-8111-111111111111"
	signOutTenantB = "22222222-2222-4222-8222-222222222222"
	signOutTenantC = "33333333-3333-4333-8333-333333333333"
)

// isolateSignOutConfig points every config, lock, secret and broker path at a
// temporary directory, so these tests can never touch a real sign-in.
func isolateSignOutConfig(t *testing.T, cfg *config.Config) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("WENDY_CONFIG_DIR", filepath.Join(dir, "wendy"))
	t.Setenv("WENDY_SECRET_STORE", "file")
	if cfg != nil {
		if err := config.Save(cfg); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "wendy")
}

func signOutToken(t *testing.T, email string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"email": email})
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func signOutContext(t *testing.T, name, cloud, tenant, operator, email string) config.AuthConfig {
	t.Helper()
	auth := config.AuthConfig{Name: name, CloudGRPC: cloud, CloudDashboard: "https://" + strings.TrimSuffix(cloud, ":443")}
	if email != "" {
		auth.APIKey = signOutToken(t, email)
	}
	if tenant != "" {
		auth.Certificates = []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/" + operator, PemPrivateKey: "key-" + name}}
	}
	return auth
}

func savedContexts(t *testing.T) (*config.Config, []string) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, auth := range cfg.Auth {
		names = append(names, auth.Name)
	}
	return cfg, names
}

func TestDesktopAccountsGroupOnePersonAcrossOrganizations(t *testing.T) {
	isolateSignOutConfig(t, &config.Config{CurrentContext: "acme", Auth: []config.AuthConfig{
		// One person, two organizations: each realm gives them a different operator ID.
		signOutContext(t, "acme", "api.cloud.test:443", signOutTenantA, "op-in-acme", "Chris@Example.com"),
		signOutContext(t, "robotics", "api.cloud.test:443", signOutTenantB, "op-in-robotics", "chris@example.com"),
		// Another person on the same Cloud.
		signOutContext(t, "ops", "api.cloud.test:443", signOutTenantC, "op-ops", "ops@example.com"),
		// The same email on another Cloud is another account.
		signOutContext(t, "dev", "api.dev.test:443", signOutTenantA, "op-dev", "chris@example.com"),
		// No token: the certificate's operator ID still links it to its org twin.
		signOutContext(t, "ops-2", "api.cloud.test:443", signOutTenantA, "op-ops", ""),
		// Neither email nor certificate: stands alone.
		{Name: "bare", CloudGRPC: "api.cloud.test:443"},
	}})
	config.SetOrganizationName("api.cloud.test:443", signOutTenantB, "Robotics Lab")

	accounts, err := DesktopAccounts()
	if err != nil {
		t.Fatal(err)
	}
	person := map[string]string{}
	for _, account := range accounts {
		person[account.Context] = account.Person
	}
	if person["acme"] == "" || person["acme"] != person["robotics"] {
		t.Fatalf("one person's organizations are not grouped: %+v", accounts)
	}
	if person["ops"] == person["acme"] || person["dev"] == person["acme"] || person["bare"] == person["acme"] {
		t.Fatalf("different people or Clouds were grouped: %+v", accounts)
	}
	if person["ops-2"] != person["ops"] {
		t.Fatalf("operator identity did not link a sign-in without an email: %+v", accounts)
	}
	if person["bare"] != "context:bare" {
		t.Fatalf("a sign-in with no identity must stand alone, got %q", person["bare"])
	}
	for _, account := range accounts {
		switch account.Context {
		case "acme":
			if !account.Active || account.OrganizationID != signOutTenantA || account.OrganizationName != "" {
				t.Fatalf("acme: %+v", account)
			}
		case "robotics":
			if account.Active || account.OrganizationID != signOutTenantB || account.OrganizationName != "Robotics Lab" {
				t.Fatalf("robotics: %+v", account)
			}
		}
	}
	active, err := DesktopAccount()
	if err != nil {
		t.Fatal(err)
	}
	if active.Context != "acme" || active.Person != person["acme"] || active.OrganizationID != signOutTenantA {
		t.Fatalf("active account lacks grouping metadata: %+v", active)
	}
}

func TestDesktopSignOutOrganizationSwitchesToSamePerson(t *testing.T) {
	dir := isolateSignOutConfig(t, &config.Config{CurrentContext: "robotics", DefaultDevice: "kept-device", Auth: []config.AuthConfig{
		signOutContext(t, "ops", "api.cloud.test:443", signOutTenantC, "op-ops", "ops@example.com"),
		signOutContext(t, "acme", "api.cloud.test:443", signOutTenantA, "op-a", "chris@example.com"),
		signOutContext(t, "robotics", "api.cloud.test:443", signOutTenantB, "op-b", "chris@example.com"),
	}})
	sessions := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessions, "stale.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := DesktopSignOut(context.Background(), []string{"robotics"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Removed) != 1 || result.Removed[0] != "robotics" {
		t.Fatalf("removed %v", result.Removed)
	}
	// "ops" comes first in the config, but the same person's other
	// organization is the right one to land on.
	if result.Current != "acme" {
		t.Fatalf("current after sign-out = %q, want acme", result.Current)
	}
	cfg, names := savedContexts(t)
	if strings.Join(names, ",") != "ops,acme" || cfg.CurrentContext != "acme" || cfg.DefaultDevice != "kept-device" {
		t.Fatalf("saved config after sign-out: contexts %v current %q device %q", names, cfg.CurrentContext, cfg.DefaultDevice)
	}
	if _, err := os.Stat(filepath.Join(sessions, "stale.json")); !os.IsNotExist(err) {
		t.Fatalf("session brokers were not invalidated: %v", err)
	}
	if current := DesktopCurrentContext(); current != "acme" {
		t.Fatalf("DesktopCurrentContext = %q", current)
	}
}

func TestDesktopSignOutWholePersonFallsBackToAnotherPerson(t *testing.T) {
	isolateSignOutConfig(t, &config.Config{CurrentContext: "acme", Auth: []config.AuthConfig{
		signOutContext(t, "acme", "api.cloud.test:443", signOutTenantA, "op-a", "chris@example.com"),
		signOutContext(t, "robotics", "api.cloud.test:443", signOutTenantB, "op-b", "chris@example.com"),
		{Name: "no-certificate", CloudGRPC: "api.cloud.test:443"},
		signOutContext(t, "ops", "api.cloud.test:443", signOutTenantC, "op-ops", "ops@example.com"),
	}})
	result, err := DesktopSignOut(context.Background(), []string{"acme", "robotics"})
	if err != nil {
		t.Fatal(err)
	}
	// A sign-in without certificates cannot be selected, so skip it.
	if result.Current != "ops" {
		t.Fatalf("current = %q, want ops", result.Current)
	}
	if _, names := savedContexts(t); strings.Join(names, ",") != "no-certificate,ops" {
		t.Fatalf("contexts = %v", names)
	}
}

func TestDesktopSignOutKeepsCurrentWhenAnotherAccountIsRemoved(t *testing.T) {
	isolateSignOutConfig(t, &config.Config{CurrentContext: "acme", Auth: []config.AuthConfig{
		signOutContext(t, "acme", "api.cloud.test:443", signOutTenantA, "op-a", "chris@example.com"),
		signOutContext(t, "ops", "api.cloud.test:443", signOutTenantC, "op-ops", "ops@example.com"),
		signOutContext(t, "robotics", "api.cloud.test:443", signOutTenantB, "op-b", "chris@example.com"),
	}})
	// A name removed elsewhere in the meantime is skipped, not an error.
	result, err := DesktopSignOut(context.Background(), []string{"ops", "already-gone"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Current != "acme" || strings.Join(result.Removed, ",") != "ops" {
		t.Fatalf("result = %+v", result)
	}
	if cfg, names := savedContexts(t); cfg.CurrentContext != "acme" || strings.Join(names, ",") != "acme,robotics" {
		t.Fatalf("current %q contexts %v", cfg.CurrentContext, names)
	}
	// Nothing left to remove is a no-op.
	if result, err := DesktopSignOut(context.Background(), []string{"ops"}); err != nil || len(result.Removed) != 0 || result.Current != "acme" {
		t.Fatalf("repeat sign-out: %+v %v", result, err)
	}
	if _, err := DesktopSignOut(context.Background(), nil); err == nil {
		t.Fatal("an empty sign-out request was accepted")
	}
}

func TestDesktopSignOutLastAccountLeavesSignedOutState(t *testing.T) {
	isolateSignOutConfig(t, &config.Config{CurrentContext: "acme", Auth: []config.AuthConfig{
		signOutContext(t, "acme", "api.cloud.test:443", signOutTenantA, "op-a", "chris@example.com"),
	}})
	result, err := DesktopSignOut(context.Background(), []string{"acme"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Current != "" {
		t.Fatalf("current = %q", result.Current)
	}
	cfg, names := savedContexts(t)
	if len(names) != 0 || cfg.CurrentContext != "" {
		t.Fatalf("contexts %v current %q", names, cfg.CurrentContext)
	}
	account, err := DesktopAccount()
	if err != nil || account.State != 0 || account.Context != "" {
		t.Fatalf("signed-out account = %+v %v", account, err)
	}
}

func TestUnsharedSecretsKeepsReferencesStillInUse(t *testing.T) {
	removed := []config.AuthConfig{{Name: "gone", APIKey: "keychain:v1:shared", RefreshToken: "keychain:v1:own",
		Certificates: []config.CertificateInfo{{PemPrivateKey: "keychain:v1:shared-key"}}}}
	kept := []config.AuthConfig{{Name: "kept", APIKey: "keychain:v1:shared",
		Certificates: []config.CertificateInfo{{PemPrivateKey: "keychain:v1:shared-key"}}}}
	got := unsharedSecrets(removed, kept)
	if len(got) != 1 || got[0].APIKey != "" || got[0].Certificates[0].PemPrivateKey != "" || got[0].RefreshToken != "keychain:v1:own" {
		t.Fatalf("unsharedSecrets = %+v", got)
	}
	if removed[0].APIKey != "keychain:v1:shared" || removed[0].Certificates[0].PemPrivateKey != "keychain:v1:shared-key" {
		t.Fatal("unsharedSecrets changed its input")
	}
}

func TestAuthLogoutRemovesOnlyTheNamedContext(t *testing.T) {
	isolateSignOutConfig(t, &config.Config{CurrentContext: "robotics", Auth: []config.AuthConfig{
		signOutContext(t, "acme", "api.cloud.test:443", signOutTenantA, "op-a", "chris@example.com"),
		signOutContext(t, "robotics", "api.cloud.test:443", signOutTenantB, "op-b", "chris@example.com"),
		signOutContext(t, "ops", "api.cloud.test:443", signOutTenantC, "op-ops", "ops@example.com"),
	}})
	cmd := newAuthLogoutCmd()
	cmd.SetArgs([]string{"missing"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetContext(context.Background())
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), `no auth context named "missing"`) {
		t.Fatalf("unknown context: %v", err)
	}
	if _, names := savedContexts(t); len(names) != 3 {
		t.Fatalf("a failed logout changed the config: %v", names)
	}

	cmd = newAuthLogoutCmd()
	cmd.SetArgs([]string{"robotics"})
	var err error
	out := captureStdout(t, func() { err = cmd.ExecuteContext(context.Background()) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `Logged out of context "robotics"`) || !strings.Contains(out, "Current context: acme") {
		t.Fatalf("output: %q", out)
	}
	cfg, names := savedContexts(t)
	if strings.Join(names, ",") != "acme,ops" || cfg.CurrentContext != "acme" {
		t.Fatalf("contexts %v current %q", names, cfg.CurrentContext)
	}
	if err := newAuthLogoutCmd().Args(newAuthLogoutCmd(), []string{"a", "b"}); err == nil {
		t.Fatal("accepted two context names")
	}
}

func TestAuthLogoutWithoutArgumentRemovesEverything(t *testing.T) {
	isolateSignOutConfig(t, &config.Config{CurrentContext: "acme", DefaultDevice: "kept-device", Auth: []config.AuthConfig{
		signOutContext(t, "acme", "api.cloud.test:443", signOutTenantA, "op-a", "chris@example.com"),
		signOutContext(t, "ops", "api.cloud.test:443", signOutTenantC, "op-ops", "ops@example.com"),
	}})
	cmd := newAuthLogoutCmd()
	cmd.SetArgs([]string{})
	var err error
	out := captureStdout(t, func() { err = cmd.ExecuteContext(context.Background()) })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "All authentication credentials removed") {
		t.Fatalf("output: %q", out)
	}
	cfg, names := savedContexts(t)
	if len(names) != 0 || cfg.CurrentContext != "" || cfg.DefaultDevice != "kept-device" {
		t.Fatalf("contexts %v current %q device %q", names, cfg.CurrentContext, cfg.DefaultDevice)
	}
	// Logging out again when nothing is saved still succeeds.
	cmd = newAuthLogoutCmd()
	cmd.SetArgs([]string{})
	captureStdout(t, func() { err = cmd.ExecuteContext(context.Background()) })
	if err != nil {
		t.Fatal(err)
	}
}
