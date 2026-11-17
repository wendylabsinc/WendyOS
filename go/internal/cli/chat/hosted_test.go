package chat

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestHostedProviderIsolatesCredentialsAndEndpoints(t *testing.T) {
	t.Setenv("OPENAI_BASE_URL", "http://device.invalid/v1")
	t.Setenv("WENDY_CHAT_BASE_URL", "http://device.invalid/v1")
	t.Setenv("WENDY_CHAT_API_KEY", "environment-key-must-not-leak")
	t.Setenv("WENDY_CHAT_MODEL", "environment-model")
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			p, err := HostedProvider(provider, "selected-model", "user-key")
			if err != nil {
				t.Fatal(err)
			}
			h := p.(*httpProvider)
			if h.config.BaseURL != defaultBaseURL(provider) || h.config.Model != "selected-model" || h.config.APIKey != "user-key" {
				t.Fatal("hosted configuration inherited unrelated settings")
			}
			if h.client.Transport.(*http.Transport).Proxy != nil {
				t.Fatal("inherited process proxy")
			}
			h.client.Transport = providerRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				wantPath, wantHost := "/v1/responses", "api.openai.com"
				if provider == "anthropic" {
					wantPath, wantHost = "/v1/messages", "api.anthropic.com"
				}
				if r.URL.Scheme != "https" || r.URL.Host != wantHost || r.URL.Path != wantPath {
					t.Fatalf("wrong cloud endpoint %s", r.URL)
				}
				if provider == "openai" {
					if r.Header.Get("Authorization") != "Bearer user-key" || r.Header.Get("x-api-key") != "" {
						t.Fatal("wrong OpenAI auth")
					}
				} else if r.Header.Get("x-api-key") != "user-key" || r.Header.Get("Authorization") != "" {
					t.Fatal("wrong Anthropic auth")
				}
				return &http.Response{StatusCode: 401, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"invalid user-key"}}`))}, nil
			})
			_, err = p.Stream(context.Background(), []Message{{Role: "user", Content: "hello"}}, nil, nil)
			if err == nil || strings.Contains(err.Error(), "user-key") || !strings.Contains(err.Error(), "401") {
				t.Fatalf("unsafe or missing error: %v", err)
			}
			if err := h.client.CheckRedirect(&http.Request{}, nil); err != http.ErrUseLastResponse {
				t.Fatal("credentials could follow redirects")
			}
		})
	}
	for _, provider := range []string{"local", "ollama", "", "https://example.com"} {
		if _, err := HostedProvider(provider, "model", "key"); err == nil {
			t.Fatalf("accepted provider %q", provider)
		}
	}
	if _, err := HostedProvider("openai", "model", ""); err == nil {
		t.Fatal("inherited environment API key")
	}
	if _, err := HostedProvider("openai", "", "key"); err == nil {
		t.Fatal("inherited environment model")
	}
}

func TestHostedModelDiscoveryUsesProviderAuthAndFiltersNonChatModels(t *testing.T) {
	for _, provider := range []string{"openai", "anthropic"} {
		c, _ := hostedConfig(provider, "", "model-list-key")
		client := hostedClient()
		client.Transport = providerRoundTripFunc(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() != defaultBaseURL(provider)+"/models" {
				t.Fatalf("wrong model list endpoint %s", r.URL)
			}
			if provider == "anthropic" && r.Header.Get("x-api-key") != "model-list-key" {
				t.Fatal("missing Anthropic auth")
			}
			if provider == "openai" && r.Header.Get("Authorization") != "Bearer model-list-key" {
				t.Fatal("missing OpenAI auth")
			}
			id := "gpt-6-astra"
			if provider == "anthropic" {
				id = "claude-sonnet-4-5"
			}
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(fmt.Sprintf(`{"data":[{"id":%q,"display_name":"Available chat model"},{"id":"text-embedding-3-small"}]}`, id)))}, nil
		})
		models, err := listCompatibleModels(context.Background(), &httpProvider{config: c, client: client})
		if err != nil || len(models) != 1 || models[0].Name != "Available chat model" {
			t.Fatalf("model discovery: %v %v", models, err)
		}
	}
}
