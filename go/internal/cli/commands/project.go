package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

var entitlementDescriptions = map[string]string{
	appconfig.EntitlementNetwork:       "Access network interfaces",
	appconfig.EntitlementBluetooth:     "Access Bluetooth peripherals",
	appconfig.EntitlementVideo:         "Deprecated: use camera instead",
	appconfig.EntitlementGPU:           "Access GPU for AI or compute workloads",
	appconfig.EntitlementNPU:           "Access the NPU for on-device AI inference",
	appconfig.EntitlementPersist:       "Persist data across restarts",
	appconfig.EntitlementAudio:         "Access audio input/output devices",
	appconfig.EntitlementCamera:        "Access camera devices",
	appconfig.EntitlementUSB:           "Access USB peripherals",
	appconfig.EntitlementI2C:           "Access I2C bus devices",
	appconfig.EntitlementGPIO:          "Access GPIO pins",
	appconfig.EntitlementSPI:           "Access SPI bus devices (displays, sensors, flash - may require GPIO access)",
	appconfig.EntitlementInput:         "Access Linux input devices (game controllers, barcode scanners, keyboards)",
	appconfig.EntitlementSerial:        "Access a USB serial device",
	appconfig.EntitlementMCP:           "Declare the app's MCP server port",
	appconfig.EntitlementHTTP:          "Declare the app's web interface port",
	appconfig.EntitlementDisplay:       "Access the device display",
	appconfig.EntitlementEpisodeWrite:  "Write application events to episode recordings",
	appconfig.EntitlementNotifications: "Send app notifications",
	appconfig.EntitlementAdmin:         "Full local control of the device agent",
	appconfig.EntitlementBuild:         "Grant privileges for nested container builds",
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
		Use:     "project",
		Short:   "View and edit your project manifest",
		Long:    "View and edit wendy.json. Run without a subcommand for a guided editor.\nUse direct commands and flags in scripts; --json never prompts.",
		Example: "  wendy project\n  wendy project add http --port 8080\n  wendy project edit ros2 --domain-id 42\n  wendy project show --json\n  wendy project validate",
		Args:    cobra.NoArgs,
		RunE:    runProjectHome,
	}

	cmd.PersistentFlags().String("file", "wendy.json", "Manifest file or project directory")
	cmd.AddCommand(newProjectShowCmd(), newProjectValidateCmd())
	cmd.AddCommand(newProjectChangeCmd("add", ""), newProjectChangeCmd("edit", ""), newProjectChangeCmd("remove", ""))
	cmd.AddCommand(newEntitlementsCmd())
	cmd.AddCommand(newFrameworksCmd())
	optimizeCmd := newOptimizeCmd()
	optimizeCmd.PreRunE = func(cmd *cobra.Command, _ []string) error {
		if cmd.Flags().Changed("file") {
			return fmt.Errorf("project optimize uses the current working directory; change into the project directory instead of passing --file")
		}
		return nil
	}
	cmd.AddCommand(optimizeCmd)
	return cmd
}

func newEntitlementsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "entitlements",
		Short:  "Manage project entitlements",
		Hidden: true,
	}

	cmd.AddCommand(
		newEntitlementsListCmd(),
		newEntitlementsAddCmd(),
		newEntitlementsRemoveCmd(),
		newProjectChangeCmd("edit", "entitlements"),
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
		fmt.Fprintf(out, "  %s  %s\n", t, entitlementDescriptions[t])
	}
	return nil
}

func listProjectEntitlements(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	doc, err := commandProjectManifest(cmd)
	if err != nil {
		return err
	}
	if err := doc.requireExisting(); err != nil {
		return err
	}
	cfg, _, err := loadProjectConfigAt(doc.path)
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
	return newProjectChangeCmd("add", "entitlements")
}

func newEntitlementsRemoveCmd() *cobra.Command {
	return newProjectChangeCmd("remove", "entitlements")
}

func newFrameworksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:    "frameworks",
		Short:  "Manage project framework configuration (e.g. ROS 2)",
		Hidden: true,
	}

	cmd.AddCommand(
		newFrameworksListCmd(),
		newFrameworksAddCmd(),
		newFrameworksRemoveCmd(),
		newProjectChangeCmd("edit", "frameworks"),
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
	doc, err := commandProjectManifest(cmd)
	if err != nil {
		return err
	}
	if err := doc.requireExisting(); err != nil {
		return err
	}
	cfg, _, err := loadProjectConfigAt(doc.path)
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
	return newProjectChangeCmd("add", "frameworks")
}

func newFrameworksRemoveCmd() *cobra.Command {
	return newProjectChangeCmd("remove", "frameworks")
}

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
	return loadProjectConfigAt("")
}

func loadProjectConfigAt(path string) (*appconfig.AppConfig, string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, "", fmt.Errorf("getting working directory: %w", err)
	}

	cfgPath := path
	if cfgPath == "" {
		cfgPath = filepath.Join(cwd, "wendy.json")
	}
	cfg, err := appconfig.LoadFromFile(cfgPath)
	if err != nil {
		return nil, "", fmt.Errorf("loading wendy.json: %w", err)
	}

	return cfg, cfgPath, nil
}
