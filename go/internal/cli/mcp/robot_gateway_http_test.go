package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

type gatewayRoundTrip func(*http.Request) (*http.Response, error)

func (f gatewayRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRobotGatewayOAuthIntrospection(t *testing.T) {
	const issuer = "https://auth.example/realms/robots"
	const resource = "https://robots.example/mcp"
	valid := func() map[string]any {
		return map[string]any{"active": true, "sub": "alice", "iss": issuer, "aud": resource, "exp": time.Now().Add(time.Hour).Unix(), "scope": RobotReadScope, "token_type": "Bearer"}
	}
	claims := valid()
	calls := 0
	client := &http.Client{Transport: gatewayRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != issuer+"/introspect" {
			t.Fatal("unexpected credential recipient")
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != "gateway" || pass != "secret" {
			t.Fatal("missing introspection authentication")
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("token") != "opaque-token" {
			t.Fatal("missing access token")
		}
		body, _ := json.Marshal(claims)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}
	auth, err := gatewayIntrospection(GatewayOAuthConfig{Issuer: issuer, IntrospectionURL: issuer + "/introspect", ClientID: "gateway", ClientSecretEnv: "SECRET"}, resource, client, func(string) string { return "secret" })
	if err != nil {
		t.Fatal(err)
	}
	p, err := auth(context.Background(), "opaque-token")
	if err != nil || p.Subject != "alice" || len(p.Scopes) != 1 {
		t.Fatalf("valid token failed: %+v %v", p, err)
	}
	for _, tc := range []struct {
		name, key string
		value     any
	}{{"revoked", "active", false}, {"wrong issuer", "iss", "https://attacker.example"}, {"wrong audience", "aud", "https://api.example"}, {"expired", "exp", time.Now().Add(-time.Second).Unix()}, {"future", "nbf", time.Now().Add(time.Hour).Unix()}, {"no expiry", "exp", nil}, {"no subject", "sub", ""}, {"DPoP", "token_type", "DPoP"}, {"bound token", "cnf", map[string]any{"jkt": "key"}}, {"ID token", "token_type", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			claims = valid()
			claims[tc.key] = tc.value
			if _, err := auth(context.Background(), "opaque-token"); err == nil {
				t.Fatal("invalid token accepted")
			}
		})
	}
	claims = valid()
	claims["aud"] = []string{"other", resource}
	if _, err := auth(context.Background(), "opaque-token"); err != nil {
		t.Fatal("audience array rejected")
	}
	if calls != 12 {
		t.Fatalf("expected introspection on every request, got %d", calls)
	}
}

func TestRobotGatewayOAuthDiscoveryAndScopeIntersection(t *testing.T) {
	cfg := gatewayTestConfig()
	cfg.HTTP = &GatewayHTTPConfig{ResourceURL: "https://robots.example/mcp", OAuth: &GatewayOAuthConfig{Issuer: "https://auth.example/robots", IntrospectionURL: "https://auth.example/robots/introspect", ClientID: "gateway", ClientSecretEnv: "SECRET"}}
	var connects int
	g, err := NewRobotGateway(cfg, func(context.Context, string) (*grpcclient.AgentConnection, error) {
		connects++
		return nil, fmt.Errorf("should not connect")
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: gatewayRoundTrip(func(*http.Request) (*http.Response, error) {
		body, _ := json.Marshal(map[string]any{"active": true, "sub": "alice", "iss": cfg.HTTP.OAuth.Issuer, "aud": cfg.HTTP.ResourceURL, "exp": time.Now().Add(time.Hour).Unix(), "scope": RobotReadScope, "token_type": "Bearer"})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}
	h, err := g.httpHandler(client, func(string) string { return "secret" })
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/.well-known/oauth-protected-resource", "/.well-known/oauth-protected-resource/mcp"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 200 || !strings.Contains(w.Body.String(), cfg.HTTP.ResourceURL) {
			t.Fatal("discovery failed")
		}
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/mcp", strings.NewReader(`{}`)))
	if w.Code != 401 || !strings.Contains(w.Header().Get("WWW-Authenticate"), "resource_metadata=") {
		t.Fatal("missing OAuth challenge")
	}
	s := httptest.NewServer(h)
	defer s.Close()
	c := gatewayHTTPClient(t, s.URL, "opaque-token")
	result, err := c.CallTool(context.Background(), callToolReq("capture_robot_image", map[string]any{"robot_id": "alpha", "camera_id": 0}))
	if err != nil || !result.IsError || connects != 0 {
		t.Fatal("grant widened the access token scope")
	}
}

func TestRobotGatewayConfigFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*RobotGatewayConfig)
	}{
		{"duplicate robot", func(c *RobotGatewayConfig) { c.Robots[1].ID = c.Robots[0].ID }},
		{"unknown grant target", func(c *RobotGatewayConfig) { c.Grants[0].Robots = []string{"missing"} }},
		{"unknown scope", func(c *RobotGatewayConfig) { c.Grants[0].Scopes = []string{"shell:*"} }},
		{"unknown local subject", func(c *RobotGatewayConfig) { c.LocalSubject = "mallory" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := gatewayTestConfig()
			tc.change(&cfg)
			if _, err := NewRobotGateway(cfg, func(context.Context, string) (*grpcclient.AgentConnection, error) { return nil, nil }); err == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
	for _, raw := range []string{`{"robots":[],"allow_everything":true}`, `{} {}`, strings.Repeat(" ", (1<<20)+1)} {
		if _, err := DecodeRobotGatewayConfig(strings.NewReader(raw)); err == nil {
			t.Fatal("invalid JSON policy accepted")
		}
	}
	for _, mode := range []string{"none", "both", "insecure", "short-token"} {
		t.Run(mode, func(t *testing.T) {
			cfg := gatewayTestConfig()
			switch mode {
			case "none":
				cfg.HTTP = nil
			case "both":
				cfg.HTTP.OAuth = &GatewayOAuthConfig{}
			case "insecure":
				cfg.HTTP.ResourceURL = "http://public.example/mcp"
			}
			g, err := NewRobotGateway(cfg, func(context.Context, string) (*grpcclient.AgentConnection, error) { return nil, nil })
			if err != nil {
				t.Fatal(err)
			}
			env := gatewayTestEnv
			if mode == "short-token" {
				env = func(string) string { return "short" }
			}
			if _, err := g.httpHandler(http.DefaultClient, env); err == nil {
				t.Fatal("unsafe HTTP configuration accepted")
			}
		})
	}
}
