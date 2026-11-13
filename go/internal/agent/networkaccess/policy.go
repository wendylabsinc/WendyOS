// Package networkaccess applies the access choice saved by the standalone
// installer. WendyOS images have no choice file and retain device networking.
package networkaccess

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
)

type Policy struct {
	AllowsNetwork bool
}

func Load(configDir string) (Policy, error) {
	data, err := os.ReadFile(filepath.Join(configDir, "network-access"))
	if os.IsNotExist(err) {
		// Preserve existing WendyOS and package-managed edge installations.
		// Fresh standalone installs explicitly write "local" before installing.
		return Policy{AllowsNetwork: true}, nil
	}
	if err != nil {
		return Policy{}, fmt.Errorf("reading network access choice: %w", err)
	}
	switch strings.TrimSpace(string(data)) {
	case "local":
		return Policy{}, nil
	case "network":
		return Policy{AllowsNetwork: true}, nil
	default:
		return Policy{}, fmt.Errorf("network-access must contain local or network")
	}
}

func (p Policy) Address(port string) string {
	host := "::"
	if !p.AllowsNetwork {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// RegistryAddress preserves explicit ports but never lets an address override
// expose the registry after the user selected local access.
func (p Policy) RegistryAddress(override string) (string, error) {
	if override == "" {
		override = "0.0.0.0:5000"
	}
	_, port, err := net.SplitHostPort(override)
	if err != nil {
		return "", fmt.Errorf("invalid registry address: %w", err)
	}
	if !p.AllowsNetwork {
		return net.JoinHostPort("127.0.0.1", port), nil
	}
	return override, nil
}

// UpdateAdvertisements hides only Wendy's service files from Avahi, retaining
// them alongside the originals so opting back in can restore their metadata.
func (p Policy) UpdateAdvertisements(serviceDir string) (bool, error) {
	changed := false
	for _, name := range []string{"wendy-agent.service", "wendyos-mdns.service"} {
		active := filepath.Join(serviceDir, name)
		disabled := active + ".local-only"
		if !p.AllowsNetwork {
			if err := os.Rename(active, disabled); err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return changed, err
			}
			changed = true
			continue
		}
		// A newly installed service file takes precedence over a saved one.
		if _, err := os.Stat(active); err == nil {
			if err := os.Remove(disabled); err != nil && !os.IsNotExist(err) {
				return changed, err
			}
			continue
		} else if !os.IsNotExist(err) {
			return changed, err
		}
		if err := os.Rename(disabled, active); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return changed, err
		}
		changed = true
	}
	return changed, nil
}
