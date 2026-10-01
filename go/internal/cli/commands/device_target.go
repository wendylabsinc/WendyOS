package commands

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// deviceEnvVar selects a target device for every command run in one shell — or
// one AI session, via the MCP server's env block — without touching the saved
// default that every session shares. Precedence: --device > WENDY_DEVICE >
// saved default.
const deviceEnvVar = "WENDY_DEVICE"

// noDeviceMessage is the error for a command that needs a device and has none.
//
// It must keep the exact prefix "no device specified; use --device flag or set
// a default" verbatim: two enabled Swift E2E tests match on that substring —
// swift/WendyE2ETests/Tests/WendyE2ETests/WendyDeviceVersionTests.swift:59 and
// swift/WendyE2ETests/Tests/WendyE2ETests/WendyDeviceInfoTests.swift:266-268 —
// reached through the JSON-mode branches at helpers.go's resolveDeviceAddress,
// connectToAgentInner and resolveTargetInner. Do not shorten or reword that
// prefix; extend the message after it instead.
const noDeviceMessage = "no device specified; use --device flag or set a default with 'wendy device set-default <device>', or set WENDY_DEVICE for this shell"

// deviceFlagFromEnv is the value applyDeviceEnv copied from WENDY_DEVICE into
// deviceFlag, or "" when --device was given or the variable is unset.
var deviceFlagFromEnv string

// envDevice returns WENDY_DEVICE with surrounding whitespace removed; a blank
// value counts as unset.
func envDevice() string {
	return strings.TrimSpace(os.Getenv(deviceEnvVar))
}

// applyDeviceEnv makes WENDY_DEVICE behave exactly like --device when the flag
// was not given. The root pre-run calls it once, before any command reads
// deviceFlag, so every resolution path — direct, cloud, run's cloud fallback,
// foxglove, build host — honours it without a lookup of its own.
func applyDeviceEnv() {
	deviceFlagFromEnv = ""
	if deviceFlag != "" {
		return
	}
	if v := envDevice(); v != "" {
		deviceFlag = v
		deviceFlagFromEnv = v
	}
}

// deviceChosenByEnv reports whether the current target came from WENDY_DEVICE.
// Code that later rewrites deviceFlag (ros2 exec's trailing --device, HIL's
// vm: selection, run's fleet split) ends that, because the target is then no
// longer the variable's.
func deviceChosenByEnv() bool {
	return deviceFlagFromEnv != "" && deviceFlag == deviceFlagFromEnv
}

// hilDeviceSelector is the device `wendy run --hil` chooses its simulator
// from. WENDY_DEVICE usually names the real device — the HIL peer — so HIL
// takes the variable only when it names a simulator (vm:NAME, sim, simulator)
// and otherwise opens its simulator picker as if no device were given. An
// explicit --device is returned unchanged.
func hilDeviceSelector() string {
	if !deviceChosenByEnv() {
		return deviceFlag
	}
	// A malformed vm: selector still names a simulator; keep it so HIL
	// reports it rather than silently picking another.
	if _, matched, err := simulatorName(deviceFlag); matched || err != nil {
		return deviceFlag
	}
	return ""
}

// noteEnvDevice tells the user a command acted on the device named by
// WENDY_DEVICE, once a connection exists. explicit is a device the caller
// named in code (SelectDevice), which outranks the variable.
func noteEnvDevice(explicit string) {
	if explicit != "" || !deviceChosenByEnv() || os.Getenv("WENDY_AGENT_SOCKET") != "" {
		return
	}
	noteImplicitDevice(deviceFlag, implicitEnvDevice)
}

// resolveOptionsDevice is the device a resolveTarget caller named through
// SelectDevice, or "".
func resolveOptionsDevice(opts []resolveOption) string {
	cfg := resolveConfig{excludeProviderKeys: make(map[string]bool)}
	for _, o := range opts {
		o(&cfg)
	}
	return cfg.device
}

// withoutDeviceOverride runs fn with --device and WENDY_DEVICE suspended, so a
// connection made inside it resolves to the saved default. set-default uses it
// to confirm and pin the device it just saved.
func withoutDeviceOverride(fn func()) {
	prevFlag, prevEnv := deviceFlag, deviceFlagFromEnv
	deviceFlag, deviceFlagFromEnv = "", ""
	defer func() { deviceFlag, deviceFlagFromEnv = prevFlag, prevEnv }()
	fn()
}

// mcpStartupDevice picks the device `wendy mcp serve` connects to on startup,
// with the same precedence as every other command: its own -d/--device, then
// WENDY_DEVICE (set per AI session in the MCP client's env block), then the
// saved default.
func mcpStartupDevice(flag string, cfg *config.Config) string {
	if flag != "" {
		return flag
	}
	if env := envDevice(); env != "" {
		return env
	}
	if cfg != nil {
		return cfg.DefaultDevice
	}
	return ""
}

// errInvalidDeviceName marks a device name the direct path cannot dial.
var errInvalidDeviceName = errors.New("invalid device name")

// allDigitDeviceHost returns device's host part (the port stripped) and
// whether it is made only of ASCII digits.
func allDigitDeviceHost(device string) (host string, numeric bool) {
	host = strings.TrimSpace(device)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "" {
		return "", false
	}
	for _, r := range host {
		if r < '0' || r > '9' {
			return host, false
		}
	}
	return host, true
}

// rejectNumericDeviceName refuses an all-digit device on the direct path
// (WDY-3126). Such a value is almost always a cloud asset ID — the cloud path
// accepts one — but a direct connection would dial "283:50051", and the macOS
// resolver answers "283" with 0.0.1.27: a connection attempt to an unrelated
// address instead of an error. Callers run it only after the cloud-context
// branch, so `wendy cloud device … --device 283` is unaffected.
func rejectNumericDeviceName(device string) error {
	host, numeric := allDigitDeviceHost(device)
	if !numeric {
		return nil
	}
	return commandErrorf(errInvalidDeviceName, "%s", numericDeviceMessage(strings.TrimSpace(device), host, numericCloudSelector(host)))
}

// numericCloudSelector is the cloud selector for asset id under the current
// login, or "" when no single login can name it.
func numericCloudSelector(id string) string {
	assetID, err := strconv.ParseInt(id, 10, 32)
	if err != nil || assetID <= 0 {
		return ""
	}
	cfg, err := config.Load()
	if err != nil {
		return ""
	}
	auth, err := config.ResolveAuth(cfg, "", nil)
	if err != nil || auth.CloudGRPC == "" || cloudAuthOrgID(auth) <= 0 {
		return ""
	}
	return cloudDeviceSelector{Endpoint: auth.CloudGRPC, OrgID: cloudAuthOrgID(auth), AssetID: int32(assetID)}.String()
}

func numericDeviceMessage(device, host, selector string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "device %q is only digits, so it is not a hostname or IP address: a direct connection would dial %s, which the system resolver can turn into an unrelated address.\n",
		device, hostPort(host, defaultAgentPort))
	if selector != "" {
		fmt.Fprintf(&b, "If %s is a Wendy Cloud asset ID, name it by its cloud selector:\n", host)
		fmt.Fprintf(&b, "  wendy --device %s <command>\n", selector)
		fmt.Fprintf(&b, "  wendy device set-default %s\n", selector)
	} else {
		fmt.Fprintf(&b, "If %s is a Wendy Cloud asset ID, name it by its cloud selector (log in with 'wendy cloud login' to see yours):\n", host)
		fmt.Fprintf(&b, "  wendy --device cloud://<cloud-grpc-host:port>/org/<org-id>/asset/%s <command>\n", host)
	}
	fmt.Fprintf(&b, "or reach it through the cloud for one command:\n  wendy cloud device <command> --device %s\n", host)
	b.WriteString("For a device on your network, use its hostname (for example wendyos-name.local) or IP address.")
	return b.String()
}
