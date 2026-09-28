package commands

import (
	"errors"
	"fmt"
	"io"
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
