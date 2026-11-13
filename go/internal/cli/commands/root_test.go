package commands

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestRootCommand_HasAllSubcommands(t *testing.T) {
	root := NewRootCmd()

	expectedSubcmds := []string{
		"run",
		"build",
		"init",
		"project",
		"discover",
		"device",
		"os",
		"auth",
		"cache",
		"info",
		"analytics",
		"utils",
	}

	cmds := root.Commands()
	cmdNames := make(map[string]bool)
	for _, c := range cmds {
		cmdNames[c.Name()] = true
	}

	for _, name := range expectedSubcmds {
		if !cmdNames[name] {
			t.Errorf("missing subcommand %q", name)
		}
	}
}

func TestRootCommand_VersionFlag(t *testing.T) {
	root := NewRootCmd()
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetArgs([]string{"--version"})

	err := root.Execute()
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, "wendy version") {
		t.Errorf("expected version output, got: %q", output)
	}
}

func TestRootCommand_JSONFlag(t *testing.T) {
	root := NewRootCmd()

	// Verify the flag exists.
	f := root.PersistentFlags().Lookup("json")
	if f == nil {
		t.Fatal("expected --json persistent flag")
	}
	if f.DefValue != "false" {
		t.Errorf("--json default = %q; want false", f.DefValue)
	}
}

func TestRootCommand_Help(t *testing.T) {
	root := NewRootCmd()
	buf := new(bytes.Buffer)
	root.SetOut(buf)
	root.SetArgs([]string{"--help"})

	err := root.Execute()
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	output := buf.String()
	expectedTexts := []string{
		"Wendy",
		"edge computing",
		"Develop & Deploy",
		"Manage",
		"Cloud",
		"Settings",
		"Flags",
	}
	for _, text := range expectedTexts {
		if !strings.Contains(strings.ToLower(output), strings.ToLower(text)) {
			t.Errorf("help output missing %q", text)
		}
	}

	// Commands that were demoted to hidden must not appear in top-level help,
	// even though they remain registered and runnable.
	for _, hidden := range []string{"build", "watch", "discover", "os", "utils", "info", "mcp", "auth", "completion"} {
		if strings.Contains(output, "\n  "+hidden+" ") {
			t.Errorf("help output should not list hidden command %q:\n%s", hidden, output)
		}
	}
}

func TestRootCommand_DeviceFlag(t *testing.T) {
	root := NewRootCmd()

	f := root.PersistentFlags().Lookup("device")
	if f == nil {
		t.Fatal("expected --device persistent flag")
	}
}

func TestNextStepHint(t *testing.T) {
	cases := map[string]string{
		"wendy discover":         "Next: run `wendy init` to create an app, then `wendy run` to deploy it.",
		"wendy device info":      "Next: run `wendy run` to build and deploy an app to this device.",
		"wendy device top":       "Next: run `wendy run` to build and deploy an app to this device.",
		"wendy device apps list": "Next: run `wendy run` to build and deploy an app to this device.",
		"wendy run":              "Next: run `wendy device logs` to stream your app's logs.",
		"wendy analytics status": "",
	}
	for path, want := range cases {
		if got := nextStepHint(path); got != want {
			t.Errorf("nextStepHint(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestTourCommandIsVisible(t *testing.T) {
	root := NewRootCmd()
	for _, c := range root.Commands() {
		if c.Name() == "tour" {
			if c.Hidden {
				t.Fatal("tour command should be visible (not hidden)")
			}
			return
		}
	}
	t.Fatal("tour command not registered on root")
}

func TestRootCommand_DeviceFlagDocumentsWENDY_DEVICE(t *testing.T) {
	f := NewRootCmd().PersistentFlags().Lookup("device")
	if f == nil || !strings.Contains(f.Usage, "WENDY_DEVICE") {
		t.Fatalf("--device usage does not mention WENDY_DEVICE: %+v", f)
	}
}

// End to end through cobra: the root pre-run applies WENDY_DEVICE before any
// command runs, and an explicit --device still wins.
func TestRootAppliesWENDY_DEVICEBeforeCommandsRun(t *testing.T) {
	restoreDeviceGlobals(t)
	setTempConfig(t, &config.Config{DefaultDevice: "saved.local"})
	t.Setenv("WENDY_ANALYTICS", "false")
	t.Setenv(deviceEnvVar, "env-device.local")

	for _, tc := range []struct {
		name    string
		args    []string
		want    string
		fromEnv bool
	}{
		{"env when no flag", []string{"probe-device"}, "env-device.local", true},
		{"flag beats env", []string{"--device", "flag-device.local", "probe-device"}, "flag-device.local", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := NewRootCmd()
			var got string
			var gotEnv bool
			root.AddCommand(&cobra.Command{Use: "probe-device", RunE: func(*cobra.Command, []string) error {
				got, gotEnv = deviceFlag, deviceChosenByEnv()
				return nil
			}})
			root.SetArgs(tc.args)
			if err := root.Execute(); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got != tc.want || gotEnv != tc.fromEnv {
				t.Fatalf("deviceFlag = %q (fromEnv=%v), want %q (fromEnv=%v)", got, gotEnv, tc.want, tc.fromEnv)
			}
		})
	}
}
