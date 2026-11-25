package commands

import (
	"io/fs"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/assets"
	"github.com/wendylabsinc/wendy/go/internal/cli/chat"
	"github.com/wendylabsinc/wendy/go/internal/cli/mcp"
)

// cliReferenceAllowlist maps a substring of a `wendy ...` invocation in
// agent-facing text to the reason the invocation may name a command or flag
// the CLI does not have. Matching invocations are not checked.
var cliReferenceAllowlist = map[string]string{
	"wendy cloud mcp' with --allow-tool": "prose quotes the command as 'wendy cloud mcp', so the closing quote stops command parsing at cloud; --allow-tool and --allow-all are wendy cloud mcp flags",
}

var invocationStartPattern = regexp.MustCompile("(?:^|[\\s(`\"'])wendy\\s+")

// agentFacingTexts returns every text Wendy ships to agents, keyed by a label
// for error messages: the MCP and relay instructions, the guide, the chat
// prompt, the end-user and MCP skill bundles, and AGENTS.md.
func agentFacingTexts(t *testing.T) map[string]string {
	t.Helper()
	sources := maps.Clone(mcp.AgentFacingTexts())
	sources["chat prompt"] = chat.SystemPrompt("", "")
	names, err := assets.EndUserSkillNames()
	if err != nil {
		t.Fatal(err)
	}
	roots := []string{"mcp-skills"}
	for _, name := range names {
		roots = append(roots, "skills/"+name)
	}
	for _, root := range roots {
		err := fs.WalkDir(assets.FS, root, func(name string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(name, ".md") {
				return err
			}
			data, err := assets.FS.ReadFile(name)
			sources[name] = string(data)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile("../../../../AGENTS.md")
	if err != nil {
		t.Fatal(err)
	}
	sources["AGENTS.md"] = string(data)
	return sources
}

// cliInvocation is one `wendy ...` command line found in agent-facing text.
type cliInvocation struct {
	text  string   // from "wendy" to where the invocation ends
	words []string // leading command words
	flags []string // long flag names, without "--" or "=value"
}

// cliInvocations finds `wendy ...` invocations in code spans and fenced
// blocks, and in plain text only where the first word is a root subcommand.
// An invocation ends at the end of the line, a closing backtick, or one of
// | ; && ). Global flags before the first word (wendy --json device ...) are
// skipped, with their value when the flag takes one.
func cliInvocations(text string, root *cobra.Command) []cliInvocation {
	subcommands := rootSubcommandNames(root)
	var found []cliInvocation
	fenced := false
	for line := range strings.Lines(text) {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~") {
			fenced = !fenced
			continue
		}
		for i, segment := range strings.Split(strings.TrimRight(line, "\r\n"), "`") {
			code := fenced || i%2 == 1
			for _, loc := range invocationStartPattern.FindAllStringIndex(segment, -1) {
				start := strings.Index(segment[loc[0]:loc[1]], "wendy") + loc[0]
				rest := segment[loc[1]:]
				if end := strings.IndexAny(rest, "|;)"); end >= 0 {
					rest = rest[:end]
				}
				rest, _, _ = strings.Cut(rest, "&&")
				inv := cliInvocation{text: strings.TrimSpace(segment[start:loc[1]] + rest)}
				tokens := strings.Fields(rest)
				for k := 0; k < len(tokens); k++ {
					token := tokens[k]
					if name, ok := strings.CutPrefix(token, "--"); ok && len(inv.words) == 0 {
						if f := root.PersistentFlags().Lookup(name); f != nil && f.NoOptDefVal == "" {
							k++
						}
						continue
					}
					if strings.HasPrefix(token, "-") || strings.ContainsAny(token, "<>${}\"'") {
						break
					}
					inv.words = append(inv.words, token)
				}
				if !code && (len(inv.words) == 0 || !subcommands[inv.words[0]]) {
					continue
				}
				for _, token := range tokens {
					if token == "--" {
						break
					}
					if name, ok := strings.CutPrefix(token, "--"); ok {
						name, _, _ = strings.Cut(name, "=")
						inv.flags = append(inv.flags, strings.TrimRight(name, ".,:"))
					}
				}
				found = append(found, inv)
			}
		}
	}
	return found
}

func rootSubcommandNames(root *cobra.Command) map[string]bool {
	names := map[string]bool{}
	for _, cmd := range root.Commands() {
		names[cmd.Name()] = true
		for _, alias := range cmd.Aliases {
			names[alias] = true
		}
	}
	return names
}

// cliProblem is a command or flag an invocation names that the CLI lacks.
type cliProblem struct{ name, kind string }

// cliProblems resolves an invocation against the command tree. A word left
// over under a command that has subcommands is an unknown subcommand, which
// root.Find alone does not report (it returns the parent).
func cliProblems(root *cobra.Command, inv cliInvocation) []cliProblem {
	path := append([]string{"wendy"}, inv.words...)
	cmd, rest, err := root.Find(inv.words)
	if err != nil {
		return []cliProblem{{strings.Join(path, " "), "command"}}
	}
	if len(rest) > 0 && cmd.HasSubCommands() {
		return []cliProblem{{strings.Join(path[:len(path)-len(rest)+1], " "), "command"}}
	}
	// Cobra adds --help (and --version on the root) when it runs a command.
	cmd.InitDefaultHelpFlag()
	cmd.InitDefaultVersionFlag()
	var problems []cliProblem
	for _, flag := range inv.flags {
		if flag != "" && cmd.Flags().Lookup(flag) == nil && cmd.InheritedFlags().Lookup(flag) == nil && root.PersistentFlags().Lookup(flag) == nil {
			problems = append(problems, cliProblem{strings.Join(path, " ") + " --" + flag, "flag"})
		}
	}
	return problems
}

func allowlistedInvocation(text string) bool {
	for key := range cliReferenceAllowlist {
		if strings.Contains(text, key) {
			return true
		}
	}
	return false
}

// Agents learn the CLI from the text we ship. A command or flag named there
// that no longer exists sends them the wrong way.
func TestAgentFacingTextNamesOnlyRealCLIFlags(t *testing.T) {
	root := NewRootCmd()
	sources := agentFacingTexts(t)
	checked := 0
	for _, label := range slices.Sorted(maps.Keys(sources)) {
		reported := map[string]bool{}
		report := func(name, kind string) {
			if !reported[name] {
				reported[name] = true
				t.Errorf("%s: %s is not a %s; fix the text or add an allowlist entry with a reason", label, name, kind)
			}
		}
		for _, inv := range cliInvocations(sources[label], root) {
			if allowlistedInvocation(inv.text) {
				continue
			}
			checked++
			for _, problem := range cliProblems(root, inv) {
				report(problem.name, problem.kind)
			}
		}
	}
	if checked == 0 {
		t.Fatal("found no wendy invocations in agent-facing text; the parser is broken")
	}
}

func TestAgentFacingTextCLICheckReportsUnknownCommandsAndFlags(t *testing.T) {
	root := NewRootCmd()
	for text, want := range map[string][]cliProblem{
		"`wendy device apps list --device x`":         nil,
		"`wendy --json device logs --app a --tail 5`": nil,
		"`wendy run --help`":                          nil,
		"`wendy device bogus`":                        {{"wendy device bogus", "command"}},
		"`wendy device bogus list --device x`":        {{"wendy device bogus", "command"}},
		"`wendy bogus`":                               {{"wendy bogus", "command"}},
		"`wendy run --bogus`":                         {{"wendy run --bogus", "flag"}},
	} {
		invocations := cliInvocations(text, root)
		if len(invocations) != 1 {
			t.Fatalf("%s: found %d invocations, want 1", text, len(invocations))
		}
		if got := cliProblems(root, invocations[0]); !slices.Equal(got, want) {
			t.Errorf("%s: problems %v, want %v", text, got, want)
		}
	}
}

func TestCLIReferenceAllowlistIsStillNeeded(t *testing.T) {
	root := NewRootCmd()
	var invocations []string
	for _, text := range agentFacingTexts(t) {
		for _, inv := range cliInvocations(text, root) {
			invocations = append(invocations, inv.text)
		}
	}
	for _, key := range slices.Sorted(maps.Keys(cliReferenceAllowlist)) {
		if !slices.ContainsFunc(invocations, func(text string) bool { return strings.Contains(text, key) }) {
			t.Errorf("allowlist entry %q matches no wendy invocation in agent-facing text; remove it", key)
		}
		if strings.TrimSpace(cliReferenceAllowlist[key]) == "" {
			t.Errorf("allowlist entry %q has no reason", key)
		}
	}
}
