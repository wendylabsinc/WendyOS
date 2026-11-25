// api-surface-snapshot prints the user-visible Wendy CLI surface in a stable
// JSON form. CI compares snapshots from the base and head of a pull request so
// additions, removals, and changes receive API review.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
)

type surface struct {
	Commands []commandSurface `json:"commands"`
}

type commandSurface struct {
	Path            string        `json:"path"`
	Use             string        `json:"use"`
	Aliases         []string      `json:"aliases,omitempty"`
	SuggestFor      []string      `json:"suggest_for,omitempty"`
	Short           string        `json:"short,omitempty"`
	Long            string        `json:"long,omitempty"`
	Example         string        `json:"example,omitempty"`
	Deprecated      string        `json:"deprecated,omitempty"`
	ValidArgs       []string      `json:"valid_args,omitempty"`
	ArgAliases      []string      `json:"arg_aliases,omitempty"`
	LocalFlags      []flagSurface `json:"local_flags,omitempty"`
	PersistentFlags []flagSurface `json:"persistent_flags,omitempty"`
}

type flagSurface struct {
	Name                string              `json:"name"`
	Shorthand           string              `json:"shorthand,omitempty"`
	Usage               string              `json:"usage,omitempty"`
	Default             string              `json:"default,omitempty"`
	NoOptDefault        string              `json:"no_opt_default,omitempty"`
	Type                string              `json:"type"`
	Deprecated          string              `json:"deprecated,omitempty"`
	ShorthandDeprecated string              `json:"shorthand_deprecated,omitempty"`
	Annotations         map[string][]string `json:"annotations,omitempty"`
}

func main() {
	if len(os.Args) == 4 && os.Args[1] == "compare" {
		base, err := readSurface(os.Args[2])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		head, err := readSurface(os.Args[3])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(compareRisk(base, head))
		return
	}
	docsMode := len(os.Args) == 2 && os.Args[1] == "--docs"
	if len(os.Args) != 1 && !docsMode {
		fmt.Fprintln(os.Stderr, "usage: api-surface-snapshot [--docs | compare BASE.json HEAD.json]")
		os.Exit(2)
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(snapshotForDocs(commands.NewRootCmd(), docsMode)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func readSurface(path string) (surface, error) {
	file, err := os.Open(path)
	if err != nil {
		return surface{}, fmt.Errorf("open CLI surface %q: %w", path, err)
	}
	defer file.Close()

	var result surface
	if err := json.NewDecoder(file).Decode(&result); err != nil {
		return surface{}, fmt.Errorf("decode CLI surface %q: %w", path, err)
	}
	return result, nil
}

// compareRisk estimates how thoroughly an API change needs testing. Critical
// install/deploy/auth/update flows and changes to an existing contract are
// high-risk, purely additive contracts are mid-risk, and other help-only or
// absent changes are low-risk.
func compareRisk(base, head surface) string {
	baseCommands := commandMap(base.Commands)
	headCommands := commandMap(head.Commands)
	added := false
	for path, baseCommand := range baseCommands {
		if !isCriticalCommand(path) {
			continue
		}
		headCommand, exists := headCommands[path]
		if !exists || !reflect.DeepEqual(baseCommand, headCommand) {
			return "high"
		}
	}
	for path := range headCommands {
		if _, exists := baseCommands[path]; !exists && isCriticalCommand(path) {
			return "high"
		}
	}

	for path, baseCommand := range baseCommands {
		headCommand, exists := headCommands[path]
		if !exists {
			return "high"
		}
		if !reflect.DeepEqual(commandContract(baseCommand), commandContract(headCommand)) {
			return "high"
		}
		localAdded, localChanged := compareFlags(baseCommand.LocalFlags, headCommand.LocalFlags)
		persistentAdded, persistentChanged := compareFlags(baseCommand.PersistentFlags, headCommand.PersistentFlags)
		if localChanged || persistentChanged {
			return "high"
		}
		added = added || localAdded || persistentAdded
	}

	for path := range headCommands {
		if _, exists := baseCommands[path]; !exists {
			added = true
		}
	}
	if added {
		return "mid"
	}
	return "low"
}

func isCriticalCommand(path string) bool {
	critical := map[string]bool{
		"auth": true, "enroll": true, "flash": true, "install": true,
		"login": true, "provision": true, "run": true, "update": true,
	}
	for _, component := range strings.Fields(path) {
		if critical[component] {
			return true
		}
	}
	return false
}

type commandContractSurface struct {
	Use        string
	Aliases    []string
	SuggestFor []string
	Deprecated string
	ValidArgs  []string
	ArgAliases []string
}

func commandContract(command commandSurface) commandContractSurface {
	return commandContractSurface{
		Use:        command.Use,
		Aliases:    command.Aliases,
		SuggestFor: command.SuggestFor,
		Deprecated: command.Deprecated,
		ValidArgs:  command.ValidArgs,
		ArgAliases: command.ArgAliases,
	}
}

type flagContractSurface struct {
	Shorthand           string
	Default             string
	NoOptDefault        string
	Type                string
	Deprecated          string
	ShorthandDeprecated string
	Annotations         map[string][]string
}

func compareFlags(base, head []flagSurface) (added, changed bool) {
	baseFlags := flagMap(base)
	headFlags := flagMap(head)
	for name, baseFlag := range baseFlags {
		headFlag, exists := headFlags[name]
		if !exists || !reflect.DeepEqual(flagContract(baseFlag), flagContract(headFlag)) {
			return false, true
		}
	}
	for name := range headFlags {
		if _, exists := baseFlags[name]; !exists {
			added = true
		}
	}
	return added, false
}

func flagContract(flag flagSurface) flagContractSurface {
	return flagContractSurface{
		Shorthand:           flag.Shorthand,
		Default:             flag.Default,
		NoOptDefault:        flag.NoOptDefault,
		Type:                flag.Type,
		Deprecated:          flag.Deprecated,
		ShorthandDeprecated: flag.ShorthandDeprecated,
		Annotations:         flag.Annotations,
	}
}

func commandMap(commands []commandSurface) map[string]commandSurface {
	result := make(map[string]commandSurface, len(commands))
	for _, command := range commands {
		result[command.Path] = command
	}
	return result
}

func flagMap(flags []flagSurface) map[string]flagSurface {
	result := make(map[string]flagSurface, len(flags))
	for _, flag := range flags {
		result[flag.Name] = flag
	}
	return result
}

func snapshot(root *cobra.Command) surface {
	return snapshotForDocs(root, false)
}

// Docs include the user-facing VM workflow without exposing internal commands
// or changing the default API-review snapshot.
func snapshotForDocs(root *cobra.Command, docsMode bool) surface {
	result := surface{}
	var walk func(*cobra.Command)
	walk = func(cmd *cobra.Command) {
		if cmd.Hidden && !(docsMode && cmd.CommandPath() == "wendy vm") {
			return
		}

		entry := commandSurface{
			Path:            cmd.CommandPath(),
			Use:             cmd.Use,
			Aliases:         sortedCopy(cmd.Aliases),
			SuggestFor:      sortedCopy(cmd.SuggestFor),
			Short:           cmd.Short,
			Long:            cmd.Long,
			Example:         cmd.Example,
			Deprecated:      cmd.Deprecated,
			ValidArgs:       sortedCopy(cmd.ValidArgs),
			ArgAliases:      sortedCopy(cmd.ArgAliases),
			LocalFlags:      flags(cmd.LocalNonPersistentFlags()),
			PersistentFlags: flags(cmd.PersistentFlags()),
		}
		result.Commands = append(result.Commands, entry)

		for _, child := range cmd.Commands() {
			walk(child)
		}
	}
	walk(root)
	sort.Slice(result.Commands, func(i, j int) bool {
		return result.Commands[i].Path < result.Commands[j].Path
	})
	return result
}

func flags(flagSet *pflag.FlagSet) []flagSurface {
	var result []flagSurface
	flagSet.VisitAll(func(flag *pflag.Flag) {
		if flag.Hidden {
			return
		}
		result = append(result, flagSurface{
			Name:                flag.Name,
			Shorthand:           flag.Shorthand,
			Usage:               flag.Usage,
			Default:             flag.DefValue,
			NoOptDefault:        flag.NoOptDefVal,
			Type:                flag.Value.Type(),
			Deprecated:          flag.Deprecated,
			ShorthandDeprecated: flag.ShorthandDeprecated,
			Annotations:         flag.Annotations,
		})
	})
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

func sortedCopy(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}
