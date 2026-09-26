package commands

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestEnrollmentOrganizationPicker(t *testing.T) {
	for _, tc := range []struct {
		name        string
		endpoint    string
		interactive bool
		json        bool
		override    int32
		noDefault   bool
		wantPicker  bool
		wantCount   int
		wantErr     bool
	}{
		{name: "default still prompts", interactive: true, wantPicker: true, wantCount: 3},
		{name: "endpoint filters organizations", interactive: true, endpoint: "prod:443", wantPicker: true, wantCount: 2},
		{name: "single organization still prompts", interactive: true, endpoint: "dev:443", wantPicker: true, wantCount: 1},
		{name: "no default", interactive: true, noDefault: true, wantPicker: true, wantCount: 3},
		{name: "non-interactive uses default"},
		{name: "JSON uses default", interactive: true, json: true},
		{name: "explicit legacy organization", interactive: true, override: 42},
		{name: "non-interactive needs default", noDefault: true, wantErr: true},
		{name: "JSON without default never prompts", interactive: true, json: true, noDefault: true, wantErr: true},
		{name: "unknown endpoint", interactive: true, endpoint: "missing:443", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withStubbedReloginDeps(t, tc.interactive, tc.json, false, nil)
			cfg := &config.Config{CurrentContext: "default", Auth: []config.AuthConfig{
				{Name: "other", CloudGRPC: "prod:443", Certificates: []config.CertificateInfo{{OrganizationID: 7}}},
				{Name: "default", CloudGRPC: "prod:443", Certificates: []config.CertificateInfo{{OrganizationID: 9}}},
				{Name: "dev", CloudGRPC: "dev:443", Certificates: []config.CertificateInfo{{OrganizationID: 1}}},
			}}
			if tc.noDefault {
				cfg.CurrentContext = ""
			}
			load := seedConfig(t, cfg)
			orig := pickAuthSessionFn
			t.Cleanup(func() { pickAuthSessionFn = orig })
			called := false
			var pickedKey string
			pickAuthSessionFn = func(choices *config.Config) (*config.AuthConfig, error) {
				called = true
				if !tc.wantPicker {
					t.Fatal("unexpected picker")
				}
				if len(choices.Auth) != tc.wantCount || choices.CurrentContext != cfg.CurrentContext {
					t.Fatalf("picker lost organizations or saved default: %+v", choices)
				}
				for _, auth := range choices.Auth {
					if tc.endpoint != "" && auth.CloudGRPC != tc.endpoint {
						t.Fatalf("picker included endpoint %q", auth.CloudGRPC)
					}
				}
				pickedKey = authSessionKey(&choices.Auth[0])
				return &choices.Auth[0], nil
			}
			selected, err := resolveEnrollmentAuthEntry(tc.endpoint, tc.override)
			if (err != nil) != tc.wantErr || called != tc.wantPicker {
				t.Fatalf("picker called=%v, err=%v", called, err)
			}
			if err == nil {
				wantKey := pickedKey
				if !tc.wantPicker {
					wantKey = authSessionKey(&cfg.Auth[1])
				}
				if authSessionKey(selected) != wantKey {
					t.Fatalf("selected %q, want %q", authSessionKey(selected), wantKey)
				}
			}
			if got := load().CurrentContext; got != cfg.CurrentContext {
				t.Fatalf("one-time selection changed default to %q", got)
			}
		})
	}
}

func TestEnrollmentOrganizationCancellationStopsBeforeDeviceAccess(t *testing.T) {
	withStubbedReloginDeps(t, true, false, false, nil)
	auth := oidcEnrollmentAuth(t)
	auth.Name = "default"
	seedConfig(t, &config.Config{CurrentContext: "default", Auth: []config.AuthConfig{*auth}})
	orig := pickAuthSessionFn
	t.Cleanup(func() { pickAuthSessionFn = orig })
	calls := 0
	pickAuthSessionFn = func(*config.Config) (*config.AuthConfig, error) {
		calls++
		return nil, ErrUserCancelled
	}
	for _, cmd := range []*cobra.Command{newDeviceEnrollCmd(), newCloudEnrollDeviceCmd()} {
		cmd.SetContext(context.Background())
		if err := cmd.RunE(cmd, nil); !errors.Is(err, ErrUserCancelled) {
			t.Fatalf("%s did not stop after cancellation: %v", cmd.Name(), err)
		}
	}
	if calls != 2 {
		t.Fatalf("picker called %d times, want once per command", calls)
	}
}

func expiredEnrollmentAuth(t *testing.T) *config.AuthConfig {
	t.Helper()
	auth := oidcEnrollmentAuth(t)
	auth.Certificates[0].PemCertificate = testCertPEM(t, time.Now().Add(-time.Hour))
	return auth
}

func stubEnrollmentLogin(t *testing.T, login func(context.Context, oidcLoginOptions) error) {
	t.Helper()
	orig := enrollmentOIDCLoginFn
	enrollmentOIDCLoginFn = login
	t.Cleanup(func() { enrollmentOIDCLoginFn = orig })
}

func TestEnrollmentExpiredCredentialsStopBeforeDeviceAccess(t *testing.T) {
	for _, oidc := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "OIDC"}[oidc], func(t *testing.T) {
			auth := expiredEnrollmentAuth(t)
			if !oidc {
				auth.OAuthIssuer = ""
			}
			withStubbedReloginDeps(t, false, false, false, nil)
			seedConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
			cmd := newDeviceEnrollCmd()
			// No device or name is supplied. Auth must fail first.
			err := cmd.RunE(cmd, nil)
			if !errors.Is(err, errCertExpired) {
				t.Fatalf("expected expiry before device access, got %v", err)
			}
			for _, want := range []string{"expired on", "wendy auth login --email <your-email>", "no logout is needed"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("missing %q in %v", want, err)
				}
			}
			alias := newCloudEnrollDeviceCmd()
			if err := alias.RunE(alias, nil); !errors.Is(err, errCertExpired) {
				t.Fatalf("cloud alias must also check expiry before device access: %v", err)
			}
			// Direct callers must also stop before dereferencing the device or
			// prompting for a name, regardless of enrollment protocol.
			if err := runEnrollDevice(context.Background(), nil, auth, "", 0); !errors.Is(err, errCertExpired) {
				t.Fatalf("direct enrollment: %v", err)
			}
		})
	}
}

func TestPrepareEnrollmentAuthDoesNotLoginWithoutInteractiveConsent(t *testing.T) {
	for _, tc := range []struct {
		name        string
		interactive bool
		json        bool
		confirm     bool
		legacy      bool
	}{
		{name: "non-interactive", confirm: true},
		{name: "JSON", interactive: true, json: true, confirm: true},
		{name: "declined", interactive: true},
		{name: "legacy needs email discovery", interactive: true, confirm: true, legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withStubbedReloginDeps(t, tc.interactive, tc.json, tc.confirm, nil)
			stubEnrollmentLogin(t, func(context.Context, oidcLoginOptions) error {
				t.Fatal("unexpected login")
				return nil
			})
			auth := expiredEnrollmentAuth(t)
			if tc.legacy {
				auth.OAuthIssuer = ""
			}
			if _, err := prepareEnrollmentAuth(context.Background(), auth); !errors.Is(err, errCertExpired) {
				t.Fatalf("expected actionable expiry error, got %v", err)
			}
		})
	}
}

func TestPrepareEnrollmentAuthOIDCRelogin(t *testing.T) {
	for _, outcome := range []string{"success", "login failed", "still expired", "different tenant"} {
		t.Run(outcome, func(t *testing.T) {
			withStubbedReloginDeps(t, true, false, true, nil)
			auth := expiredEnrollmentAuth(t)
			auth.OAuthClientID = "custom-client"
			auth.OAuthResource = "https://custom.example/api"
			auth.PKIResource = "https://custom.example/identity"
			auth.PKIEndpoint = "https://custom.example/certificate"
			seedConfig(t, &config.Config{Auth: []config.AuthConfig{*auth}})
			loginErr := errors.New("browser login cancelled")
			freshPEM := testCertPEM(t, time.Now().Add(time.Hour))
			key := struct{}{}
			ctx := context.WithValue(context.Background(), key, "caller")
			stubEnrollmentLogin(t, func(gotCtx context.Context, opts oidcLoginOptions) error {
				if gotCtx != ctx {
					t.Error("login must use the command context")
				}
				want := oidcLoginOptions{
					Issuer: auth.OAuthIssuer, ClientID: auth.OAuthClientID,
					CloudResource: auth.OAuthResource, IdentityResource: auth.PKIResource,
					IdentityEndpoint: auth.PKIEndpoint,
					CloudURL:         auth.CloudDashboard, CloudGRPC: auth.CloudGRPC,
				}
				if !reflect.DeepEqual(opts, want) {
					t.Fatalf("login options = %+v, want %+v", opts, want)
				}
				if outcome == "login failed" {
					return loginErr
				}
				fresh := *auth
				fresh.Certificates = append([]config.CertificateInfo(nil), auth.Certificates...)
				fresh.APIKey = "fresh-api-token"
				if outcome != "still expired" {
					fresh.Certificates[0].PemCertificate = freshPEM
				}
				other := fresh
				other.Certificates = append([]config.CertificateInfo(nil), fresh.Certificates...)
				other.Certificates[0].PrincipalURI = "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/other"
				entries := []config.AuthConfig{other, fresh}
				if outcome == "different tenant" {
					entries = entries[:1]
				}
				return config.Save(&config.Config{Auth: entries})
			})
			fresh, err := prepareEnrollmentAuth(ctx, auth)
			switch outcome {
			case "success":
				if err != nil {
					t.Fatal(err)
				}
				if fresh.APIKey != "fresh-api-token" || fresh.Certificates[0].PemCertificate != freshPEM || authSessionKey(fresh) != authSessionKey(auth) {
					t.Fatal("enrollment must use the new credentials for the selected tenant")
				}
			case "login failed":
				if !errors.Is(err, loginErr) {
					t.Fatalf("lost login error: %v", err)
				}
			case "still expired":
				if !errors.Is(err, errCertExpired) {
					t.Fatalf("expired credentials accepted: %v", err)
				}
			case "different tenant":
				if err == nil || !strings.Contains(err.Error(), "did not replace the selected") {
					t.Fatalf("must not switch tenants: %v", err)
				}
			}
		})
	}
}

func TestPrepareEnrollmentAuthPreservesRenewal(t *testing.T) {
	auth := oidcEnrollmentAuth(t)
	orig := ensureFreshCertificateFn
	t.Cleanup(func() { ensureFreshCertificateFn = orig })
	called := false
	ensureFreshCertificateFn = func(_ context.Context, got *config.AuthConfig) error {
		called = true
		if got != auth {
			t.Fatal("renewing a different session")
		}
		return nil
	}
	fresh, err := prepareEnrollmentAuth(context.Background(), auth)
	if err != nil || fresh != auth || !called {
		t.Fatalf("valid certificate must retain renewal preflight: called=%v, err=%v", called, err)
	}
}
