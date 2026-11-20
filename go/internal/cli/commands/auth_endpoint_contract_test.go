package commands

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The old dashboard URL remains supported only through explicit --legacy.
// Assert literal supported authorities: comparing to the same implementation
// constants would also pass if a URL accidentally reverted.
func TestAuthEndpointContract(t *testing.T) {
	t.Setenv(serviceAccountKeyEnv, "")
	oldOIDC, oldLegacy := performOIDCLoginFn, performLoginFn
	t.Cleanup(func() { performOIDCLoginFn, performLoginFn = oldOIDC, oldLegacy })
	for _, tc := range []struct {
		name         string
		args         []string
		oidc, legacy string
	}{
		{"default", nil, "https://auth.wendy.dev", ""},
		{"production", []string{"--production"}, "https://auth.wendy.dev", ""},
		{"development", []string{"--development"}, "https://auth.dev.wendy.sh", ""},
		{"legacy", []string{"--legacy"}, "", "https://cloud.wendy.sh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oidc, legacy := "", ""
			performOIDCLoginFn = func(_ context.Context, o oidcLoginOptions) error { oidc = o.AuthorizationBase; return nil }
			performLoginFn = func(_ context.Context, dashboard, _ string) error { legacy = dashboard; return nil }
			cmd := newAuthLoginCmd()
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SetArgs(tc.args)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			if oidc != tc.oidc || legacy != tc.legacy {
				t.Fatalf("OIDC=%q legacy=%q; want %q %q", oidc, legacy, tc.oidc, tc.legacy)
			}
		})
	}
	results := make(chan oidcCallbackResult, 1)
	server := httptest.NewServer(oidcCallbackHandler("bound-state", signedInURL(defaultProdAuthBase), results))
	defer server.Close()
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Get(server.URL + "/callback?code=fixture-code&state=bound-state&iss=https%3A%2F%2Fauth.wendy.dev%2Frealms%2Facme")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusSeeOther || response.Header.Get("Location") != "https://auth.wendy.dev/signed-in" {
		t.Fatalf("callback status=%d redirect=%q", response.StatusCode, response.Header.Get("Location"))
	}
	select {
	case result := <-results:
		if result.Err != nil || result.Code != "fixture-code" {
			t.Fatalf("callback=%+v", result)
		}
	default:
		t.Fatal("callback did not deliver the authorization code")
	}
}
