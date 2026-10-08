//go:build darwin

package commands

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

// wifiScanCacheHint is empty on macOS: CoreWLAN's scanForNetworks performs
// a synchronous fresh scan, so the returned set is current.
const wifiScanCacheHint = ""

// corewlanPreamble opens the shared CoreWLAN interface and defines the
// security-label helper. The two obtain snippets below append a `networks`
// binding; corewlanPrintLoop then emits the tab-delimited rows.
const corewlanPreamble = `
import CoreWLAN
let client = CWWiFiClient.shared()
guard let iface = client.interface() else {
    fputs("no wifi interface\n", stderr)
    exit(1)
}
// Strongest advertised suite wins; transition (mixed-mode) networks count as
// the newer suite, matching how the agent labels nmcli scan results.
func securityLabel(_ net: CWNetwork) -> String {
    if net.supportsSecurity(.wpa3Personal) || net.supportsSecurity(.wpa3Transition) { return "WPA3" }
    if net.supportsSecurity(.wpa3Enterprise) { return "WPA3-Ent" }
    if net.supportsSecurity(.wpa2Enterprise) || net.supportsSecurity(.enterprise) || net.supportsSecurity(.wpaEnterprise) || net.supportsSecurity(.wpaEnterpriseMixed) { return "WPA2-Ent" }
    if net.supportsSecurity(.wpa2Personal) || net.supportsSecurity(.personal) { return "WPA2" }
    if net.supportsSecurity(.wpaPersonal) || net.supportsSecurity(.wpaPersonalMixed) { return "WPA" }
    if net.supportsSecurity(.WEP) || net.supportsSecurity(.dynamicWEP) { return "WEP" }
    if net.supportsSecurity(.none) || net.supportsSecurity(.OWE) || net.supportsSecurity(.oweTransition) { return "Open" }
    return ""
}
`

// corewlanFreshObtain performs a synchronous on-demand scan (several seconds).
const corewlanFreshObtain = `
let networks: [CWNetwork]
do {
    networks = try iface.scanForNetworks(withSSID: nil)
} catch {
    fputs("scan failed: \(error)\n", stderr)
    exit(1)
}
`

// corewlanCachedObtain returns the most recent scan results without scanning,
// so it returns instantly (and is empty until the OS has scanned at least once).
const corewlanCachedObtain = `
let networks = Array(iface.cachedScanResults() ?? [])
`

const corewlanPrintLoop = `
for net in networks.sorted(by: { $0.rssiValue > $1.rssiValue }) {
    guard let ssid = net.ssid, !ssid.isEmpty else { continue }
    // Strip C0/DEL/C1 control characters before printing: SSIDs come
    // from beacon frames, and a tab would shift the tab-delimited
    // fields, letting attacker bytes land in the security column.
    let clean = String(ssid.unicodeScalars.filter {
        $0.value >= 0x20 && $0.value != 0x7F && !(0x80...0x9F).contains($0.value)
    }.map { Character($0) })
    guard !clean.isEmpty else { continue }
    print("\(clean)\t\(net.rssiValue)\t\(securityLabel(net))")
}
`

// scanLocalWifiNetworks uses CoreWLAN (via a small Swift script) to perform a
// fresh on-demand scan of WiFi networks visible to the host machine.
func scanLocalWifiNetworks(ctx context.Context) ([]localWifiNetwork, error) {
	nets, err := runCorewlanScan(ctx, corewlanPreamble+corewlanFreshObtain+corewlanPrintLoop)
	if err != nil {
		if errors.Is(err, errNoWifiAdapter) {
			return nil, err
		}
		return nil, fmt.Errorf("scanning WiFi networks: %w", err)
	}
	return nets, nil
}

// cachedLocalWifiNetworks returns CoreWLAN's most recent scan results without
// triggering a fresh scan, so a streaming picker can paint instantly while
// scanLocalWifiNetworks runs the slower on-demand scan. Best-effort: any
// failure yields no networks rather than an error.
func cachedLocalWifiNetworks(ctx context.Context) []localWifiNetwork {
	nets, err := runCorewlanScan(ctx, corewlanPreamble+corewlanCachedObtain+corewlanPrintLoop)
	if err != nil {
		return nil
	}
	return nets
}

// runCorewlanScan runs the given Swift program and parses its tab-delimited
// "SSID\tRSSI\tSecurity" output. A "no wifi interface" failure is reported as
// errNoWifiAdapter; other exec failures pass through (with captured stderr).
func runCorewlanScan(ctx context.Context, script string) ([]localWifiNetwork, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/swift", "-")
	cmd.Stdin = strings.NewReader(script)

	output, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && strings.Contains(string(ee.Stderr), "no wifi interface") {
			return nil, errNoWifiAdapter
		}
		return nil, exitErrWithStderr(err)
	}

	seen := make(map[string]bool)
	var networks []localWifiNetwork

	scanner := bufio.NewScanner(strings.NewReader(string(output)))
	for scanner.Scan() {
		parts := strings.SplitN(scanner.Text(), "\t", 3)
		if len(parts) < 2 {
			continue
		}

		ssid := tui.StripControl(parts[0])
		if ssid == "" || seen[ssid] {
			continue
		}
		seen[ssid] = true

		var signal int32
		if rssi, err := strconv.Atoi(parts[1]); err == nil {
			// Rough RSSI → percentage: -30 dBm = 100%, -90 dBm = 0%
			pct := (rssi + 90) * 100 / 60
			if pct > 100 {
				pct = 100
			}
			if pct < 0 {
				pct = 0
			}
			signal = int32(pct)
		}

		security := ""
		if len(parts) >= 3 {
			security = tui.StripControl(strings.TrimSpace(parts[2]))
		}

		networks = append(networks, localWifiNetwork{SSID: ssid, SignalStrength: signal, Security: security})
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("parsing WiFi scan output: %w", err)
	}

	return networks, nil
}

const supportsKeychainLookup = true

// lookupKeychainPassword attempts to retrieve a saved WiFi password from the
// macOS System Keychain using the `security` command. Returns ("", nil) if the
// SSID is not found or the user denies the authorization prompt.
func lookupKeychainPassword(ssid string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password",
		"-D", "AirPort network password",
		"-a", ssid,
		"-w",
	)
	output, err := cmd.Output()
	if err != nil {
		// Not found or user denied — not an error we need to surface.
		return "", nil
	}
	return strings.TrimSpace(string(output)), nil
}
