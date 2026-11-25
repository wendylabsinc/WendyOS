package main

import (
	"reflect"
	"testing"

	"github.com/spf13/cobra"
)

func TestSnapshotCapturesPublicCommandsAndFlags(t *testing.T) {
	root := &cobra.Command{Use: "wendy", Short: "root help"}
	root.PersistentFlags().StringP("device", "d", "", "target device")

	run := &cobra.Command{
		Use:       "run [path]",
		Aliases:   []string{"deploy", "start"},
		ValidArgs: []string{"."},
	}
	run.Flags().Bool("detach", false, "do not stream logs")
	run.Flags().Bool("internal", false, "internal flag")
	if err := run.Flags().MarkHidden("internal"); err != nil {
		t.Fatal(err)
	}
	root.AddCommand(run)
	root.AddCommand(&cobra.Command{Use: "secret", Hidden: true})

	got := snapshot(root)
	if len(got.Commands) != 2 {
		t.Fatalf("snapshot contains %d commands, want 2: %#v", len(got.Commands), got)
	}
	if got.Commands[0].Path != "wendy" || got.Commands[1].Path != "wendy run" {
		t.Fatalf("unexpected command paths: %q, %q", got.Commands[0].Path, got.Commands[1].Path)
	}
	if !reflect.DeepEqual(got.Commands[1].Aliases, []string{"deploy", "start"}) {
		t.Fatalf("aliases = %#v", got.Commands[1].Aliases)
	}
	if len(got.Commands[0].PersistentFlags) != 1 || got.Commands[0].PersistentFlags[0].Name != "device" {
		t.Fatalf("persistent flags = %#v", got.Commands[0].PersistentFlags)
	}
	if len(got.Commands[1].LocalFlags) != 1 || got.Commands[1].LocalFlags[0].Name != "detach" {
		t.Fatalf("local flags = %#v", got.Commands[1].LocalFlags)
	}
}

func TestSortedCopyDoesNotMutateCommandMetadata(t *testing.T) {
	aliases := []string{"z", "a"}
	got := sortedCopy(aliases)

	if !reflect.DeepEqual(got, []string{"a", "z"}) {
		t.Fatalf("sortedCopy() = %#v", got)
	}
	if !reflect.DeepEqual(aliases, []string{"z", "a"}) {
		t.Fatalf("sortedCopy mutated input: %#v", aliases)
	}
}

func TestCompareRisk(t *testing.T) {
	base := surface{Commands: []commandSurface{
		{
			Path:  "wendy inspect",
			Use:   "inspect [path]",
			Short: "Inspect an app",
			LocalFlags: []flagSurface{
				{Name: "detach", Default: "false", Type: "bool", Usage: "Do not stream logs"},
			},
		},
	}}

	tests := []struct {
		name string
		head surface
		want string
	}{
		{name: "unchanged", head: base, want: "low"},
		{
			name: "help only",
			head: surface{Commands: []commandSurface{
				{Path: "wendy inspect", Use: "inspect [path]", Short: "Inspect an application", LocalFlags: base.Commands[0].LocalFlags},
			}},
			want: "low",
		},
		{
			name: "additive flag",
			head: surface{Commands: []commandSurface{
				{
					Path: "wendy inspect", Use: "inspect [path]", Short: "Inspect an app",
					LocalFlags: append(base.Commands[0].LocalFlags, flagSurface{Name: "verbose", Default: "false", Type: "bool"}),
				},
			}},
			want: "mid",
		},
		{
			name: "changed default",
			head: surface{Commands: []commandSurface{
				{
					Path: "wendy inspect", Use: "inspect [path]", Short: "Inspect an app",
					LocalFlags: []flagSurface{{Name: "detach", Default: "true", Type: "bool", Usage: "Do not stream logs"}},
				},
			}},
			want: "high",
		},
		{name: "removed command", head: surface{}, want: "high"},
		{
			name: "critical flow help",
			head: surface{Commands: []commandSurface{
				{Path: "wendy install", Use: "install", Short: "Install WendyOS"},
			}},
			want: "high",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			testBase := base
			if test.name == "critical flow help" {
				testBase = surface{Commands: []commandSurface{
					{Path: "wendy install", Use: "install", Short: "Install the OS"},
				}}
			}
			if got := compareRisk(testBase, test.head); got != test.want {
				t.Fatalf("compareRisk() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDocsSnapshotIncludesVMButNotInternalCommands(t *testing.T) {
	root := &cobra.Command{Use: "wendy"}
	vm := &cobra.Command{Use: "vm", Hidden: true}
	vm.AddCommand(&cobra.Command{Use: "start <name>"})
	vm.AddCommand(&cobra.Command{Use: "internal", Hidden: true})
	root.AddCommand(vm, &cobra.Command{Use: "secret", Hidden: true})
	public := snapshot(root)
	docs := snapshotForDocs(root, true)
	if len(public.Commands) != 1 {
		t.Fatalf("default snapshot exposed hidden commands: %#v", public)
	}
	var paths []string
	for _, command := range docs.Commands {
		paths = append(paths, command.Path)
	}
	if !reflect.DeepEqual(paths, []string{"wendy", "wendy vm", "wendy vm start"}) {
		t.Fatalf("unexpected documentation commands: %v", paths)
	}
}
