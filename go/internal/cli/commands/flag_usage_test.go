package commands

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// pflag shows the first `backticked` phrase of a flag's usage as its
// placeholder in --help. Quoting a command in backticks therefore renders as
// "--device wendy run   Target device hostname; wendy run accepts ...".
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
				if name, _ := pflag.UnquoteUsage(f); strings.ContainsAny(name, " \t") {
					t.Errorf("%s --%s: help shows placeholder %q; quote commands in flag usage with '...', not backticks",
						cmd.CommandPath(), f.Name, name)
				}
			})
		}
		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(NewRootCmd())
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
