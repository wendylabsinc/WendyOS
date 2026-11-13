package browserauth

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHostedMCPLoginRequiresVerifiedTenantPermission(t *testing.T) {
	for _, tc := range []struct {
		name        string
		allowMCP    any
		allowHosted any
	}{
		{"absent claims", nil, nil},
		{"MCP disabled", false, true},
		{"hosted MCP disabled", true, false},
		{"MCP string", "true", true},
		{"hosted MCP string", true, "true"},
		{"MCP number", 1, true},
		{"hosted MCP number", true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := setup(t, func(claims map[string]any) {
				if tc.allowMCP != nil {
					claims["allow_mcp"] = tc.allowMCP
				}
				if tc.allowHosted != nil {
					claims["allow_hosted_mcp"] = tc.allowHosted
				}
			})
			s.RequireHostedMCPPolicy = true
			store := &memoryStore{}
			s.Store = store
			original := s.Client
			pkiCalled := false
			s.Client = doerFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() == IdentityEndpoint {
					pkiCalled = true
				}
				return original.Do(r)
			})
			_, err := s.Complete(context.Background(), "valid-code", s.state, testIssuer)
			if err == nil || !strings.Contains(err.Error(), "does not allow hosted MCP") {
				t.Fatalf("expected hosted policy refusal, got %v", err)
			}
			if pkiCalled || len(store.raw) != 0 || s.tokens.Access != "" {
				t.Fatal("refused hosted login provisioned an identity or saved credentials")
			}
		})
	}
}

func TestHostedMCPLoginChecksCloudResourcePolicy(t *testing.T) {
	s, _ := setup(t, func(claims map[string]any) {
		claims["allow_mcp"] = true
		claims["allow_hosted_mcp"] = claims["aud"] == IdentityResource
	})
	s.RequireHostedMCPPolicy = true
	store := &memoryStore{}
	s.Store = store
	if _, err := s.Complete(context.Background(), "valid-code", s.state, testIssuer); err == nil || !strings.Contains(err.Error(), "does not allow hosted MCP") {
		t.Fatalf("Cloud resource token must also permit hosted MCP, got %v", err)
	}
	if len(store.raw) != 0 || s.tokens.Access != "" {
		t.Fatal("refused Cloud token was retained as an authenticated session")
	}
}

func TestHostedMCPRefreshRechecksTenantPolicy(t *testing.T) {
	allowed := true
	s, _ := setup(t, func(claims map[string]any) {
		claims["allow_mcp"] = true
		claims["allow_hosted_mcp"] = allowed
	})
	s.RequireHostedMCPPolicy = true
	store := &memoryStore{}
	s.Store = store
	if _, err := s.Complete(context.Background(), "valid-code", s.state, testIssuer); err != nil {
		t.Fatal(err)
	}
	oldAccess, oldSaved := s.tokens.Access, string(store.raw)
	allowed = false
	s.profile.Expires = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if err := s.refreshCloudToken(context.Background()); err == nil || !strings.Contains(err.Error(), "does not allow hosted MCP") {
		t.Fatalf("policy change was not enforced on refresh: %v", err)
	}
	if s.tokens.Access != oldAccess || string(store.raw) != oldSaved {
		t.Fatal("disallowed refreshed token replaced the previous credentials")
	}
}

func TestHostedMCPRestoreDoesNotTrustOldNonMCPLogin(t *testing.T) {
	for _, expired := range []bool{false, true} {
		name := "unexpired"
		if expired {
			name = "requires refresh"
		}
		t.Run(name, func(t *testing.T) {
			// Ordinary browser sign-in remains usable without MCP claims. The
			// same saved login cannot become a hosted gateway session.
			s, _ := setup(t, nil)
			store := &memoryStore{}
			s.Store = store
			if _, err := s.Complete(context.Background(), "valid-code", s.state, testIssuer); err != nil {
				t.Fatal(err)
			}
			if expired {
				s.profile.Expires = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
				if err := s.saveLocked(); err != nil {
					t.Fatal(err)
				}
			}
			next := &Session{Client: s.Client, Store: store, RequireHostedMCPPolicy: true}
			if _, err := next.Restore(context.Background()); err == nil || !strings.Contains(err.Error(), "does not allow hosted MCP") {
				t.Fatalf("old login bypassed hosted policy: %v", err)
			}
			if next.tokens.Access != "" || next.profile.Subject != "" {
				t.Fatal("failed restore exposed an authenticated in-memory identity")
			}
		})
	}
}
