//go:build linux

package commands

import (
	"bufio"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

type localWifiNetwork struct {
	SSID           string
	SignalStrength int32 // 0–100 percentage, or 0 if unknown
}

// scanLocalWifiNetworks uses nmcli on Linux to list WiFi networks visible to
// the host machine.
func scanLocalWifiNetworks() ([]localWifiNetwork, error) {
	// Trigger a rescan first (may fail if already scanning).
	_ = exec.Command("nmcli", "device", "wifi", "rescan").Run()

	cmd := exec.Command("nmcli", "-t", "-f", "SSID,SIGNAL", "device", "wifi", "list")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("scanning WiFi networks: %w", err)
	}

	seen := make(map[string]bool)
	var networks []localWifiNetwork

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		fields := strings.SplitN(scanner.Text(), ":", 2)
		if len(fields) < 2 {
			continue
		}

		ssid := fields[0]
		if ssid == "" || seen[ssid] {
			continue
		}
		seen[ssid] = true

		var signal int32
		if s, err := strconv.Atoi(fields[1]); err == nil {
			signal = int32(s)
		}

		networks = append(networks, localWifiNetwork{SSID: ssid, SignalStrength: signal})
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("parsing WiFi scan output: %w", err)
	}

	return networks, nil
}

const supportsKeychainLookup = false

// lookupKeychainPassword is not supported on Linux.
func lookupKeychainPassword(_ string) (string, error) {
	return "", nil
}
