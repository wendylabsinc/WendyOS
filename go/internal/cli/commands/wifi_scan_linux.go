//go:build linux

package commands

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/nmcli"
)

// wifiScanCacheHint is empty on Linux: nmcli's `device wifi rescan` triggers
// a fresh scan before the list call, so the returned set is current.
const wifiScanCacheHint = ""

// scanLocalWifiNetworks uses nmcli on Linux to list WiFi networks visible to
// the host machine. Returns errNoWifiAdapter when the host has no wifi-type
// device, so callers can offer to skip WiFi setup instead of failing.
func scanLocalWifiNetworks(ctx context.Context) ([]localWifiNetwork, error) {
	nmcliPath, err := exec.LookPath("nmcli")
	if err != nil {
		return nil, fmt.Errorf("nmcli not found on PATH: %w", err)
	}

	// Distinguish "no WiFi hardware" from a transient scan failure before
	// attempting the scan (WDY-1474). A status-command failure is ignored:
	// the scan below will surface its own error.
	if statusOut, statusErr := nmcli.Command(ctx, nmcliPath, "-t", "-f", "DEVICE,TYPE", "device", "status").Output(); statusErr == nil {
		if !nmcliHasWifiDevice(string(statusOut)) {
			return nil, errNoWifiAdapter
		}
	}

	// Trigger a rescan first (may fail if already scanning).
	_ = nmcli.Command(ctx, nmcliPath, "device", "wifi", "rescan").Run()

	return nmcliListWifi(ctx, nmcliPath)
}

// cachedLocalWifiNetworks reads nmcli's current scan cache without forcing a
// rescan, so a streaming picker can paint instantly while scanLocalWifiNetworks
// runs the slower `device wifi rescan`. `--rescan no` keeps the list call from
// blocking on a fresh scan when the cache is stale. Best-effort: any failure
// (including no nmcli on PATH, or an nmcli too old to know `--rescan`) yields
// no networks rather than an error — the authoritative scan still runs after.
func cachedLocalWifiNetworks(ctx context.Context) []localWifiNetwork {
	nmcliPath, err := exec.LookPath("nmcli")
	if err != nil {
		return nil
	}
	nets, err := nmcliListWifi(ctx, nmcliPath, "--rescan", "no")
	if err != nil {
		return nil
	}
	return nets
}

// nmcliListWifi lists the WiFi networks nmcli currently knows about and parses
// the result. It does not trigger a rescan itself; extraArgs (e.g. "--rescan",
// "no") are appended to the list command.
func nmcliListWifi(ctx context.Context, nmcliPath string, extraArgs ...string) ([]localWifiNetwork, error) {
	args := append([]string{"-t", "-f", "SSID,SIGNAL,SECURITY", "device", "wifi", "list"}, extraArgs...)
	cmd := nmcli.Command(ctx, nmcliPath, args...)
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("scanning WiFi networks: %w", exitErrWithStderr(err))
	}

	seen := make(map[string]bool)
	var networks []localWifiNetwork

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		// Use the shared nmcli parser so SSIDs containing literal `:` (escaped
		// by nmcli as `\:`) and `\` survive intact, and so the parsing is
		// consistent with the agent side.
		fields := nmcli.Split(scanner.Text(), 3)
		if len(fields) < 2 {
			continue
		}

		ssid := tui.StripControl(fields[0])
		if ssid == "" || seen[ssid] {
			continue
		}
		seen[ssid] = true

		var signal int32
		if s, err := strconv.Atoi(fields[1]); err == nil {
			signal = int32(s)
		}

		security := ""
		if len(fields) >= 3 {
			security = normalizeWifiSecurity(tui.StripControl(fields[2]))
		}

		networks = append(networks, localWifiNetwork{SSID: ssid, SignalStrength: signal, Security: security})
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("parsing WiFi scan output: %w", err)
	}

	return networks, nil
}

// nmcliHasWifiDevice reports whether `nmcli -t -f DEVICE,TYPE device status`
// output lists at least one wifi-type device. wifi-p2p entries don't count —
// they are virtual P2P interfaces, not scannable adapters.
func nmcliHasWifiDevice(output string) bool {
	scanner := bufio.NewScanner(strings.NewReader(output))
	for scanner.Scan() {
		fields := nmcli.Split(scanner.Text(), 2)
		if len(fields) >= 2 && fields[1] == "wifi" {
			return true
		}
	}
	return false
}

const supportsKeychainLookup = false

// lookupKeychainPassword is not supported on Linux.
func lookupKeychainPassword(_ string) (string, error) {
	return "", nil
}
