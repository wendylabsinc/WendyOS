package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/shlex"
	"github.com/spf13/cobra"
)

// Validate the actual documented argv against Cobra without Execute: that would
// run device discovery, installation, authentication, or deployment. The public
// generated reference omits hidden compatibility commands still used by these
// guides, including discover, auth and device attach.
func parseDocumentedCommand(line string) (*cobra.Command, error) {
	argv, err := shlex.Split(line)
	if err != nil {
		return nil, err
	}
	if len(argv) == 0 || argv[0] != "wendy" {
		return nil, fmt.Errorf("not a Wendy command: %q", line)
	}
	root := NewRootCmd()
	root.InitDefaultVersionFlag()
	root.InitDefaultHelpFlag()
	command, rest, err := root.Find(argv[1:])
	if err != nil {
		return nil, err
	}
	command.InitDefaultHelpFlag()
	if err := command.ParseFlags(rest); err != nil {
		return nil, err
	}
	// A command group cannot silently accept an unknown subcommand as an
	// argument. Runnable commands may legitimately take app names, paths or
	// placeholder arguments, and attach forwards everything following --.
	if !command.Runnable() && len(command.Flags().Args()) != 0 {
		return nil, fmt.Errorf("unknown subcommand of %s: %v", command.CommandPath(), command.Flags().Args())
	}
	return command, nil
}

var inlineDocCode = regexp.MustCompile("`([^`\\n]+)`")

func documentedCommands(raw string) []string {
	raw = strings.ReplaceAll(raw, "\\\r\n", " ")
	raw = strings.ReplaceAll(raw, "\\\n", " ")
	result := []string{}
	seen := map[string]bool{}
	add := func(line string) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "wendy ") && !strings.Contains(line, "[flags]") && !seen[line] {
			seen[line] = true
			result = append(result, line)
		}
	}
	fenced, shell := false, false
	for _, line := range strings.Split(raw, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if fenced {
				fenced, shell = false, false
			} else {
				fenced = true
				language := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
				shell = language == "sh" || language == "bash" || language == "shell"
			}
			continue
		}
		if fenced {
			if shell {
				add(line)
			}
			continue
		}
		for _, match := range inlineDocCode.FindAllStringSubmatch(line, -1) {
			add(match[1])
		}
	}
	return result
}

func TestDocumentationCommandContracts(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..")
	docs := "go/internal/cli/assets/docs/"
	sources := []struct {
		path    string
		minimum int
	}{
		{"README.md", 6},
		{"Examples/NvidiaPAIR/README.md", 20},
		{docs + "guides/nvidia-pair.mdx", 10},
		{docs + "guides/wendy-net.mdx", 8},
		{docs + "clients/wendy-cli/commands/discover.md", 8},
		{docs + "development/debugging.md", 7},
		{docs + "faq.mdx", 3},
	}
	// Flag values in this package bind globals. Keep this test serial and
	// restore the existing selection/output globals afterward.
	oldJSON, oldDevice := jsonOutput, deviceFlag
	defer func() { jsonOutput, deviceFlag = oldJSON, oldDevice }()
	for _, source := range sources {
		t.Run(source.path, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, source.path))
			if err != nil {
				t.Fatal(err)
			}
			lines := documentedCommands(string(raw))
			if len(lines) < source.minimum {
				t.Fatalf("only %d documented commands extracted; want at least %d", len(lines), source.minimum)
			}
			for _, line := range lines {
				if _, err := parseDocumentedCommand(line); err != nil {
					t.Errorf("%s: %v", line, err)
				}
			}
			t.Logf("parsed %d actual documented command lines with Cobra", len(lines))
		})
	}
}

func TestDocumentationCommandParserRejectsRegressions(t *testing.T) {
	oldJSON, oldDevice := jsonOutput, deviceFlag
	defer func() { jsonOutput, deviceFlag = oldJSON, oldDevice }()
	for _, line := range []string{
		"wendy discover --type lan --timeout 5s",
		"wendy discover --type mesh --timeout 5s",
		"wendy auth login", "wendy cloud login",
		"wendy --json device attach --device DEVICE_A example -- pairctl rpc cluster:invite-status '{\"inviteId\":\"fixture\"}'",
	} {
		if _, err := parseDocumentedCommand(line); err != nil {
			t.Errorf("valid documented argv rejected: %s: %v", line, err)
		}
	}
	for _, line := range []string{
		"wendy discover --type lan --timeot 5s",
		"wendy discover --type lan --timeout 5fortnights",
		"wendy net configure --participate=perhaps",
		"wendy net removed-command",
		"wendy run --removed-build-option fixture",
	} {
		if _, err := parseDocumentedCommand(line); err == nil {
			t.Errorf("invalid argv accepted: %s", line)
		}
	}
}
