package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

type GatewayHTTPConfig struct {
	ResourceURL       string                    `json:"resource_url"`
	OAuth             *GatewayOAuthConfig       `json:"oauth,omitempty"`
	DevelopmentTokens []GatewayDevelopmentToken `json:"development_tokens,omitempty"`
}

type GatewayOAuthConfig struct {
	Issuer           string `json:"issuer"`
	IntrospectionURL string `json:"introspection_url"`
	ClientID         string `json:"client_id"`
	ClientSecretEnv  string `json:"client_secret_env"`
}

type GatewayDevelopmentToken struct {
	Subject  string `json:"subject"`
	TokenEnv string `json:"token_env"`
}

type gatewayAuthenticate func(context.Context, string) (gatewayPrincipal, error)

// Bind even stateless transport tasks to the authenticated subject. mcp-go's
// default anonymous HTTP sessions do not provide principal isolation.
type gatewaySession struct{ subject string }

func (g gatewaySession) Initialize()                                           {}
func (g gatewaySession) Initialized() bool                                     { return false }
func (g gatewaySession) NotificationChannel() chan<- mcpgo.JSONRPCNotification { return nil }
func (g gatewaySession) SessionID() string                                     { return "principal:" + g.subject }

type gatewayTaskContextKey struct{}

// HTTPHandler refuses to start without explicit authentication. The caller must
// put this handler behind TLS at resource_url; localhost is only for development.
func (g *RobotGateway) HTTPHandler() (http.Handler, error) {
	return g.httpHandler(&http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, os.Getenv)
}

func gatewayHTTPS(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("expected an HTTPS URL without credentials, query, or fragment")
	}
	return u, nil
}

func (g *RobotGateway) httpHandler(client *http.Client, getenv func(string) string) (http.Handler, error) {
	cfg := g.cfg.HTTP
	if cfg == nil {
		return nil, fmt.Errorf("HTTP requires http configuration and authentication")
	}
	u, err := url.Parse(cfg.ResourceURL)
	if err != nil || u.Host == "" || u.Path != "/mcp" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("resource_url must be an absolute URL ending in /mcp")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1") && cfg.OAuth == nil) {
		return nil, fmt.Errorf("resource_url requires HTTPS except for local development tokens")
	}
	if (cfg.OAuth == nil) == (len(cfg.DevelopmentTokens) == 0) {
		return nil, fmt.Errorf("configure exactly one HTTP authentication mode: oauth or development_tokens")
	}
	origin := u.Scheme + "://" + u.Host
	metadataURL := origin + "/.well-known/oauth-protected-resource/mcp"
	metadata := map[string]any{"resource": cfg.ResourceURL, "scopes_supported": robotGatewayScopes, "bearer_methods_supported": []string{"header"}}
	var authenticate gatewayAuthenticate
	if cfg.OAuth != nil {
		authenticate, err = gatewayIntrospection(*cfg.OAuth, cfg.ResourceURL, client, getenv)
		if err != nil {
			return nil, err
		}
		metadata["authorization_servers"] = []string{cfg.OAuth.Issuer}
	} else {
		type credential struct {
			hash    [32]byte
			subject string
		}
		var credentials []credential
		for _, token := range cfg.DevelopmentTokens {
			secret := getenv(token.TokenEnv)
			if len(secret) < 32 {
				return nil, fmt.Errorf("development token environment variable %s must contain at least 32 bytes", token.TokenEnv)
			}
			known := false
			for _, grant := range g.cfg.Grants {
				if grant.Subject == token.Subject {
					known = true
				}
			}
			if !known {
				return nil, fmt.Errorf("development token must identify a configured subject")
			}
			hash := sha256.Sum256([]byte(secret))
			for _, old := range credentials {
				if old.hash == hash {
					return nil, fmt.Errorf("development tokens must be distinct")
				}
			}
			credentials = append(credentials, credential{hash, token.Subject})
		}
		authenticate = func(_ context.Context, token string) (gatewayPrincipal, error) {
			hash := sha256.Sum256([]byte(token))
			for _, c := range credentials {
				if subtle.ConstantTimeCompare(hash[:], c.hash[:]) == 1 {
					return gatewayPrincipal{c.subject, robotGatewayScopes}, nil
				}
			}
			return gatewayPrincipal{}, fmt.Errorf("invalid token")
		}
	}
	transport := server.NewStreamableHTTPServer(g.protocol, server.WithStateLess(true), server.WithDisableStreaming(true), server.WithHTTPContextFunc(func(ctx context.Context, r *http.Request) context.Context {
		p := r.Context().Value(gatewayPrincipalKey{}).(gatewayPrincipal)
		ctx = context.WithValue(ctx, gatewayPrincipalKey{}, p)
		if r.Context().Value(gatewayTaskContextKey{}) == true {
			ctx = context.WithoutCancel(ctx)
		}
		return g.protocol.WithContext(ctx, gatewaySession{p.Subject})
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "ok\n")
			return
		}
		if cfg.OAuth != nil && (r.URL.Path == "/.well-known/oauth-protected-resource/mcp" || r.URL.Path == "/.well-known/oauth-protected-resource") && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(metadata)
			return
		}
		if r.URL.Path != "/mcp" {
			http.NotFound(w, r)
			return
		}
		// ChatGPT calls server-side. Reject unrelated browser origins and do not
		// infer a trusted origin from forwarded headers or the request Host.
		if requestOrigin := r.Header.Get("Origin"); requestOrigin != "" && requestOrigin != origin {
			http.Error(w, "origin not allowed", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		parts := strings.Fields(r.Header.Get("Authorization"))
		var principal gatewayPrincipal
		var authErr error
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || len(parts[1]) > 16384 {
			authErr = fmt.Errorf("missing bearer token")
		} else {
			principal, authErr = authenticate(r.Context(), parts[1])
		}
		if authErr != nil {
			challenge := `Bearer`
			if cfg.OAuth != nil {
				challenge += fmt.Sprintf(` resource_metadata=%q`, metadataURL)
			}
			w.Header().Set("WWW-Authenticate", challenge)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		ctx := context.WithValue(r.Context(), gatewayPrincipalKey{}, principal)
		if grant, _ := g.grant(ctx); grant == nil {
			http.Error(w, "account has no robot grant", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "request too large", http.StatusRequestEntityTooLarge)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		var envelope struct {
			Method string `json:"method"`
			Params struct {
				Name string          `json:"name"`
				Task json.RawMessage `json:"task"`
			} `json:"params"`
		}
		if json.Unmarshal(raw, &envelope) == nil {
			if strings.HasPrefix(envelope.Method, "tasks/") && !g.hasScope(ctx, RobotEventsScope) {
				http.Error(w, "event scope required", http.StatusForbidden)
				return
			}
			if envelope.Method == "tools/call" && envelope.Params.Name == "wait_for_device_event" && len(envelope.Params.Task) > 0 && string(envelope.Params.Task) != "null" {
				ctx = context.WithValue(ctx, gatewayTaskContextKey{}, true)
			}
		}
		transport.ServeHTTP(w, r.WithContext(ctx))
	}), nil
}

func gatewayIntrospection(cfg GatewayOAuthConfig, resource string, client *http.Client, getenv func(string) string) (gatewayAuthenticate, error) {
	issuer, err := gatewayHTTPS(cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oauth issuer: %w", err)
	}
	endpoint, err := gatewayHTTPS(cfg.IntrospectionURL)
	if err != nil {
		return nil, fmt.Errorf("oauth introspection endpoint: %w", err)
	}
	if issuer.Host != endpoint.Host {
		return nil, fmt.Errorf("introspection endpoint must share the configured issuer origin")
	}
	secret := getenv(cfg.ClientSecretEnv)
	if cfg.ClientID == "" || secret == "" {
		return nil, fmt.Errorf("OAuth introspection client and secret environment variable are required")
	}
	return func(ctx context.Context, token string) (gatewayPrincipal, error) {
		form := url.Values{"token": {token}, "token_type_hint": {"access_token"}}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(form.Encode()))
		if err != nil {
			return gatewayPrincipal{}, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth(cfg.ClientID, secret)
		resp, err := client.Do(req)
		if err != nil {
			return gatewayPrincipal{}, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return gatewayPrincipal{}, fmt.Errorf("introspection failed")
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
		if err != nil || len(data) > 65536 {
			return gatewayPrincipal{}, fmt.Errorf("invalid introspection response")
		}
		var claims struct {
			Active       bool            `json:"active"`
			Subject      string          `json:"sub"`
			Issuer       string          `json:"iss"`
			Audience     json.RawMessage `json:"aud"`
			Expiry       int64           `json:"exp"`
			NotBefore    int64           `json:"nbf"`
			Scope        string          `json:"scope"`
			TokenType    string          `json:"token_type"`
			Confirmation json.RawMessage `json:"cnf"`
		}
		if err := json.Unmarshal(data, &claims); err != nil {
			return gatewayPrincipal{}, err
		}
		var aud string
		var audiences []string
		if json.Unmarshal(claims.Audience, &aud) == nil {
			audiences = []string{aud}
		} else {
			_ = json.Unmarshal(claims.Audience, &audiences)
		}
		now := time.Now().Unix()
		// Sender-constrained credentials cannot be accepted as ordinary bearer tokens.
		if !claims.Active || claims.Subject == "" || claims.Issuer != cfg.Issuer || !slices.Contains(audiences, resource) || claims.Expiry <= now || claims.NotBefore > now || !strings.EqualFold(claims.TokenType, "Bearer") || (len(claims.Confirmation) != 0 && string(claims.Confirmation) != "null") {
			return gatewayPrincipal{}, fmt.Errorf("invalid access token")
		}
		return gatewayPrincipal{claims.Subject, strings.Fields(claims.Scope)}, nil
	}, nil
}
