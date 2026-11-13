package rosbattery

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ConfigFile is the monitor's config file name inside the agent's config
// directory (/etc/wendy-agent by default, or $WENDY_CONFIG_PATH), following the
// provisioning.json precedent alongside it.
const ConfigFile = "ros2-battery.json"

// fileConfig is the on-disk shape. Enabled is a pointer so an absent key can be
// told from an explicit false: absent means "use the default", which is on.
type fileConfig struct {
	Enabled    *bool    `json:"enabled,omitempty"`
	Interfaces []string `json:"interfaces,omitempty"`
	DomainID   *int     `json:"domainId,omitempty"`
	Topic      string   `json:"topic,omitempty"`
	Type       string   `json:"type,omitempty"`
}

// DefaultConfig is the behaviour with no config file present: discover
// automatically on the ROS 2 default domain across the device's peripheral
// interfaces. Setting "interfaces" is what opts another link, such as a
// wireless one, back in.
func DefaultConfig() Config {
	return Config{Enabled: true}
}

// LoadConfig reads ConfigFile from configDir.
//
// An absent file is not an error — it yields DefaultConfig, which is the
// intended out-of-the-box behaviour. A malformed file *is* an error, because
// silently ignoring a config someone deliberately wrote is worse than saying
// so; callers are expected to log it and carry on with the default.
func LoadConfig(configDir string) (Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(filepath.Join(configDir, ConfigFile))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return cfg, nil
		}
		return cfg, fmt.Errorf("reading %s: %w", ConfigFile, err)
	}

	var fc fileConfig
	if err := json.Unmarshal(data, &fc); err != nil {
		return cfg, fmt.Errorf("parsing %s: %w", ConfigFile, err)
	}

	if fc.Enabled != nil {
		cfg.Enabled = *fc.Enabled
	}
	if len(fc.Interfaces) > 0 {
		cfg.Interfaces = fc.Interfaces
	}
	if fc.DomainID != nil {
		cfg.DomainID = *fc.DomainID
	}
	cfg.Topic = fc.Topic
	cfg.Type = fc.Type
	return cfg, nil
}
