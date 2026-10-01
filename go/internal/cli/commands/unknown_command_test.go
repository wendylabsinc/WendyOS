package commands

import (
	"strings"
	"testing"
)

func TestUnknownSubcommandErrorRejects(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string // substring the error must contain
	}{
		{
			name: "unknown group under device",
			args: []string{"device", "cloud"},
			want: `unknown command "cloud" for "wendy device"`,
		},
		{
			// The spelling that reads as a valid command: cobra honours the help
			// flag before it validates arguments, so this must be caught ahead of
			// cobra or it prints the `wendy device` help and exits 0.
			name: "unknown group with the help flag",
			args: []string{"device", "cloud", "--help"},
			want: `unknown command "cloud" for "wendy device"`,
		},
		{
			name: "unknown subcommand two levels down",
			args: []string{"device", "apps", "banana"},
			want: `unknown command "banana" for "wendy device apps"`,
		},
		{
			// A flag value must never be mistaken for the mistyped subcommand.
			name: "flag value before the unknown subcommand",
			args: []string{"device", "--device", "somehost", "banana"},
			want: `unknown command "banana" for "wendy device"`,
		},
		{
			name: "unknown subcommand under a cloud-scoped group",
			args: []string{"cloud", "device", "banana"},
			want: `unknown command "banana" for "wendy cloud device"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := UnknownSubcommandError(tt.args)
			if err == nil {
				t.Fatalf("UnknownSubcommandError(%q) = nil, want an error", tt.args)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("UnknownSubcommandError(%q) = %q, want it to contain %q", tt.args, err, tt.want)
			}
		})
	}
}

func TestUnknownSubcommandErrorAllows(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"no arguments", []string{}},
		{"group on its own", []string{"device"}},
		{"group with the help flag", []string{"device", "--help"}},
		{"group with a persistent flag", []string{"device", "--device", "somehost"}},
		{"nested group on its own", []string{"device", "apps"}},
		{"leaf command", []string{"device", "info"}},
		{"leaf command with the help flag", []string{"device", "apps", "start", "--help"}},
		{"leaf command with a positional", []string{"device", "apps", "start", "myapp"}},
		{"real cloud-scoped leaf", []string{"cloud", "device", "unenroll"}},
		// Root-level resolution stays cobra's job: its legacyArgs check already
		// reports these, and bailing out here is what keeps the hidden
		// __complete and help commands working, since cobra registers those
		// during Execute rather than at construction.
		{"unknown command at the root", []string{"banana"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := UnknownSubcommandError(tt.args); err != nil {
				t.Errorf("UnknownSubcommandError(%q) = %q, want nil", tt.args, err)
			}
		})
	}
}

func TestUnknownSubcommandErrorSuggests(t *testing.T) {
	err := UnknownSubcommandError([]string{"device", "unenrol"})
	if err == nil {
		t.Fatal("UnknownSubcommandError = nil, want an error")
	}
	if !strings.Contains(err.Error(), "unenroll") {
		t.Errorf("UnknownSubcommandError = %q, want it to suggest %q", err, "unenroll")
	}
}

func TestBundledDeviceArgumentExplainsShellMistake(t *testing.T) {
	for _, args := range [][]string{
		{"--device vm:robot", "device", "ros2", "topics"},
		{"--json", "--device\tvm:robot", "device", "ros2", "echo", "/odom"},
	} {
		err := UnknownSubcommandError(args)
		if err == nil || !strings.Contains(err.Error(), "one argument") ||
			!strings.Contains(err.Error(), `--device "$DEVICE"`) || !strings.Contains(err.Error(), "retrying") {
			t.Fatalf("missing actionable shell diagnostic for %q: %v", args, err)
		}
	}
}

func TestBundledDeviceArgumentPreservesValuesAndForwardedArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--device", "robot with spaces", "device", "info"},
		{"--device=robot with spaces", "device", "info"},
		{"--device", "--device literal", "device", "info"},
		{"device", "attach", "app", "--", "program", "--device literal"},
		{"chat", "--prompt", "--device literal"},
		{"--", "--device literal"},
	} {
		if err := bundledDeviceArgumentError(args); err != nil {
			t.Fatalf("valid value or forwarded argument rejected: %q: %v", args, err)
		}
	}
}
