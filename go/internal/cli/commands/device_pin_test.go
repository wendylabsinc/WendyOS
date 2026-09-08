package commands

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discoverycache"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// TestCloudGRPCForOrg maps the org carried by a verifying mTLS cert back to the
// cloud host of the auth session that issued it — the cloud half of the
// WDY-1149 (org, cloud) pin.
func TestCloudGRPCForOrg(t *testing.T) {
	cfg := &config.Config{Auth: []config.AuthConfig{
		{CloudGRPC: "grpc.a.sh:443", Certificates: []config.CertificateInfo{{OrganizationID: 7}}},
		{CloudGRPC: "grpc.b.sh:443", Certificates: []config.CertificateInfo{{OrganizationID: 9}}},
	}}

	if got := cloudGRPCForOrg(cfg, 9); got != "grpc.b.sh:443" {
		t.Errorf("org 9: got %q, want grpc.b.sh:443", got)
	}
	if got := cloudGRPCForOrg(cfg, 7); got != "grpc.a.sh:443" {
		t.Errorf("org 7: got %q, want grpc.a.sh:443", got)
	}
	if got := cloudGRPCForOrg(cfg, 42); got != "" {
		t.Errorf("unknown org: got %q, want empty", got)
	}
}

// writePinTestConfig points config.Load/Save at a temp HOME holding an auth
// session for org 7 on grpc.a.sh:443 plus the given pins, and returns a reader
// for the pins as they stand on disk.
func writePinTestConfig(t *testing.T, pins map[string]config.DevicePin) func() map[string]config.DevicePin {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// config.Load resolves ~/.wendy via os.UserHomeDir, which reads USERPROFILE
	// on Windows and HOME elsewhere.
	t.Setenv("USERPROFILE", home)

	cfg := &config.Config{
		Auth: []config.AuthConfig{
			{CloudGRPC: "grpc.a.sh:443", Certificates: []config.CertificateInfo{{OrganizationID: 7}}},
		},
		DevicePins: pins,
	}
	dir := filepath.Join(home, ".wendy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return func() map[string]config.DevicePin {
		t.Helper()
		got, loadErr := config.Load()
		if loadErr != nil {
			t.Fatalf("reload config: %v", loadErr)
		}
		return got.DevicePins
	}
}

// TestEnforceDeviceIdentityRejectsUnprovisionedPinnedDevice is the core of this
// change: a hostname we have previously seen enrolled now answers with no mTLS
// identity at all. That is a reflash, a factory reset, or something else
// squatting the name — never a silent "hint and continue".
func TestEnforceDeviceIdentityRejectsUnprovisionedPinnedDevice(t *testing.T) {
	stubNonInteractive(t)
	readPins := writePinTestConfig(t, map[string]config.DevicePin{
		"wendy-thor": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"},
	})

	err := enforceDeviceIdentity("wendy-thor.local", observedDeviceIdentity{})
	if err == nil {
		t.Fatal("pinned device answering unprovisioned: want an error, got nil")
	}
	if !strings.Contains(err.Error(), "wendy-thor") {
		t.Errorf("error %q does not name the device", err)
	}
	// Refusing must not quietly discard the pin — the next connect has to make
	// the same challenge, not sail through as a first use.
	if pin, ok := readPins()["wendy-thor"]; !ok || pin.AssetID != "42" {
		t.Errorf("pin after refusal = %+v (present=%v), want the original pin intact", pin, ok)
	}
}

// TestEnforceDeviceIdentityAllowsUnpinnedUnprovisionedDevice keeps the normal
// out-of-the-box flow working: a device that was never enrolled has no pin, so
// connecting to it over plaintext is exactly what is supposed to happen.
func TestEnforceDeviceIdentityAllowsUnpinnedUnprovisionedDevice(t *testing.T) {
	stubNonInteractive(t)
	writePinTestConfig(t, nil)

	if err := enforceDeviceIdentity("wendy-thor.local", observedDeviceIdentity{}); err != nil {
		t.Fatalf("unpinned unprovisioned device: want nil, got %v", err)
	}
}

func TestEnforceDeviceIdentityRecordsAssetOnFirstUse(t *testing.T) {
	stubNonInteractive(t)
	readPins := writePinTestConfig(t, nil)

	err := enforceDeviceIdentity("wendy-thor.local", observedDeviceIdentity{mTLS: true, orgID: 7, assetID: "42"})
	if err != nil {
		t.Fatalf("first use: want nil, got %v", err)
	}
	pin, ok := readPins()["wendy-thor"]
	if !ok {
		t.Fatal("first use did not record a pin")
	}
	if pin.OrgID != 7 || pin.CloudGRPC != "grpc.a.sh:443" || pin.AssetID != "42" {
		t.Errorf("recorded pin = %+v, want org 7 / grpc.a.sh:443 / asset 42", pin)
	}
}

// TestEnforceDeviceIdentityRejectsDifferentAsset is the org-blind case the pin
// used to miss entirely: both devices are legitimately enrolled in org 7 via
// the same cloud, so only the asset id reveals that the hostname now points at
// a different machine.
func TestEnforceDeviceIdentityRejectsDifferentAsset(t *testing.T) {
	stubNonInteractive(t)
	readPins := writePinTestConfig(t, map[string]config.DevicePin{
		"wendy-thor": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"},
	})

	err := enforceDeviceIdentity("wendy-thor.local", observedDeviceIdentity{mTLS: true, orgID: 7, assetID: "43"})
	if err == nil {
		t.Fatal("different asset id at pinned hostname: want an error, got nil")
	}
	if pin := readPins()["wendy-thor"]; pin.AssetID != "42" {
		t.Errorf("pin after refusal = %+v, want the original asset 42 intact", pin)
	}
}

func TestEnforceDeviceIdentityAcceptsRotatedCertForSameDevice(t *testing.T) {
	stubNonInteractive(t)
	writePinTestConfig(t, map[string]config.DevicePin{
		"wendy-thor": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"},
	})

	if err := enforceDeviceIdentity("wendy-thor.local", observedDeviceIdentity{mTLS: true, orgID: 7, assetID: "42"}); err != nil {
		t.Fatalf("same org+cloud+asset (rotated cert): want nil, got %v", err)
	}
}

// TestEnforceDeviceIdentityBackfillsLegacyPin upgrades pins written before
// asset ids existed, without prompting: org and cloud already match, so there
// is nothing to challenge — but the asset must be recorded so the NEXT swap is
// caught.
func TestEnforceDeviceIdentityBackfillsLegacyPin(t *testing.T) {
	stubNonInteractive(t)
	readPins := writePinTestConfig(t, map[string]config.DevicePin{
		"wendy-thor": {OrgID: 7, CloudGRPC: "grpc.a.sh:443"},
	})

	if err := enforceDeviceIdentity("wendy-thor.local", observedDeviceIdentity{mTLS: true, orgID: 7, assetID: "42"}); err != nil {
		t.Fatalf("legacy pin backfill: want nil, got %v", err)
	}
	if pin := readPins()["wendy-thor"]; pin.AssetID != "42" {
		t.Errorf("pin after backfill = %+v, want asset 42 recorded", pin)
	}
}

// TestEnforceDeviceIdentityAcceptsAssetlessCert keeps older agents usable: a
// certificate with no asset identity proves nothing about which device it is,
// and absence of evidence must not read as a swap.
func TestEnforceDeviceIdentityAcceptsAssetlessCert(t *testing.T) {
	stubNonInteractive(t)
	readPins := writePinTestConfig(t, map[string]config.DevicePin{
		"wendy-thor": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"},
	})

	if err := enforceDeviceIdentity("wendy-thor.local", observedDeviceIdentity{mTLS: true, orgID: 7}); err != nil {
		t.Fatalf("cert without asset identity: want nil, got %v", err)
	}
	if pin := readPins()["wendy-thor"]; pin.AssetID != "42" {
		t.Errorf("pin = %+v, want asset 42 left untouched", pin)
	}
}

// TestClearDevicePinForRepin backs the escape hatch both refusal messages point
// at: `wendy device set-default <host>` names the device explicitly, which is
// the user asserting they mean this one — so it drops the stale pin and lets
// the connect that follows record a fresh one. Without this the advice is a
// dead end, because that command's own connect hits the same refusal.
func TestClearDevicePinForRepin(t *testing.T) {
	readPins := writePinTestConfig(t, map[string]config.DevicePin{
		"wendy-thor": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"},
		"wendy-orin": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "9"},
	})

	clearDevicePinForRepin("wendy-thor.local")

	pins := readPins()
	if _, ok := pins["wendy-thor"]; ok {
		t.Error("pin for the named device survived clearDevicePinForRepin")
	}
	if _, ok := pins["wendy-orin"]; !ok {
		t.Error("clearDevicePinForRepin dropped an unrelated device's pin")
	}
}

// TestSetDefaultClearsPinForHostPortDevice covers the escape hatch for the one
// default shape that used to have none: `wendy device set-default
// my-host.local:50051`. Enforcement keys that host under "my-host" (pinKeyForAddr
// strips the port), so passing set-default's raw argument through to
// clearDevicePinForRepin cleared a key nothing was ever filed under — the pin
// survived, the connect that follows hit the refusal, and with the interactive
// "trust it anyway?" prompt now gone there was no other way back.
func TestSetDefaultClearsPinForHostPortDevice(t *testing.T) {
	stubNonInteractive(t)
	readPins := writePinTestConfig(t, map[string]config.DevicePin{
		"wendy-thor": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"},
		"wendy-orin": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "9"},
	})

	// set-default connects afterwards to record a fresh pin. Keep that off the
	// network: nothing resolves, so the dial fails and set-default ignores it
	// (an offline device is pinned on its next successful connection instead).
	origLookup, origBrowse, origLadder := osLookupHostFn, lanBrowseFn, dialAgentLadderFn
	osLookupHostFn = func(context.Context, string) ([]string, error) { return nil, errors.New("no resolver in test") }
	lanBrowseFn = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
	dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
		return nil, nil, errors.New("device offline in test")
	}
	t.Cleanup(func() { osLookupHostFn, lanBrowseFn, dialAgentLadderFn = origLookup, origBrowse, origLadder })

	cmd := newDeviceSetDefaultCmd()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd.SetContext(ctx)
	if err := cmd.RunE(cmd, []string{"wendy-thor.local:50051"}); err != nil {
		t.Fatalf("set-default with an explicit port: %v", err)
	}

	pins := readPins()
	if pin, ok := pins["wendy-thor"]; ok {
		t.Errorf("pin %+v survived `set-default wendy-thor.local:50051`: the user named the device explicitly and still has no way past its refusal", pin)
	}
	if _, ok := pins["wendy-orin"]; !ok {
		t.Error("set-default dropped an unrelated device's pin")
	}
}

// TestEnforceDeviceIdentityHardFails covers the approved policy change: a
// mismatch is refused identically in every mode, with no prompt.
//
// The arrangement is deliberately the one the old code let through — an
// interactive TTY, where a user could answer "yes" — because that answer is
// precisely what an attacker needs. "The human consented" is not evidence about
// which device answered, so it must not be a way through.
func TestEnforceDeviceIdentityHardFails(t *testing.T) {
	// Org 7 asset 43 against a pin for org 7 asset 42: both devices are
	// legitimately enrolled in the same organisation and cloud, so only the
	// asset id says the hostname now points at a different machine. It is the
	// case a user is least able to adjudicate from a one-line prompt.
	readPins := writePinTestConfig(t, map[string]config.DevicePin{
		"wendy-thor": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"},
	})
	obs := observedDeviceIdentity{mTLS: true, orgID: 7, assetID: "43"}

	origJSON := jsonOutput
	t.Cleanup(func() { jsonOutput = origJSON })

	// Interactive, human output: the mode that used to prompt.
	stubInteractive(t)
	jsonOutput = false
	interactiveErr := enforceDeviceIdentity("wendy-thor.local", obs)
	if interactiveErr == nil {
		t.Fatal("interactive mismatch: want a refusal, got nil — a prompt is still deciding this")
	}
	if !strings.Contains(interactiveErr.Error(), "wendy device unpin wendy-thor.local") {
		t.Errorf("refusal %q does not name the command that resolves it", interactiveErr)
	}

	// A prompt answered "yes" would have re-pinned to asset 43. The original pin
	// surviving is the observable proof that nothing accepted the new identity —
	// an assertion on the error alone would also pass if the user had declined a
	// prompt that still ran.
	if pin := readPins()["wendy-thor"]; pin.AssetID != "42" {
		t.Errorf("pin after refusal = %+v, want the original asset 42 intact", pin)
	}

	// Same input, other two modes. Identical text is the point: a refusal that
	// reads differently depending on where it is printed invites the reader to
	// assume the interactive one is negotiable.
	jsonOutput = true
	jsonErr := enforceDeviceIdentity("wendy-thor.local", obs)
	jsonOutput = false
	stubNonInteractive(t)
	headlessErr := enforceDeviceIdentity("wendy-thor.local", obs)

	if jsonErr == nil || headlessErr == nil {
		t.Fatalf("mismatch must be refused in every mode: json=%v, non-interactive=%v", jsonErr, headlessErr)
	}
	if jsonErr.Error() != interactiveErr.Error() || headlessErr.Error() != interactiveErr.Error() {
		t.Errorf("refusal differs by mode:\n  interactive:     %v\n  json:            %v\n  non-interactive: %v", interactiveErr, jsonErr, headlessErr)
	}

	// The assertions above prove the OUTCOME no longer depends on the mode. This
	// proves the MECHANISM is gone: with no confirmation prompt anywhere in the
	// identity path, there is no mode-dependent branch left for one to hide in.
	// A function seam would only catch a prompt re-added THROUGH the seam; the
	// likely regression — someone reaching for tui.Confirm* directly, to be
	// helpful — is what this catches.
	//
	// The identity path is located by its function declarations across every
	// non-test file in the package, not by filename: hardcoding "device_pin.go"
	// would make this assertion pass while covering nothing the moment the
	// identity code moves to another file.
	assertNoPromptInIdentityPath(t)
}

// assertNoPromptInIdentityPath fails if whichever non-test file in this package
// declares the identity refusals also reaches for an interactive confirmation,
// and fails just as loudly if no file declares them at all — an assertion that
// has quietly stopped scanning anything is worse than no assertion.
func assertNoPromptInIdentityPath(t *testing.T) {
	t.Helper()

	// The declarations that mark a file as part of the identity path. Both
	// refusals live behind these two.
	decls := []string{"func enforceDeviceIdentity(", "func challengeUnprovisionedDevice("}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package directory: %v", err)
	}
	found := make(map[string]string, len(decls))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, readErr := os.ReadFile(name)
		if readErr != nil {
			t.Fatalf("reading %s: %v", name, readErr)
		}
		text := string(src)
		var declared []string
		for _, decl := range decls {
			if strings.Contains(text, decl) {
				declared = append(declared, decl)
				found[decl] = name
			}
		}
		if len(declared) == 0 {
			continue
		}
		if strings.Contains(text, "tui.Confirm") {
			t.Errorf("%s declares %v and calls a tui.Confirm* prompt: the identity path must refuse, never ask — a MITM warning that can be dismissed gets dismissed", name, declared)
		}
	}
	for _, decl := range decls {
		if found[decl] == "" {
			t.Errorf("no non-test file in this package declares %q, so this check is scanning nothing; point it at wherever the identity refusals now live", decl)
		}
	}
}

// TestEnforceDeviceIdentityAppliesToNonDefault guards the scope widening: the
// pin used to be checked only for the saved default device, so `--device
// <host>` — just as spoofable — went unchecked.
//
// It also pins down the correctness trap in that widening. The LKG stub asserts
// the key the DIAL was constrained by, and the refusal text carries the key the
// CHECK was made against; asserting both are "wendy-thor.local" is what proves
// a host cannot be pinned under one key and verified under another, which would
// disable enforcement while looking like it works.
func TestEnforceDeviceIdentityAppliesToNonDefault(t *testing.T) {
	stubNonInteractive(t)

	origFlag := deviceFlag
	deviceFlag = "wendy-thor.local"
	t.Cleanup(func() { deviceFlag = origFlag })

	readPins := writePinTestConfig(t, map[string]config.DevicePin{
		"wendy-thor": {OrgID: 9, CloudGRPC: "grpc.b.sh:443", AssetID: "42"},
	})
	// The whole point is that this device is NOT the default: with a default set
	// the old code would have checked it anyway and the test would prove nothing.
	if cfg, err := config.Load(); err != nil || cfg.DefaultDevice != "" {
		t.Fatalf("precondition: no default device may be set (default=%q, err=%v)", cfg.DefaultDevice, err)
	}

	// Reach the device over the cache fast path so no name resolution or real
	// dialling happens; the connection it hands back carries a certificate for
	// org 7, while the pin above says org 9.
	seedLKGCache(t, discoverycache.Entry{
		ID: "dev-1", Hostname: "wendy-thor.local", IP: "10.0.0.9", Port: 50052, MTLS: true,
	}, 0)
	var dialedPinKey string
	origLKG := dialAgentLKGFn
	dialAgentLKGFn = func(ctx context.Context, e discoverycache.Entry, pinKey string) (*grpcclient.AgentConnection, error, lkgOutcome) {
		dialedPinKey = pinKey
		return &grpcclient.AgentConnection{
			IsMTLS:   true,
			CertInfo: &config.CertificateInfo{OrganizationID: 7},
		}, nil, lkgConnected
	}
	origLadder := dialAgentLadderFn
	dialAgentLadderFn = func(ctx context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
		t.Errorf("general ladder ran despite an LKG hit (addr %s)", target.Addr)
		return nil, nil, errors.New("unreachable")
	}
	origDiscover := discoverLANDevices
	discoverLANDevices = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
	t.Cleanup(func() {
		dialAgentLKGFn, dialAgentLadderFn = origLKG, origLadder
		discoverLANDevices = origDiscover
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := connectToAgent(ctx, SuppressProvisioningHint(), SuppressUpdateCheck(), NonInteractive())
	if conn != nil {
		conn.Close()
		t.Fatal("connectToAgent returned a connection to a --device host whose certificate contradicts its pin")
	}
	if err == nil {
		t.Fatal("connectToAgent returned no error for a --device host whose certificate contradicts its pin")
	}
	if !strings.Contains(err.Error(), `device "wendy-thor.local" identity changed`) ||
		!strings.Contains(err.Error(), "wendy device unpin wendy-thor.local") {
		t.Fatalf("err = %v, want the identity refusal naming the device and the unpin escape hatch", err)
	}
	if dialedPinKey != "wendy-thor.local" {
		t.Errorf("dial was constrained by pin key %q, but the check was made against \"wendy-thor.local\"; a host pinned under one key and verified under another is unenforced", dialedPinKey)
	}
	if pin := readPins()["wendy-thor"]; pin.OrgID != 9 {
		t.Errorf("pin after refusal = %+v, want the original org 9 intact", pin)
	}
}

// TestPickerPinKeyMatchesDeviceFlagKey is the fourth path's half of the same
// agreement TestEnforceDeviceIdentityAppliesToNonDefault proves for --device:
// picking a device from the TUI must file its pin exactly where naming it on
// the command line looks for it.
//
// The DisplayName here is the shape a real WendyOS device advertises — a
// Title-Cased friendly name from the `displayname` TXT record, built from the
// device name, while the hostname is "wendyos-" + that name. Keying on it would
// produce two pins for one device, each path blind to the other's, which reads
// as enforcement while enforcing nothing.
func TestPickerPinKeyMatchesDeviceFlagKey(t *testing.T) {
	lan := &models.LANDevice{DisplayName: "Agx Orin", Hostname: "wendyos-agx-orin.local"}

	origFlag := deviceFlag
	deviceFlag = "wendyos-agx-orin.local"
	t.Cleanup(func() { deviceFlag = origFlag })
	_, flagKey, _, err := resolveDeviceAddress()
	if err != nil {
		t.Fatalf("resolveDeviceAddress: %v", err)
	}

	cfg := &config.Config{}
	cfg.SetDevicePin(pinKeyForLANDevice(lan), 7, "grpc.a.sh:443", "42", "")
	if _, ok := cfg.DevicePinFor(flagKey); !ok {
		t.Fatalf("a pin recorded for the picked device (key %q) is invisible to --device %q (key %q): pins under %v",
			pinKeyForLANDevice(lan), deviceFlag, flagKey, cfg.DevicePins)
	}

	// And the trap itself: the display name must not be what keys the pin.
	if strings.EqualFold(pinKeyForLANDevice(lan), lan.DisplayName) {
		t.Errorf("picker pin key = %q, want the hostname %q — the display name is not a transform of the hostname", pinKeyForLANDevice(lan), lan.Hostname)
	}
}

// TestPickerEnforcesPinBeforeAgentUpdate is the picker path's ORDERING, which
// is a separate question from whether the pin is checked at all.
//
// With no default device, `wendy run` goes straight to the picker. If something
// squats the pinned hostname and answers, every step taken between the connect
// and the pin check is a step taken against a device the CLI is about to
// reject — and one of those steps is the agent update check, which prompts
// "Update the agent now?" and, on a yes, uploads a wendy-agent binary and
// restarts it. Pushing an executable to an unverified device and only then
// deciding not to trust it is the wrong order in the way that matters.
//
// The named-device path states this invariant in connectToAgent; this asserts
// the picker path honours it too, by making the update check fail the test if
// it is reached at all.
func TestPickerEnforcesPinBeforeAgentUpdate(t *testing.T) {
	stubNonInteractive(t)
	readPins := writePinTestConfig(t, map[string]config.DevicePin{
		"wendyos-agx-orin": {OrgID: 9, CloudGRPC: "grpc.b.sh:443", AssetID: "42"},
	})

	lan := &models.LANDevice{
		DisplayName: "Agx Orin",
		Hostname:    "wendyos-agx-orin.local",
		IPAddress:   "10.0.0.9",
		Port:        50051,
		IsMTLS:      true,
	}

	// Whoever answers there presents a certificate for org 7; the pin above says
	// this hostname is an org 9 device. Nothing about the picker row is evidence
	// either way — mDNS is unauthenticated, so the row is the attacker's claim.
	origLadder := dialAgentLadderFn
	dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
		return &grpcclient.AgentConnection{
			IsMTLS:   true,
			CertInfo: &config.CertificateInfo{OrganizationID: 7},
		}, nil, nil
	}
	origUpdate := checkAndOfferUpdateFn
	checkAndOfferUpdateFn = func(_ context.Context, conn *grpcclient.AgentConnection) (*grpcclient.AgentConnection, error) {
		t.Error("the agent update check ran on a picker selection whose pin refuses it: that check can upload and restart a wendy-agent binary, so reaching it means the CLI offered to push an executable to a device it had not verified")
		return conn, nil
	}
	t.Cleanup(func() { dialAgentLadderFn, checkAndOfferUpdateFn = origLadder, origUpdate })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// suppressUpdateCheck is false on purpose: the update check is ENABLED here,
	// so the only thing that can keep it from running is the pin standing in
	// front of it.
	picked := &models.DiscoveredDevice{DisplayName: lan.DisplayName, LAN: lan}
	sel, err := connectPickedLANDevice(ctx, picked, preferredLANAddress(*lan), false)
	if err == nil {
		if sel != nil && sel.Agent != nil {
			sel.Agent.Close()
		}
		t.Fatal("picker selection returned a connection to a device whose certificate contradicts its pin")
	}
	if sel != nil {
		t.Errorf("refused selection = %+v, want nil — a refusal must not hand back a usable target", sel)
	}
	if !strings.Contains(err.Error(), `device "wendyos-agx-orin.local" identity changed`) ||
		!strings.Contains(err.Error(), "wendy device unpin wendyos-agx-orin.local") {
		t.Fatalf("err = %v, want the identity refusal naming the picked device and the unpin escape hatch", err)
	}
	if pin := readPins()["wendyos-agx-orin"]; pin.OrgID != 9 {
		t.Errorf("pin after refusal = %+v, want the original org 9 intact", pin)
	}

	// A refusal is not "the LAN attempt failed", so it must not slide into the
	// Bluetooth fallback: that would reach the very device the pin just
	// rejected, over a second transport, and report success.
	withBLE := &models.DiscoveredDevice{
		DisplayName: lan.DisplayName,
		LAN:         lan,
		Bluetooth:   &models.BluetoothDevice{DisplayName: "Agx Orin"},
	}
	bleSel, bleErr := connectPickedLANDevice(ctx, withBLE, preferredLANAddress(*lan), false)
	if bleErr == nil || (bleSel != nil && bleSel.Bluetooth != nil) {
		t.Fatalf("a refused pin fell back to Bluetooth (sel=%+v, err=%v); the BLE fallback is for a device that did not answer, not one that answered as somebody else", bleSel, bleErr)
	}
}

// TestPickerLadderRefusalDoesNotFallBackToBluetooth closes the gap the doc
// comment on connectPickedLANDevice already claimed was closed.
//
// There are two refusals that can reach the picker, and only one of them
// arrived where the Bluetooth fallback could not see it. The post-connect one
// (TestPickerEnforcesPinBeforeAgentUpdate) comes back after a successful dial.
// The dial ladder's own — a wrong-device abort, or a pinned host that offered
// no authenticated endpoint — comes back as a connect ERROR, and the very next
// line handed the caller the row's BLE half instead.
//
// That is the whole attack: answer the victim's mDNS name over LAN, get
// rejected, and have the CLI quietly connect to a BLE peer advertising the same
// name — where attemptBLEConnect sets no ExpectedIdentity and
// enforceSelectedDevicePin is a no-op for a Bluetooth selection, so nothing
// checks anything.
func TestPickerLadderRefusalDoesNotFallBackToBluetooth(t *testing.T) {
	stubNonInteractive(t)
	writePinTestConfig(t, map[string]config.DevicePin{
		"wendyos-agx-orin": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"},
	})
	setPinCache(t)

	lan := &models.LANDevice{
		DisplayName: "Agx Orin",
		Hostname:    "wendyos-agx-orin.local",
		IPAddress:   "10.0.0.9",
		Port:        50051,
		IsMTLS:      true,
	}

	refusals := map[string]error{
		"wrong device answered": identityRefusal("wendyos-agx-orin.local", &certs.IdentityMismatchError{
			WantOrg: 7, WantAsset: "42", GotOrg: 7, GotAsset: "43",
		}),
		"pinned host offered no authenticated endpoint": pinnedHostNoAuthenticatedEndpointError(
			"wendyos-agx-orin.local", []string{"10.0.0.9:50051"},
			[]mtlsAttemptError{{addr: "10.0.0.9:50051", err: errors.New("connection refused")}}, false),
	}

	for name, refusal := range refusals {
		t.Run(name, func(t *testing.T) {
			origLadder := dialAgentLadderFn
			dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
				return nil, nil, refusal
			}
			origUpdate := checkAndOfferUpdateFn
			checkAndOfferUpdateFn = func(_ context.Context, conn *grpcclient.AgentConnection) (*grpcclient.AgentConnection, error) {
				t.Error("the agent update check ran despite the dial being refused")
				return conn, nil
			}
			t.Cleanup(func() { dialAgentLadderFn, checkAndOfferUpdateFn = origLadder, origUpdate })

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			withBLE := &models.DiscoveredDevice{
				DisplayName: lan.DisplayName,
				LAN:         lan,
				Bluetooth:   &models.BluetoothDevice{DisplayName: "Agx Orin"},
			}
			sel, err := connectPickedLANDevice(ctx, withBLE, preferredLANAddress(*lan), false)
			if err == nil {
				t.Fatalf("a refused dial fell through to a usable selection (%+v); the BLE fallback is for a device that did not answer, not one that answered as somebody else", sel)
			}
			if sel != nil {
				t.Errorf("refused selection = %+v, want nil", sel)
			}
			// Typed, not text-matched. The two refusals are deliberately
			// DIFFERENT sentinels — an unreachable device must not be told its
			// identity is suspect — so the gate is the predicate that spans
			// them, not either sentinel alone. Asserting one sentinel here is
			// what would let the split silently reopen the fallback.
			if !blocksUnauthenticatedFallback(err) {
				t.Errorf("err = %v does not block the unauthenticated fallback; the fallback is decided on the error's type, not its wording", err)
			}
		})
	}

	// The fallback itself must survive: an ordinary connect failure — the device
	// is off, the port is shut — is exactly what BLE is there for.
	t.Run("an ordinary connect failure still falls back", func(t *testing.T) {
		origLadder := dialAgentLadderFn
		dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
			return nil, nil, errors.New("dial tcp 10.0.0.9:50051: connect: connection refused")
		}
		t.Cleanup(func() { dialAgentLadderFn = origLadder })

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		ble := &models.BluetoothDevice{DisplayName: "Agx Orin"}
		withBLE := &models.DiscoveredDevice{DisplayName: lan.DisplayName, LAN: lan, Bluetooth: ble}
		sel, err := connectPickedLANDevice(ctx, withBLE, preferredLANAddress(*lan), false)
		if err != nil {
			t.Fatalf("an unreachable LAN half must still fall back to BLE, got %v", err)
		}
		if sel == nil || sel.Bluetooth != ble {
			t.Fatalf("selection = %+v, want the row's Bluetooth half", sel)
		}
	})
}

// TestDefaultDeviceNameForPrefersPinKey covers what `wendy device set-default`
// with no argument stores.
//
// selected.Agent.Host is the address the picker dialled, which on any LAN row
// is an IP. Saving that made the default device a DHCP lease — and worse, it
// keyed every later connection's pin on the IP while the picker had just pinned
// the device under its HOSTNAME seconds earlier. pinCandidateKeys cannot map an
// IP back to a hostname row, so that pin was never consulted again and
// enforcement was silently off for the whole configuration.
func TestDefaultDeviceNameForPrefersPinKey(t *testing.T) {
	picked := &SelectedDevice{
		Agent:  &grpcclient.AgentConnection{Host: "10.0.0.9"},
		PinKey: "wendyos-agx-orin.local",
	}

	got, err := defaultDeviceNameFor(picked)
	if err != nil {
		t.Fatalf("defaultDeviceNameFor: %v", err)
	}
	if got != "wendyos-agx-orin.local" {
		t.Fatalf("default device name = %q, want the pin key %q — an IP is a DHCP lease, not a device", got, picked.PinKey)
	}

	// The consequence, stated directly: the pin the picker just wrote has to be
	// visible to every later connect keyed on the saved default.
	cfg := &config.Config{}
	cfg.SetDevicePin(picked.PinKey, 7, "grpc.a.sh:443", "42", "")
	if _, ok := cfg.DevicePinFor(pinKeyForAddr(got)); !ok {
		t.Fatalf("the pin recorded under %q is invisible to a default of %q: every later connect keys on a name no pin was filed under, and enforcement is off",
			picked.PinKey, got)
	}

	// A selection with no pin key has nothing else to identify it; the address
	// stays the fallback, exactly as before.
	noKey := &SelectedDevice{Agent: &grpcclient.AgentConnection{Host: "10.0.0.9"}}
	if got, err := defaultDeviceNameFor(noKey); err != nil || got != "10.0.0.9" {
		t.Fatalf("keyless selection = (%q, %v), want the dialled host as the fallback", got, err)
	}

	ext := &SelectedDevice{External: &models.ExternalDevice{ProviderKey: "adb"}}
	if got, err := defaultDeviceNameFor(ext); err != nil || got != "adb" {
		t.Fatalf("external selection = (%q, %v), want its provider key", got, err)
	}
	if _, err := defaultDeviceNameFor(&SelectedDevice{}); err == nil {
		t.Error("an empty selection must not name a default device")
	}
}

// TestPickerRunsUpdateCheckOnceThePinHolds is the other half of the ordering
// assertion above: the pin gate must not have simply disabled the update check.
// A device whose pin matches still gets the check, and the connection the check
// hands back (a restarted agent yields a NEW connection) is the one the caller
// receives.
func TestPickerRunsUpdateCheckOnceThePinHolds(t *testing.T) {
	stubNonInteractive(t)
	writePinTestConfig(t, map[string]config.DevicePin{
		"wendyos-agx-orin": {OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"},
	})

	lan := &models.LANDevice{
		DisplayName: "Agx Orin",
		Hostname:    "wendyos-agx-orin.local",
		IPAddress:   "10.0.0.9",
		Port:        50051,
		IsMTLS:      true,
	}

	origLadder := dialAgentLadderFn
	dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
		return &grpcclient.AgentConnection{
			IsMTLS:   true,
			CertInfo: &config.CertificateInfo{OrganizationID: 7},
		}, nil, nil
	}
	replacement := &grpcclient.AgentConnection{IsMTLS: true, CertInfo: &config.CertificateInfo{OrganizationID: 7}}
	updateChecked := false
	origUpdate := checkAndOfferUpdateFn
	checkAndOfferUpdateFn = func(context.Context, *grpcclient.AgentConnection) (*grpcclient.AgentConnection, error) {
		updateChecked = true
		return replacement, nil
	}
	t.Cleanup(func() { dialAgentLadderFn, checkAndOfferUpdateFn = origLadder, origUpdate })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	picked := &models.DiscoveredDevice{DisplayName: lan.DisplayName, LAN: lan}
	sel, err := connectPickedLANDevice(ctx, picked, preferredLANAddress(*lan), false)
	if err != nil {
		t.Fatalf("matching pin: want the selection through, got %v", err)
	}
	if !updateChecked {
		t.Error("the update check never ran for a device whose pin matches: the pin gate must order the check, not remove it")
	}
	if sel == nil || sel.Agent != replacement {
		t.Errorf("selection carries %p, want the connection the update check returned (%p) — an agent that restarts hands back a new connection", sel, replacement)
	}
	if sel != nil && sel.PinKey != "wendyos-agx-orin.local" {
		t.Errorf("PinKey = %q, want the hostname the pin is filed under", sel.PinKey)
	}
}

// TestEnforceDeviceIdentityRejectsDifferentOrg is the original WDY-1149
// behaviour, kept intact by the asset work.
func TestEnforceDeviceIdentityRejectsDifferentOrg(t *testing.T) {
	stubNonInteractive(t)
	writePinTestConfig(t, map[string]config.DevicePin{
		"wendy-thor": {OrgID: 9, CloudGRPC: "grpc.b.sh:443", AssetID: "42"},
	})

	if err := enforceDeviceIdentity("wendy-thor.local", observedDeviceIdentity{mTLS: true, orgID: 7, assetID: "42"}); err == nil {
		t.Fatal("different org at pinned hostname: want an error, got nil")
	}
}
