package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wendylabsinc/wendy/internal/shared/wendyconf"
)

// writeConfigFiles writes the agent binary and optional wendy.conf to
// mountPoint. agentBinary is the raw binary content. Each credential in creds
// becomes a `[wifi]` / `[wifi.N]` section; deviceName (if non-empty) is written
// under `[device]`. wendy.conf is omitted entirely when both are empty.
func writeConfigFiles(mountPoint string, agentBinary []byte, creds []wendyconf.WifiCredential, deviceName string) error {
	binPath := filepath.Join(mountPoint, "wendy-agent")
	if err := os.WriteFile(binPath, agentBinary, 0o755); err != nil {
		return fmt.Errorf("writing wendy-agent to config partition: %w", err)
	}

	if len(creds) == 0 && deviceName == "" {
		return nil
	}

	for _, c := range creds {
		if strings.ContainsAny(c.SSID, "\n\r") || strings.ContainsAny(c.Password, "\n\r") {
			return fmt.Errorf("WiFi SSID and password must not contain newline characters")
		}
	}
	if strings.ContainsAny(deviceName, "\n\r") {
		return fmt.Errorf("device name must not contain newline characters")
	}

	var conf []byte
	if len(creds) > 0 {
		conf = wendyconf.Marshal(creds)
	}
	if deviceName != "" {
		if len(conf) > 0 {
			conf = append(conf, '\n')
		}
		conf = append(conf, []byte(fmt.Sprintf("[device]\nname = %s\n", deviceName))...)
	}

	confPath := filepath.Join(mountPoint, "wendy.conf")
	if err := os.WriteFile(confPath, conf, 0o644); err != nil {
		return fmt.Errorf("writing wendy.conf to config partition: %w", err)
	}
	return nil
}
