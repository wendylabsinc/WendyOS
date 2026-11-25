package chat

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// HostedProvider is the app's explicit BYOK boundary. No environment, device
// transport, custom endpoint, or fallback is consulted here.
func HostedProvider(provider, model, key string) (Provider, error) {
	c, err := hostedConfig(provider, model, key)
	if err != nil {
		return nil, err
	}
	if !validModelID(model) {
		return nil, fmt.Errorf("choose a cloud model")
	}
	return &httpProvider{config: c, client: hostedClient()}, nil
}

func HostedModels(ctx context.Context, provider, key string) ([]ModelOption, error) {
	c, err := hostedConfig(provider, "", key)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client := hostedClient()
	defer client.CloseIdleConnections()
	return listCompatibleModels(ctx, &httpProvider{config: c, client: client})
}

func hostedConfig(provider, model, key string) (Config, error) {
	if provider != "openai" && provider != "anthropic" {
		return Config{}, fmt.Errorf("choose OpenAI or Anthropic")
	}
	if strings.TrimSpace(key) == "" {
		return Config{}, fmt.Errorf("add your API key for this provider")
	}
	if len(key) > 8192 || strings.ContainsAny(key, "\r\n\x00") {
		return Config{}, fmt.Errorf("invalid API key")
	}
	return resolveConnection(Config{Provider: provider, Model: model, APIKey: key, BaseURL: defaultBaseURL(provider), MaxTokens: 4096}, false)
}

func hostedClient() *http.Client {
	// Direct HTTPS only; don't inherit a process proxy configured for a device.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{Transport: transport, Timeout: 15 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
