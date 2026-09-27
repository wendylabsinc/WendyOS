//go:build windows

package commands

import (
	"context"
	"fmt"
	"os/exec"
)

// scanLocalWifiNetworks lists WiFi networks visible to the host using
// `netsh wlan show networks mode=bssid`.
//
// Important: netsh reads the Windows WLAN service's *cached* scan list.
// The service asks the driver to rescan periodically and, per Microsoft's
// docs, may not scan at all while already associated to a network. This
// command therefore does not guarantee a fresh, on-demand scan the way the
// macOS CoreWLAN and Linux `nmcli device wifi rescan` paths do.
//
// A proper fix is to call Native Wifi's WlanScan API
// (https://learn.microsoft.com/en-us/windows/win32/api/wlanapi/nf-wlanapi-wlanscan)
// via syscall — that work is intentionally deferred while this implementation
// is the near-term fallback. Callers should treat empty results as "possibly
// stale cache" and surface the retry hint defined by wifiScanCacheHint.
func scanLocalWifiNetworks(ctx context.Context) ([]localWifiNetwork, error) {
	cmd := exec.CommandContext(ctx, "netsh", "wlan", "show", "networks", "mode=bssid")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("scanning WiFi networks: %w", exitErrWithStderr(err))
	}
	return parseNetshNetworks(string(output)), nil
}

// cachedLocalWifiNetworks returns no networks on Windows: netsh already reads
// the WLAN service's cached scan list (see scanLocalWifiNetworks), so the
// single scan is effectively instant and a separate cached pre-paint would
// just run netsh twice. streamLocalWifiScan falls through to the fresh scan.
func cachedLocalWifiNetworks(_ context.Context) []localWifiNetwork {
	return nil
}

// wifiScanCacheHint is appended to empty-scan messages on Windows because
// netsh reads cached results — see scanLocalWifiNetworks for details.
const wifiScanCacheHint = "Windows scans for nearby networks periodically; if your network is missing, wait a few seconds and try again, or pass --ssid to specify it directly"

const supportsKeychainLookup = false

// lookupKeychainPassword is not supported on Windows. Windows has no
// equivalent of macOS's auto-stored "AirPort network password" Keychain
// entry; lookup would require a paired write path under a Wendy-owned
// Credential Manager target. Matches the Linux behavior.
func lookupKeychainPassword(_ string) (string, error) {
	return "", nil
}
