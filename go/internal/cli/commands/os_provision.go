package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// writeConfigFiles writes the agent binary and optional wendy.conf to mountPoint.
// agentBinary is the raw binary content. ssid/password are written under [wifi]
// and deviceName under [device]; wendy.conf is omitted entirely when both are empty.
func writeConfigFiles(mountPoint string, agentBinary []byte, ssid, password, deviceName string) error {
	binPath := filepath.Join(mountPoint, "wendy-agent")
	if err := os.WriteFile(binPath, agentBinary, 0o755); err != nil {
		return fmt.Errorf("writing wendy-agent to config partition: %w", err)
	}

	if ssid == "" && deviceName == "" {
		return nil
	}

	var conf strings.Builder
	if ssid != "" {
		if strings.ContainsAny(ssid, "\n\r") || strings.ContainsAny(password, "\n\r") {
			return fmt.Errorf("WiFi SSID and password must not contain newline characters")
		}
		fmt.Fprintf(&conf, "[wifi]\nssid = %s\npassword = %s\n", ssid, password)
	}
	if deviceName != "" {
		if strings.ContainsAny(deviceName, "\n\r") {
			return fmt.Errorf("device name must not contain newline characters")
		}
		fmt.Fprintf(&conf, "[device]\nname = %s\n", deviceName)
	}

	confPath := filepath.Join(mountPoint, "wendy.conf")
	if err := os.WriteFile(confPath, []byte(conf.String()), 0o644); err != nil {
		return fmt.Errorf("writing wendy.conf to config partition: %w", err)
	}

	return nil
}
