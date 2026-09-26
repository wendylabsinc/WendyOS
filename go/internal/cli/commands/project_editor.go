package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/google/shlex"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
)

// Seams keep the guided workflow testable without a terminal or external editor.
var projectPick = pickProjectItem
var projectPrompt = func(label, hint, value string, validate tui.ValidateFunc) (string, error) {
	return tui.PromptTextWithDefault(label, hint, value, validate)
}
var projectConfirm = func(question string) (bool, error) { return tui.Confirm(question) }
var projectOpenEditor = openProjectEditor

func pickProjectItem(title string, items []tui.PickerItem) (string, error) {
	picker := tui.NewPickerWithTitle(title)
	picker.Filterable = true
	p := tea.NewProgram(picker)
	go func() {
		p.Send(tui.PickerAddMsg{Items: items})
		p.Send(tui.PickerDoneMsg{})
	}()
	result, err := p.Run()
	if err != nil {
		return "", err
	}
	m := result.(tui.PickerModel)
	if m.Cancelled() {
		return "", ErrUserCancelled
	}
	if m.Selected() == nil {
		return "", fmt.Errorf("no selection")
	}
	return m.Selected().Value.(string), nil
}

func projectInteractive() bool { return !jsonOutput && isInteractiveTerminal() }

func projectFlag(cmd *cobra.Command, name string) string {
	value, _ := cmd.Flags().GetString(name)
	return value
}

func commandProjectManifest(cmd *cobra.Command) (*projectManifest, error) {
	return loadProjectManifest(projectFlag(cmd, "file"))
}

func newProjectShowCmd() *cobra.Command {
	return &cobra.Command{Use: "show", Short: "Show app settings, capabilities, integrations, and services", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			doc, err := commandProjectManifest(cmd)
			if err != nil {
				return err
			}
			return showProject(cmd, doc)
		},
	}
}

func newProjectValidateCmd() *cobra.Command {
	return &cobra.Command{Use: "validate [path]", Short: "Validate a project manifest and explain configuration errors", Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := projectFlag(cmd, "file")
			if len(args) > 0 {
				path = args[0]
			}
			doc, err := loadProjectManifest(path)
			if err != nil {
				return err
			}
			if err := doc.requireExisting(); err != nil {
				return err
			}
			warnings, validationErr := doc.validate()
			if jsonOutput {
				result := map[string]any{"path": doc.path, "valid": validationErr == nil, "warnings": warnings}
				if validationErr != nil {
					result["error"] = validationErr.Error()
				}
				if err := json.NewEncoder(cmd.OutOrStdout()).Encode(result); err != nil {
					return err
				}
			} else if validationErr == nil {
				fmt.Fprintf(cmd.OutOrStdout(), "%s is valid.\n", doc.path)
				printAppConfigWarnings(cmd.ErrOrStderr(), warnings)
			}
			return validationErr
		},
	}
}

func newProjectChangeCmd(action, group string) *cobra.Command {
	cmd := &cobra.Command{
		Use: action + " [name]", Short: strings.ToUpper(action[:1]) + action[1:] + " a capability or integration", Args: cobra.MaximumNArgs(1),
		Example: "  wendy project add http --port 8080\n  wendy project add persist --name recordings --path /data\n  wendy project edit ros2 --domain-id 42\n  wendy project add camera --service vision\n  wendy project edit --raw",
	}
	switch action {
	case "add":
		cmd.Example = "  wendy project add\n  wendy project add http --port 8080\n  wendy project add persist --name recordings --path /data\n  wendy project add camera --service vision"
	case "edit":
		cmd.Example = "  wendy project edit\n  wendy project edit ros2 --domain-id 42\n  wendy project edit app --version 0.2.0\n  wendy project edit --raw"
	case "remove":
		cmd.Example = "  wendy project remove\n  wendy project remove camera\n  wendy project remove persist --entry 1"
	}
	cmd.Long = cmd.Short + ". Omit the name to open a searchable picker.\nUse `wendy project " + action + " <name> --help` for that item's fields.\n\nNames:"
	for _, f := range projectCatalog() {
		if group == "entitlements" && f.name == "ros2" || group == "frameworks" && f.name != "ros2" {
			continue
		}
		cmd.Long += fmt.Sprintf("\n  %-16s %s", f.name, f.label)
	}
	if action == "edit" && group == "" {
		cmd.Long += "\n  app              App identity, language, platform, and version"
	}
	cmd.Flags().Bool("dry-run", false, "Validate and preview the changes without saving")
	cmd.Flags().String("service", "", "Edit settings for this service instead of app defaults")
	cmd.Flags().Int("entry", -1, "Entitlement array index to edit or remove when a type appears more than once")
	if action == "add" {
		_ = cmd.Flags().MarkHidden("entry")
	}
	if action != "remove" {
		projectFieldFlags(cmd)
	}
	if action == "edit" {
		cmd.Flags().Bool("raw", false, "Edit a temporary copy in $VISUAL or $EDITOR, then validate and review it")
	}
	defaultHelp := cmd.HelpFunc()
	cmd.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		positional := cmd.Flags().Args()
		if len(positional) == 0 {
			defaultHelp(cmd, args)
			return
		}
		feature, ok := findProjectFeature(positional[0])
		if !ok {
			defaultHelp(cmd, args)
			return
		}
		allowed := map[string]bool{"dry-run": true, "service": true, "entry": true, "help": true}
		for _, field := range feature.fields {
			allowed[field.flag] = true
		}
		var hidden []*pflag.Flag
		cmd.LocalNonPersistentFlags().VisitAll(func(flag *pflag.Flag) {
			if !allowed[flag.Name] && !flag.Hidden {
				flag.Hidden = true
				hidden = append(hidden, flag)
			}
		})
		long, example := cmd.Long, cmd.Example
		cmd.Long = feature.label + ". " + feature.description
		cmd.Example = ""
		defer func() {
			cmd.Long = long
			cmd.Example = example
			for _, flag := range hidden {
				flag.Hidden = false
			}
		}()
		defaultHelp(cmd, args)
	})
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var names []string
		for _, f := range projectCatalog() {
			if group == "entitlements" && f.name == "ros2" || group == "frameworks" && f.name != "ros2" {
				continue
			}
			names = append(names, f.name+"\t"+f.label)
		}
		if action == "edit" && group == "" {
			names = append(names, "app\tApp identity, language, platform, and version")
		}
		return names, cobra.ShellCompDirectiveNoFileComp
	}
	_ = cmd.RegisterFlagCompletionFunc("service", completeProjectServices)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		raw, _ := cmd.Flags().GetBool("raw")
		doc, err := commandProjectManifest(cmd)
		if err != nil && !(raw && doc != nil && doc.original != nil) {
			return err
		}
		if err := doc.requireExisting(); err != nil {
			return err
		}
		name := ""
		if len(args) > 0 {
			name = args[0]
		}
		guided := len(args) == 0 || raw
		if raw {
			if name != "" || projectFlag(cmd, "service") != "" {
				return fmt.Errorf("--raw edits the whole manifest; omit the name and --service")
			}
			if changed, err := checkProjectFieldFlags(cmd, projectFeature{}); changed || err != nil {
				return fmt.Errorf("--raw cannot be combined with field flags")
			}
			if cmd.Flags().Changed("entry") {
				return fmt.Errorf("--raw edits the whole manifest; omit --entry")
			}
			if !projectInteractive() {
				return fmt.Errorf("--raw requires an interactive terminal; use field flags in scripts")
			}
			err = projectOpenEditor(cmd, doc)
		} else {
			guided, err = changeProjectFeature(cmd, doc, action, name, group)
		}
		if err != nil {
			return projectCancelError(err)
		}
		dryRun, _ := cmd.Flags().GetBool("dry-run")
		return finishProjectEdit(cmd, doc, guided, dryRun)
	}
	return cmd
}

func projectCancelError(err error) error {
	if errors.Is(err, tui.ErrCancelled) {
		return ErrUserCancelled
	}
	return err
}

func changeProjectFeature(cmd *cobra.Command, doc *projectManifest, action, name, group string) (bool, error) {
	service := projectFlag(cmd, "service")
	scope, err := doc.scope(service)
	if err != nil {
		return false, err
	}
	interactive := projectInteractive()
	guided := name == ""
	if name == "" {
		if !interactive {
			return false, fmt.Errorf("specify what to %s, for example `wendy project %s camera`; run `wendy project --help` for examples", action, action)
		}
		items := projectFeatureItems(scope, action, group)
		if len(items) == 0 {
			return false, fmt.Errorf("no configured items to %s in this scope", action)
		}
		name, err = projectPick("Choose what to "+action, items)
		if err != nil {
			return guided, err
		}
	}
	f, ok := findProjectFeature(name)
	if !ok {
		kind := "capability or integration"
		if group == "frameworks" {
			kind = "framework type"
		}
		var names []string
		for _, item := range projectFeatureItems(scope, "add", group) {
			names = append(names, item.Value.(string))
		}
		return guided, fmt.Errorf("unknown %s %q\nAvailable: %s", kind, name, strings.Join(names, ", "))
	}
	if group == "entitlements" && f.name == "ros2" {
		return guided, fmt.Errorf("%q is a framework; use `wendy project frameworks add ros2` or `wendy project add ros2`", name)
	}
	if group == "frameworks" && f.name != "ros2" {
		return guided, fmt.Errorf("unknown framework type %q; available: ros2", name)
	}
	if f.name == "app" && (action != "edit" || service != "" || group != "") {
		return guided, fmt.Errorf("use `wendy project edit app` without --service to edit app settings")
	}
	changed, err := checkProjectFieldFlags(cmd, f)
	if err != nil {
		return guided, err
	}
	guided = guided || interactive && !changed && action != "remove"
	entry, _ := cmd.Flags().GetInt("entry")
	if cmd.Flags().Changed("entry") && (entry < 0 || f.name == "app" || f.name == "ros2") {
		return guided, fmt.Errorf("--entry must be a nonnegative entitlement array index")
	}
	if interactive && action == "remove" && entry == -1 {
		entries, _ := scope["entitlements"].([]any)
		matches := 0
		for _, raw := range entries {
			if item, ok := raw.(map[string]any); ok && item["type"] == f.name {
				matches++
			}
		}
		guided = guided || matches > 1
	}
	obj, set, err := projectFeatureTarget(scope, f.name, action, entry, interactive)
	if err != nil {
		return guided, err
	}
	if action == "remove" {
		return guided, set(nil)
	}
	if action == "edit" && !changed && !interactive {
		return guided, fmt.Errorf("specify fields to edit, for example `wendy project edit ros2 --domain-id 42`; use --help for flags")
	}
	for _, field := range f.fields {
		flagSet := cmd.Flags().Changed(field.flag)
		value := projectFieldValue(field, obj)
		if flagSet {
			value = projectFlag(cmd, field.flag)
		}
		// Only mesh uses these fields. Keep existing values unless explicitly changed.
		if f.name == "network" && field.key != "mode" && obj["mode"] != "mesh" && !flagSet {
			continue
		}
		required := field.required || f.name == "network" && field.key == "serviceCIDR" && obj["mode"] == "mesh"
		if interactive && !flagSet && (guided || required && value == "") {
			guided = true
			if value == "" && action == "add" {
				value = field.defaultValue
			}
			if len([]rune(value)) > 256 {
				return guided, fmt.Errorf("%s is too long for the form; use --%s or `wendy project edit --raw`", field.label, field.flag)
			}
			promptField := field
			promptField.required = required
			value, err = projectPrompt(field.label, field.hint, value, func(v string) error { _, err := parseProjectField(promptField, v); return err })
			if err != nil {
				return guided, err
			}
			flagSet = true
		}
		if required && value == "" {
			return guided, fmt.Errorf("%s requires --%s; use `wendy project %s %s --help`", f.name, field.flag, action, f.name)
		}
		if !flagSet {
			if action == "add" && f.name == "network" && field.key == "mode" {
				obj[field.key] = field.defaultValue
			}
			continue
		}
		parsed, err := parseProjectField(field, value)
		if err != nil {
			return guided, fmt.Errorf("--%s: %w", field.flag, err)
		}
		if parsed == nil {
			delete(obj, field.key)
		} else {
			obj[field.key] = parsed
		}
		if f.name == "network" && field.key == "mode" && parsed != "mesh" {
			delete(obj, "serviceCIDR")
		}
	}
	return guided, set(obj)
}

func projectFeatureItems(scope projectObject, action, group string) []tui.PickerItem {
	var items []tui.PickerItem
	for index, f := range projectCatalog() {
		if group == "entitlements" && f.name == "ros2" || group == "frameworks" && f.name != "ros2" {
			continue
		}
		if action == "add" && f.name == "video" {
			continue
		}
		if action != "add" && !projectHasFeature(scope, f.name) {
			continue
		}
		items = append(items, tui.PickerItem{Name: f.label + " [" + f.name + "]", Description: f.description, Value: f.name, SortKey: fmt.Sprintf("%02d", index)})
	}
	return items
}

func projectHasFeature(scope projectObject, name string) bool {
	if name == "ros2" {
		fw, _ := scope["frameworks"].(map[string]any)
		return fw["ros2"] != nil
	}
	entries, _ := scope["entitlements"].([]any)
	for _, entry := range entries {
		if obj, ok := entry.(map[string]any); ok && obj["type"] == name {
			return true
		}
	}
	return false
}

func projectFeatureTarget(scope projectObject, name, action string, selected int, interactive bool) (projectObject, func(projectObject) error, error) {
	if name == "app" {
		return scope, func(projectObject) error { return nil }, nil
	}
	if name == "ros2" {
		fw, _ := scope["frameworks"].(map[string]any)
		if fw == nil {
			fw = projectObject{}
		}
		obj, exists := fw["ros2"]
		if action == "add" && exists {
			return nil, nil, fmt.Errorf("framework %q already exists; use `wendy project edit ros2`", name)
		}
		if action != "add" && !exists {
			return nil, nil, fmt.Errorf("framework %q not found in this scope; inherited settings must be edited at app scope", name)
		}
		if !exists {
			obj = projectObject{}
		}
		object, ok := obj.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("frameworks.ros2 must be an object; use `wendy project edit --raw` to repair it")
		}
		return object, func(value projectObject) error {
			if value == nil {
				delete(fw, name)
			} else {
				fw[name] = value
			}
			if len(fw) == 0 {
				delete(scope, "frameworks")
			} else {
				scope["frameworks"] = fw
			}
			return nil
		}, nil
	}
	entries, _ := scope["entitlements"].([]any)
	var matches []int
	for i, entry := range entries {
		if obj, ok := entry.(map[string]any); ok && obj["type"] == name {
			matches = append(matches, i)
		}
	}
	if action == "add" {
		if len(matches) > 0 && !slices.Contains([]string{"persist", "i2c", "serial"}, name) {
			return nil, nil, fmt.Errorf("%q already exists; use `wendy project edit %s`", name, name)
		}
		if selected != -1 {
			return nil, nil, fmt.Errorf("--entry is only used with edit or remove")
		}
		return projectObject{"type": name}, func(value projectObject) error { scope["entitlements"] = append(entries, value); return nil }, nil
	}
	if len(matches) == 0 {
		return nil, nil, fmt.Errorf("%q not found in this scope; inherited settings must be edited at app scope", name)
	}
	if selected == -1 && len(matches) > 1 {
		if !interactive {
			return nil, nil, fmt.Errorf("multiple %s entries; choose --entry from these indices: %v", name, matches)
		}
		var items []tui.PickerItem
		for _, i := range matches {
			items = append(items, tui.PickerItem{Name: fmt.Sprintf("%s [%d]", name, i), Description: projectEntrySummary(entries[i].(map[string]any)), Value: strconv.Itoa(i)})
		}
		choice, err := projectPick("Choose the entry", items)
		if err != nil {
			return nil, nil, err
		}
		selected, _ = strconv.Atoi(choice)
	}
	if selected == -1 {
		selected = matches[0]
	}
	if !slices.Contains(matches, selected) {
		return nil, nil, fmt.Errorf("entry %d is not a %s entry", selected, name)
	}
	return entries[selected].(map[string]any), func(value projectObject) error {
		if value == nil {
			entries = slices.Delete(entries, selected, selected+1)
		} else {
			entries[selected] = value
		}
		if len(entries) == 0 {
			delete(scope, "entitlements")
		} else {
			scope["entitlements"] = entries
		}
		return nil
	}, nil
}

func finishProjectEdit(cmd *cobra.Command, doc *projectManifest, guided, dryRun bool) error {
	warnings, err := doc.validate()
	if err != nil {
		return err
	}
	before := projectObject{}
	if doc.original != nil {
		before, _ = parseProjectObject(doc.original)
	}
	data, err := doc.data()
	if err != nil {
		return err
	}
	after, err := parseProjectObject(data)
	if err != nil {
		return err
	}
	changes := projectChanges(before, after, "")
	if changes == nil {
		changes = []projectChange{}
	}
	if !jsonOutput {
		printAppConfigWarnings(cmd.ErrOrStderr(), warnings)
		if guided || dryRun {
			fmt.Fprintf(cmd.OutOrStdout(), "Changes to %s:\n", doc.path)
			for _, change := range changes {
				a, _ := json.Marshal(change.Before)
				b, _ := json.Marshal(change.After)
				fmt.Fprintf(cmd.OutOrStdout(), "  %s\n    - %s\n    + %s\n", change.Path, a, b)
			}
		}
	}
	save := !dryRun && len(changes) > 0
	if save && guided {
		save, err = projectConfirm("Save these changes?")
		if err != nil {
			return projectCancelError(err)
		}
	}
	if save {
		if err := doc.save(); err != nil {
			return err
		}
	}
	if jsonOutput {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"path": doc.path, "saved": save, "dryRun": dryRun, "changes": changes, "warnings": warnings})
	}
	switch {
	case save:
		fmt.Fprintf(cmd.OutOrStdout(), "Saved %s. Run `wendy run` to deploy the changes.\n", doc.path)
	case len(changes) == 0:
		fmt.Fprintln(cmd.OutOrStdout(), "No changes.")
	default:
		fmt.Fprintln(cmd.OutOrStdout(), "No changes saved.")
	}
	return nil
}

func openProjectEditor(cmd *cobra.Command, doc *projectManifest) error {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		return fmt.Errorf("set VISUAL or EDITOR to your editor, for example `EDITOR='code --wait' wendy project edit --raw`")
	}
	args, err := shlex.Split(editor)
	if err != nil || len(args) == 0 {
		return fmt.Errorf("invalid editor command %q", editor)
	}
	file, err := os.CreateTemp(filepath.Dir(doc.path), ".wendy-edit-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	data, err := doc.data()
	if doc.root == nil {
		data, err = doc.original, nil
	}
	if err != nil {
		file.Close()
		return err
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	process := exec.CommandContext(cmd.Context(), args[0], append(args[1:], file.Name())...)
	process.Stdin, process.Stdout, process.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	if err := process.Run(); err != nil {
		return fmt.Errorf("editor: %w", err)
	}
	data, err = os.ReadFile(file.Name())
	if err != nil {
		return err
	}
	root, err := parseProjectObject(data)
	if err != nil {
		return err
	}
	doc.root = root
	return nil
}

func completeProjectServices(cmd *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
	doc, err := commandProjectManifest(cmd)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	return projectServiceNames(doc), cobra.ShellCompDirectiveNoFileComp
}

func projectServiceNames(doc *projectManifest) []string {
	names := map[string]bool{}
	services, _ := doc.root["services"].(map[string]any)
	for name := range services {
		names[name] = true
	}
	if doc.compose != nil {
		for name := range doc.compose.Services {
			names[name] = true
		}
	}
	result := make([]string, 0, len(names))
	for name := range names {
		result = append(result, name)
	}
	slices.Sort(result)
	return result
}
