package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// restoreDeviceGlobals snapshots the package globals device resolution reads,
// so a test can set them freely.
func restoreDeviceGlobals(t *testing.T) {
	t.Helper()
	flag, env, js, noticed := deviceFlag, deviceFlagFromEnv, jsonOutput, noticedImplicitDevice
	t.Cleanup(func() { deviceFlag, deviceFlagFromEnv, jsonOutput, noticedImplicitDevice = flag, env, js, noticed })
}

func TestApplyDeviceEnvPrecedence(t *testing.T) {
	restoreDeviceGlobals(t)
	for _, tc := range []struct {
		name, flag, env, want string
		fromEnv               bool
	}{
		{"flag wins", "flag.local", "env.local", "flag.local", false},
		{"env when no flag", "", "env.local", "env.local", true},
		{"env is trimmed", "", "  env.local \n", "env.local", true},
		{"blank env is unset", "", "   ", "", false},
		{"neither", "", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			deviceFlag, deviceFlagFromEnv = tc.flag, "stale"
			t.Setenv(deviceEnvVar, tc.env)
			applyDeviceEnv()
			if deviceFlag != tc.want {
				t.Errorf("deviceFlag = %q, want %q", deviceFlag, tc.want)
			}
			if got := deviceChosenByEnv(); got != tc.fromEnv {
				t.Errorf("deviceChosenByEnv() = %v, want %v", got, tc.fromEnv)
			}
		})
	}
}

// ros2 exec's trailing --device, HIL's vm: selection and run's fleet split
// rewrite deviceFlag after startup; the target is then not WENDY_DEVICE's.
func TestDeviceChosenByEnvStopsWhenFlagIsRewritten(t *testing.T) {
	restoreDeviceGlobals(t)
	deviceFlag = ""
	t.Setenv(deviceEnvVar, "thor.local")
	applyDeviceEnv()
	deviceFlag = "vm:hil"
	if deviceChosenByEnv() {
		t.Fatal("a rewritten deviceFlag is still reported as chosen by WENDY_DEVICE")
	}
}

func TestResolveDeviceAddressPrefersWENDY_DEVICEOverTheDefault(t *testing.T) {
	restoreDeviceGlobals(t)
	deviceFlag, deviceFlagFromEnv = "", ""
	setTempConfig(t, &config.Config{DefaultDevice: "saved.local"})
	t.Setenv(deviceEnvVar, "env-device.local")
	applyDeviceEnv()

	addr, pinKey, isDefault, err := resolveDeviceAddress()
	if err != nil {
		t.Fatal(err)
	}
	if addr != "env-device.local:50051" || pinKey != "env-device.local" || isDefault {
		t.Fatalf("got (%q, %q, isDefault=%v), want the WENDY_DEVICE target and isDefault=false", addr, pinKey, isDefault)
	}
}

func TestNoDeviceErrorMentionsWENDY_DEVICE(t *testing.T) {
	restoreDeviceGlobals(t)
	deviceFlag, deviceFlagFromEnv = "", ""
	t.Setenv(deviceEnvVar, "")
	setTempConfig(t, &config.Config{})
	_, _, _, err := resolveDeviceAddress()
	if !errors.Is(err, errNoDevice) {
		t.Fatalf("err = %v, want errNoDevice", err)
	}
	for _, want := range []string{
		// The exact prefix two enabled Swift E2E tests match on (see
		// noDeviceMessage's doc comment) — must never be shortened or reworded.
		"no device specified; use --device flag or set a default",
		"WENDY_DEVICE",
		"wendy device set-default <device>",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// The cloud mirror of `wendy device` and `cloud tunnel`/`cloud run` read
// deviceFlag through effectiveDeviceName, so WENDY_DEVICE reaches them too.
func TestCloudCommandsFollowWENDY_DEVICE(t *testing.T) {
	restoreDeviceGlobals(t)
	deviceFlag = ""
	t.Setenv(deviceEnvVar, "thor")
	applyDeviceEnv()
	if got := effectiveDeviceName(""); got != "thor" {
		t.Errorf("effectiveDeviceName(\"\") = %q, want thor", got)
	}
	if got := effectiveDeviceName("orin"); got != "orin" {
		t.Errorf("a command's own --device must still win; got %q", got)
	}
}

func TestNoteEnvDeviceOnlyAnnouncesEnvChosenTargets(t *testing.T) {
	restoreDeviceGlobals(t)
	setTempConfig(t, &config.Config{})
	jsonOutput = false
	for _, tc := range []struct {
		name, flag, fromEnv, explicit, socket string
		want                                  bool
	}{
		{"env-chosen", "thor.local", "thor.local", "", "", true},
		{"typed --device", "thor.local", "", "", "", false},
		{"rewritten after env", "vm:hil", "thor.local", "", "", false},
		{"caller named a device", "thor.local", "thor.local", "orin.local", "", false},
		{"agent socket", "thor.local", "thor.local", "", "/run/wendy/agent.sock", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("WENDY_AGENT_SOCKET", tc.socket)
			deviceFlag, deviceFlagFromEnv, noticedImplicitDevice = tc.flag, tc.fromEnv, false
			noteEnvDevice(tc.explicit)
			if noticedImplicitDevice != tc.want {
				t.Errorf("announced = %v, want %v", noticedImplicitDevice, tc.want)
			}
		})
	}
}

func TestMCPStartupDevicePrecedence(t *testing.T) {
	cfg := &config.Config{DefaultDevice: "saved.local"}
	t.Setenv(deviceEnvVar, "")
	if got := mcpStartupDevice("", cfg); got != "saved.local" {
		t.Errorf("no flag, no env: got %q, want the saved default", got)
	}
	t.Setenv(deviceEnvVar, "env.local")
	if got := mcpStartupDevice("", cfg); got != "env.local" {
		t.Errorf("env set: got %q, want env.local", got)
	}
	if got := mcpStartupDevice("flag.local", cfg); got != "flag.local" {
		t.Errorf("-d given: got %q, want flag.local", got)
	}
	if got := mcpStartupDevice("", nil); got != "env.local" {
		t.Errorf("nil config: got %q, want env.local", got)
	}
}

// set-default's confirming connect must reach the device it just saved, not
// a --device or WENDY_DEVICE override that happens to be in effect.
func TestSetDefaultConfirmsTheSavedDeviceNotTheOverride(t *testing.T) {
	restoreDeviceGlobals(t)
	stubNonInteractive(t)
	writePinTestConfig(t, nil)
	deviceFlag, deviceFlagFromEnv = "other.local", "other.local"

	var dialled []string
	origLookup, origBrowse, origLadder := osLookupHostFn, lanBrowseFn, dialAgentLadderFn
	osLookupHostFn = func(context.Context, string) ([]string, error) { return nil, errors.New("no resolver in test") }
	lanBrowseFn = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
	dialAgentLadderFn = func(_ context.Context, target dialTarget) (*grpcclient.AgentConnection, error, error) {
		dialled = append(dialled, target.PinKey)
		return nil, nil, errors.New("device offline in test")
	}
	// The failed connect reaches the provisioned-mTLS hint's LAN browse and
	// the USB-direct fallback. Keep both off the network: their real probes
	// outlive this test and land on later tests' fake dialers.
	origDiscover, origUSB := discoverLANDevices, usbDirectCandidatesFn
	discoverLANDevices = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
	usbDirectCandidatesFn = func() []discovery.USBDirectCandidate { return nil }
	t.Cleanup(func() {
		osLookupHostFn, lanBrowseFn, dialAgentLadderFn = origLookup, origBrowse, origLadder
		discoverLANDevices, usbDirectCandidatesFn = origDiscover, origUSB
	})

	cmd := newDeviceSetDefaultCmd()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd.SetContext(ctx)
	if err := cmd.RunE(cmd, []string{"wendy-thor.local:50051"}); err != nil {
		t.Fatalf("set-default: %v", err)
	}
	for _, key := range dialled {
		if key == "other.local" {
			t.Fatalf("set-default's confirming connect dialled the override other.local; dialled %v", dialled)
		}
	}
	if len(dialled) == 0 || dialled[0] != "wendy-thor.local" {
		t.Fatalf("dialled %v, want the saved default wendy-thor.local", dialled)
	}
	if deviceFlag != "other.local" || deviceFlagFromEnv != "other.local" {
		t.Fatalf("override not restored: deviceFlag=%q fromEnv=%q", deviceFlag, deviceFlagFromEnv)
	}
}

// Review Focus 4: only a host made purely of digits is refused.
func TestRejectNumericDeviceName(t *testing.T) {
	setTempConfig(t, &config.Config{})
	for _, bad := range []string{"283", "283:50051", " 283 "} {
		if err := rejectNumericDeviceName(bad); !errors.Is(err, errInvalidDeviceName) {
			t.Errorf("rejectNumericDeviceName(%q) = %v, want errInvalidDeviceName", bad, err)
		}
	}
	for _, ok := range []string{
		"", "10.0.0.5", "10.0.0.5:50051", "283.local", "vm:283", "[::1]:50051",
		"wendyos-283", "0x1f", "cloud://grpc.a.sh:443/org/7/asset/283", "docker",
	} {
		if err := rejectNumericDeviceName(ok); err != nil {
			t.Errorf("rejectNumericDeviceName(%q) = %v, want nil", ok, err)
		}
	}
}

func TestNumericDeviceErrorNamesTheExactCloudSelector(t *testing.T) {
	setTempConfig(t, &config.Config{Auth: []config.AuthConfig{
		{CloudGRPC: "grpc.a.sh:443", Certificates: []config.CertificateInfo{{OrganizationID: 7}}},
	}})
	msg := rejectNumericDeviceName("283").Error()
	for _, want := range []string{
		"283:50051",
		"wendy --device cloud://grpc.a.sh:443/org/7/asset/283 <command>",
		"wendy device set-default cloud://grpc.a.sh:443/org/7/asset/283",
		"wendy cloud device <command> --device 283",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error does not contain %q:\n%s", want, msg)
		}
	}
	// The selector it prints must be one the direct path accepts.
	if _, matched, err := parseCloudDeviceSelector("cloud://grpc.a.sh:443/org/7/asset/283"); !matched || err != nil {
		t.Fatalf("printed selector is not accepted by parseCloudDeviceSelector: matched=%v err=%v", matched, err)
	}

	setTempConfig(t, &config.Config{})
	msg = rejectNumericDeviceName("283").Error()
	for _, want := range []string{"cloud://<cloud-grpc-host:port>/org/<org-id>/asset/283", "wendy cloud login", "wendy cloud device <command> --device 283"} {
		if !strings.Contains(msg, want) {
			t.Errorf("logged-out error does not contain %q:\n%s", want, msg)
		}
	}
}

func TestResolveDeviceAddressRejectsNumericDevice(t *testing.T) {
	restoreDeviceGlobals(t)
	setTempConfig(t, &config.Config{})
	deviceFlag = "283"
	if _, _, _, err := resolveDeviceAddress(); !errors.Is(err, errInvalidDeviceName) {
		t.Fatalf("err = %v, want errInvalidDeviceName", err)
	}
	deviceFlag = ""
	setTempConfig(t, &config.Config{DefaultDevice: "283"}) // a numeric default saved by an older CLI
	if _, _, _, err := resolveDeviceAddress(); !errors.Is(err, errInvalidDeviceName) {
		t.Fatalf("numeric saved default: err = %v, want errInvalidDeviceName", err)
	}
}

// Never dial "283:50051" (the macOS resolver turns it into 0.0.1.27), and do
// not swallow the error into the interactive picker either.
func TestConnectToAgentRejectsNumericDeviceBeforeDialling(t *testing.T) {
	restoreDeviceGlobals(t)
	stubNonInteractive(t)
	setTempConfig(t, &config.Config{})
	deviceFlag, jsonOutput = "283", false
	origLadder := dialAgentLadderFn
	dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
		t.Error("dialled a numeric device name; it must be rejected before any connection attempt")
		return nil, nil, errors.New("unreachable in test")
	}
	t.Cleanup(func() { dialAgentLadderFn = origLadder })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := connectToAgent(ctx, NonInteractive()); !errors.Is(err, errInvalidDeviceName) {
		t.Fatalf("err = %v, want errInvalidDeviceName", err)
	}
}

func TestResolveTargetRejectsNumericDeviceBeforeDiscovery(t *testing.T) {
	restoreDeviceGlobals(t)
	stubNonInteractive(t)
	setTempConfig(t, &config.Config{})
	deviceFlag = "283"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := resolveTargetInner(ctx, NonInteractive()); !errors.Is(err, errInvalidDeviceName) {
		t.Fatalf("err = %v, want errInvalidDeviceName", err)
	}
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("took %s: provider discovery ran before the name was rejected", waited)
	}
}

// The cloud path treats a bare number as an asset ID; the direct-path check
// must not intercept it.
func TestNumericDeviceIsLeftToTheCloudPath(t *testing.T) {
	restoreDeviceGlobals(t)
	stubNonInteractive(t)
	setTempConfig(t, &config.Config{}) // not logged in: the cloud path fails for that reason instead
	ctx := context.WithValue(context.Background(), cloudDeviceContextKey{}, cloudDeviceConfig{DeviceName: "283"})
	if _, err := resolveTargetInner(ctx, NonInteractive()); errors.Is(err, errInvalidDeviceName) {
		t.Fatalf("the cloud path rejected asset ID 283 as a device name: %v", err)
	}
}

func TestSetDefaultRejectsNumericDevice(t *testing.T) {
	restoreDeviceGlobals(t)
	stubNonInteractive(t)
	setTempConfig(t, &config.Config{DefaultDevice: "kept.local"})
	cmd := newDeviceSetDefaultCmd()
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"283"}); !errors.Is(err, errInvalidDeviceName) {
		t.Fatalf("err = %v, want errInvalidDeviceName", err)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultDevice != "kept.local" {
		t.Fatalf("a rejected numeric default was saved: %q", cfg.DefaultDevice)
	}
}
