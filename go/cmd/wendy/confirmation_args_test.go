package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
)

func TestLeadingConfirmationFlagReachesVMCreate(t *testing.T) {
	t.Setenv("WENDY_ANALYTICS", "false")
	t.Setenv("WENDY_NO_UPDATE_CHECK", "1")
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	for _, prefix := range [][]string{
		{"--json", "--yes"}, {"--yes", "--json"}, {"-y", "--json"},
		{"--json", "--yes=true"}, {"--json", "--yes=false"},
		{"--json", "--device", "--yes", "--yes"},
	} {
		t.Run(strings.Join(prefix, " "), func(t *testing.T) {
			root := commands.NewRootCmd()
			root.SetOut(io.Discard)
			root.SetErr(io.Discard)
			target, _, err := root.Find([]string{"vm", "create"})
			if err != nil {
				t.Fatal(err)
			}
			called := false
			// Keep the real command tree and flag parser, replacing only side effects.
			root.PersistentPreRunE = nil
			root.PersistentPostRunE = nil
			target.RunE = func(cmd *cobra.Command, args []string) error {
				called = true
				if len(args) != 1 || args[0] != "robot-lab" {
					t.Fatalf("args = %q", args)
				}
				yes, _ := cmd.Flags().GetBool("yes")
				wantYes := !strings.Contains(strings.Join(prefix, " "), "--yes=false")
				if yes != wantYes {
					t.Errorf("yes = %v, want %v", yes, wantYes)
				}
				profile, _ := cmd.Flags().GetString("profile")
				disk, _ := cmd.Flags().GetInt("disk")
				if profile != "generic" || disk != 8 {
					t.Errorf("profile=%q disk=%d", profile, disk)
				}
				if strings.Contains(strings.Join(prefix, " "), "--device --yes") {
					device, _ := cmd.Flags().GetString("device")
					if device != "--yes" {
						t.Errorf("device value rewritten: %q", device)
					}
				}
				return nil
			}
			args := append(append([]string{}, prefix...), "vm", "create", "robot-lab", "--profile", "generic", "--disk", "8")
			executed, err := executeInvocation(context.Background(), root, args)
			if err != nil {
				t.Fatalf("executeInvocation(%q): %v", args, err)
			}
			if !called || executed.CommandPath() != "wendy vm create" {
				t.Fatalf("called=%v command=%v", called, executed)
			}
		})
	}
}

func TestLeadingConfirmationFlagReachesVMList(t *testing.T) {
	t.Setenv("WENDY_ANALYTICS", "false")
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	root := commands.NewRootCmd()
	target, _, err := root.Find([]string{"vm", "list"})
	if err != nil {
		t.Fatal(err)
	}
	root.PersistentPreRunE = nil
	root.PersistentPostRunE = nil
	called := false
	target.RunE = func(cmd *cobra.Command, args []string) error {
		called = true
		yes, _ := cmd.Flags().GetBool("yes")
		if !yes || len(args) != 0 {
			t.Errorf("yes=%v args=%q", yes, args)
		}
		return nil
	}
	executed, err := executeInvocation(context.Background(), root, []string{"--yes", "vm", "list"})
	if err != nil || !called || executed.CommandPath() != "wendy vm list" {
		t.Fatalf("called=%v executed=%v err=%v", called, executed, err)
	}
}

func TestNormalizeLeadingConfirmationFlagPreservesArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--device", "--yes", "vm", "list"}, {"--device", "-y", "vm", "list"},
		{"--device=--yes", "vm", "list"}, {"--yes=false", "vm", "list"},
		{"-y=false", "vm", "list"}, {"--", "--yes", "vm", "list"},
		{"--unknown", "--yes", "vm", "list"}, {"run", "--user-args", "--yes"},
		{"device", "attach", "app", "--", "program", "--yes"},
	} {
		got := normalizeLeadingConfirmationFlag(args)
		if strings.Join(got, "\x00") != strings.Join(args, "\x00") {
			t.Errorf("%q became %q", args, got)
		}
	}
	args := []string{"--yes", "vm", "list"}
	_ = normalizeLeadingConfirmationFlag(args)
	if args[0] != "--yes" {
		t.Fatal("normalization mutated caller's arguments")
	}
}

func TestLeadingConfirmationFlagDoesNotGrantUnsupportedFlag(t *testing.T) {
	t.Setenv("WENDY_ANALYTICS", "false")
	t.Setenv("WENDY_CONFIG_DIR", t.TempDir())
	root := commands.NewRootCmd()
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	_, err := executeInvocation(context.Background(), root, []string{"--yes", "discover", "--help"})
	if err == nil || !strings.Contains(err.Error(), "unknown flag: --yes") {
		t.Fatalf("unsupported flag: %v", err)
	}
}
