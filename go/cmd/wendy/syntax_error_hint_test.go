package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestSyntaxErrorHint(t *testing.T) {
	// Isolate the cache on macOS, Linux, and Windows.
	dir := t.TempDir()
	for _, key := range []string{"HOME", "XDG_CACHE_HOME", "LocalAppData"} {
		t.Setenv(key, dir)
	}
	usage := commands.UnknownSubcommandError([]string{"device", "banana"})
	if usage == nil {
		t.Fatal("expected a syntax error")
	}

	// Each row is the next invocation, sharing the on-disk streak.
	for _, step := range []struct {
		name        string
		err         error
		interactive bool
		json        bool
		hint        bool
	}{
		{"noninteractive first", usage, false, false, false},
		{"noninteractive second", usage, false, false, false},
		{"first", usage, true, false, false},
		{"noninteractive error leaves streak alone", usage, false, false, false},
		{"noninteractive success leaves streak alone", nil, false, false, false},
		{"second", usage, true, false, true},
		{"third", usage, true, false, false},
		{"success resets", nil, true, false, false},
		{"first after success", usage, true, false, false},
		{"second in JSON stays quiet", usage, true, true, false},
		{"third back in text stays quiet", usage, true, false, false},
		{"runtime error resets", errors.New("app start failed"), true, false, false},
		{"first after runtime error", usage, true, false, false},
		{"second after runtime error", usage, true, false, true},
		{"cancellation resets", context.Canceled, true, false, false},
		{"first after cancellation", usage, true, false, false},
		{"second after cancellation", usage, true, false, true},
	} {
		args := []string{"--json=false"}
		if step.json {
			args = []string{"--json"}
		}
		var out strings.Builder
		reportCommandResult(&out, step.err, nil, args, step.interactive)
		if strings.Contains(out.String(), commands.DiscordSupportPrompt) != step.hint {
			t.Fatalf("%s: want hint=%v, got %q", step.name, step.hint, out.String())
		}
		if step.hint && !strings.HasSuffix(out.String(), "\n\n"+commands.DiscordSupportPrompt+"\n") {
			t.Fatalf("%s: hint must follow a blank line: %q", step.name, out.String())
		}
		if step.json {
			decodeEnvelope(t, out.String())
		}
	}
}

func TestSyntaxErrorHintLeavesReadOnlyInvocationsUnchanged(t *testing.T) {
	for _, name := range []string{"doctor", "update"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for _, key := range []string{"HOME", "XDG_CACHE_HOME", "LocalAppData"} {
				t.Setenv(key, dir)
			}
			root := &cobra.Command{Use: "wendy"}
			cmd := &cobra.Command{Use: name}
			cmd.Flags().Bool("check", true, "")
			root.AddCommand(cmd)
			var out strings.Builder
			reportCommandResult(&out, nil, cmd, nil, true)
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				t.Fatalf("read-only invocation initialized its cache: entries=%v err=%v", entries, err)
			}
			cache, err := config.CacheDir()
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(cache, syntaxErrorCountFile)
			if err := os.WriteFile(path, []byte("1\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			reportCommandResult(&out, errors.New("offline"), cmd, nil, true)
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "1\n" {
				t.Fatalf("read-only invocation changed the existing streak: data=%q err=%v", data, err)
			}
		})
	}
}
