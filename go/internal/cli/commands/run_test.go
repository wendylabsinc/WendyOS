package commands

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
)

func TestWendyPlatform(t *testing.T) {
	cases := []struct {
		deviceType string
		want       string
	}{
		{"jetson-agx-orin", "nvidia-jetson"},
		{"jetson-orin-nano", "nvidia-jetson"},
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
			got := expandHookEnv(tc.input, tc.hostname, tc.appID)
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

	cmd := startPostStartHook(context.Background(), cfg, "device.local")
	if cmd != nil {
		t.Errorf("startPostStartHook() returned non-nil cmd for openURL-only hook")
	}
	if got != "http://device.local:3001/com.example.app" {
		t.Errorf("openURL = %q, want expanded URL", got)
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

	startPostStartHook(context.Background(), cfg, "device.local")
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
	cmd := startPostStartHook(context.Background(), cfg, "h")
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

	startPostStartHook(context.Background(), cfg, "h")
	if called {
		t.Errorf("browserOpen was called for cli-only hook")
	}
}

func TestStartPostStartHook_NoHookReturnsNil(t *testing.T) {
	cfg := &appconfig.AppConfig{AppID: "com.example.app"}
	if cmd := startPostStartHook(context.Background(), cfg, "h"); cmd != nil {
		t.Errorf("startPostStartHook() = %v, want nil for missing hooks", cmd)
	}

	cfg.Hooks = &appconfig.HooksConfig{}
	if cmd := startPostStartHook(context.Background(), cfg, "h"); cmd != nil {
		t.Errorf("startPostStartHook() = %v, want nil for empty Hooks", cmd)
	}

	cfg.Hooks.PostStart = &appconfig.HookCommand{}
	if cmd := startPostStartHook(context.Background(), cfg, "h"); cmd != nil {
		t.Errorf("startPostStartHook() = %v, want nil for empty PostStart", cmd)
	}
}

func TestResolveRunTarget_DirectSucceeds_NoCloudFallback(t *testing.T) {
	// When resolveTarget succeeds, resolveRunTarget returns same result.
	// We can test this by checking that when deviceFlag is empty and no
	// default device, we get the "no device" error (picker falls through).
	// Since we can't test interactive picker in unit tests, test the
	// non-interactive path.
	ctx := context.Background()
	// Non-interactive: resolveTarget will error "no device specified"
	_, err := resolveRunTarget(ctx, NonInteractive())
	if err == nil {
		t.Fatal("expected error with no device in non-interactive mode")
	}
	// Should be the direct error, not a cloud error (no auth configured)
	if !strings.Contains(err.Error(), "no device") {
		t.Errorf("unexpected error: %v", err)
	}
}
