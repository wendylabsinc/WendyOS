package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

func showProject(cmd *cobra.Command, doc *projectManifest) error {
	if jsonOutput {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"path": doc.path, "exists": doc.original != nil, "compose": doc.compose != nil, "manifest": doc.root, "services": projectServiceNames(doc)})
	}
	out := cmd.OutOrStdout()
	if len(doc.root) == 0 {
		if doc.compose != nil {
			fmt.Fprintln(out, "Compose project. A Wendy manifest is optional.")
			fmt.Fprintf(out, "Services: %s\n", strings.Join(projectServiceNames(doc), ", "))
		} else {
			fmt.Fprintf(out, "No manifest at %s.\n", doc.path)
		}
		fmt.Fprintln(out, "Run `wendy project` in a terminal to create a manifest for this project.")
		return nil
	}
	fmt.Fprintf(out, "%v · %s\n", doc.root["appId"], doc.path)
	for _, key := range []string{"language", "platform", "version"} {
		if value, ok := doc.root[key]; ok {
			fmt.Fprintf(out, "  %s: %v\n", key, value)
		}
	}
	printProjectScope(cmd, doc.root, "App settings")
	services, _ := doc.root["services"].(map[string]any)
	for _, name := range projectServiceNames(doc) {
		scope, _ := services[name].(map[string]any)
		printProjectScope(cmd, scope, "Service "+name+" (local settings; also uses app defaults)")
	}
	if _, err := doc.validate(); err != nil {
		fmt.Fprintf(out, "\nNeeds attention: %s\n", err)
	}
	fmt.Fprintln(out, "\nUse `wendy project add`, `edit`, `remove`, or `validate`.")
	return nil
}

func printProjectScope(cmd *cobra.Command, scope projectObject, title string) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "\n%s\n", title)
	entries, _ := scope["entitlements"].([]any)
	for i, raw := range entries {
		entry, ok := raw.(map[string]any)
		if !ok {
			fmt.Fprintf(out, "  [%d] Invalid entry\n", i)
			continue
		}
		name, _ := entry["type"].(string)
		label := name
		if f, ok := findProjectFeature(name); ok {
			label = f.label
		}
		fmt.Fprintf(out, "  [%d] %s: %s\n", i, label, projectEntrySummary(entry))
	}
	fw, _ := scope["frameworks"].(map[string]any)
	if ros2, ok := fw["ros2"].(map[string]any); ok {
		domain := ros2["domainId"]
		if domain == nil {
			domain = "derived from appId"
		}
		rmw, distro, discovery := ros2["rmw"], ros2["distro"], ros2["discoveryScope"]
		if rmw == nil {
			rmw = appconfig.ROS2DefaultRMW
		}
		if distro == nil {
			distro = appconfig.ROS2DefaultDistro
		}
		if discovery == nil {
			discovery = "app"
		}
		fmt.Fprintf(out, "  ROS 2: domain %v, %v, %v, discovery %v\n", domain, rmw, distro, discovery)
	}
	if len(entries) == 0 && len(fw) == 0 {
		fmt.Fprintln(out, "  No local capabilities or integrations.")
	}
	for _, key := range []string{"env", "resources", "readiness", "hooks", "run", "files"} {
		if _, exists := scope[key]; exists {
			fmt.Fprintf(out, "  %s configured; use `wendy project edit --raw` for details\n", key)
		}
	}
}

func projectEntrySummary(entry projectObject) string {
	switch entry["type"] {
	case "persist":
		return fmt.Sprintf("%v → %v", entry["name"], entry["path"])
	case "http", "mcp":
		return fmt.Sprintf("port %v", entry["port"])
	case "i2c", "serial":
		return fmt.Sprint(entry["device"])
	case "network":
		if mode, ok := entry["mode"]; ok {
			return fmt.Sprint(mode)
		}
		return "host (implicit legacy default)"
	case "gpio":
		if pins, ok := entry["pins"]; ok {
			return fmt.Sprintf("pins %v", pins)
		}
		return "all GPIO chips"
	case "camera":
		if list, ok := entry["allowlist"]; ok {
			return fmt.Sprintf("allowed cameras %v", list)
		}
	}
	return "enabled"
}

func runProjectHome(cmd *cobra.Command, _ []string) error {
	doc, err := commandProjectManifest(cmd)
	if err != nil {
		return err
	}
	if !projectInteractive() {
		return showProject(cmd, doc)
	}
	if doc.original == nil {
		if err := showProject(cmd, doc); err != nil {
			return err
		}
		choice, err := projectPick("Set up this project", []tui.PickerItem{
			{Name: "Create a Wendy manifest", Description: "Configure the existing project without scaffolding source files", Value: "create"},
			{Name: "Exit", Value: "exit"},
		})
		if err != nil {
			return projectCancelError(err)
		}
		if choice == "exit" {
			return nil
		}
		appID := filepath.Base(filepath.Dir(doc.path))
		if appconfig.ValidateAppID(appID) != nil {
			appID = "my-app"
		}
		doc.root = projectObject{"$schema": "https://wendy.dev/schemas/wendy.json", "appId": appID, "platform": "linux"}
		edit := newProjectChangeCmd("edit", "")
		if _, err := changeProjectFeature(edit, doc, "edit", "app", ""); err != nil {
			return projectCancelError(err)
		}
	}
	service := ""
	for {
		if err := showProject(cmd, doc); err != nil {
			return err
		}
		title := "Edit project (app defaults)"
		if service != "" {
			title = "Edit service " + service
		}
		items := []tui.PickerItem{
			{Name: "Add a capability or integration", Value: "add", SortKey: "1"},
			{Name: "Edit configured items", Value: "edit", SortKey: "2"},
			{Name: "Remove a configured item", Value: "remove", SortKey: "3"},
			{Name: "Edit app settings", Value: "app", SortKey: "4"},
			{Name: "Open manifest in editor", Value: "raw", SortKey: "6"},
			{Name: "Validate configuration", Value: "validate", SortKey: "7"},
			{Name: "Review and save", Value: "save", SortKey: "8"},
			{Name: "Discard changes and exit", Value: "exit", SortKey: "9"},
		}
		if len(projectServiceNames(doc)) > 0 {
			items = append(items, tui.PickerItem{Name: "Choose app or service scope", Value: "scope", SortKey: "5"})
		}
		action, err := projectPick(title, items)
		if errors.Is(err, ErrUserCancelled) || errors.Is(err, tui.ErrCancelled) {
			return nil
		}
		if err != nil {
			return err
		}
		before, err := doc.data()
		if err != nil {
			return err
		}
		switch action {
		case "exit":
			return nil
		case "save":
			if _, err = doc.validate(); err == nil {
				return finishProjectEdit(cmd, doc, true, false)
			}
		case "validate":
			var warnings []string
			warnings, err = doc.validate()
			if err == nil {
				fmt.Fprintln(cmd.OutOrStdout(), "Configuration is valid.")
				printAppConfigWarnings(cmd.ErrOrStderr(), warnings)
			}
		case "scope":
			scopes := []tui.PickerItem{{Name: "App defaults", Value: ""}}
			for _, name := range projectServiceNames(doc) {
				scopes = append(scopes, tui.PickerItem{Name: name, Value: name})
			}
			var selected string
			selected, err = projectPick("Choose a scope", scopes)
			if err == nil {
				service = selected
			}
		case "raw":
			err = projectOpenEditor(cmd, doc)
		default:
			name := ""
			if action == "app" {
				action, name = "edit", "app"
			}
			edit := newProjectChangeCmd(action, "")
			if name != "app" {
				_ = edit.Flags().Set("service", service)
			}
			_, err = changeProjectFeature(edit, doc, action, name, "")
		}
		if err != nil {
			doc.root, _ = parseProjectObject(before)
			if !errors.Is(err, ErrUserCancelled) && !errors.Is(err, tui.ErrCancelled) {
				fmt.Fprintln(cmd.ErrOrStderr(), err)
			}
		}
	}
}
