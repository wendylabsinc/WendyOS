package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// executeRootForError runs the real command tree and returns its error. Every
// case below fails before PersistentPreRunE, so nothing touches config.
func executeRootForError(t *testing.T, args ...string) error {
	t.Helper()
	root := NewRootCmd()
	root.SetArgs(args)
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	_, err := root.ExecuteC()
	if err == nil {
		t.Fatalf("wendy %q succeeded, want a usage error", args)
	}
	return err
}

func TestIsUsageErrorRecognisesEveryCobraSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"unknown flag", []string{"device", "info", "--bogus"}},
		{"flag missing its value", []string{"device", "logs", "--app"}},
		{"stray positional (NoArgs)", []string{"device", "info", "extra"}},
		{"too many positionals", []string{"device", "apps", "start", "a", "b"}},
		{"unknown root command", []string{"banana"}},
		{"unknown root command with a suggestion", []string{"devic"}},
		{"missing required flag", []string{"__session-broker"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := executeRootForError(t, tc.args...); !IsUsageError(err) {
				t.Errorf("IsUsageError(%q) = false, want true", err)
			}
		})
	}

	// No command uses flag groups yet; pin cobra's wording for when one does.
	for _, tc := range []struct {
		mark func(*cobra.Command)
		args []string
		want string
	}{
		{func(c *cobra.Command) { c.MarkFlagsRequiredTogether("a", "b") }, []string{"--a=1"}, "if any flags in the group "},
		// A non-nil empty slice: nil would make cobra parse the test binary's own flags.
		{func(c *cobra.Command) { c.MarkFlagsOneRequired("a", "b") }, []string{}, "at least one of the flags in the group "},
		{func(c *cobra.Command) { c.MarkFlagsMutuallyExclusive("a", "b") }, []string{"--a=1", "--b=2"}, "if any flags in the group "},
	} {
		cmd := &cobra.Command{Use: "x", RunE: func(*cobra.Command, []string) error { return nil }}
		cmd.Flags().String("a", "", "")
		cmd.Flags().String("b", "", "")
		tc.mark(cmd)
		cmd.SetArgs(tc.args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		err := cmd.Execute()
		if err == nil || !strings.HasPrefix(err.Error(), tc.want) || !IsUsageError(err) {
			t.Errorf("flag-group error = %v, want a usage error starting %q", err, tc.want)
		}
	}
}

func TestUnknownSubcommandErrorsAreUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"device", "banana"},
		{"--device foo", "device", "info"},
	} {
		err := UnknownSubcommandError(args)
		var usage *usageError
		if !errors.As(err, &usage) {
			t.Errorf("UnknownSubcommandError(%q) = %v, want a usage error", args, err)
		}
	}
}

func TestIsUsageErrorRejectsCommandFailures(t *testing.T) {
	for _, err := range []error{
		nil,
		errors.New("build failed"),
		fmt.Errorf("probing device: %w", errors.New("unknown command from agent")),
		// Messages a device or agent sends back are returned unwrapped (see
		// ros2RPCError); only cobra's exact shapes count as usage errors.
		errors.New("unknown command from agent"),
		errors.New(`unknown command "echo" for "ros2"`),
		errors.New(`unknown command "x" for "wendy" was rejected by the agent`),
		errors.New("required flag(s) missing in the app manifest"),
		errors.New("if any flags in the group are set, the agent restarts"),
	} {
		if IsUsageError(err) {
			t.Errorf("IsUsageError(%v) = true, want false", err)
		}
	}
	if err := usageErrorf("pass --%s", "force"); !IsUsageError(err) || err.Error() != "pass --force" {
		t.Errorf("usageErrorf = %v, want a usage error reading %q", err, "pass --force")
	}
	if markUsage(nil) != nil {
		t.Error("markUsage(nil) must stay nil")
	}
}

// Flag checks a command makes itself in RunE are usage errors too.
func TestRunEFlagChecksAreUsageErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, tc := range []struct {
		cmd  *cobra.Command
		args []string
	}{
		{newOSInstallCmd(), []string{"--nightly", "--version", "1.0.0"}},
		{newOSInstallCmd(), []string{"--rootfs-only", "image.img", "disk4"}},
		{newOSInstallCmd(), []string{"--pr", "12", "--nightly"}},
		{newOSInstallCmd(), []string{"--device-type", "raspberry-pi-5", "image.img", "disk4"}},
		{newBuildCmd(), []string{"--builder", "nonsense"}},
		{newBuildCmd(), []string{"--max-concurrency", "-1"}},
		{newBuildCmd(), []string{"--dockerfile", "Dockerfile", "--build-type", "swift"}},
	} {
		tc.cmd.SetArgs(tc.args)
		tc.cmd.SetOut(io.Discard)
		tc.cmd.SetErr(io.Discard)
		if err := tc.cmd.Execute(); !IsUsageError(err) {
			t.Errorf("%s %q: err = %v, want a usage error", tc.cmd.Name(), tc.args, err)
		}
	}
}

// wendy run's invalid flag values stay config_invalid for ErrorClass (see
// TestRunValidationErrorClasses) but are marked as usage errors, which the
// CLI's own classification ranks first.
func TestRunFlagValueErrorsAreUsageErrors(t *testing.T) {
	for _, opts := range []runOptions{
		{chunking: "invalid"},
		{builder: "invalid"},
		{maxConcurrency: -1},
		{buildType: "invalid"},
	} {
		dir := t.TempDir()
		for name, data := range map[string]string{"Dockerfile": "FROM scratch\n", "wendy.json": `{"appId":"sh.wendy.usage"}`} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		opts.prefix, opts.yes = dir, true
		if err := runCommand(context.Background(), opts); !IsUsageError(err) {
			t.Errorf("run %+v: err = %v, want a usage error", opts, err)
		}
	}
}
