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
	original, originalLegacy, originalDiscover := performOIDCLoginFn, performLoginFn, discoverOIDCIssuerFn
	t.Cleanup(func() {
		performOIDCLoginFn, performLoginFn, discoverOIDCIssuerFn = original, originalLegacy, originalDiscover
	})
	performLoginFn = func(context.Context, string, string) error {
		t.Fatal("Cloud login must not use the legacy dashboard flow")
		return nil
	}
	discoverOIDCIssuerFn = func(context.Context, string, string) (string, error) {
		t.Fatal("realm-less login must not discover an issuer before browser sign-in")
		return "", nil
	}
	var got oidcLoginOptions
	performOIDCLoginFn = func(_ context.Context, opts oidcLoginOptions) error {
		got = opts
		return nil
	}

	for _, tc := range []struct {
		name   string
		args   []string
		target cloudLoginTarget
	}{
		{"default", []string{}, productionCloudLoginTarget},
		{"production", []string{"--production"}, productionCloudLoginTarget},
		{"development", []string{"--development"}, developmentCloudLoginTarget},
		{"legacy disabled", []string{"--legacy=false"}, productionCloudLoginTarget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got = oidcLoginOptions{}
			cmd := newAuthLoginCmd()
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			cmd.SetArgs(tc.args)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			want := oidcLoginOptions{
				AuthorizationBase: tc.target.authBase,
				ClientID:          "wendy-cli",
				CloudResource:     tc.target.cloudResource,
				IdentityResource:  defaultPKIIdentityResource,
				IdentityEndpoint:  tc.target.identityEndpoint,
				CloudURL:          tc.target.cloudDashboard,
				CloudGRPC:         tc.target.cloudGRPC,
			}
			if got != want {
				t.Fatalf("OIDC options = %+v, want %+v", got, want)
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
		{name: "OIDC path defaults to production", target: productionCloudLoginTarget},
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

// --email with no pinned realm falls through to the realm-less universal flow:
// no issuer, the email as login_hint, and one line telling the user so.
func TestAuthLoginEmailWithoutPinnedRealmUsesUniversalFlow(t *testing.T) {
	t.Setenv(serviceAccountKeyEnv, "")
	originalDiscover, originalLogin := discoverOIDCIssuerFn, performOIDCLoginFn
	t.Cleanup(func() { discoverOIDCIssuerFn, performOIDCLoginFn = originalDiscover, originalLogin })
	discoverOIDCIssuerFn = func(context.Context, string, string) (string, error) { return "", nil }
	var got oidcLoginOptions
	performOIDCLoginFn = func(_ context.Context, opts oidcLoginOptions) error {
		got = opts
		return nil
	}

	cmd := newAuthLoginCmd()
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	cmd.SetArgs([]string{"--development", "--email", "person@example.com"})
	var err error
	out := captureStdout(t, func() { err = cmd.Execute() })
	if err != nil {
		t.Fatal(err)
	}
	if got.Issuer != "" || got.AuthorizationBase != developmentCloudLoginTarget.authBase || got.LoginHint != "person@example.com" {
		t.Fatalf("OIDC options = %+v, want the universal flow with the email as login hint", got)
	}
	if !strings.Contains(out, "No organization is pinned for person@example.com; continuing with the universal sign-in.") {
		t.Fatalf("missing fall-through notice, got:\n%s", out)
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

	for _, issuer := range []string{"", "https://auth.example/realms/acme"} {
		t.Run("issuer="+issuer, func(t *testing.T) {
			cmd := newAuthLoginCmd()
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			cmd.SetArgs([]string{
				"--issuer", issuer,
				"--auth", "https://auth.example",
				"--cloud", "https://cloud.example",
				"--cloud-grpc", "api.example:443",
				"--resource", "https://cloud.example/api",
				"--pki-resource", "https://pki.example/identity",
				"--pki-identity-endpoint", "https://identity.example/v1/identity/certificate",
			})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			want := oidcLoginOptions{
				Issuer: issuer, AuthorizationBase: "https://auth.example", ClientID: "wendy-cli",
				CloudURL: "https://cloud.example", CloudGRPC: "api.example:443",
				CloudResource: "https://cloud.example/api", IdentityResource: "https://pki.example/identity",
				IdentityEndpoint: "https://identity.example/v1/identity/certificate",
			}
			if got != want {
				t.Fatalf("OIDC options = %+v, want %+v", got, want)
			}
		})
	}
}

func TestAuthLoginExplicitLegacy(t *testing.T) {
	t.Setenv(serviceAccountKeyEnv, "")
	original := performLoginFn
	t.Cleanup(func() { performLoginFn = original })
	var dashboard, grpc string
	performLoginFn = func(_ context.Context, gotDashboard, gotGRPC string) error {
		dashboard, grpc = gotDashboard, gotGRPC
		return nil
	}

	for _, tc := range []struct {
		name      string
		args      []string
		dashboard string
		grpc      string
	}{
		{name: "default endpoints", args: []string{"--legacy"}, dashboard: defaultCloudDashboard, grpc: defaultCloudGRPC},
		{name: "custom endpoints", args: []string{"--legacy", "--cloud", "legacy.example", "--cloud-grpc", "grpc.legacy.example:443"}, dashboard: "https://legacy.example", grpc: "grpc.legacy.example:443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dashboard, grpc = "", ""
			cmd := newAuthLoginCmd()
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			cmd.SetArgs(tc.args)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if dashboard != tc.dashboard || grpc != tc.grpc {
				t.Fatalf("legacy target = %q %q, want %q %q", dashboard, grpc, tc.dashboard, tc.grpc)
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
		"auth":                  defaultProdAuthBase,
		"pki-identity-endpoint": defaultProdPKIIdentityEndpoint,
	} {
		if got := cmd.Flags().Lookup(name).DefValue; got != want {
			t.Errorf("--%s default = %q, want production default %q", name, got, want)
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
	for _, want := range []string{"Wendy Cloud in your browser", "saves the sign-in", "Pass --email"} {
		if !strings.Contains(cmd.Long, want) {
			t.Errorf("long help does not mention %q: %s", want, cmd.Long)
		}
	}
}
