package commands

import (
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// allowedPlaceholders are the words flag usages deliberately put in
// backticks so --help shows them as the flag's value ("--device host").
// Add a word here only when it names the value a flag takes.
var allowedPlaceholders = map[string]bool{"host": true, "id": true}

// pflag shows the first `backticked` phrase of a flag's usage as its
// placeholder in --help. Quoting a command in backticks therefore renders as
// "--device wendy run   Target device hostname; wendy run accepts ...", and
// even a single quoted word (`wendy`, `--json`) replaces the value type.
func TestFlagUsagePlaceholdersAreSingleWords(t *testing.T) {
	seen := map[*pflag.Flag]bool{}
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		for _, set := range []*pflag.FlagSet{cmd.LocalFlags(), cmd.PersistentFlags()} {
			set.VisitAll(func(f *pflag.Flag) {
				if seen[f] {
					return
				}
				seen[f] = true
				if problem := placeholderProblem(f); problem != "" {
					t.Errorf("%s --%s: %s", cmd.CommandPath(), f.Name, problem)
				}
			})
		}
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(NewRootCmd())
}

// placeholderProblem explains what is wrong with the placeholder --help
// shows for f, or returns "".
func placeholderProblem(f *pflag.Flag) string {
	name, _ := pflag.UnquoteUsage(f)
	switch {
	case strings.ContainsAny(name, " \t"):
		return fmt.Sprintf("help shows placeholder %q; quote commands in flag usage with '...', not backticks", name)
	case strings.Contains(f.Usage, "`") && !allowedPlaceholders[name]:
		return fmt.Sprintf("help shows placeholder %q from backticks in its usage; quote with '...', or add the word to allowedPlaceholders if it names the flag's value", name)
	}
	return ""
}

func TestPlaceholderProblem(t *testing.T) {
	for _, tc := range []struct {
		usage string
		bad   bool
	}{
		{"Target device hostname", false},
		{"Target device `host`", false},
		{"Target device; `wendy run` accepts a list", true},
		{"Output format; `wendy` reads it back", true},
		{"Same as `--json`", true},
	} {
		fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
		fs.String("x", "", tc.usage)
		if got := placeholderProblem(fs.Lookup("x")) != ""; got != tc.bad {
			t.Errorf("placeholderProblem(%q) flagged = %v, want %v", tc.usage, got, tc.bad)
		}
	}
}

func TestDeviceFlagHelpShowsHostPlaceholder(t *testing.T) {
	name, usage := pflag.UnquoteUsage(NewRootCmd().PersistentFlags().Lookup("device"))
	if name != "host" {
		t.Errorf("--device placeholder = %q, want %q", name, "host")
	}
	if !strings.Contains(usage, "'wendy run' accepts a comma-separated list") {
		t.Errorf("--device usage lost the multi-device note: %q", usage)
	}
}
