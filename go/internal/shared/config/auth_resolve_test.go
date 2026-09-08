package config

import (
	"errors"
	"strings"
	"testing"
)

func twoSessions() *Config {
	return &Config{Auth: []AuthConfig{
		{CloudDashboard: "https://cloud.wendy.dev", CloudGRPC: "prod:443", Certificates: []CertificateInfo{{OrganizationID: 7}}},
		{CloudDashboard: "http://localhost:3000", CloudGRPC: "localhost:50051", Certificates: []CertificateInfo{{OrganizationID: 1}}},
	}}
}

func TestResolveAuthNotLoggedIn(t *testing.T) {
	if _, err := ResolveAuth(&Config{}, "", nil); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("want ErrNotLoggedIn, got %v", err)
	}
}

func TestResolveAuthFlagWins(t *testing.T) {
	cfg := twoSessions()
	cfg.DefaultCloudGRPC = "prod:443"
	auth, err := ResolveAuth(cfg, "localhost:50051", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if auth.CloudGRPC != "localhost:50051" {
		t.Fatalf("flag must win, got %s", auth.CloudGRPC)
	}
}

func TestResolveAuthFlagNoMatch(t *testing.T) {
	_, err := ResolveAuth(twoSessions(), "missing:443", nil)
	if err == nil || !strings.Contains(err.Error(), "no auth session for missing:443") {
		t.Fatalf("want no-session error, got %v", err)
	}
}

func TestResolveAuthSingleSession(t *testing.T) {
	cfg := &Config{Auth: []AuthConfig{{CloudGRPC: "prod:443", Certificates: []CertificateInfo{{OrganizationID: 7}}}}}
	auth, err := ResolveAuth(cfg, "", nil)
	if err != nil || auth.CloudGRPC != "prod:443" {
		t.Fatalf("single session should resolve, got %v / %v", auth, err)
	}
}

func TestResolveAuthSingleSessionNoCerts(t *testing.T) {
	cfg := &Config{Auth: []AuthConfig{{CloudGRPC: "prod:443"}}}
	if _, err := ResolveAuth(cfg, "", nil); err == nil || !strings.Contains(err.Error(), "no certificates") {
		t.Fatalf("want no-certificates error, got %v", err)
	}
}

func TestResolveAuthSingleTokenOnlySession(t *testing.T) {
	cfg := &Config{Auth: []AuthConfig{{CloudGRPC: "api.dev.wendy.sh:443", APIKey: "access-token"}}}
	auth, err := ResolveAuth(cfg, "", nil)
	if err != nil || auth.CloudGRPC != "api.dev.wendy.sh:443" {
		t.Fatalf("token-only session should resolve, got %v / %v", auth, err)
	}
}

func TestResolveAuthValidDefault(t *testing.T) {
	cfg := twoSessions()
	cfg.DefaultCloudGRPC = "localhost:50051"
	auth, err := ResolveAuth(cfg, "", nil)
	if err != nil || auth.CloudGRPC != "localhost:50051" {
		t.Fatalf("default should be used, got %v / %v", auth, err)
	}
}

func TestResolveAuthStaleDefaultFallsThrough(t *testing.T) {
	cfg := twoSessions()
	cfg.DefaultCloudGRPC = "gone:443"
	if _, err := ResolveAuth(cfg, "", nil); !errors.Is(err, ErrMultipleSessions) {
		t.Fatalf("stale default should fall through to ErrMultipleSessions, got %v", err)
	}
}

func TestResolveAuthMultipleNoPicker(t *testing.T) {
	err := func() error { _, e := ResolveAuth(twoSessions(), "", nil); return e }()
	if !errors.Is(err, ErrMultipleSessions) {
		t.Fatalf("want ErrMultipleSessions, got %v", err)
	}
	if !strings.Contains(err.Error(), "--cloud-grpc") {
		t.Fatalf("message must mention --cloud-grpc, got %v", err)
	}
}

func TestResolveAuthMultipleUsesPicker(t *testing.T) {
	cfg := twoSessions()
	called := false
	pick := func(c *Config) (*AuthConfig, error) { called = true; return &c.Auth[1], nil }
	auth, err := ResolveAuth(cfg, "", pick)
	if err != nil || !called || auth.CloudGRPC != "localhost:50051" {
		t.Fatalf("picker should be used, got %v / called=%v / %v", auth, called, err)
	}
}

func TestResolveAuthPickerResultCertValidated(t *testing.T) {
	cfg := twoSessions()
	cfg.Auth[1].Certificates = nil // picker returns a cert-less session
	pick := func(c *Config) (*AuthConfig, error) { return &c.Auth[1], nil }
	if _, err := ResolveAuth(cfg, "", pick); err == nil || !strings.Contains(err.Error(), "no certificates") {
		t.Fatalf("want no-certificates error from picker result, got %v", err)
	}
}

// sameEndpointOrgs models the common real-world shape: several orgs, all on
// the production cloud endpoint, in login order (org 9 first).
func sameEndpointOrgs() *Config {
	return &Config{Auth: []AuthConfig{
		{CloudDashboard: "https://cloud.wendy.dev", CloudGRPC: "prod:443", Certificates: []CertificateInfo{{OrganizationID: 9}}},
		{CloudDashboard: "https://cloud.wendy.dev", CloudGRPC: "prod:443", Certificates: []CertificateInfo{{OrganizationID: 2}}},
		{CloudDashboard: "https://cloud.wendy.dev", CloudGRPC: "prod:443", Certificates: []CertificateInfo{{OrganizationID: 75}}},
	}}
}

func TestResolveAuthFlagPrefersDefaultOrgOnSharedEndpoint(t *testing.T) {
	cfg := sameEndpointOrgs()
	cfg.DefaultOrgID = 75
	auth, err := ResolveAuth(cfg, "prod:443", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := auth.Certificates[0].OrganizationID; got != 75 {
		t.Fatalf("flag + default org must pick org 75, got org %d", got)
	}
}

func TestResolveAuthFlagFallsBackToFirstWithoutDefaultOrg(t *testing.T) {
	auth, err := ResolveAuth(sameEndpointOrgs(), "prod:443", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := auth.Certificates[0].OrganizationID; got != 9 {
		t.Fatalf("without a default org the first session wins, got org %d", got)
	}
}

func TestResolveAuthFlagIgnoresDefaultOrgFromOtherEndpoint(t *testing.T) {
	cfg := sameEndpointOrgs()
	cfg.Auth = append(cfg.Auth, AuthConfig{CloudGRPC: "dev:50051", Certificates: []CertificateInfo{{OrganizationID: 4}}})
	cfg.DefaultOrgID = 4 // default org lives on a different endpoint
	auth, err := ResolveAuth(cfg, "prod:443", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := auth.Certificates[0].OrganizationID; got != 9 {
		t.Fatalf("default org on another endpoint must not hijack the flag, got org %d", got)
	}
}

// The bug this file exists to pin down: `wendy auth use <org>` on a shared
// endpoint used to persist only DefaultCloudGRPC, which resolves to the FIRST
// session on that endpoint — the org the user selected was silently ignored.
// With DefaultOrgID persisted alongside, resolution honors the selection.
func TestResolveAuthDefaultOrgDisambiguatesSharedEndpoint(t *testing.T) {
	cfg := sameEndpointOrgs()
	cfg.DefaultCloudGRPC = "prod:443"
	cfg.DefaultOrgID = 75
	auth, err := ResolveAuth(cfg, "", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := auth.Certificates[0].OrganizationID; got != 75 {
		t.Fatalf("default org must disambiguate, got org %d", got)
	}
}

func TestDefaultAuthLookup(t *testing.T) {
	cfg := twoSessions()
	if _, ok := cfg.DefaultAuth(); ok {
		t.Fatal("no default set should return ok=false")
	}
	cfg.DefaultCloudGRPC = "prod:443"
	a, ok := cfg.DefaultAuth()
	if !ok || a.CloudGRPC != "prod:443" {
		t.Fatalf("want prod session, got %v / %v", a, ok)
	}
	cfg.DefaultCloudGRPC = "gone:443"
	if _, ok := cfg.DefaultAuth(); ok {
		t.Fatal("stale default should return ok=false")
	}
}

func TestResolveAuthPrefersOperatorSessionOverLegacyDuplicate(t *testing.T) {
	legacy := AuthConfig{
		CloudGRPC:    "api.dev.wendy.sh:443",
		APIKey:       "legacy-token",
		Certificates: []CertificateInfo{{OrganizationID: 0}},
	}
	operator := AuthConfig{
		CloudGRPC:    "api.dev.wendy.sh:443",
		APIKey:       "operator-token",
		OAuthIssuer:  "https://auth.dev.wendy.sh/realms/acme",
		Certificates: []CertificateInfo{{OrganizationID: 0, PrincipalURI: "spiffe://wendy.sh/tenant/tenant/operator/user"}},
	}
	cfg := &Config{
		DefaultCloudGRPC: "api.dev.wendy.sh:443",
		Auth:             []AuthConfig{legacy, operator},
	}

	auth, err := ResolveAuth(cfg, "", nil)
	if err != nil {
		t.Fatalf("ResolveAuth: %v", err)
	}
	if auth.OAuthIssuer != operator.OAuthIssuer {
		t.Fatalf("resolved legacy duplicate instead of operator session: %#v", auth)
	}

	auth, err = ResolveAuth(cfg, "api.dev.wendy.sh:443", nil)
	if err != nil {
		t.Fatalf("ResolveAuth with endpoint: %v", err)
	}
	if auth.OAuthIssuer != operator.OAuthIssuer {
		t.Fatalf("endpoint resolved legacy duplicate instead of operator session: %#v", auth)
	}
}

func TestResolveAuthDefaultOrgPrefersOperatorSessionOverLegacyDuplicate(t *testing.T) {
	cfg := &Config{
		DefaultOrgID: 7,
		Auth: []AuthConfig{
			{CloudGRPC: "api.dev.wendy.sh:443", Certificates: []CertificateInfo{{OrganizationID: 7}}},
			{CloudGRPC: "api.dev.wendy.sh:443", OAuthIssuer: "https://auth.dev.wendy.sh/realms/acme", Certificates: []CertificateInfo{{OrganizationID: 7}}},
		},
	}
	auth, err := ResolveAuth(cfg, "", nil)
	if err != nil {
		t.Fatalf("ResolveAuth: %v", err)
	}
	if auth.OAuthIssuer == "" {
		t.Fatal("default org resolved legacy duplicate instead of operator session")
	}
}

func TestResolveAuthPreferenceDoesNotChangeSelectedOrg(t *testing.T) {
	cfg := &Config{Auth: []AuthConfig{
		{CloudGRPC: "prod:443", Certificates: []CertificateInfo{{OrganizationID: 9}}},
		{CloudGRPC: "prod:443", OAuthIssuer: "https://auth.wendy.sh/realms/other", Certificates: []CertificateInfo{{OrganizationID: 2}}},
	}}
	auth, err := ResolveAuth(cfg, "prod:443", nil)
	if err != nil {
		t.Fatalf("ResolveAuth: %v", err)
	}
	if got := auth.Certificates[0].OrganizationID; got != 9 {
		t.Fatalf("operator preference changed selected org to %d, want first org 9", got)
	}
}
