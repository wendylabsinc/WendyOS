package chat

import (
	"strings"
	"testing"
)

func clearProviderEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"WENDY_CHAT_PROVIDER", "WENDY_CHAT_MODEL", "WENDY_CHAT_BASE_URL", "WENDY_CHAT_API_KEY", "WENDY_CHAT_MAX_TOKENS",
		"OPENAI_API_KEY", "OPENAI_BASE_URL", "ANTHROPIC_API_KEY",
	} {
		t.Setenv(key, "")
	}
}

func TestResolveConfigLocalDefaults(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("OPENAI_API_KEY", "cloud-key-must-stay-local")
	t.Setenv("ANTHROPIC_API_KEY", "another-cloud-key")
	for _, test := range []struct{ provider, endpoint string }{
		{"ollama", "http://localhost:11434/v1"},
		{"local", "http://localhost:8080/v1"},
	} {
		t.Run(test.provider, func(t *testing.T) {
			config, err := ResolveConfig(Config{Provider: test.provider, Model: "my-local-model:custom"})
			if err != nil {
				t.Fatal(err)
			}
			if config.BaseURL != test.endpoint || config.APIKey != "" || config.Model != "my-local-model:custom" || config.MaxTokens != 4096 {
				t.Fatalf("unexpected local config: provider=%s base=%s model=%s max=%d key present=%v", config.Provider, config.BaseURL, config.Model, config.MaxTokens, config.APIKey != "")
			}
		})
	}
}

func TestResolveConfigPrecedence(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("WENDY_CHAT_PROVIDER", "anthropic")
	t.Setenv("WENDY_CHAT_MODEL", "environment-model")
	t.Setenv("WENDY_CHAT_BASE_URL", "http://localhost:9000/v1/")
	t.Setenv("WENDY_CHAT_API_KEY", "wendy-key")
	t.Setenv("WENDY_CHAT_MAX_TOKENS", "2048")
	config, err := ResolveConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	if config != (Config{Provider: "anthropic", Model: "environment-model", BaseURL: "http://localhost:9000/v1", APIKey: "wendy-key", MaxTokens: 2048}) {
		t.Fatal("Wendy environment was not applied")
	}
	explicit := Config{Provider: "local", Model: "explicit-model", BaseURL: "http://localhost:8081/v1", APIKey: "explicit-key", MaxTokens: 1024}
	config, err = ResolveConfig(explicit)
	if err != nil || config != explicit {
		t.Fatalf("explicit config did not take precedence: %v", err)
	}
}

func TestResolveConfigCloudCredentials(t *testing.T) {
	for _, test := range []struct{ provider, variable, host string }{
		{"openai", "OPENAI_API_KEY", "api.openai.com"},
		{"anthropic", "ANTHROPIC_API_KEY", "api.anthropic.com"},
	} {
		t.Run(test.provider, func(t *testing.T) {
			clearProviderEnv(t)
			if _, err := ResolveConfig(Config{Provider: test.provider, Model: "test-model"}); err == nil || !strings.Contains(err.Error(), test.variable) {
				t.Fatalf("missing key error = %v", err)
			}
			t.Setenv(test.variable, "test-cloud-key")
			config, err := ResolveConfig(Config{Provider: test.provider, Model: "test-model"})
			if err != nil || config.APIKey != "test-cloud-key" {
				t.Fatalf("cloud credentials not inherited: %v", err)
			}
			for _, base := range []string{"http://localhost:1234/v1", "https://custom.example/v1", "https://" + test.host + ":9443/v1"} {
				config, err = ResolveConfig(Config{Provider: test.provider, Model: "test-model", BaseURL: base})
				if err != nil || config.APIKey != "" {
					t.Fatalf("custom endpoint must not inherit cloud key: %v", err)
				}
			}
		})
	}
}

func TestResolveConfigOpenAIBaseURL(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("OPENAI_BASE_URL", "http://localhost:1234/v1")
	t.Setenv("OPENAI_API_KEY", "cloud-secret")
	config, err := ResolveConfig(Config{Model: "local-model"})
	if err != nil || config.Provider != "openai" || config.BaseURL != "http://localhost:1234/v1" || config.APIKey != "" {
		t.Fatalf("OpenAI-compatible base was not applied safely: %v", err)
	}
}

func TestResolveConfigValidation(t *testing.T) {
	clearProviderEnv(t)
	for _, test := range []struct {
		name   string
		config Config
		want   string
	}{
		{"model", Config{Provider: "local"}, "--model"},
		{"provider", Config{Provider: "unknown", Model: "test"}, "unknown chat provider"},
		{"scheme", Config{Provider: "local", Model: "test", BaseURL: "ftp://localhost/v1"}, "absolute http"},
		{"relative", Config{Provider: "local", Model: "test", BaseURL: "/v1"}, "absolute http"},
		{"userinfo", Config{Provider: "local", Model: "test", BaseURL: "https://user:secret@localhost/v1"}, "cannot contain credentials"},
		{"query", Config{Provider: "local", Model: "test", BaseURL: "https://localhost/v1?key=secret"}, "cannot contain credentials"},
		{"fragment", Config{Provider: "local", Model: "test", BaseURL: "https://localhost/v1#secret"}, "cannot contain credentials"},
		{"tokens", Config{Provider: "local", Model: "test", MaxTokens: -1}, "must be positive"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := ResolveConfig(test.config)
			if err == nil || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("validation error = %v; want %s without secrets", err, test.want)
			}
		})
	}
	for _, invalid := range []string{"0", "-1", "invalid"} {
		t.Setenv("WENDY_CHAT_MAX_TOKENS", invalid)
		if _, err := ResolveConfig(Config{Provider: "local", Model: "test"}); err == nil {
			t.Errorf("accepted invalid max tokens %q", invalid)
		}
	}
}

func TestResolveConnectionAllowsSetup(t *testing.T) {
	clearProviderEnv(t)
	for _, provider := range []string{"openai", "anthropic", "ollama", "local"} {
		c, err := ResolveConnection(Config{Provider: provider})
		if err != nil {
			t.Fatalf("%s requires setup before discovery: %v", provider, err)
		}
		if c.Provider != provider || c.BaseURL == "" || c.Model != "" || c.APIKey != "" || c.MaxTokens != 4096 {
			t.Fatalf("unexpected partial connection for %s", provider)
		}
	}
	for _, c := range []Config{
		{BaseURL: "ftp://localhost/v1"},
		{APIKey: "invalid\ncredential"},
		{MaxTokens: -1},
		{Provider: "unknown"},
	} {
		if _, err := ResolveConnection(c); err == nil {
			t.Error("setup accepted invalid connection")
		}
	}
}

func TestResolveConnectionIsolated(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("WENDY_CHAT_PROVIDER", "anthropic")
	t.Setenv("WENDY_CHAT_MODEL", "environment-model")
	t.Setenv("WENDY_CHAT_BASE_URL", "https://unrelated.example/v1")
	t.Setenv("WENDY_CHAT_API_KEY", "environment-credential")
	t.Setenv("WENDY_CHAT_MAX_TOKENS", "invalid")
	c, err := resolveConnection(Config{Provider: "ollama"}, false)
	if err != nil || c.BaseURL != "http://localhost:11434/v1" || c.Model != "" || c.APIKey != "" || c.MaxTokens != 4096 {
		t.Fatalf("local probe inherited environment settings: %v", err)
	}
}
