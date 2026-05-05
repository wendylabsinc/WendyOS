//go:build linux

package commands

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/wendylabsinc/wendy/internal/shared/nmcli"
)

type localWifiNetwork struct {
	SSID           string
	SignalStrength int32 // 0–100 percentage, or 0 if unknown
}

// scanLocalWifiNetworks uses nmcli on Linux to list WiFi networks visible to
// the host machine.
func scanLocalWifiNetworks() ([]localWifiNetwork, error) {
	nmcliPath, err := exec.LookPath("nmcli")
	if err != nil {
		return nil, fmt.Errorf("nmcli not found on PATH: %w", err)
	}

	// Trigger a rescan first (may fail if already scanning).
	_ = nmcli.Command(context.Background(), nmcliPath, "device", "wifi", "rescan").Run()

	cmd := nmcli.Command(context.Background(), nmcliPath, "-t", "-f", "SSID,SIGNAL", "device", "wifi", "list")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("scanning WiFi networks: %w", err)
	}

	seen := make(map[string]bool)
	var networks []localWifiNetwork

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		// Use the shared nmcli parser so SSIDs containing literal `:` (escaped
		// by nmcli as `\:`) and `\` survive intact, and so the parsing is
		// consistent with the agent side.
		fields := nmcli.Split(scanner.Text(), 2)
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
