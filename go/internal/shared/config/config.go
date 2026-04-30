// Package config manages the CLI configuration stored at ~/.wendy/config.json.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Config represents the top-level CLI configuration.
type Config struct {
	Auth               []AuthConfig     `json:"auth,omitempty"`
	Analytics          *AnalyticsConfig `json:"analytics,omitempty"`
	DefaultDevice      string           `json:"defaultDevice,omitempty"`
	LastCLIUpdateCheck string           `json:"lastCLIUpdateCheck,omitempty"` // RFC3339
}

// AuthConfig holds authentication details for a cloud environment.
type AuthConfig struct {
	CloudDashboard string            `json:"cloudDashboard"`
	CloudGRPC      string            `json:"cloudGRPC"`
	APIKey         string            `json:"apiKey,omitempty"`
	Certificates   []CertificateInfo `json:"certificates,omitempty"`
}

// CertificateInfo holds certificate material for mTLS authentication.
type CertificateInfo struct {
	PemCertificate      string `json:"pemCertificate,omitempty"`
	PemCertificateChain string `json:"pemCertificateChain,omitempty"`
	PemPrivateKey       string `json:"pemPrivateKey,omitempty"`
	OrganizationID      int    `json:"organizationId"`
	UserID              string `json:"userId,omitempty"`
}

// AnalyticsConfig holds analytics preferences.
type AnalyticsConfig struct {
	Enabled bool `json:"enabled"`
}

// ConfigDir returns the path to the ~/.wendy directory, creating it if necessary.
func ConfigDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determining home directory: %w", err)
	}

	dir := filepath.Join(home, ".wendy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("creating config directory: %w", err)
	}

	return dir, nil
}

// CacheDir returns the platform-appropriate cache directory for wendy, creating
// it if necessary.
//
//   - macOS:   ~/Library/Caches/wendy
//   - Linux:   $XDG_CACHE_HOME/wendy  (falls back to ~/.cache/wendy)
func CacheDir() (string, error) {
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("determining cache directory: %w", err)
	}

	cacheDir := filepath.Join(dir, "wendy")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("creating cache directory: %w", err)
	}

	return cacheDir, nil
}

// configPath returns the full path to config.json.
func configPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

// Load reads the CLI configuration from ~/.wendy/config.json.
// If the file does not exist, an empty Config is returned without error.
func Load() (*Config, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("reading config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config: %w", err)
	}

	return &cfg, nil
}

// Save writes the configuration to ~/.wendy/config.json.
func Save(cfg *Config) error {
	path, err := configPath()
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}

	return nil
}

// AddAuth adds or replaces an auth entry, matching on cloudDashboard and cloudGRPC.
func (c *Config) AddAuth(auth AuthConfig) {
	for i, existing := range c.Auth {
		if existing.CloudDashboard == auth.CloudDashboard && existing.CloudGRPC == auth.CloudGRPC {
			c.Auth[i] = auth
			return
		}
	}
	c.Auth = append(c.Auth, auth)
}
