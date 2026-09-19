package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func TestResolveStagefileGPUTargetKeepsWatchTarget(t *testing.T) {
	dir := t.TempDir()
	const cudaStagefile = "version: 1\nstages:\n  - name: app\n    from: ubuntu:22.04\n    pin: false\n    cuda: true\n"
	if err := os.WriteFile(filepath.Join(dir, stagefileSourceName), []byte(cudaStagefile), 0o600); err != nil {
		t.Fatal(err)
	}
	watchTarget := &SelectedDevice{}

	got, err := resolveStagefileGPUTarget(context.Background(), dir, watchTarget, runOptions{})
	if err != nil {
		t.Fatalf("resolveStagefileGPUTarget: %v", err)
	}
	if got != watchTarget {
		t.Fatal("CUDA Stagefile cycle replaced the watch session's selected target")
	}
}

// containerDisplayName must print the real container identity in deploy
// output: "{appID}_{serviceName}" when the config describes one service of a
// multi-service app, not the bare appID (WDY-1828). Assertions use
// strings.Contains so terminal styling (tui.App) cannot break them.
func TestContainerDisplayName(t *testing.T) {
	t.Run("single-container app prints bare appID", func(t *testing.T) {
		cfg := &appconfig.AppConfig{AppID: "sh.wendy.examples.hellovlm"}
		got := containerDisplayName(cfg)
		if !strings.Contains(got, "sh.wendy.examples.hellovlm") {
			t.Fatalf("containerDisplayName = %q, want it to contain %q", got, "sh.wendy.examples.hellovlm")
		}
		if strings.Contains(got, "_") {
			t.Fatalf("containerDisplayName = %q, single-container app must not gain a service suffix", got)
		}
	})

	t.Run("service of multi-service app prints appID_service", func(t *testing.T) {
		cfg := &appconfig.AppConfig{AppID: "sh.wendy.examples.hellovlm", ServiceName: "llm"}
		got := containerDisplayName(cfg)
		if !strings.Contains(got, "sh.wendy.examples.hellovlm_llm") {
			t.Fatalf("containerDisplayName = %q, want the full container name %q, not the bare appID", got, "sh.wendy.examples.hellovlm_llm")
		}
	})
}

func TestWendyPlatform(t *testing.T) {
	cases := []struct {
		deviceType string
		want       string
	}{
		{"jetson-agx-orin", "nvidia-jetson"},
		{"jetson-orin-nano", "nvidia-jetson"},
		{"jetson-agx-thor", "nvidia-jetson"},
		{"raspberrypi5", "generic"},
		{"unknown-device", "generic"},
		{"", "generic"},
	}
	for _, tc := range cases {
		t.Run(tc.deviceType, func(t *testing.T) {
			if got := wendyPlatform(tc.deviceType); got != tc.want {
				t.Fatalf("wendyPlatform(%q) = %q, want %q", tc.deviceType, got, tc.want)
			}
		})
	}
}

func TestExpandHookEnv(t *testing.T) {
	t.Setenv("WENDY_TEST_VAR", "from-env")

	cases := []struct {
		name     string
		input    string
		hostname string
		appID    string
		want     string
	}{
		{"unix style hostname", "http://${WENDY_HOSTNAME}:3001", "device.local", "app", "http://device.local:3001"},
		{"unix style appid", "/var/lib/${WENDY_APP_ID}", "h", "com.example.app", "/var/lib/com.example.app"},
		{"windows style hostname", "start http://%WENDY_HOSTNAME%:3001", "device.local", "app", "start http://device.local:3001"},
		{"windows style appid", "echo %WENDY_APP_ID%", "h", "com.example.app", "echo com.example.app"},
		{"mixed", "%WENDY_HOSTNAME% ${WENDY_APP_ID}", "host", "app", "host app"},
		{"unknown unix var falls through to env", "${WENDY_TEST_VAR}", "h", "a", "from-env"},
		{"unknown windows var left for cmd.exe", "%PATH_THAT_IS_NOT_WENDY%", "h", "a", "%PATH_THAT_IS_NOT_WENDY%"},
		{"no expansion needed", "echo hello", "h", "a", "echo hello"},
		{"repeated", "%WENDY_HOSTNAME% then %WENDY_HOSTNAME%", "h", "a", "h then h"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := expandHookEnv(tc.input, tc.hostname, tc.appID, "")
			if got != tc.want {
				t.Errorf("expandHookEnv(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestShellCommandWindowsUsesS(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-specific behavior")
	}
	shell, flags := shellCommand()
	if shell != "cmd.exe" {
		t.Errorf("shellCommand() shell = %q, want cmd.exe", shell)
	}
	if len(flags) != 2 || flags[0] != "/S" || flags[1] != "/C" {
		t.Errorf("shellCommand() flags = %v, want [/S /C]", flags)
	}
}

func TestShellCommandUnix(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-specific behavior")
	}
	shell, flags := shellCommand()
	if shell != "sh" {
		t.Errorf("shellCommand() shell = %q, want sh", shell)
	}
	if len(flags) != 1 || flags[0] != "-c" {
		t.Errorf("shellCommand() flags = %v, want [-c]", flags)
	}
}

func TestStartPostStartHook_OpenURL(t *testing.T) {
	original := browserOpen
	t.Cleanup(func() { browserOpen = original })

	var got string
	browserOpen = func(url string) error {
		got = url
		return nil
	}

	cfg := &appconfig.AppConfig{
		AppID: "com.example.app",
		Hooks: &appconfig.HooksConfig{
			PostStart: &appconfig.HookCommand{
				OpenURL: "http://${WENDY_HOSTNAME}:3001/${WENDY_APP_ID}",
			},
		},
	}

	cmd := startPostStartHook(context.Background(), cfg, "device.local", "")
	if cmd != nil {
		t.Errorf("startPostStartHook() returned non-nil cmd for openURL-only hook")
	}
	if got != "http://device.local:3001/com.example.app" {
		t.Errorf("openURL = %q, want expanded URL", got)
	}
}

// TestStartPostStartHook_OpenURLIPv6HostBracketed locks the fix for the
// malformed-URL bug: an IPv6 hostname substituted into an openURL template
// must be bracketed, otherwise "http://2600:...:f7:6001" reads the port as
// one more hextet and is unparseable.
func TestStartPostStartHook_OpenURLIPv6HostBracketed(t *testing.T) {
	original := browserOpen
	t.Cleanup(func() { browserOpen = original })

	var got string
	browserOpen = func(url string) error {
		got = url
		return nil
	}

	cfg := &appconfig.AppConfig{
		AppID: "com.example.app",
		Hooks: &appconfig.HooksConfig{
			PostStart: &appconfig.HookCommand{
				OpenURL: "http://${WENDY_HOSTNAME}:6001",
			},
		},
	}

	startPostStartHook(context.Background(), cfg, "2600:1011:a003:4221:be41:6859:13c0:f7", "")
	if got != "http://[2600:1011:a003:4221:be41:6859:13c0:f7]:6001" {
		t.Errorf("openURL = %q, want bracketed IPv6 URL", got)
	}
}

func TestStartPostStartHook_OpenURLWindowsStyleVars(t *testing.T) {
	original := browserOpen
	t.Cleanup(func() { browserOpen = original })

	var got string
	browserOpen = func(url string) error {
		got = url
		return nil
	}

	cfg := &appconfig.AppConfig{
		AppID: "com.example.app",
		Hooks: &appconfig.HooksConfig{
			PostStart: &appconfig.HookCommand{
				OpenURL: "http://%WENDY_HOSTNAME%:3001",
			},
		},
	}

	startPostStartHook(context.Background(), cfg, "device.local", "")
	if got != "http://device.local:3001" {
		t.Errorf("openURL = %q, want %q", got, "http://device.local:3001")
	}
}

func TestStartPostStartHook_OpenURLErrorDoesNotPropagate(t *testing.T) {
	original := browserOpen
	t.Cleanup(func() { browserOpen = original })

	browserOpen = func(url string) error {
		return errors.New("simulated browser failure")
	}

	cfg := &appconfig.AppConfig{
		AppID: "com.example.app",
		Hooks: &appconfig.HooksConfig{
			PostStart: &appconfig.HookCommand{
				OpenURL: "http://localhost:3001",
			},
		},
	}

	// Should not panic and should not block; CLI hook is not set so returns nil.
	cmd := startPostStartHook(context.Background(), cfg, "h", "")
	if cmd != nil {
		t.Errorf("startPostStartHook() returned non-nil cmd")
	}
}

func TestStartPostStartHook_OpenURLNotCalledWhenEmpty(t *testing.T) {
	original := browserOpen
	t.Cleanup(func() { browserOpen = original })

	called := false
	browserOpen = func(url string) error {
		called = true
		return nil
	}

	cfg := &appconfig.AppConfig{
		AppID: "com.example.app",
		Hooks: &appconfig.HooksConfig{
			PostStart: &appconfig.HookCommand{
				CLI: "echo hello",
			},
		},
	}

	startPostStartHook(context.Background(), cfg, "h", "")
	if called {
		t.Errorf("browserOpen was called for cli-only hook")
	}
}

func TestStartPostStartHook_NoHookReturnsNil(t *testing.T) {
	cfg := &appconfig.AppConfig{AppID: "com.example.app"}
	if cmd := startPostStartHook(context.Background(), cfg, "h", ""); cmd != nil {
		t.Errorf("startPostStartHook() = %v, want nil for missing hooks", cmd)
	}

	cfg.Hooks = &appconfig.HooksConfig{}
	if cmd := startPostStartHook(context.Background(), cfg, "h", ""); cmd != nil {
		t.Errorf("startPostStartHook() = %v, want nil for empty Hooks", cmd)
	}

	cfg.Hooks.PostStart = &appconfig.HookCommand{}
	if cmd := startPostStartHook(context.Background(), cfg, "h", ""); cmd != nil {
		t.Errorf("startPostStartHook() = %v, want nil for empty PostStart", cmd)
	}
}

func TestResolveServiceEnv(t *testing.T) {
	t.Setenv("MESH_PEERS", "259,260,261")
	t.Setenv("MESH_SELF", "259")
	// MESH_UNSET intentionally not set.

	cfg := &appconfig.AppConfig{
		Services: map[string]*appconfig.ServiceConfig{
			"node": {Env: map[string]string{
				"MESH_PEERS":    "${MESH_PEERS}", // host-expanded
				"MESH_SELF":     "${MESH_SELF}",  // host-expanded
				"MESH_UNSET":    "${MESH_UNSET}", // empty -> dropped
				"POLL_INTERVAL": "5",             // literal
			}},
		},
	}

	got := resolveServiceEnv(cfg)
	want := []string{
		"MESH_PEERS=259,260,261",
		"MESH_SELF=259",
		"POLL_INTERVAL=5",
	}
	if len(got) != len(want) {
		t.Fatalf("resolveServiceEnv() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] { // output is sorted, so index-compare is stable
			t.Fatalf("resolveServiceEnv()[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}

	if resolveServiceEnv(nil) != nil {
		t.Fatal("resolveServiceEnv(nil) should be nil")
	}
	if resolveServiceEnv(&appconfig.AppConfig{}) != nil {
		t.Fatal("resolveServiceEnv(no services) should be nil")
	}
}

func TestExpandServiceEnv(t *testing.T) {
	t.Setenv("MESH_PEERS", "265,266,267")
	// MESH_UNSET intentionally not set.

	svc := &appconfig.ServiceConfig{Env: map[string]string{
		"MESH_PEERS": "${MESH_PEERS}",
		"MESH_UNSET": "${MESH_UNSET}",
		"LITERAL":    "5",
	}}

	got := expandServiceEnv(nil, svc)
	want := []string{"LITERAL=5", "MESH_PEERS=265,266,267"}
	if len(got) != len(want) {
		t.Fatalf("expandServiceEnv() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expandServiceEnv()[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}

	if expandServiceEnv(nil, nil) != nil {
		t.Fatal("expandServiceEnv(nil) should be nil")
	}
	if expandServiceEnv(nil, &appconfig.ServiceConfig{}) != nil {
		t.Fatal("expandServiceEnv(no env) should be nil")
	}
}

// TestExpandServiceEnv_AppLevelDefault covers WDY-2040: app-level env is the
// default for each service, and the service's own env wins key by key.
func TestExpandServiceEnv_AppLevelDefault(t *testing.T) {
	appCfg := &appconfig.AppConfig{Env: map[string]string{
		"SHARED":   "app",
		"OVERRIDE": "app",
	}}
	svc := &appconfig.ServiceConfig{Env: map[string]string{
		"OVERRIDE": "service",
		"OWN":      "service",
	}}

	got := expandServiceEnv(appCfg, svc)
	want := []string{"OVERRIDE=service", "OWN=service", "SHARED=app"}
	if len(got) != len(want) {
		t.Fatalf("expandServiceEnv() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("expandServiceEnv()[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// TestResolveServiceEnv_AppLevel covers WDY-2040: a single-container app has no
// services map, so its env comes from the app level alone.
func TestResolveServiceEnv_AppLevel(t *testing.T) {
	t.Setenv("HOST_TOKEN", "abc123")

	cfg := &appconfig.AppConfig{Env: map[string]string{
		"OTEL_LOGS_EXPORTER": "console",
		"TOKEN":              "${HOST_TOKEN}",
		"UNSET":              "${NOT_SET_IN_HOST_ENV}",
	}}

	got := resolveServiceEnv(cfg)
	want := []string{"OTEL_LOGS_EXPORTER=console", "TOKEN=abc123"}
	if len(got) != len(want) {
		t.Fatalf("resolveServiceEnv() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resolveServiceEnv()[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

// A service env entry overrides the app-level default of the same key when the
// whole app is deployed as one container request.
func TestResolveServiceEnv_ServiceOverridesAppLevel(t *testing.T) {
	cfg := &appconfig.AppConfig{
		Env: map[string]string{"SHARED": "app", "ONLY_APP": "app"},
		Services: map[string]*appconfig.ServiceConfig{
			"api": {Env: map[string]string{"SHARED": "service"}},
		},
	}

	got := resolveServiceEnv(cfg)
	want := []string{"ONLY_APP=app", "SHARED=service"}
	if len(got) != len(want) {
		t.Fatalf("resolveServiceEnv() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("resolveServiceEnv()[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestValidateEnvFlag(t *testing.T) {
	if err := validateEnvFlag([]string{"OK=1", "EMPTY=", "WITH_EQUALS=a=b"}); err != nil {
		t.Fatalf("validateEnvFlag rejected valid entries: %v", err)
	}
	for _, entry := range []string{"NO_EQUALS", "BAD-KEY=1", "1LEADING=1", "=1"} {
		if err := validateEnvFlag([]string{entry}); err == nil {
			t.Errorf("validateEnvFlag(%q) = nil, want an error", entry)
		}
	}
}

// TestDebugRequiresDebugpy asserts the --debug pre-deploy gate for Stagefile
// Python projects: the agent always wraps a Python entrypoint in debugpy
// when debug mode is requested, but nothing injects debugpy into the image
// (removed by 70493f702, by design), so a Stagefile project must declare
// debugpy in its pip requirements or the deploy should fail fast with an
// actionable message instead of crash-looping on device.
func TestDebugRequiresDebugpy(t *testing.T) {
	const stagefileYAML = "version: 1\n" +
		"stages:\n" +
		"  - name: app\n" +
		"    from: python:3.11-slim\n" +
		"    install:\n" +
		"      pip:\n" +
		"        requirements: requirements.txt\n" +
		"    entrypoint:\n" +
		"      exec: [python, main.py]\n"

	writeFile := func(t *testing.T, dir, name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	tests := []struct {
		name       string
		setup      func(t *testing.T, dir string)
		appCfg     *appconfig.AppConfig
		wantErr    bool
		errContain string
	}{
		{
			name: "stagefile python project without debugpy errors",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "build.stagefile.yaml", stagefileYAML)
				writeFile(t, dir, "requirements.txt", "flask==3.0\n")
			},
			appCfg:     &appconfig.AppConfig{Language: "python"},
			wantErr:    true,
			errContain: "debugpy",
		},
		{
			name: "stagefile python project with debugpy is fine",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "build.stagefile.yaml", stagefileYAML)
				writeFile(t, dir, "requirements.txt", "flask==3.0\ndebugpy>=1.8\n")
			},
			appCfg: &appconfig.AppConfig{Language: "python"},
		},
		{
			name: "stagefile python project with odd-cased DebugPy is fine",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "build.stagefile.yaml", stagefileYAML)
				writeFile(t, dir, "requirements.txt", "flask==3.0\nDebugPy==1.8.0\n")
			},
			appCfg: &appconfig.AppConfig{Language: "python"},
		},
		{
			name:   "non-stagefile directory is fine",
			setup:  func(t *testing.T, dir string) {},
			appCfg: &appconfig.AppConfig{Language: "python"},
		},
		{
			name: "non-python language is fine",
			setup: func(t *testing.T, dir string) {
				writeFile(t, dir, "build.stagefile.yaml", stagefileYAML)
				writeFile(t, dir, "requirements.txt", "flask==3.0\n")
			},
			appCfg: &appconfig.AppConfig{Language: "swift"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			tt.setup(t, dir)
			err := debugRequiresDebugpy(dir, tt.appCfg)
			if tt.wantErr {
				if err == nil {
					t.Fatal("debugRequiresDebugpy() = nil, want an error")
				}
				if !strings.Contains(err.Error(), tt.errContain) {
					t.Fatalf("debugRequiresDebugpy() error = %q, want it to mention %q", err.Error(), tt.errContain)
				}
				return
			}
			if err != nil {
				t.Fatalf("debugRequiresDebugpy() = %v, want nil", err)
			}
		})
	}
}

// shouldOfferLiteReinstall must fire only for the exact situation where a
// reinstall prompt is safe and actionable: the provider reported the firmware
// cannot host the app's requirements (with a known chip), the device is
// reachable over USB, and a human is there to answer (WDY-2319). Everything
// else keeps the plain build error.
func TestShouldOfferLiteReinstall(t *testing.T) {
	usb := models.ExternalDevice{DisplayName: "lite", ConnectionInfo: map[string]string{"type": "USB"}}
	lan := models.ExternalDevice{DisplayName: "lite", ConnectionInfo: map[string]string{"type": "LAN"}}
	native := &providers.AppRequirementsUnsupportedError{Device: usb, Missing: "native apps"}
	wasm := &providers.AppRequirementsUnsupportedError{Device: usb, Missing: "WASM apps"}

	cases := []struct {
		name        string
		err         error
		device      models.ExternalDevice
		interactive bool
		want        *providers.AppRequirementsUnsupportedError
	}{
		{"USB and interactive offers reinstall", native, usb, true, native},
		{"WASM apps gate the same way", wasm, usb, true, wasm},
		{"matches through error wrapping", fmt.Errorf("building: %w", native), usb, true, native},
		{"other build errors never prompt", errors.New("boom"), usb, true, nil},
		{"LAN devices cannot be reflashed here", native, lan, true, nil},
		{"non-interactive keeps the error", native, usb, false, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := shouldOfferLiteReinstall(tc.err, tc.device, tc.interactive)
			if got != tc.want || ok != (tc.want != nil) {
				t.Errorf("shouldOfferLiteReinstall() = (%v, %v), want (%v, %v)", got, ok, tc.want, tc.want != nil)
			}
		})
	}
}

// deviceNeedsInstall distinguishes a genuinely unflashed board (offer wording:
// "install") from one whose existing firmware just lacks a capability (offer
// wording: "reinstall") — see offerLiteReinstallAndRebuild.
func TestDeviceNeedsInstall(t *testing.T) {
	cases := []struct {
		name   string
		device models.ExternalDevice
		want   bool
	}{
		{"marked needsInstall", models.ExternalDevice{ConnectionInfo: map[string]string{"needsInstall": "true", "type": "USB"}}, true},
		{"capability mismatch has no marker", models.ExternalDevice{ConnectionInfo: map[string]string{"type": "USB"}}, false},
		{"nil ConnectionInfo", models.ExternalDevice{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := deviceNeedsInstall(tc.device); got != tc.want {
				t.Errorf("deviceNeedsInstall() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Exercise runCommand itself: resolveDockerfile already supported explicit
// Stagefiles, but run skipped it whenever --dockerfile was supplied. HIL
// supplies its inference build file through the same option.
func TestRunCommandCompilesExplicitStagefileBeforeLoadingConfig(t *testing.T) {
	for _, source := range []string{"build.stagefile.yaml", "inference.stagefile.yaml", "./build.stagefile.yaml", "./inference.stagefile.yaml"} {
		t.Run(source, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, source), []byte(swiftStagefileSource), 0o600); err != nil {
				t.Fatal(err)
			}
			// Stop after build-file resolution, without a device or Docker.
			if err := os.WriteFile(filepath.Join(dir, "wendy.json"), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
			err := runCommand(context.Background(), runOptions{prefix: dir, dockerfile: source, yes: true, debug: true})
			if err == nil || !strings.Contains(err.Error(), "wendy.json") {
				t.Fatalf("expected config error after compilation, got %v", err)
			}
			generated, _ := generatedBuildFileFor(filepath.Clean(source))
			b, err := os.ReadFile(filepath.Join(dir, generated))
			if err != nil {
				t.Fatalf("run did not compile explicit Stagefile: %v", err)
			}
			if !strings.Contains(string(b), "-c debug") {
				t.Fatalf("explicit Stagefile lost --debug: %s", b)
			}
		})
	}
}

func TestResolveStagefileGPUTargetIgnoresUnselectedCUDAStagefile(t *testing.T) {
	dir := t.TempDir()
	cuda := "version: 1\nstages:\n  - name: app\n    from: ubuntu:22.04\n    pin: false\n    cuda: true\n"
	if err := os.WriteFile(filepath.Join(dir, stagefileSourceName), []byte(cuda), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cpu.stagefile.yaml"), []byte(swiftStagefileSource), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"Dockerfile", "cpu.stagefile.yaml"} {
		got, err := resolveStagefileGPUTarget(context.Background(), dir, nil, runOptions{dockerfile: source})
		if err != nil || got != nil {
			t.Fatalf("%s unexpectedly resolved a GPU target: %v, %v", source, got, err)
		}
	}
}
