package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

func TestValidateChunkingMode(t *testing.T) {
	valid := []string{"", chunkingAuto, chunkingForce, chunkingOff}
	for _, m := range valid {
		if err := validateChunkingMode(m); err != nil {
			t.Errorf("validateChunkingMode(%q) = %v; want nil", m, err)
		}
	}
	for _, m := range []string{"auto ", "AUTO", "yes", "true", "none"} {
		if err := validateChunkingMode(m); err == nil {
			t.Errorf("validateChunkingMode(%q) = nil; want error", m)
		}
	}
}

func TestResolveRestartPolicy_Default(t *testing.T) {
	opts := runOptions{}
	rp := resolveRestartPolicy(opts)
	if rp == nil {
		t.Fatal("resolveRestartPolicy returned nil")
	}
	if rp.Mode != agentpb.RestartPolicyMode_DEFAULT {
		t.Errorf("Mode = %v; want DEFAULT", rp.Mode)
	}
}

func TestResolveRestartPolicy_UnlessStopped(t *testing.T) {
	opts := runOptions{restartUnlessStopped: true}
	rp := resolveRestartPolicy(opts)
	if rp.Mode != agentpb.RestartPolicyMode_UNLESS_STOPPED {
		t.Errorf("Mode = %v; want UNLESS_STOPPED", rp.Mode)
	}
}

func TestResolveRestartPolicy_OnFailure(t *testing.T) {
	opts := runOptions{restartOnFailure: true}
	rp := resolveRestartPolicy(opts)
	if rp.Mode != agentpb.RestartPolicyMode_ON_FAILURE {
		t.Errorf("Mode = %v; want ON_FAILURE", rp.Mode)
	}
}

func TestResolveRestartPolicy_NoRestart(t *testing.T) {
	opts := runOptions{noRestart: true}
	rp := resolveRestartPolicy(opts)
	if rp.Mode != agentpb.RestartPolicyMode_NO {
		t.Errorf("Mode = %v; want NO", rp.Mode)
	}
}

func TestResolveRestartPolicy_UnlessStoppedTakesPrecedence(t *testing.T) {
	// When multiple flags are set, restartUnlessStopped should win (checked first).
	opts := runOptions{
		restartUnlessStopped: true,
		restartOnFailure:     true,
		noRestart:            true,
	}
	rp := resolveRestartPolicy(opts)
	if rp.Mode != agentpb.RestartPolicyMode_UNLESS_STOPPED {
		t.Errorf("Mode = %v; want UNLESS_STOPPED (should take precedence)", rp.Mode)
	}
}

func TestNewRunCmd(t *testing.T) {
	cmd := newRunCmd()
	if cmd.Use != "run" {
		t.Errorf("Use = %q; want %q", cmd.Use, "run")
	}
	if cmd.Short == "" {
		t.Error("Short should not be empty")
	}

	// Verify expected flags exist.
	expectedFlags := []string{"build-type", "builder", "debug", "deploy", "detach", "restart-unless-stopped", "restart-on-failure", "no-restart", "prefix", "user-args", "watch", "debounce", "verbose"}
	for _, name := range expectedFlags {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("missing flag %q", name)
		}
	}
}

// TestWithWatchInvariants verifies that watch runs attached and non-interactive,
// creates session state, and preserves an explicit --detach request.
func TestWithWatchInvariants(t *testing.T) {
	got := withWatchInvariants(runOptions{})
	if got.detach {
		t.Error("watch runs attached: it streams the app's logs between redeploys")
	}
	if !got.yes {
		t.Error("yes should be forced true in watch mode")
	}
	if got.watchState == nil {
		t.Error("watch mode should initialize per-session deploy state")
	}
	if !got.isWatch() {
		t.Error("session state should mark the run as part of a watch session")
	}

	if detached := withWatchInvariants(runOptions{detach: true}); !detached.detach {
		t.Error("withWatchInvariants cleared an explicit --detach")
	}

	// Other options must be preserved.
	in := runOptions{product: "demo", prefix: "apps/demo", chunking: chunkingForce}
	out := withWatchInvariants(in)
	if out.product != "demo" || out.prefix != "apps/demo" || out.chunking != chunkingForce {
		t.Errorf("watch invariants clobbered unrelated options: %+v", out)
	}
	if again := withWatchInvariants(out); again.watchState != out.watchState {
		t.Error("watch invariants replaced existing session state")
	}
}

// TestRunCmdWatchFlagAlias verifies `wendy run --watch` parses and that the
// debounce/verbose flags carry the same defaults as the standalone `wendy watch`
// command they mirror.
func TestRunCmdWatchFlagAlias(t *testing.T) {
	cmd := newRunCmd()
	if err := cmd.Flags().Parse([]string{"--watch"}); err != nil {
		t.Fatalf("parse --watch: %v", err)
	}
	if debounce := cmd.Flags().Lookup("debounce"); debounce == nil || debounce.DefValue != "400" {
		t.Fatalf("debounce flag default = %v; want 400", debounce)
	}
}

// TestRunResolveOptions_NoProviderExclusionByDefault verifies `wendy run`
// itself no longer excludes local run targets: the interactive picker hides
// them centrally (unless WENDY_SHOW_LOCAL_DEVICES), so runResolveOptions leaves
// provider filtering to the picker.
func TestRunResolveOptions_NoProviderExclusionByDefault(t *testing.T) {
	cfg := resolveConfig{excludeProviderKeys: map[string]bool{}}
	for _, o := range runResolveOptions(runOptions{}) {
		o(&cfg)
	}
	if len(cfg.excludeProviderKeys) != 0 {
		t.Errorf("runResolveOptions should not exclude any providers; got %v", cfg.excludeProviderKeys)
	}
}

// TestRunResolveOptions_YesIsNonInteractive verifies --yes keeps suppressing the
// interactive picker (preserved while refactoring the option builder).
func TestRunResolveOptions_YesIsNonInteractive(t *testing.T) {
	cfg := resolveConfig{excludeProviderKeys: map[string]bool{}}
	for _, o := range runResolveOptions(runOptions{yes: true}) {
		o(&cfg)
	}
	if !cfg.nonInteractive {
		t.Error("--yes should set non-interactive resolve")
	}
}

func TestRunResolveOptions_WatchDisablesCrossProcessBroker(t *testing.T) {
	cfg := resolveConfig{excludeProviderKeys: map[string]bool{}}
	for _, o := range runResolveOptions(runOptions{watchState: &watchDeployState{}}) {
		o(&cfg)
	}
	if !cfg.disableSessionBroker {
		t.Error("watch should keep its direct long-lived connection instead of using the invocation broker")
	}
}

// TestRunResolveOptions_NoBluetooth verifies `wendy run` never opts into BLE
// discovery: it cannot deploy over BLE, so the picker must not scan for or list
// BLE devices the user would only be told are unusable.
func TestRunResolveOptions_NoBluetooth(t *testing.T) {
	cfg := resolveConfig{excludeProviderKeys: map[string]bool{}}
	for _, o := range runResolveOptions(runOptions{}) {
		o(&cfg)
	}
	if cfg.includeBluetooth {
		t.Error("wendy run must not include Bluetooth devices in device resolution")
	}
}

// TestIncludeBluetooth verifies BLE discovery is off unless a command opts in,
// so a new command that has no BLE code path gets the safe default.
func TestIncludeBluetooth(t *testing.T) {
	cfg := resolveConfig{excludeProviderKeys: map[string]bool{}}
	if cfg.includeBluetooth {
		t.Error("resolveConfig zero value should exclude Bluetooth")
	}
	IncludeBluetooth()(&cfg)
	if !cfg.includeBluetooth {
		t.Error("IncludeBluetooth() should enable Bluetooth discovery")
	}
}

func TestResolveRunWorkingDir_Default(t *testing.T) {
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	tempDir := t.TempDir()
	if err := os.Chdir(tempDir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(prevWD)
	})

	got, err := resolveRunWorkingDir(runOptions{})
	if err != nil {
		t.Fatalf("resolveRunWorkingDir: %v", err)
	}
	if canonicalPath(t, got) != canonicalPath(t, tempDir) {
		t.Fatalf("got %q, want %q", got, tempDir)
	}
}

func TestResolveRunWorkingDir_RelativePrefix(t *testing.T) {
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	root := t.TempDir()
	projectDir := filepath.Join(root, "apps", "demo")
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(prevWD)
	})

	got, err := resolveRunWorkingDir(runOptions{prefix: filepath.Join("apps", "demo")})
	if err != nil {
		t.Fatalf("resolveRunWorkingDir: %v", err)
	}
	if canonicalPath(t, got) != canonicalPath(t, projectDir) {
		t.Fatalf("got %q, want %q", got, projectDir)
	}
}

func TestResolveRunWorkingDir_MissingPrefix(t *testing.T) {
	_, err := resolveRunWorkingDir(runOptions{prefix: filepath.Join(t.TempDir(), "missing")})
	if err == nil {
		t.Fatal("expected error for missing directory")
	}
	if got := err.Error(); got == "" || !strings.Contains(got, "does not exist") {
		t.Fatalf("unexpected error: %q", got)
	}
}

func TestResolveRunWorkingDir_NotDirectory(t *testing.T) {
	tempDir := t.TempDir()
	filePath := filepath.Join(tempDir, "wendy.json")
	if err := os.WriteFile(filePath, []byte("{}"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err := resolveRunWorkingDir(runOptions{prefix: filePath})
	if err == nil {
		t.Fatal("expected error for file path")
	}
	if got := err.Error(); got == "" || !strings.Contains(got, "not a directory") {
		t.Fatalf("unexpected error: %q", got)
	}
}

func canonicalPath(t *testing.T, path string) string {
	t.Helper()

	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved
	}

	return filepath.Clean(path)
}

func TestNewBuildCmd(t *testing.T) {
	cmd := newBuildCmd()
	if cmd.Use != "build" {
		t.Errorf("Use = %q; want %q", cmd.Use, "build")
	}
	if cmd.Short == "" {
		t.Error("Short should not be empty")
	}
	if cmd.Flags().Lookup("build-type") == nil {
		t.Error("missing flag \"build-type\"")
	}
	if cmd.Flags().Lookup("builder") == nil {
		t.Error("missing flag \"builder\"")
	}
	if cmd.Flags().Lookup("service") == nil {
		t.Error("missing flag \"service\"")
	}
	if cmd.Flags().Lookup("max-concurrency") == nil {
		t.Error("missing flag \"max-concurrency\"")
	}
}

func TestNewDeviceCmd(t *testing.T) {
	cmd := newDeviceCmd()
	if cmd.Use != "device" {
		t.Errorf("Use = %q; want %q", cmd.Use, "device")
	}
	if cmd.Short == "" {
		t.Error("Short should not be empty")
	}

	// Verify subcommands exist.
	subCmds := cmd.Commands()
	subNames := make(map[string]bool)
	for _, c := range subCmds {
		subNames[c.Name()] = true
	}

	expectedSubs := []string{"info", "version", "set-default", "get-default", "unset-default", "setup", "update", "model"}
	for _, name := range expectedSubs {
		if !subNames[name] {
			t.Errorf("device command missing subcommand %q", name)
		}
	}
	if !subNames["info"] || !subNames["version"] {
		t.Fatalf("prerequisite device info/version subcommands missing; cannot continue assertions")
	}
	if infoCmd, _, err := cmd.Find([]string{"info"}); err != nil || infoCmd.Hidden {
		t.Errorf("device info should be visible; cmd=%v err=%v", infoCmd, err)
	}
	if versionCmd, _, err := cmd.Find([]string{"version"}); err != nil || !versionCmd.Hidden {
		t.Errorf("device version should be hidden; cmd=%v err=%v", versionCmd, err)
	}
	if setDefaultCmd, _, err := cmd.Find([]string{"set-default"}); err != nil || !setDefaultCmd.Hidden {
		t.Errorf("device set-default should be hidden; cmd=%v err=%v", setDefaultCmd, err)
	}

	buf := new(bytes.Buffer)
	cmd.SetOut(buf)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("device help: %v", err)
	}
	help := buf.String()
	if !strings.Contains(help, "info") {
		t.Fatalf("device help missing info command: %s", help)
	}
	if strings.Contains(help, "\n  version") {
		t.Fatalf("device help should not list deprecated version command: %s", help)
	}
}

func TestDeviceGetDefault_Set(t *testing.T) {
	origJSON := jsonOutput
	t.Cleanup(func() { jsonOutput = origJSON })
	jsonOutput = false
	setTempConfig(t, &config.Config{DefaultDevice: "wendy-thor.local"})

	buf := new(bytes.Buffer)
	cmd := newDeviceGetDefaultCmd()
	cmd.SetOut(buf)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("get-default: %v", err)
	}
	if !strings.Contains(buf.String(), "wendy-thor.local") {
		t.Fatalf("output %q missing default device", buf.String())
	}
}

func TestDeviceGetDefault_NotSet(t *testing.T) {
	origJSON := jsonOutput
	t.Cleanup(func() { jsonOutput = origJSON })
	jsonOutput = false
	setTempConfig(t, &config.Config{})

	buf := new(bytes.Buffer)
	cmd := newDeviceGetDefaultCmd()
	cmd.SetOut(buf)
	cmd.SetErr(new(bytes.Buffer))
	cmd.SetArgs([]string{})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("get-default: %v", err)
	}
	if !strings.Contains(strings.ToLower(buf.String()), "no default") {
		t.Fatalf("output %q should indicate no default device is set", buf.String())
	}
}

func TestDeprecatedDeviceVersionWarnsInHumanOutput(t *testing.T) {
	stderr, err := executeDeprecatedDeviceVersion(t, []string{"--json=false", "--device", "local", "device", "version"})
	if err == nil {
		t.Fatal("expected device version against local provider to fail")
	}
	if !strings.Contains(stderr, "Warning: 'wendy device version' is deprecated; use 'wendy device info' instead.") {
		t.Fatalf("expected deprecation warning on stderr, got %q", stderr)
	}
}

func TestDeprecatedDeviceVersionDoesNotWarnInJSONOutput(t *testing.T) {
	stderr, err := executeDeprecatedDeviceVersion(t, []string{"--json", "--device", "local", "device", "version"})
	if err == nil {
		t.Fatal("expected device version against local provider to fail")
	}
	if strings.Contains(stderr, "deprecated") {
		t.Fatalf("--json output should not include deprecation warning on stderr, got %q", stderr)
	}
}

func TestDeprecatedCloudDeviceVersionWarnsWithCloudReplacement(t *testing.T) {
	stderr, err := executeDeprecatedDeviceVersion(t, []string{"--json=false", "--device", "demo", "cloud", "device", "version"})
	if err == nil {
		t.Fatal("expected cloud device version without auth to fail")
	}
	want := "Warning: 'wendy cloud device version' is deprecated; use 'wendy cloud device info' instead."
	if !strings.Contains(stderr, want) {
		t.Fatalf("expected cloud-specific deprecation warning on stderr, got %q", stderr)
	}
	if strings.Contains(stderr, "use 'wendy device info'") {
		t.Fatalf("cloud warning should not point at direct device replacement, got %q", stderr)
	}
}

func TestDeprecatedCloudDeviceVersionDoesNotWarnInJSONOutput(t *testing.T) {
	stderr, err := executeDeprecatedDeviceVersion(t, []string{"--json", "--device", "demo", "cloud", "device", "version"})
	if err == nil {
		t.Fatal("expected cloud device version without auth to fail")
	}
	if strings.Contains(stderr, "deprecated") {
		t.Fatalf("--json output should not include cloud deprecation warning on stderr, got %q", stderr)
	}
}

func executeDeprecatedDeviceVersion(t *testing.T, args []string) (string, error) {
	t.Helper()
	return executeRootCommand(t, args)
}

func executeRootCommand(t *testing.T, args []string) (string, error) {
	t.Helper()

	origJSON := jsonOutput
	origDevice := deviceFlag
	t.Cleanup(func() {
		jsonOutput = origJSON
		deviceFlag = origDevice
	})

	t.Setenv("HOME", t.TempDir())
	t.Setenv("WENDY_ANALYTICS", "false")

	root := NewRootCmd()
	root.SetArgs(args)
	root.SetOut(new(bytes.Buffer))
	stderr := new(bytes.Buffer)
	root.SetErr(stderr)

	err := root.Execute()
	return stderr.String(), err
}

func TestNewCloudDeviceCmd(t *testing.T) {
	cmd := newCloudDeviceCmd()
	if cmd.Use != "device" {
		t.Errorf("Use = %q; want %q", cmd.Use, "device")
	}
	if cmd.Short == "" {
		t.Error("Short should not be empty")
	}
	if cmd.PersistentFlags().Lookup("cloud-grpc") == nil {
		t.Error("missing persistent flag \"cloud-grpc\"")
	}
	if cmd.PersistentFlags().Lookup("broker-url") == nil {
		t.Error("missing persistent flag \"broker-url\"")
	}

	subCmds := cmd.Commands()
	subNames := make(map[string]bool)
	for _, c := range subCmds {
		subNames[c.Name()] = true
	}

	expectedSubs := []string{"info", "version", "set-default", "unset-default", "setup", "update", "wifi", "apps", "ps"}
	for _, name := range expectedSubs {
		if !subNames[name] {
			t.Errorf("cloud device command missing mirrored subcommand %q", name)
		}
	}
	if versionCmd, _, err := cmd.Find([]string{"version"}); err != nil || !versionCmd.Hidden {
		t.Errorf("cloud device version should remain hidden; cmd=%v err=%v", versionCmd, err)
	}
}

func TestPsAliasIsHiddenButRunnable(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  *cobra.Command
	}{
		{name: "device", cmd: newDeviceCmd()},
		{name: "cloud device", cmd: newCloudDeviceCmd()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// ps is hidden from help but must remain resolvable so the alias
			// keeps working for anyone who already relies on it.
			psCmd, _, err := tc.cmd.Find([]string{"ps"})
			if err != nil {
				t.Fatalf("Find(ps): %v", err)
			}
			if psCmd.Name() != "ps" || !psCmd.Hidden {
				t.Fatalf("ps should be a hidden (but runnable) command; cmd=%v hidden=%v", psCmd.Name(), psCmd.Hidden)
			}
			if !strings.Contains(psCmd.Short, "alias for 'apps list'") {
				t.Fatalf("ps help should point at apps list, got %q", psCmd.Short)
			}

			buf := new(bytes.Buffer)
			tc.cmd.SetOut(buf)
			tc.cmd.SetErr(new(bytes.Buffer))
			tc.cmd.SetArgs([]string{"--help"})
			if err := tc.cmd.Execute(); err != nil {
				t.Fatalf("help: %v", err)
			}
			if strings.Contains(buf.String(), "\n  ps ") {
				t.Fatalf("parent help should not list hidden ps alias: %s", buf.String())
			}
		})
	}
}

func TestBluetoothBtAliasRemainsAccessibleWhenHidden(t *testing.T) {
	for _, tc := range []struct {
		name string
		cmd  *cobra.Command
	}{
		{name: "device", cmd: newDeviceCmd()},
		{name: "cloud device", cmd: newCloudDeviceCmd()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bluetoothCmd, _, err := tc.cmd.Find([]string{"bt"})
			if err != nil {
				t.Fatalf("Find(bt): %v", err)
			}
			if bluetoothCmd.Name() != "bluetooth" || !bluetoothCmd.Hidden {
				t.Fatalf("bt should resolve to hidden compatibility bluetooth command; cmd=%v hidden=%v", bluetoothCmd.Name(), bluetoothCmd.Hidden)
			}
			if !slices.Contains(bluetoothCmd.Aliases, "bt") {
				t.Fatalf("bluetooth aliases = %v; want bt", bluetoothCmd.Aliases)
			}
		})
	}
}

func TestCameraWatchIsHiddenAliasForView(t *testing.T) {
	cameraCmd := newCameraCmd()

	watchCmd, _, err := cameraCmd.Find([]string{"watch"})
	if err != nil {
		t.Fatalf("Find(watch): %v", err)
	}
	if watchCmd.Name() != "watch" || !watchCmd.Hidden {
		t.Fatalf("watch should be a hidden command; cmd=%v hidden=%v", watchCmd.Name(), watchCmd.Hidden)
	}

	viewCmd, _, err := cameraCmd.Find([]string{"view"})
	if err != nil {
		t.Fatalf("Find(view): %v", err)
	}
	if viewCmd.Hidden {
		t.Fatal("view should remain a visible command")
	}

	// watch must accept the same flags as view so it behaves identically.
	for _, flag := range []string{"id", "width", "height", "fps", "stdout"} {
		if watchCmd.Flags().Lookup(flag) == nil {
			t.Fatalf("watch is missing the %q flag that view exposes", flag)
		}
	}

	// watch must stay out of the parent's help output.
	buf := new(bytes.Buffer)
	cameraCmd.SetOut(buf)
	cameraCmd.SetErr(new(bytes.Buffer))
	cameraCmd.SetArgs([]string{"--help"})
	if err := cameraCmd.Execute(); err != nil {
		t.Fatalf("help: %v", err)
	}
	if strings.Contains(buf.String(), "watch") {
		t.Fatalf("camera help should not list the hidden watch alias: %s", buf.String())
	}
}

func TestNewAuthCmd(t *testing.T) {
	cmd := newAuthCmd()
	if cmd.Use != "auth" {
		t.Errorf("Use = %q; want %q", cmd.Use, "auth")
	}
	if cmd.Short == "" {
		t.Error("Short should not be empty")
	}

	subCmds := cmd.Commands()
	subNames := make(map[string]bool)
	for _, c := range subCmds {
		subNames[c.Name()] = true
	}

	expectedSubs := []string{"login", "logout", "refresh-certs"}
	for _, name := range expectedSubs {
		if !subNames[name] {
			t.Errorf("auth command missing subcommand %q", name)
		}
	}
}

func makeELFHeader(machine uint16) []byte {
	hdr := make([]byte, 20)
	hdr[0], hdr[1], hdr[2], hdr[3] = 0x7f, 'E', 'L', 'F'
	hdr[4] = 2 // 64-bit
	hdr[5] = 1 // little-endian
	hdr[18] = byte(machine)
	hdr[19] = byte(machine >> 8)
	return hdr
}

func TestCheckELFArchitecture(t *testing.T) {
	amd64ELF := makeELFHeader(62)
	arm64ELF := makeELFHeader(183)
	notELF := []byte("#!/bin/sh\necho hi\n")

	cases := []struct {
		name       string
		data       []byte
		deviceArch string
		wantErr    bool
	}{
		{"amd64 binary on amd64 device", amd64ELF, "amd64", false},
		{"arm64 binary on arm64 device", arm64ELF, "arm64", false},
		{"amd64 binary on arm64 device", amd64ELF, "arm64", true},
		{"arm64 binary on amd64 device", arm64ELF, "amd64", true},
		{"non-ELF accepted on any device", notELF, "arm64", false},
		{"too short data accepted", []byte{0x7f, 'E'}, "amd64", false},
		{"unsupported device arch rejected", amd64ELF, "riscv64", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkELFArchitecture(tc.data, tc.deviceArch)
			if tc.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}
