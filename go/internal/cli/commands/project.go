package commands

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

var entitlementDescriptions = map[string]string{
	appconfig.EntitlementNetwork:   "Access network interfaces",
	appconfig.EntitlementBluetooth: "Access Bluetooth peripherals",
	appconfig.EntitlementVideo:     "Deprecated: use camera instead",
	appconfig.EntitlementGPU:       "Access GPU for AI or compute workloads",
	appconfig.EntitlementNPU:       "Access the NPU for on-device AI inference",
	appconfig.EntitlementPersist:   "Persist data across restarts",
	appconfig.EntitlementAudio:     "Access audio input/output devices",
	appconfig.EntitlementCamera:    "Access camera devices",
	appconfig.EntitlementUSB:       "Access USB peripherals",
	appconfig.EntitlementI2C:       "Access I2C bus devices",
	appconfig.EntitlementGPIO:      "Access GPIO pins",
	appconfig.EntitlementSPI:       "Access SPI bus devices (displays, sensors, flash - may require GPIO access)",
	appconfig.EntitlementInput:     "Access Linux input devices (game controllers, barcode scanners, keyboards)",
}

// frameworkDescriptions mirrors entitlementDescriptions for the "frameworks"
// key in wendy.json, so `wendy project frameworks list --show-all` gives the
// same quality of discoverability `wendy project entitlements list --show-all`
// already gives for entitlements.
var frameworkDescriptions = map[string]string{
	appconfig.FrameworkROS2: "ROS 2 runtime config (RMW implementation, distro, domain ID, discovery scope) — see `wendy docs ros2`",
}

func newProjectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Manage Wendy project configuration",
	}

	cmd.AddCommand(newEntitlementsCmd())
	cmd.AddCommand(newFrameworksCmd())
	cmd.AddCommand(newOptimizeCmd())
	return cmd
}

func newEntitlementsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "entitlements",
		Short: "Manage project entitlements",
	}

	cmd.AddCommand(
		newEntitlementsListCmd(),
		newEntitlementsAddCmd(),
		newEntitlementsRemoveCmd(),
	)
	return cmd
}

func newEntitlementsListCmd() *cobra.Command {
	var showAll bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List project entitlements",
		RunE: func(cmd *cobra.Command, args []string) error {
			if showAll {
				return listAllEntitlementTypes(cmd)
			}
			return listProjectEntitlements(cmd)
		},
	}

	cmd.Flags().BoolVar(&showAll, "show-all", false, "Show all available entitlement types")
	return cmd
}

// listAllEntitlementTypes and its output siblings below write to
// cmd.OutOrStdout() rather than cobra's cmd.Print*, which — despite the name
// — writes to OutOrStderr(). Using cmd.Print* here would mean `wendy project
// entitlements list --show-all --json | jq` silently sees nothing on stdout,
// while OutOrStdout() defaults to os.Stdout and still honors cmd.SetOut.
func listAllEntitlementTypes(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	types := appconfig.ValidEntitlementTypes

	if jsonOutput {
		data, err := json.Marshal(types)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(data))
		return nil
	}

	fmt.Fprintln(out, "Available entitlement types:")
	for _, t := range types {
		fmt.Fprintf(out, "  %s\n", t)
	}
	return nil
}

func listProjectEntitlements(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	cfg, _, err := loadProjectConfig()
	if err != nil {
		return err
	}

	if jsonOutput {
		data, err := json.Marshal(cfg.Entitlements)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(data))
		return nil
	}

	if len(cfg.Entitlements) == 0 {
		fmt.Fprintln(out, "No entitlements configured.")
		return nil
	}

	fmt.Fprintln(out, "Project entitlements:")
	for _, e := range cfg.Entitlements {
		fmt.Fprintf(out, "  %s\n", e.Type)
	}
	return nil
}

func newEntitlementsAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add [type]",
		Short: "Add an entitlement to the project",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, cfgPath, err := loadProjectConfig()
			if err != nil {
				return err
			}

			existing := make(map[string]bool, len(cfg.Entitlements))
			for _, e := range cfg.Entitlements {
				existing[e.Type] = true
			}

			var entType string
			if len(args) > 0 {
				entType = args[0]
			} else {
				// Build picker items from entitlement types not yet in the project.
				var items []tui.PickerItem
				for _, t := range appconfig.ValidEntitlementTypes {
					if !existing[t] {
						items = append(items, tui.PickerItem{Name: t, Description: entitlementDescriptions[t], Value: t})
					}
				}
				if len(items) == 0 {
					cliLogln("All entitlement types are already added.")
					return nil
				}

				selected, err := pickFromItems("Select an entitlement to add", items)
				if err != nil {
					return err
				}
				entType = selected
			}

			// ROS 2 (and any future framework) is a common guess here, since
			// nothing else in the CLI names "frameworks" as the place device
			// integrations live. Redirect before falling into the generic
			// "unknown type" error, which would otherwise say nothing about
			// where "ros2" actually belongs.
			if slices.Contains(appconfig.ValidFrameworkTypes, entType) {
				return fmt.Errorf("%q is a framework, not an entitlement — configure it with `wendy project frameworks add %s`",
					entType, entType)
			}

			if !slices.Contains(appconfig.ValidEntitlementTypes, entType) {
				return fmt.Errorf("unknown entitlement type %q\nValid types: %s",
					entType, strings.Join(appconfig.ValidEntitlementTypes, ", "))
			}

			if existing[entType] {
				return fmt.Errorf("entitlement %q already exists", entType)
			}

			ent := appconfig.Entitlement{Type: entType}

			if err := promptEntitlementFields(&ent); err != nil {
				if errors.Is(err, tui.ErrCancelled) {
					return ErrUserCancelled
				}
				return err
			}

			cfg.Entitlements = append(cfg.Entitlements, ent)

			if err := saveProjectConfig(cfg, cfgPath); err != nil {
				return err
			}

			cliSuccess("Added %q entitlement", entType)
			return nil
		},
	}
}

func newEntitlementsRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove [type]",
		Short: "Remove an entitlement from the project",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, cfgPath, err := loadProjectConfig()
			if err != nil {
				return err
			}

			var entType string
			if len(args) > 0 {
				entType = args[0]
			} else {
				if len(cfg.Entitlements) == 0 {
					cliLogln("No entitlements configured.")
					return nil
				}

				var items []tui.PickerItem
				for _, e := range cfg.Entitlements {
					items = append(items, tui.PickerItem{Name: e.Type, Description: entitlementDescriptions[e.Type], Value: e.Type})
				}

				selected, err := pickFromItems("Select an entitlement to remove", items)
				if err != nil {
					return err
				}
				entType = selected
			}

			idx := -1
			for i, e := range cfg.Entitlements {
				if e.Type == entType {
					idx = i
					break
				}
			}

			if idx == -1 {
				return fmt.Errorf("entitlement %q not found in project", entType)
			}

			cfg.Entitlements = slices.Delete(cfg.Entitlements, idx, idx+1)

			if err := saveProjectConfig(cfg, cfgPath); err != nil {
				return err
			}

			cliSuccess("Removed %q entitlement", entType)
			return nil
		},
	}
}

// newFrameworksCmd builds the `wendy project frameworks` command group. It
// mirrors `wendy project entitlements` (list/add/remove, same error quality
// for an unknown type) for the "frameworks" key in wendy.json, which
// previously had no CLI-native way to discover its valid values or shape —
// unlike entitlements, whose `add` command already lists valid types on a bad
// guess.
func newFrameworksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "frameworks",
		Short: "Manage project framework configuration (e.g. ROS 2)",
	}

	cmd.AddCommand(
		newFrameworksListCmd(),
		newFrameworksAddCmd(),
		newFrameworksRemoveCmd(),
	)
	return cmd
}

// configuredFrameworkTypes returns the framework keys actually set in fw, in
// the same order as appconfig.ValidFrameworkTypes.
func configuredFrameworkTypes(fw *appconfig.FrameworksConfig) []string {
	if fw == nil {
		return nil
	}
	var types []string
	if fw.ROS2 != nil {
		types = append(types, appconfig.FrameworkROS2)
	}
	return types
}

func newFrameworksListCmd() *cobra.Command {
	var showAll bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List project framework configuration",
		RunE: func(cmd *cobra.Command, args []string) error {
			if showAll {
				return listAllFrameworkTypes(cmd)
			}
			return listProjectFrameworks(cmd)
		},
	}

	cmd.Flags().BoolVar(&showAll, "show-all", false, "Show all available framework types")
	return cmd
}

func listAllFrameworkTypes(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	types := appconfig.ValidFrameworkTypes

	if jsonOutput {
		data, err := json.Marshal(types)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(data))
		return nil
	}

	fmt.Fprintln(out, "Available framework types:")
	for _, t := range types {
		if desc := frameworkDescriptions[t]; desc != "" {
			fmt.Fprintf(out, "  %s — %s\n", t, desc)
		} else {
			fmt.Fprintf(out, "  %s\n", t)
		}
	}
	return nil
}

func listProjectFrameworks(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	cfg, _, err := loadProjectConfig()
	if err != nil {
		return err
	}

	configured := configuredFrameworkTypes(cfg.Frameworks)

	if jsonOutput {
		data, err := json.Marshal(configured)
		if err != nil {
			return err
		}
		fmt.Fprintln(out, string(data))
		return nil
	}

	if len(configured) == 0 {
		fmt.Fprintln(out, "No frameworks configured.")
		return nil
	}

	fmt.Fprintln(out, "Project frameworks:")
	for _, t := range configured {
		fmt.Fprintf(out, "  %s\n", t)
	}
	return nil
}

func newFrameworksAddCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "add [type]",
		Short: "Add a framework to the project",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, cfgPath, err := loadProjectConfig()
			if err != nil {
				return err
			}

			existing := configuredFrameworkTypes(cfg.Frameworks)
			existingSet := make(map[string]bool, len(existing))
			for _, t := range existing {
				existingSet[t] = true
			}

			var fwType string
			if len(args) > 0 {
				fwType = args[0]
			} else {
				var items []tui.PickerItem
				for _, t := range appconfig.ValidFrameworkTypes {
					if !existingSet[t] {
						items = append(items, tui.PickerItem{Name: t, Description: frameworkDescriptions[t], Value: t})
					}
				}
				if len(items) == 0 {
					cliLogln("All framework types are already added.")
					return nil
				}

				selected, err := pickFromItems("Select a framework to add", items)
				if err != nil {
					return err
				}
				fwType = selected
			}

			if !slices.Contains(appconfig.ValidFrameworkTypes, fwType) {
				return fmt.Errorf("unknown framework type %q\nValid types: %s",
					fwType, strings.Join(appconfig.ValidFrameworkTypes, ", "))
			}

			if existingSet[fwType] {
				return fmt.Errorf("framework %q already exists", fwType)
			}

			if cfg.Frameworks == nil {
				cfg.Frameworks = &appconfig.FrameworksConfig{}
			}
			switch fwType {
			case appconfig.FrameworkROS2:
				// All ROS2Config fields are optional with sensible defaults
				// (humble, CycloneDDS, a stable per-app domain ID), so unlike
				// persist/i2c/gpio entitlements there is nothing required to
				// prompt for here — an empty config already enables it.
				cfg.Frameworks.ROS2 = &appconfig.ROS2Config{}
			}

			if err := saveProjectConfig(cfg, cfgPath); err != nil {
				return err
			}

			cliSuccess("Added %q framework", fwType)
			if fwType == appconfig.FrameworkROS2 {
				cliLogln("Using defaults (distro %q, rmw %q, domain ID derived from appId). "+
					"Edit \"frameworks.ros2\" in wendy.json to customize, or see `wendy docs ros2`.",
					appconfig.ROS2DefaultDistro, appconfig.ROS2DefaultRMW)
			}
			return nil
		},
	}
}

func newFrameworksRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove [type]",
		Short: "Remove a framework from the project",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, cfgPath, err := loadProjectConfig()
			if err != nil {
				return err
			}

			existing := configuredFrameworkTypes(cfg.Frameworks)

			var fwType string
			if len(args) > 0 {
				fwType = args[0]
			} else {
				if len(existing) == 0 {
					cliLogln("No frameworks configured.")
					return nil
				}

				var items []tui.PickerItem
				for _, t := range existing {
					items = append(items, tui.PickerItem{Name: t, Description: frameworkDescriptions[t], Value: t})
				}

				selected, err := pickFromItems("Select a framework to remove", items)
				if err != nil {
					return err
				}
				fwType = selected
			}

			if !slices.Contains(existing, fwType) {
				return fmt.Errorf("framework %q not found in project", fwType)
			}

			switch fwType {
			case appconfig.FrameworkROS2:
				cfg.Frameworks.ROS2 = nil
			}
			if cfg.Frameworks != nil && cfg.Frameworks.ROS2 == nil {
				cfg.Frameworks = nil
			}

			if err := saveProjectConfig(cfg, cfgPath); err != nil {
				return err
			}

			cliSuccess("Removed %q framework", fwType)
			return nil
		},
	}
}

// promptEntitlementFields interactively prompts for required fields based on
// the entitlement type. Uses Bubble Tea text inputs with inline validation
// so the user can fix errors without restarting the wizard.
func promptEntitlementFields(ent *appconfig.Entitlement) error {
	notEmpty := func(label string) tui.ValidateFunc {
		return func(v string) error {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("%s cannot be empty", label)
			}
			return nil
		}
	}

	switch ent.Type {
	case appconfig.EntitlementPersist:
		name, err := tui.PromptText(
			"App ID",
			"shared namespace — apps with the same ID can access each other's data",
			notEmpty("app ID"),
		)
		if err != nil {
			return err
		}
		ent.Name = name

		path, err := tui.PromptTextWithDefault(
			"Mount path",
			"inside your container",
			"/data",
			notEmpty("mount path"),
		)
		if err != nil {
			return err
		}
		ent.Path = path

	case appconfig.EntitlementI2C:
		device, err := tui.PromptTextWithDefault(
			"I2C device",
			"",
			"/dev/i2c-1",
			notEmpty("I2C device"),
		)
		if err != nil {
			return err
		}
		ent.Device = device

	case appconfig.EntitlementGPIO:
		var pins []int
		_, err := tui.PromptText(
			"GPIO pins",
			"comma-separated, e.g. 17,27,22 — leave empty for all",
			func(v string) error {
				if strings.TrimSpace(v) == "" {
					pins = nil
					return nil
				}
				p, err := parsePins(v)
				if err != nil {
					return err
				}
				pins = p
				return nil
			},
		)
		if err != nil {
			return err
		}
		ent.Pins = pins
	}

	return nil
}

func parsePins(input string) ([]int, error) {
	parts := strings.Split(input, ",")
	var pins []int
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		pin, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("invalid pin %q: %w", p, err)
		}
		pins = append(pins, pin)
	}
	if len(pins) == 0 {
		return nil, fmt.Errorf("gpio entitlement requires at least one pin")
	}
	return pins, nil
}

// pickFromItems shows an interactive picker with the given title and items,
// returning the selected item's Value as a string.
func pickFromItems(title string, items []tui.PickerItem) (string, error) {
	return pickFromItemsWithColumns(title, items, nil)
}

func pickFromItemsWithColumns(title string, items []tui.PickerItem, columns []tui.PickerColumn) (string, error) {
	picker := tui.NewPickerWithTitle(title)
	if len(columns) > 0 {
		picker = tui.NewPickerWithTitleAndColumns(title, columns)
	}
	p := tea.NewProgram(picker)

	go func() {
		p.Send(tui.PickerAddMsg{Items: items})
		p.Send(tui.PickerDoneMsg{})
	}()

	finalModel, err := p.Run()
	if err != nil {
		return "", fmt.Errorf("picker: %w", err)
	}

	pm := finalModel.(tui.PickerModel)
	if pm.Cancelled() {
		return "", ErrUserCancelled
	}
	if pm.Selected() == nil {
		return "", fmt.Errorf("no selection")
	}

	return pm.Selected().Value.(string), nil
}

func loadProjectConfig() (*appconfig.AppConfig, string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, "", fmt.Errorf("getting working directory: %w", err)
	}

	cfgPath := filepath.Join(cwd, "wendy.json")
	cfg, err := appconfig.LoadFromFile(cfgPath)
	if err != nil {
		return nil, "", fmt.Errorf("loading wendy.json: %w", err)
	}

	return cfg, cfgPath, nil
}

func saveProjectConfig(cfg *appconfig.AppConfig, path string) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	data = append(data, '\n')

	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("writing wendy.json: %w", err)
	}

	return nil
}
