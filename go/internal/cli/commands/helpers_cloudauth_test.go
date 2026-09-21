package commands

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestIsUnauthenticatedCloudError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{
			name: "structured unauthorized cert error",
			err:  cloudCertError{code: cloudpb.CertificateErrorCode_CERTIFICATE_ERROR_UNAUTHORIZED, message: "no valid client certificate identity"},
			want: true,
		},
		{
			name: "structured unauthorized wrapped in %w",
			err:  fmt.Errorf("refreshing certificate: %w", cloudCertError{code: cloudpb.CertificateErrorCode_CERTIFICATE_ERROR_UNAUTHORIZED}),
			want: true,
		},
		{
			name: "structured cert error with a different code",
			err:  cloudCertError{code: cloudpb.CertificateErrorCode_CERTIFICATE_ERROR_INVALID_CSR},
			want: false,
		},
		{
			name: "gRPC Unauthenticated status",
			err:  status.Error(codes.Unauthenticated, "identity required"),
			want: true,
		},
		{
			name: "gRPC Unauthenticated wrapped in %w",
			err:  fmt.Errorf("listing devices: %w", status.Error(codes.Unauthenticated, "identity required")),
			want: true,
		},
		{
			name: "gRPC PermissionDenied is not unauthenticated",
			err:  status.Error(codes.PermissionDenied, "forbidden"),
			want: false,
		},
		{
			name: "plain error",
			err:  errors.New("connection refused"),
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isUnauthenticatedCloudError(tc.err); got != tc.want {
				t.Errorf("isUnauthenticatedCloudError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestLoginTargetsForAuth(t *testing.T) {
	t.Run("nil falls back to defaults", func(t *testing.T) {
		dash, grpc := loginTargetsForAuth(nil)
		if dash != defaultCloudDashboard || grpc != defaultCloudGRPC {
			t.Errorf("got (%q, %q), want defaults (%q, %q)", dash, grpc, defaultCloudDashboard, defaultCloudGRPC)
		}
	})
	t.Run("prefers the auth entry endpoints and adds a scheme", func(t *testing.T) {
		dash, grpc := loginTargetsForAuth(&config.AuthConfig{CloudDashboard: "cloud.example.com", CloudGRPC: "grpc.example.com:443"})
		if dash != "https://cloud.example.com" {
			t.Errorf("dashboard = %q, want https-prefixed", dash)
		}
		if grpc != "grpc.example.com:443" {
			t.Errorf("grpc = %q", grpc)
		}
	})
}

// withStubbedReloginDeps swaps the package vars the re-login flow depends on and
// restores them via t.Cleanup, so tests can drive the prompt deterministically.
func withStubbedReloginDeps(t *testing.T, interactive, json, confirm bool, login func(ctx context.Context, dashboard, grpc string) error) {
	t.Helper()
	origInteractive := isInteractiveTerminalFn
	origJSON := jsonOutput
	origConfirm := confirmFn
	origLogin := performLoginFn
	t.Cleanup(func() {
		isInteractiveTerminalFn = origInteractive
		jsonOutput = origJSON
		confirmFn = origConfirm
		performLoginFn = origLogin
	})
	isInteractiveTerminalFn = func() bool { return interactive }
	jsonOutput = json
	confirmFn = func(string) bool { return confirm }
	performLoginFn = login
}

func TestOfferReloginOnUnauthenticated(t *testing.T) {
	unauth := cloudCertError{code: cloudpb.CertificateErrorCode_CERTIFICATE_ERROR_UNAUTHORIZED}

	t.Run("non-unauthenticated error never prompts", func(t *testing.T) {
		called := false
		withStubbedReloginDeps(t, true, false, true, func(context.Context, string, string) error { called = true; return nil })
		if offerReloginOnUnauthenticated(context.Background(), nil, errors.New("boom")) {
			t.Error("expected false for a non-auth error")
		}
		if called {
			t.Error("login must not run for a non-auth error")
		}
	})

	t.Run("json mode does not prompt", func(t *testing.T) {
		called := false
		withStubbedReloginDeps(t, true, true, true, func(context.Context, string, string) error { called = true; return nil })
		if offerReloginOnUnauthenticated(context.Background(), nil, unauth) {
			t.Error("expected false in json mode")
		}
		if called {
			t.Error("login must not run in json mode")
		}
	})

	t.Run("non-interactive does not prompt", func(t *testing.T) {
		called := false
		withStubbedReloginDeps(t, false, false, true, func(context.Context, string, string) error { called = true; return nil })
		if offerReloginOnUnauthenticated(context.Background(), nil, unauth) {
			t.Error("expected false when non-interactive")
		}
		if called {
			t.Error("login must not run when non-interactive")
		}
	})

	t.Run("user declines the prompt", func(t *testing.T) {
		called := false
		withStubbedReloginDeps(t, true, false, false, func(context.Context, string, string) error { called = true; return nil })
		if offerReloginOnUnauthenticated(context.Background(), nil, unauth) {
			t.Error("expected false when the user declines")
		}
		if called {
			t.Error("login must not run when declined")
		}
	})

	t.Run("accepted and login succeeds", func(t *testing.T) {
		var gotDash, gotGRPC string
		withStubbedReloginDeps(t, true, false, true, func(_ context.Context, dash, grpc string) error {
			gotDash, gotGRPC = dash, grpc
			return nil
		})
		auth := &config.AuthConfig{CloudDashboard: "https://cloud.wendy.dev", CloudGRPC: "grpc.wendy.dev:443"}
		if !offerReloginOnUnauthenticated(context.Background(), auth, unauth) {
			t.Error("expected true after a successful re-login")
		}
		if gotDash != "https://cloud.wendy.dev" || gotGRPC != "grpc.wendy.dev:443" {
			t.Errorf("login targeted (%q, %q), want the auth entry's endpoints", gotDash, gotGRPC)
		}
	})

	t.Run("accepted but login fails", func(t *testing.T) {
		withStubbedReloginDeps(t, true, false, true, func(context.Context, string, string) error {
			return errors.New("browser flow cancelled")
		})
		if offerReloginOnUnauthenticated(context.Background(), nil, unauth) {
			t.Error("expected false when the login flow fails")
		}
	})
}

func TestReloadAuthEntryMatchesDashboardAndOrganization(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", t.TempDir())
	t.Setenv("WENDY_SECRET_STORE", "file")
	entry := func(dashboard string, org int, user string) config.AuthConfig {
		return config.AuthConfig{CloudDashboard: dashboard, CloudGRPC: "shared.example:443", Certificates: []config.CertificateInfo{{OrganizationID: org, UserID: user}}}
	}
	expected := entry("chosen.example", 42, "refreshed")
	cfg := &config.Config{Auth: []config.AuthConfig{entry("other.example", 42, "wrong-dashboard"), entry("chosen.example", 43, "wrong-org"), expected}}
	if err := config.Save(cfg); err != nil {
		t.Fatal(err)
	}
	previous := entry("chosen.example", 42, "stale")
	got := reloadAuthEntry(&previous)
	if got == nil || got.Certificates[0].UserID != "refreshed" {
		t.Fatalf("wrong reloaded identity: %+v", got)
	}
}
