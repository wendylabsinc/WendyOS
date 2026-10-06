package commands

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

// TestAuthLoginModeSelection covers target/mode conflicts before any network or
// credential operation is attempted.
func TestAuthLoginModeSelection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "target flags are mutually exclusive",
			args:    []string{"--production", "--development"},
			wantErr: "mutually exclusive",
		},
		{
			name:    "legacy conflicts with development",
			args:    []string{"--legacy", "--development"},
			wantErr: "mutually exclusive",
		},
		{
			name:    "legacy conflicts with email",
			args:    []string{"--legacy", "--email", "a@b.com"},
			wantErr: "--legacy selects the old cloud-dashboard login",
		},
		{
			name:    "legacy conflicts with issuer",
			args:    []string{"--legacy", "--issuer", "https://auth.example/realms/x"},
			wantErr: "--legacy selects the old cloud-dashboard login",
		},
		{
			name:    "api-key conflicts with oidc",
			args:    []string{"--api-key", "wnd_x", "--email", "a@b.com"},
			wantErr: "select different login modes",
		},
		{
			name:    "api-key conflicts with a built-in target",
			args:    []string{"--api-key", "wnd_x", "--development"},
			wantErr: "select different login modes",
		},
		{
			name:    "api-key needs cloud-grpc",
			args:    []string{"--api-key", "wnd_x"},
			wantErr: "--cloud-grpc is required",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(serviceAccountKeyEnv, "")
			cmd := newAuthLoginCmd()
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(tc.args)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("args %v: got err %v, want containing %q", tc.args, err, tc.wantErr)
			}
		})
	}
}

func TestAuthLoginTargetsStartRealmLess(t *testing.T) {
	t.Setenv(serviceAccountKeyEnv, "")
	original := performOIDCLoginFn
	t.Cleanup(func() { performOIDCLoginFn = original })
	var got oidcLoginOptions
	performOIDCLoginFn = func(_ context.Context, opts oidcLoginOptions) error {
		got = opts
		return nil
	}

	for _, tc := range []struct {
		flag   string
		target cloudLoginTarget
	}{
		{"--production", productionCloudLoginTarget},
		{"--development", developmentCloudLoginTarget},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			got = oidcLoginOptions{}
			cmd := newAuthLoginCmd()
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			cmd.SetArgs([]string{tc.flag})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if got.Issuer != "" || got.AuthorizationBase != tc.target.authBase {
				t.Fatalf("realm-less routing = issuer %q, base %q", got.Issuer, got.AuthorizationBase)
			}
			if got.CloudResource != tc.target.cloudResource || got.IdentityEndpoint != tc.target.identityEndpoint {
				t.Fatalf("target presets not reused: %+v", got)
			}
		})
	}
}

func TestAuthLoginCloudTargetDefaults(t *testing.T) {
	t.Setenv(serviceAccountKeyEnv, "")
	originalDiscover := discoverOIDCIssuerFn
	originalLogin := performOIDCLoginFn
	t.Cleanup(func() {
		discoverOIDCIssuerFn = originalDiscover
		performOIDCLoginFn = originalLogin
	})

	var gotAuthBase string
	discoverOIDCIssuerFn = func(_ context.Context, authBase, email string) (string, error) {
		gotAuthBase = authBase
		if email != "person@example.com" {
			t.Fatalf("email = %q", email)
		}
		return authBase + "/realms/acme", nil
	}
	var gotOptions oidcLoginOptions
	performOIDCLoginFn = func(_ context.Context, opts oidcLoginOptions) error {
		gotOptions = opts
		return nil
	}

	for _, tc := range []struct {
		name   string
		args   []string
		target cloudLoginTarget
	}{
		{name: "existing OIDC path defaults to development", target: developmentCloudLoginTarget},
		{name: "production explicitly", args: []string{"--production"}, target: productionCloudLoginTarget},
		{name: "development explicitly", args: []string{"--development"}, target: developmentCloudLoginTarget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotAuthBase = ""
			gotOptions = oidcLoginOptions{}
			cmd := newAuthLoginCmd()
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			cmd.SetArgs(append(tc.args, "--email", "person@example.com"))
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if gotAuthBase != tc.target.authBase {
				t.Errorf("auth base = %q, want %q", gotAuthBase, tc.target.authBase)
			}
			want := oidcLoginOptions{
				Issuer:            tc.target.authBase + "/realms/acme",
				AuthorizationBase: tc.target.authBase,
				ClientID:          "wendy-cli",
				CloudResource:     tc.target.cloudResource,
				IdentityResource:  defaultPKIIdentityResource,
				IdentityEndpoint:  tc.target.identityEndpoint,
				CloudURL:          tc.target.cloudDashboard,
				CloudGRPC:         tc.target.cloudGRPC,
			}
			if gotOptions != want {
				t.Fatalf("OIDC options = %+v, want %+v", gotOptions, want)
			}
		})
	}
}

func TestAuthLoginCustomOIDCEndpointsOverrideTargetDefaults(t *testing.T) {
	t.Setenv(serviceAccountKeyEnv, "")
	original := performOIDCLoginFn
	t.Cleanup(func() { performOIDCLoginFn = original })
	var got oidcLoginOptions
	performOIDCLoginFn = func(_ context.Context, opts oidcLoginOptions) error {
		got = opts
		return nil
	}

	cmd := newAuthLoginCmd()
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs([]string{
		"--issuer", "https://auth.example/realms/acme",
		"--cloud", "https://cloud.example",
		"--cloud-grpc", "api.example:443",
		"--resource", "https://cloud.example/api",
		"--pki-resource", "https://pki.example/identity",
		"--pki-identity-endpoint", "https://identity.example/v1/identity/certificate",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got.CloudURL != "https://cloud.example" || got.CloudGRPC != "api.example:443" ||
		got.CloudResource != "https://cloud.example/api" || got.IdentityResource != "https://pki.example/identity" ||
		got.IdentityEndpoint != "https://identity.example/v1/identity/certificate" {
		t.Fatalf("custom endpoints were not preserved: %+v", got)
	}
}

func TestAuthLoginDefaultsToLegacy(t *testing.T) {
	t.Setenv(serviceAccountKeyEnv, "")
	original := performLoginFn
	t.Cleanup(func() { performLoginFn = original })
	var dashboard, grpc string
	performLoginFn = func(_ context.Context, gotDashboard, gotGRPC string) error {
		dashboard, grpc = gotDashboard, gotGRPC
		return nil
	}

	for _, tc := range []struct {
		name string
		args []string
	}{
		{name: "implicit"},
		{name: "explicit", args: []string{"--legacy"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dashboard, grpc = "", ""
			cmd := newAuthLoginCmd()
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			cmd.SetArgs(tc.args)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if dashboard != defaultCloudDashboard || grpc != defaultCloudGRPC {
				t.Fatalf("legacy target = %q %q, want %q %q", dashboard, grpc, defaultCloudDashboard, defaultCloudGRPC)
			}
		})
	}
}

func TestAuthLoginTargetFlagsStayHidden(t *testing.T) {
	cmd := newAuthLoginCmd()
	for _, name := range []string{"production", "development", "legacy"} {
		flag := cmd.Flags().Lookup(name)
		if flag == nil {
			t.Fatalf("--%s flag is missing", name)
		}
		if flag.DefValue != "false" {
			t.Fatalf("--%s default = %q, want false", name, flag.DefValue)
		}
		if !flag.Hidden {
			t.Fatalf("--%s must stay hidden during the Cloud transition", name)
		}
	}
	for name, want := range map[string]string{
		"auth":                  defaultDevAuthBase,
		"pki-identity-endpoint": defaultDevPKIIdentityEndpoint,
	} {
		if got := cmd.Flags().Lookup(name).DefValue; got != want {
			t.Errorf("--%s default = %q, want existing development default %q", name, got, want)
		}
	}

	var help bytes.Buffer
	cmd.SetOut(&help)
	if err := cmd.Help(); err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{"--production", "--development", "--legacy"} {
		if strings.Contains(help.String(), hidden) || strings.Contains(cmd.Long, hidden) {
			t.Errorf("user-facing help exposes transitional flag %s", hidden)
		}
	}
	for _, want := range []string{"dashboard flow by default", "For the OIDC flow, pass --email"} {
		if !strings.Contains(cmd.Long, want) {
			t.Errorf("long help does not mention %q: %s", want, cmd.Long)
		}
	}
}
