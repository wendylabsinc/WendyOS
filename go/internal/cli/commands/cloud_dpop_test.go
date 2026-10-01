//go:build darwin || linux || windows

package commands

import (
	"context"
	"testing"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// dpopBoundSession builds an OAuth session bound to a fresh ML-DSA DPoP key with
// an expiry far enough out that ensureOAuthAccessToken short-circuits (no
// network refresh).
func dpopBoundSession(t *testing.T) *config.AuthConfig {
	t.Helper()
	keyPEM, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		t.Fatalf("GenerateMLDSAKeyPair: %v", err)
	}
	return &config.AuthConfig{
		CloudGRPC:      "api.dev.wendy.sh:443",
		OAuthIssuer:    "https://auth.dev.wendy.sh/realms/acme",
		DPoPPrivateKey: keyPEM,
		APIKey:         "header.payload.sig-access-token",
		OAuthExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}
}

// cloudContext must not emit Bearer for a cnf-bound token (the DPoP interceptor
// carries it), but must still emit Bearer for an unbound API-key session.
func TestCloudContextBearerOnlyForUnboundSessions(t *testing.T) {
	t.Run("bound OAuth session: no Bearer", func(t *testing.T) {
		ctx, err := cloudContext(context.Background(), dpopBoundSession(t))
		if err != nil {
			t.Fatalf("cloudContext: %v", err)
		}
		md, _ := metadata.FromOutgoingContext(ctx)
		if az := md["authorization"]; len(az) != 0 {
			t.Fatalf("authorization = %v, want none (bound token goes via the DPoP interceptor)", az)
		}
	})
	t.Run("unbound API-key session: Bearer", func(t *testing.T) {
		ctx, err := cloudContext(context.Background(), &config.AuthConfig{CloudGRPC: "self.example:443", APIKey: "api-key-123"})
		if err != nil {
			t.Fatalf("cloudContext: %v", err)
		}
		md, _ := metadata.FromOutgoingContext(ctx)
		if az := md["authorization"]; len(az) != 1 || az[0] != "Bearer api-key-123" {
			t.Fatalf("authorization = %v, want [Bearer api-key-123]", az)
		}
	})
}

// dpopDialOptions installs the interceptor only for a bound session.
func TestDPoPDialOptionsWiring(t *testing.T) {
	if opts := dpopDialOptions(dpopBoundSession(t)); len(opts) != 2 {
		t.Fatalf("bound session got %d dial options, want 2", len(opts))
	}
	if opts := dpopDialOptions(&config.AuthConfig{CloudGRPC: "self:443", APIKey: "k"}); opts != nil {
		t.Fatalf("unbound session got %d dial options, want nil", len(opts))
	}
}

// The CLI provider returns the (short-circuit) access token and a usable bound
// key. The refresh-under-lock behaviour is covered by TestOAuthRefresh*.
func TestCLIDPoPTokenProvider(t *testing.T) {
	auth := dpopBoundSession(t)
	token, key, err := cliDPoPTokenProvider(auth)(context.Background())
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	if token != auth.APIKey {
		t.Errorf("token = %q, want the session access token", token)
	}
	if key == nil {
		t.Fatal("provider returned a nil signing key")
	}
}
