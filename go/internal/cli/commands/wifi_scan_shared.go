package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// errNoWifiAdapter signals that the host has no usable WiFi hardware, as
// opposed to a transient scan failure. Platform scanners return it (possibly
// wrapped) so the install flow can show a specific message (WDY-1474).
// Keep its text in sync with the user-facing copy in wifiScanFailureNotice.
var errNoWifiAdapter = errors.New("no WiFi adapter detected on this machine")

// exitErrWithStderr enriches an error from exec's Output() with the captured
// stderr, which otherwise surfaces only as "exit status N". Non-ExitError
// values and empty stderr pass through unchanged.
func exitErrWithStderr(err error) error {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if stderr := bytes.TrimSpace(ee.Stderr); len(stderr) > 0 {
			return fmt.Errorf("%w: %s", err, stderr)
		}
	}
	return err
}

// streamLocalWifiScan emits host WiFi scan results into emit as they become
// available, so an interactive picker paints immediately instead of sitting on
// "Scanning..." for the full duration of a fresh rescan. It first emits the
// instant cached set (cachedLocalWifiNetworks), then the authoritative fresh
// set once the platform rescan completes (scanLocalWifiNetworks). This mirrors
// the trickle-in behavior of the device-side picker (pickWifiNetwork), where
// SSIDs appear while the rescan runs rather than all at once at the end.
//
// emit may be called zero, one, or two times with cumulative batches; callers
// feed each into the picker, which dedups by SSID. The returned error is the
// fresh scan's error (errNoWifiAdapter, a transient failure, or nil) — the
// cached pre-paint is best-effort and never surfaces an error.
func streamLocalWifiScan(ctx context.Context, emit func([]localWifiNetwork)) error {
	if cached := cachedLocalWifiNetworks(ctx); len(cached) > 0 {
		emit(cached)
	}
	nets, err := scanLocalWifiNetworks(ctx)
	if err != nil {
		return err
	}
	if len(nets) > 0 {
		emit(nets)
	}
	return nil
}

// wifiScanFailureNotice renders the user-facing message shown when a host
// WiFi scan produced no usable networks. scanErr == nil means the scan
// succeeded but found nothing.
func wifiScanFailureNotice(scanErr error) string {
	switch {
	case scanErr == nil:
		msg := "No WiFi networks found."
		if wifiScanCacheHint != "" {
			msg += " " + wifiScanCacheHint + "."
		}
		return msg
	case errors.Is(scanErr, errNoWifiAdapter):
		return "No WiFi adapter detected on this machine."
	default:
		return fmt.Sprintf("WiFi scan failed: %v", scanErr)
	}
}
