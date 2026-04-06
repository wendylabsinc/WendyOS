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
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
)

var entitlementDescriptions = map[string]string{
	appconfig.EntitlementNetwork:   "Access network interfaces",
	appconfig.EntitlementBluetooth: "Access Bluetooth peripherals",
	appconfig.EntitlementVideo:     "Deprecated alias for camera devices",
	appconfig.EntitlementGPU:       "Access GPU for AI or compute workloads",
	appconfig.EntitlementPersist:   "Persist data across restarts",
	appconfig.EntitlementAudio:     "Access audio input/output devices",
	appconfig.EntitlementCamera:    "Access camera devices (preferred)",
	appconfig.EntitlementUSB:       "Access USB peripherals",
	appconfig.EntitlementI2C:       "Access I2C bus devices",
	appconfig.EntitlementGPIO:      "Access GPIO pins",
	appconfig.EntitlementSPI:       "Access SPI bus devices (displays, sensors, flash - may require GPIO access)",
	appconfig.EntitlementInput:     "Access HID input devices (barcode scanners, keyboards)",
}

func displayEntitlementType(entType string) string {
	if entType == appconfig.EntitlementVideo {
		return entType + " (deprecated alias for camera)"
	}
	return entType
}

func selectableEntitlementTypes() []string {
	types := make([]string, 0, len(appconfig.ValidEntitlementTypes))
	for _, t := range appconfig.ValidEntitlementTypes {
		if t == appconfig.EntitlementVideo {
			continue
		}
		types = append(types, t)
	}
	return types
}

func newProjectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Manage Wendy project configuration",
	}

	cmd.AddCommand(newEntitlementsCmd())
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

func listAllEntitlementTypes(cmd *cobra.Command) error {
	types := appconfig.ValidEntitlementTypes

	if jsonOutput {
		data, err := json.Marshal(types)
		if err != nil {
			return err
		}
		cmd.Println(string(data))
		return nil
	}

	cmd.Println("Available entitlement types:")
	for _, t := range types {
		cmd.Printf("  %s\n", displayEntitlementType(t))
	}
	return nil
}

func listProjectEntitlements(cmd *cobra.Command) error {
	cfg, _, err := loadProjectConfig()
	if err != nil {
		return err
	}

	if jsonOutput {
		data, err := json.Marshal(cfg.Entitlements)
		if err != nil {
			return err
		}
		cmd.Println(string(data))
		return nil
	}

	if len(cfg.Entitlements) == 0 {
		cmd.Println("No entitlements configured.")
		return nil
	}

	cmd.Println("Project entitlements:")
	for _, e := range cfg.Entitlements {
		cmd.Printf("  %s\n", displayEntitlementType(e.Type))
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
				for _, t := range selectableEntitlementTypes() {
					if !existing[t] {
						items = append(items, tui.PickerItem{Name: t, Description: entitlementDescriptions[t], Value: t})
					}
				}
				if len(items) == 0 {
					fmt.Println("All entitlement types are already added.")
					return nil
				}

				selected, err := pickFromItems("Select an entitlement to add", items)
				if err != nil {
					return err
				}
				entType = selected
			}

			if !slices.Contains(appconfig.ValidEntitlementTypes, entType) {
				return fmt.Errorf("unknown entitlement type %q\nValid types: %s",
					entType, strings.Join(appconfig.ValidEntitlementTypes, ", "))
			}
			if entType == appconfig.EntitlementVideo {
				cmd.Printf("Warning: entitlement %q is deprecated; using %q instead.\n",
					appconfig.EntitlementVideo, appconfig.EntitlementCamera)
				entType = appconfig.EntitlementCamera
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

			fmt.Printf("Added %q entitlement\n", entType)
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
					fmt.Println("No entitlements configured.")
					return nil
				}

				var items []tui.PickerItem
				for _, e := range cfg.Entitlements {
					items = append(items, tui.PickerItem{Name: displayEntitlementType(e.Type), Description: entitlementDescriptions[e.Type], Value: e.Type})
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

			fmt.Printf("Removed %q entitlement\n", entType)
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
	picker := tui.NewPickerWithTitle(title)
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
