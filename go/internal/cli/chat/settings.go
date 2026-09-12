package chat

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// MergeSettings combines flags, environment settings, and saved preferences in
// that order. Saved models and credentials belong to a provider and endpoint.
// Generic environment values apply to the selected connection unless another
// provider or endpoint is explicitly specified in the environment. Defaults and
// validation are applied by ResolveConnection afterwards.
func MergeSettings(explicit, saved Config) Config {
	c := explicit
	c.environmentApplied = true
	c.Provider = strings.ToLower(firstValue(explicit.Provider, os.Getenv("WENDY_CHAT_PROVIDER"), saved.Provider))
	provider := firstValue(c.Provider, "openai")
	envProvider := strings.ToLower(firstValue(os.Getenv("WENDY_CHAT_PROVIDER"), provider))
	envMatchesProvider := provider == envProvider
	savedMatchesProvider := strings.EqualFold(provider, firstValue(saved.Provider, "openai"))

	envBase := firstValue(os.Getenv("WENDY_CHAT_BASE_URL"))
	if envProvider == "openai" {
		envBase = firstValue(envBase, os.Getenv("OPENAI_BASE_URL"))
	}
	c.BaseURL = firstValue(explicit.BaseURL)
	if c.BaseURL == "" && envMatchesProvider {
		c.BaseURL = envBase
	}
	if c.BaseURL == "" && savedMatchesProvider {
		c.BaseURL = saved.BaseURL
	}
	endpoint := firstValue(c.BaseURL, defaultBaseURL(provider))
	// An omitted environment provider/base is not an implicit OpenAI binding.
	// This supports WENDY_CHAT_API_KEY=... wendy chat --provider local --base-url
	// ... while still rejecting a key explicitly configured for another server.
	envMatches := envMatchesProvider && (envBase == "" || sameEndpoint(endpoint, envBase))
	savedMatches := savedMatchesProvider && sameEndpoint(endpoint, firstValue(saved.BaseURL, defaultBaseURL(provider)))

	c.Model = firstValue(explicit.Model)
	c.APIKey = firstValue(explicit.APIKey)
	if envMatches {
		c.Model = firstValue(c.Model, os.Getenv("WENDY_CHAT_MODEL"))
		c.APIKey = firstValue(c.APIKey, os.Getenv("WENDY_CHAT_API_KEY"))
	}
	// Provider-specific credentials are always scoped to the official endpoint.
	if keyEnv := RequiredAPIKeyEnv(Config{Provider: provider, BaseURL: endpoint}); keyEnv != "" {
		c.APIKey = firstValue(c.APIKey, os.Getenv(keyEnv))
	}
	if savedMatches {
		c.Model = firstValue(c.Model, saved.Model)
		c.APIKey = firstValue(c.APIKey, saved.APIKey)
	}
	if c.MaxTokens == 0 {
		if value := strings.TrimSpace(os.Getenv("WENDY_CHAT_MAX_TOKENS")); value != "" {
			c.maxTokensEnvironment = value
		} else {
			c.MaxTokens = saved.MaxTokens
		}
	}
	return c
}

func sameEndpoint(a, b string) bool {
	normalize := func(raw string) string {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil {
			return raw
		}
		u.Scheme = strings.ToLower(u.Scheme)
		u.Host = strings.ToLower(u.Host)
		if (u.Scheme == "https" && u.Port() == "443") || (u.Scheme == "http" && u.Port() == "80") {
			u.Host = u.Hostname()
			if strings.Contains(u.Host, ":") {
				u.Host = "[" + u.Host + "]"
			}
		}
		u.Path = strings.TrimRight(u.Path, "/")
		return u.String()
	}
	return normalize(a) == normalize(b)
}

func settingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate chat settings: %w", err)
	}
	return filepath.Join(home, ".wendy", "chat.json"), nil
}

// LoadSettings loads the user's last chosen connection. A first run returns an
// empty Config. It never loads environment credentials or other Wendy settings.
func LoadSettings() (Config, error) {
	path, err := settingsPath()
	if err != nil {
		return Config{}, err
	}
	return loadSettings(path)
}

func loadSettings(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read chat settings: %w", err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		// Do not include JSON parse details: the document can contain credentials.
		return Config{}, fmt.Errorf("chat settings contain invalid JSON; run 'wendy chat --setup' to configure chat again")
	}
	resolved, err := resolveConnection(c, false)
	if err != nil {
		return Config{}, fmt.Errorf("invalid chat settings: %w", err)
	}
	return resolved, nil
}

// SaveSettings remembers the supplied connection in ~/.wendy/chat.json. As with
// Wendy's existing credentials, pasted keys are kept in a private local file.
// Callers decide whether a key should be saved; environment keys are not loaded
// here. A rename makes replacement atomic and every written file has mode 0600.
func SaveSettings(c Config) error {
	path, err := settingsPath()
	if err != nil {
		return err
	}
	return saveSettings(path, c)
}

func saveSettings(path string, c Config) error {
	c, err := resolveConnection(c, false)
	if err != nil {
		return fmt.Errorf("save chat settings: %w", err)
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("encode chat settings: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create chat settings directory: %w", err)
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".chat-*.json")
	if err != nil {
		return fmt.Errorf("create chat settings: %w", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return fmt.Errorf("protect chat settings: %w", err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write chat settings: %w", err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync chat settings: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close chat settings: %w", err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("replace chat settings: %w", err)
	}
	return nil
}
