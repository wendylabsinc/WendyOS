package commands

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
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

func TestResolveDeviceAddressUsesWENDY_DEVICE(t *testing.T) {
	restoreDeviceGlobals(t)
	deviceFlag, deviceFlagFromEnv = "", ""
	setTempConfig(t, &config.Config{})
	t.Setenv(deviceEnvVar, "env-device.local")
	applyDeviceEnv()

	addr, pinKey, err := resolveDeviceAddress()
	if err != nil {
		t.Fatal(err)
	}
	if addr != "env-device.local:50051" || pinKey != "env-device.local" {
		t.Fatalf("got (%q, %q), want the WENDY_DEVICE target", addr, pinKey)
	}
}

func TestNoDeviceErrorMentionsWENDY_DEVICE(t *testing.T) {
	restoreDeviceGlobals(t)
	deviceFlag, deviceFlagFromEnv = "", ""
	t.Setenv(deviceEnvVar, "")
	setTempConfig(t, &config.Config{})
	_, _, err := resolveDeviceAddress()
	if !errors.Is(err, errNoDevice) {
		t.Fatalf("err = %v, want errNoDevice", err)
	}
	for _, want := range []string{
		// The exact prefix two enabled Swift E2E tests match on (see
		// noDeviceMessage's doc comment) — must never be shortened or reworded.
		"no device specified; use --device flag",
		"WENDY_DEVICE",
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
	t.Setenv(deviceEnvVar, "")
	if got := mcpStartupDevice(""); got != "" {
		t.Errorf("no flag, no env: got %q, want none", got)
	}
	t.Setenv(deviceEnvVar, "env.local")
	if got := mcpStartupDevice(""); got != "env.local" {
		t.Errorf("env set: got %q, want env.local", got)
	}
	if got := mcpStartupDevice("flag.local"); got != "flag.local" {
		t.Errorf("-d given: got %q, want flag.local", got)
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
	if _, _, err := resolveDeviceAddress(); !errors.Is(err, errInvalidDeviceName) {
		t.Fatalf("err = %v, want errInvalidDeviceName", err)
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
