package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/browserauth"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudlink"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

func linkedGatewayConfig(t *testing.T) RobotGatewayConfig {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return RobotGatewayConfig{StateDirectory: dir, HTTP: &GatewayHTTPConfig{ResourceURL: "https://gateway.example/mcp", CloudLink: &cloudlink.Config{Services: browserauth.Settings{AuthBase: "https://auth.example", ClientID: "hosted-login", IdentityResource: "https://pki.example/identity", IdentityEndpoint: "https://identity.example/v1/identity/certificate", CloudResource: "https://cloud.example/api", CloudGRPC: "api.example:443", RelayIssuer: "https://api.example"}, StateFile: filepath.Join(dir, "accounts.enc"), EncryptionKeyEnv: "KEY", Scopes: []string{RobotReadScope, RobotCameraScope}, Clients: []cloudlink.Client{{ID: "chatgpt", Name: "ChatGPT", RedirectURIs: []string{"https://chatgpt.example/callback"}}}}}}
}

func TestRobotGatewayCloudLinkMetadataAndNoOperatorFallback(t *testing.T) {
	cfg := linkedGatewayConfig(t)
	operatorCalls := 0
	g, err := NewRobotGateway(cfg, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		operatorCalls++
		t.Error("operator credentials used by linked mode")
		return nil, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	handler, err := g.httpHandler(&http.Client{}, func(string) string { return base64.StdEncoding.EncodeToString([]byte(strings.Repeat("k", 32))) })
	if err != nil {
		t.Fatal(err)
	}
	defer g.CloseCloudLink()
	for _, path := range []string{"/.well-known/oauth-protected-resource/mcp", "/.well-known/oauth-authorization-server"} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", "https://gateway.example"+path, nil))
		if w.Code != 200 {
			t.Fatal(path, w.Code, w.Body.String())
		}
		var metadata map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &metadata); err != nil {
			t.Fatal(err)
		}
		if path == "/.well-known/oauth-authorization-server" && metadata["issuer"] != "https://gateway.example" {
			t.Fatal(metadata)
		}
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest("POST", "https://gateway.example/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	if w.Code != 401 || !strings.Contains(w.Header().Get("WWW-Authenticate"), "resource_metadata=") {
		t.Fatal("missing OAuth resource challenge", w.Code, w.Header())
	}
	if _, err := g.connect(context.Background(), "local-device"); err == nil {
		t.Fatal("local connector accepted without a linked principal")
	}
	ctx := context.WithValue(context.Background(), gatewayPrincipalKey{}, gatewayPrincipal{Subject: "revoked-or-unknown", Scopes: []string{RobotReadScope}})
	if _, err := g.connect(ctx, "linked-cloud:00000000-0000-4000-8000-000000000001"); err == nil {
		t.Fatal("unknown account obtained a connection")
	}
	if operatorCalls != 0 {
		t.Fatal("host credential fallback occurred")
	}
}

func TestRobotGatewayCloudLinkRejectsLocalAndPrivilegeExpansion(t *testing.T) {
	for _, alter := range []func(*RobotGatewayConfig){
		func(c *RobotGatewayConfig) { c.LocalSubject = "host" },
		func(c *RobotGatewayConfig) {
			c.Robots = []GatewayRobot{{ID: "robot", Name: "Local", Device: "localhost:50052"}}
		},
		func(c *RobotGatewayConfig) {
			c.Grants = []GatewayGrant{{Subject: "host", Scopes: []string{RobotReadScope}}}
		},
		func(c *RobotGatewayConfig) { c.AllowHostOperations = true },
		func(c *RobotGatewayConfig) { c.AllowSimulators = true },
		func(c *RobotGatewayConfig) {
			c.HTTP.CloudLink.Scopes = append(c.HTTP.CloudLink.Scopes, RobotProjectScope)
		},
		func(c *RobotGatewayConfig) {
			c.HTTP.CloudLink.Scopes = append(c.HTTP.CloudLink.Scopes, RobotEventsScope)
		},
		func(c *RobotGatewayConfig) { c.StateDirectory = "" },
	} {
		cfg := linkedGatewayConfig(t)
		alter(&cfg)
		if err := cfg.validate(); err == nil {
			t.Fatal("Cloud account linking accepted local access or unsupported scopes")
		}
	}
}
