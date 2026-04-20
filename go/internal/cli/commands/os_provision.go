package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// writeConfigFiles writes the agent binary and optional wendy.conf to mountPoint.
// agentBinary is the raw binary content. ssid/password are written to wendy.conf
// only if ssid is non-empty.
func writeConfigFiles(mountPoint string, agentBinary []byte, ssid, password string) error {
	binPath := filepath.Join(mountPoint, "wendy-agent")
	if err := os.WriteFile(binPath, agentBinary, 0o755); err != nil {
		return fmt.Errorf("writing wendy-agent to config partition: %w", err)
	}

	if ssid == "" {
		return nil
	}

	if strings.ContainsAny(ssid, "\n\r") || strings.ContainsAny(password, "\n\r") {
		return fmt.Errorf("WiFi SSID and password must not contain newline characters")
	}

	conf := fmt.Sprintf("[wifi]\nssid = %s\npassword = %s\n", ssid, password)
	confPath := filepath.Join(mountPoint, "wendy.conf")
	if err := os.WriteFile(confPath, []byte(conf), 0o644); err != nil {
		return fmt.Errorf("writing wendy.conf to config partition: %w", err)
	}

	return nil
}
