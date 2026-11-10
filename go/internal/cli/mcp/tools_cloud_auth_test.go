package mcp

import (
	"context"
	"crypto"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// tokenCheckingAssetServer is a v2 AssetService that, like Cloud, answers an
// access token it does not accept with Unauthenticated.
type tokenCheckingAssetServer struct {
	fakeAssetV2Server
	accept string

	mu   sync.Mutex
	seen []string
}

func (s *tokenCheckingAssetServer) ListAssets(req *cloudpbv2.ListAssetsRequest, stream cloudpbv2.AssetService_ListAssetsServer) error {
	md, _ := metadata.FromIncomingContext(stream.Context())
	authorization := strings.Join(md.Get("authorization"), ",")
	s.mu.Lock()
	s.seen = append(s.seen, authorization)
	s.mu.Unlock()
	if authorization != "DPoP "+s.accept {
		return status.Error(codes.Unauthenticated, "authentication required")
	}
	return s.fakeAssetV2Server.ListAssets(req, stream)
}

func startTokenCheckingAssetServer(t *testing.T, svc *tokenCheckingAssetServer) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	cloudpbv2.RegisterAssetServiceServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	return lis.Addr().String()
}

func (s *tokenCheckingAssetServer) authorizations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seen...)
}

// expiredOAuthSession is a v2 operator session whose DPoP-bound access token
// expired an hour ago, as config.json holds it once no MCP call has refreshed it.
func expiredOAuthSession(t *testing.T, cloudGRPC string) (*config.AuthConfig, crypto.Signer) {
	t.Helper()
	auth := v2TestAuth(t, cloudGRPC)
	auth.OAuthIssuer = "https://auth.example/realms/acme"
	auth.DPoPPrivateKey = auth.Certificates[0].PemPrivateKey
	auth.APIKey = "expired-token"
	auth.OAuthExpiresAt = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	key, err := certs.ParseSigningPrivateKeyPEM([]byte(auth.DPoPPrivateKey))
	if err != nil {
		t.Fatal(err)
	}
	return auth, key
}

func useDPoPTokenProvider(t *testing.T, fn func(*config.AuthConfig) cloudrequest.DPoPTokenProvider) {
	t.Helper()
	prev := dpopTokenProviderFn
	SetDPoPTokenProvider(fn)
	t.Cleanup(func() { dpopTokenProviderFn = prev })
}

// The CLI installs a provider that renews an expired access token. Cloud tools
// must send what it returns, not the token stored when the session was read.
func TestCloudDiscoverSendsTokenFromInstalledProvider(t *testing.T) {
	cloud := &tokenCheckingAssetServer{
		fakeAssetV2Server: fakeAssetV2Server{online: []*cloudpbv2.Asset{{Id: "11111111-1111-4111-8111-111111111111", Name: "edge-one", IsComputeDevice: true}}},
		accept:            "renewed-token",
	}
	auth, key := expiredOAuthSession(t, startTokenCheckingAssetServer(t, cloud))
	var calls int
	useDPoPTokenProvider(t, func(got *config.AuthConfig) cloudrequest.DPoPTokenProvider {
		return func(context.Context) (string, crypto.Signer, error) {
			calls++
			if got.APIKey != "expired-token" {
				t.Errorf("provider got session with token %q, want the stored one", got.APIKey)
			}
			return "renewed-token", key, nil
		}
	})
	s := New(&config.Config{Auth: []config.AuthConfig{*auth}}, nil)

	result, err := s.callTool(context.Background(), "cloud_discover", nil)
	if err != nil || result.IsError {
		t.Fatalf("cloud_discover: %v %v", toolResultText(t, result), err)
	}
	if rows := listPayload(t, result, "devices"); len(rows) != 1 {
		t.Fatalf("devices = %v, want edge-one", rows)
	}
	if calls == 0 {
		t.Fatal("installed provider was not used")
	}
	for _, got := range cloud.authorizations() {
		if got != "DPoP renewed-token" {
			t.Fatalf("Cloud saw authorization %q, want the renewed token", got)
		}
	}
}

// Cloud answers an expired token with Unauthenticated. That is a sign-in
// problem the agent can fix with auth_login, not an INTERNAL error.
func TestCloudDiscoverRejectedTokenIsAuthRequired(t *testing.T) {
	cloud := &tokenCheckingAssetServer{accept: "some-other-token"}
	auth, _ := expiredOAuthSession(t, startTokenCheckingAssetServer(t, cloud))
	s := New(&config.Config{Auth: []config.AuthConfig{*auth}}, nil)

	result, err := s.callTool(context.Background(), "cloud_discover", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Fatal("cloud_discover succeeded with a token Cloud rejects")
	}
	if code := structuredMap(t, result)["error_code"]; code != string(errCodeAuthRequired) {
		t.Fatalf("error_code = %v, want AUTH_REQUIRED: %s", code, toolResultText(t, result))
	}
	if text := toolResultText(t, result); !strings.Contains(text, "auth_login") || !strings.Contains(text, "sign in again") {
		t.Fatalf("message does not say to sign in again with auth_login: %q", text)
	}
}

// A session whose token cannot be renewed (the refresh token is gone or
// refused) must ask for a sign-in, naming why.
func TestCloudDiscoverFailedRenewalIsAuthRequired(t *testing.T) {
	cloud := &tokenCheckingAssetServer{accept: "renewed-token"}
	auth, _ := expiredOAuthSession(t, startTokenCheckingAssetServer(t, cloud))
	useDPoPTokenProvider(t, func(*config.AuthConfig) cloudrequest.DPoPTokenProvider {
		return func(context.Context) (string, crypto.Signer, error) {
			return "", nil, errors.New("refreshing OAuth session: token endpoint returned 400: invalid_grant")
		}
	})
	s := New(&config.Config{Auth: []config.AuthConfig{*auth}}, nil)

	result, err := s.callTool(context.Background(), "cloud_discover", nil)
	if err != nil {
		t.Fatal(err)
	}
	if code := structuredMap(t, result)["error_code"]; code != string(errCodeAuthRequired) {
		t.Fatalf("error_code = %v, want AUTH_REQUIRED: %s", code, toolResultText(t, result))
	}
	text := toolResultText(t, result)
	if !strings.Contains(text, "invalid_grant") || !strings.Contains(text, "auth_login") {
		t.Fatalf("message must give the cause and the auth_login step: %q", text)
	}
	if len(cloud.authorizations()) != 0 {
		t.Fatal("an RPC was sent without a usable token")
	}
}

// device_list keeps local devices when Cloud rejects the session, and its
// Cloud warning says the session needs a new sign-in.
func TestDeviceListWarnsWhenCloudRejectsSession(t *testing.T) {
	cloud := &tokenCheckingAssetServer{accept: "some-other-token"}
	auth, _ := expiredOAuthSession(t, startTokenCheckingAssetServer(t, cloud))
	s := New(&config.Config{Auth: []config.AuthConfig{*auth}}, nil)

	result, err := s.callTool(context.Background(), "device_list", nil)
	if err != nil || result.IsError {
		t.Fatalf("device_list: %v %v", result, err)
	}
	warnings := listPayload(t, result, "warnings")
	if len(warnings) != 1 || warnings[0]["source"] != "cloud" || warnings[0]["error_code"] != string(errCodeAuthRequired) {
		t.Fatalf("want one cloud AUTH_REQUIRED warning, got %v", warnings)
	}
	if msg, _ := warnings[0]["message"].(string); !strings.Contains(msg, "auth_login") {
		t.Fatalf("warning does not say how to sign in again: %q", msg)
	}
}
