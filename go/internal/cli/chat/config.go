package chat

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// Config selects a model and API. BaseURL is an API root such as
// http://localhost:11434/v1. APIKey is optional for custom endpoints.
type Config struct {
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	BaseURL   string `json:"base_url,omitempty"`
	APIKey    string `json:"api_key,omitempty"`
	MaxTokens int    `json:"max_tokens,omitempty"`

	// A merged setup configuration has already considered the environment.
	// Keep cleared credentials cleared when the user changes providers in setup.
	environmentApplied   bool
	maxTokensEnvironment string
}

// ResolveConfig applies explicit options, Wendy environment variables, and
// provider defaults in that order. Model names are deliberately never guessed.
func ResolveConfig(c Config) (Config, error) {
	c, err := ResolveConnection(c)
	if err != nil {
		return Config{}, err
	}
	if c.Model == "" {
		return Config{}, fmt.Errorf("choose a model with --model or WENDY_CHAT_MODEL (for local models, use the name loaded by your server)")
	}
	if keyEnv := RequiredAPIKeyEnv(c); keyEnv != "" && c.APIKey == "" {
		return Config{}, fmt.Errorf("%s requires an API key; set %s or WENDY_CHAT_API_KEY", c.Provider, keyEnv)
	}
	return c, nil
}

// ResolveConnection resolves and validates connection settings without requiring
// a model or credential, so interactive setup can discover models and ask for
// missing information. Call ResolveConfig before starting a conversation.
func ResolveConnection(c Config) (Config, error) {
	return resolveConnection(c, !c.environmentApplied)
}

// resolveConnection can also resolve isolated local probes without inheriting
// any endpoint or credentials from the user's environment.
func resolveConnection(c Config, applyEnv bool) (Config, error) {
	if applyEnv {
		c.Provider = firstValue(c.Provider, os.Getenv("WENDY_CHAT_PROVIDER"))
		c.Model = firstValue(c.Model, os.Getenv("WENDY_CHAT_MODEL"))
		c.BaseURL = firstValue(c.BaseURL, os.Getenv("WENDY_CHAT_BASE_URL"))
		c.APIKey = firstValue(c.APIKey, os.Getenv("WENDY_CHAT_API_KEY"))
		if c.MaxTokens == 0 {
			c.maxTokensEnvironment = strings.TrimSpace(os.Getenv("WENDY_CHAT_MAX_TOKENS"))
		}
	}
	c.Provider = strings.ToLower(firstValue(c.Provider, "openai"))
	c.Model = firstValue(c.Model)
	c.BaseURL = firstValue(c.BaseURL)
	c.APIKey = firstValue(c.APIKey)
	switch c.Provider {
	case "openai":
		if applyEnv {
			c.BaseURL = firstValue(c.BaseURL, os.Getenv("OPENAI_BASE_URL"))
		}
	case "anthropic", "ollama", "local":
	default:
		return Config{}, fmt.Errorf("unknown chat provider %q; use openai, anthropic, ollama, or local", c.Provider)
	}
	c.BaseURL = firstValue(c.BaseURL, defaultBaseURL(c.Provider))
	u, err := url.Parse(c.BaseURL)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.Opaque != "" {
		return Config{}, fmt.Errorf("chat base URL must be an absolute http:// or https:// API URL, such as http://localhost:11434/v1")
	}
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return Config{}, fmt.Errorf("chat base URL cannot contain credentials, a query, or a fragment; use WENDY_CHAT_API_KEY for authentication")
	}
	c.BaseURL = strings.TrimRight(c.BaseURL, "/")
	// Never forward a cloud credential from the environment to an unrelated
	// server. Custom endpoints opt in with the Wendy-specific key instead.
	if keyEnv := RequiredAPIKeyEnv(c); applyEnv && keyEnv != "" {
		c.APIKey = firstValue(c.APIKey, os.Getenv(keyEnv))
	}
	if strings.ContainsAny(c.APIKey, "\r\n") {
		return Config{}, fmt.Errorf("chat API key contains an invalid newline")
	}
	if c.MaxTokens == 0 {
		if value := c.maxTokensEnvironment; value != "" {
			c.MaxTokens, err = strconv.Atoi(value)
			if err != nil || c.MaxTokens <= 0 {
				return Config{}, fmt.Errorf("WENDY_CHAT_MAX_TOKENS must be a positive integer")
			}
		}
	}
	if c.MaxTokens < 0 {
		return Config{}, fmt.Errorf("chat max tokens must be positive")
	}
	if c.MaxTokens == 0 {
		c.MaxTokens = 4096
	}
	c.maxTokensEnvironment = ""
	return c, nil
}

// RequiredAPIKeyEnv identifies the credential needed by an official hosted API.
// Custom endpoints and local servers do not require credentials by default.
func RequiredAPIKeyEnv(c Config) string {
	provider := strings.ToLower(firstValue(c.Provider, "openai"))
	u, err := url.Parse(firstValue(c.BaseURL, defaultBaseURL(provider)))
	if err != nil || u.Scheme != "https" || (u.Port() != "" && u.Port() != "443") || u.User != nil {
		return ""
	}
	if provider == "openai" && strings.EqualFold(u.Hostname(), "api.openai.com") {
		return "OPENAI_API_KEY"
	}
	if provider == "anthropic" && strings.EqualFold(u.Hostname(), "api.anthropic.com") {
		return "ANTHROPIC_API_KEY"
	}
	return ""
}

func defaultBaseURL(provider string) string {
	switch provider {
	case "openai":
		return "https://api.openai.com/v1"
	case "anthropic":
		return "https://api.anthropic.com/v1"
	case "ollama":
		return "http://localhost:11434/v1"
	case "local":
		return "http://localhost:8080/v1"
	default:
		return ""
	}
}

func firstValue(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
