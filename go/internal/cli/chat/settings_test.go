package chat

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsRoundTripPrivate(t *testing.T) {
	clearProviderEnv(t)
	path := filepath.Join(t.TempDir(), ".wendy", "chat.json")
	c := Config{Provider: "local", BaseURL: "http://localhost:1234/v1", Model: "my-model", APIKey: "pasted-credential", MaxTokens: 2048}
	if err := saveSettings(path, c); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSettings(path)
	if err != nil || loaded != c {
		t.Fatalf("settings did not round trip: %v", err)
	}
	for _, test := range []struct {
		path string
		mode os.FileMode
	}{{path, 0600}, {filepath.Dir(path), 0700}} {
		info, err := os.Stat(test.path)
		if err != nil || info.Mode().Perm() != test.mode {
			t.Fatalf("settings permissions: %v, want %o", err, test.mode)
		}
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	c.Model = "another-model"
	if err := saveSettings(path, c); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("replacement settings permissions: %v", err)
	}
	loaded, err = loadSettings(path)
	if err != nil || loaded.Model != c.Model {
		t.Fatalf("settings were not replaced: %v", err)
	}
	files, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(files) != 1 || files[0].Name() != "chat.json" {
		t.Fatalf("temporary settings files remain: %v", err)
	}
}

func TestSettingsNeverInheritEnvironment(t *testing.T) {
	clearProviderEnv(t)
	t.Setenv("WENDY_CHAT_API_KEY", "environment-credential")
	t.Setenv("OPENAI_API_KEY", "cloud-credential")
	t.Setenv("WENDY_CHAT_BASE_URL", "https://unrelated.example/v1")
	t.Setenv("WENDY_CHAT_MAX_TOKENS", "invalid")
	path := filepath.Join(t.TempDir(), "chat.json")
	if err := saveSettings(path, Config{Provider: "openai", Model: "chosen-model"}); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadSettings(path)
	if err != nil || loaded.APIKey != "" || loaded.BaseURL != "https://api.openai.com/v1" {
		t.Fatalf("settings unexpectedly inherited environment: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(data), "credential") {
		t.Fatalf("environment credential was persisted: %v", err)
	}
}

func TestSettingsMissingAndInvalid(t *testing.T) {
	clearProviderEnv(t)
	path := filepath.Join(t.TempDir(), "chat.json")
	c, err := loadSettings(path)
	if err != nil || c != (Config{}) {
		t.Fatalf("missing settings should start setup: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"api_key": "secret",`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSettings(path); err == nil || strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "--setup") {
		t.Fatalf("invalid settings error must be helpful and redact contents: %v", err)
	}
	if err := saveSettings(path, Config{Provider: "local", BaseURL: "https://user:secret@localhost"}); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("invalid settings should be rejected without exposing credentials: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != `{"api_key": "secret",` {
		t.Fatalf("failed save changed previous settings: %v", err)
	}
}

func TestMergeSettingsPrecedence(t *testing.T) {
	clearProviderEnv(t)
	saved := Config{Provider: "local", BaseURL: "http://localhost:1234/v1", Model: "saved-model", APIKey: "saved-key", MaxTokens: 1024}
	merged, err := ResolveConfig(MergeSettings(Config{}, saved))
	if err != nil || merged.Model != saved.Model || merged.APIKey != saved.APIKey || merged.BaseURL != saved.BaseURL || merged.MaxTokens != saved.MaxTokens {
		t.Fatalf("saved settings not loaded: %v", err)
	}
	t.Setenv("WENDY_CHAT_MODEL", "env-model")
	t.Setenv("WENDY_CHAT_API_KEY", "env-key")
	t.Setenv("WENDY_CHAT_MAX_TOKENS", "2048")
	merged, err = ResolveConfig(MergeSettings(Config{}, saved))
	if err != nil || merged.Model != "env-model" || merged.APIKey != "env-key" || merged.MaxTokens != 2048 {
		t.Fatalf("environment did not override saved settings: %v", err)
	}
	explicit := Config{Provider: "local", BaseURL: saved.BaseURL, Model: "flag-model", APIKey: "flag-key", MaxTokens: 512}
	merged, err = ResolveConfig(MergeSettings(explicit, saved))
	if err != nil || merged.Model != explicit.Model || merged.APIKey != explicit.APIKey || merged.MaxTokens != explicit.MaxTokens {
		t.Fatalf("flags did not override environment settings: %v", err)
	}
	t.Setenv("WENDY_CHAT_MAX_TOKENS", "invalid")
	if _, err := ResolveConnection(MergeSettings(Config{}, saved)); err == nil || !strings.Contains(err.Error(), "WENDY_CHAT_MAX_TOKENS") {
		t.Fatalf("invalid environment setting silently ignored: %v", err)
	}
}

func TestMergeSettingsConnectionIsolation(t *testing.T) {
	for _, test := range []struct {
		name     string
		explicit Config
		env      map[string]string
		provider string
		baseURL  string
	}{
		{"provider flag", Config{Provider: "ollama"}, nil, "ollama", "http://localhost:11434/v1"},
		{"endpoint flag", Config{BaseURL: "http://localhost:9000/v1"}, nil, "openai", "http://localhost:9000/v1"},
		{"provider environment", Config{}, map[string]string{"WENDY_CHAT_PROVIDER": "ollama"}, "ollama", "http://localhost:11434/v1"},
		{"endpoint environment", Config{}, map[string]string{"WENDY_CHAT_BASE_URL": "http://localhost:9000/v1"}, "openai", "http://localhost:9000/v1"},
		{"openai endpoint environment", Config{}, map[string]string{"OPENAI_BASE_URL": "http://localhost:9000/v1"}, "openai", "http://localhost:9000/v1"},
		{"provider overrides environment", Config{Provider: "ollama"}, map[string]string{"WENDY_CHAT_PROVIDER": "openai", "WENDY_CHAT_API_KEY": "environment-key", "WENDY_CHAT_MODEL": "environment-model", "WENDY_CHAT_BASE_URL": "https://api.openai.com/v1"}, "ollama", "http://localhost:11434/v1"},
		{"endpoint overrides environment", Config{BaseURL: "http://localhost:9000/v1"}, map[string]string{"WENDY_CHAT_BASE_URL": "https://api.openai.com/v1", "WENDY_CHAT_API_KEY": "environment-key", "WENDY_CHAT_MODEL": "environment-model"}, "openai", "http://localhost:9000/v1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearProviderEnv(t)
			t.Setenv("OPENAI_API_KEY", "cloud-key")
			for key, value := range test.env {
				t.Setenv(key, value)
			}
			saved := Config{Provider: "openai", BaseURL: "https://api.openai.com/v1", APIKey: "saved-cloud-key", Model: "saved-cloud-model"}
			c, err := ResolveConnection(MergeSettings(test.explicit, saved))
			if err != nil || c.Provider != test.provider || c.BaseURL != test.baseURL || c.APIKey != "" || c.Model != "" {
				t.Fatalf("incompatible connection settings inherited: %v; provider=%s endpoint=%s key present=%v model present=%v", err, c.Provider, c.BaseURL, c.APIKey != "", c.Model != "")
			}
		})
	}
}

func TestMergeSettingsGenericEnvironmentWithExplicitConnection(t *testing.T) {
	for _, test := range []struct {
		name     string
		explicit Config
		saved    Config
		envBase  string
		baseURL  string
		model    string
	}{
		{
			name:     "custom endpoint flags and generic key",
			explicit: Config{Provider: "local", BaseURL: "http://custom/v1", Model: "flag-model"},
			baseURL:  "http://custom/v1",
			model:    "flag-model",
		},
		{
			name:     "provider flag and generic model",
			explicit: Config{Provider: "local"},
			baseURL:  "http://localhost:8080/v1",
			model:    "environment-model",
		},
		{
			name:     "provider flag and generic endpoint",
			explicit: Config{Provider: "local"},
			envBase:  "http://custom/v1",
			baseURL:  "http://custom/v1",
			model:    "environment-model",
		},
		{
			name:     "generic settings override saved cloud connection",
			explicit: Config{Provider: "local", BaseURL: "http://custom/v1"},
			saved:    Config{Provider: "openai", BaseURL: "https://api.openai.com/v1", Model: "saved-model", APIKey: "saved-key"},
			baseURL:  "http://custom/v1",
			model:    "environment-model",
		},
		{
			name:     "generic settings follow explicit endpoint with saved provider",
			explicit: Config{BaseURL: "http://custom/v1"},
			saved:    Config{Provider: "openai", BaseURL: "https://api.openai.com/v1", Model: "saved-model", APIKey: "saved-key"},
			baseURL:  "http://custom/v1",
			model:    "environment-model",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			clearProviderEnv(t)
			t.Setenv("WENDY_CHAT_API_KEY", "generic-environment-key")
			t.Setenv("WENDY_CHAT_MODEL", "environment-model")
			t.Setenv("WENDY_CHAT_BASE_URL", test.envBase)
			t.Setenv("OPENAI_API_KEY", "cloud-key")
			t.Setenv("ANTHROPIC_API_KEY", "another-cloud-key")
			c, err := ResolveConfig(MergeSettings(test.explicit, test.saved))
			if err != nil || c.APIKey != "generic-environment-key" || c.Model != test.model || c.BaseURL != test.baseURL {
				t.Fatalf("generic environment did not compose with explicit connection: %v; endpoint=%s model=%s", err, c.BaseURL, c.Model)
			}
		})
	}
}

func TestMergeSettingsEquivalentEndpointAndCredentialPriority(t *testing.T) {
	clearProviderEnv(t)
	saved := Config{Provider: "openai", BaseURL: "https://api.openai.com/v1", APIKey: "saved-key", Model: "saved-model"}
	c, err := ResolveConfig(MergeSettings(Config{BaseURL: "https://API.OPENAI.COM:443/v1/"}, saved))
	if err != nil || c.APIKey != saved.APIKey || c.Model != saved.Model {
		t.Fatalf("equivalent endpoint lost saved connection: %v", err)
	}
	t.Setenv("OPENAI_API_KEY", "environment-key")
	c, err = ResolveConfig(MergeSettings(Config{}, saved))
	if err != nil || c.APIKey != "environment-key" {
		t.Fatalf("cloud environment credential did not override saved credential: %v", err)
	}
	c.APIKey = ""
	c, err = ResolveConnection(c)
	if err != nil || c.APIKey != "" {
		t.Fatalf("setup reapplied a deliberately cleared environment credential: %v", err)
	}
}
