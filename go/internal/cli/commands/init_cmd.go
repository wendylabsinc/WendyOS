package commands

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
)

const (
	targetWendyOS   = "wendyos"
	targetWendyLite = "wendy-lite"

	langSwift  = "swift"
	langPython = "python"

	assistantClaude = "claude"
	assistantCodex  = "codex"
	assistantSkip   = "skip"
)

// Languages available per target platform.
var wendyOSLanguages = []struct {
	key         string
	name        string
	description string
}{
	{langSwift, "Swift", "Native Swift application (no container needed)"},
	{langPython, "Python", "Python application using uv (containerized)"},
}

var wendyLiteLanguages = []struct {
	key         string
	name        string
	description string
}{
	{langSwift, "Swift", "Swift compiled to WASM"},
}

// Entitlement questions asked during interactive setup.
// Each maps a user-facing question to an entitlement type.
type entitlementQuestion struct {
	question    string
	entitlement string
	description string
}

type initOptions struct {
	appID               string
	target              string
	language            string
	entitlements        []string
	noExtraEntitlements bool
	gpioPins            string
	i2cDevice           string
	persistName         string
	persistPath         string
	assistant           string
	installClaudeSkills bool

	appIDSet        bool
	targetSet       bool
	languageSet     bool
	entitlementsSet bool
	gpioPinsSet     bool
	i2cDeviceSet    bool
	persistNameSet  bool
	persistPathSet  bool
	assistantSet    bool
}

// Questions for WendyOS devices.
var wendyOSEntitlementQuestions = []entitlementQuestion{
	{"Will your app run AI or GPU-accelerated workloads?", appconfig.EntitlementGPU, "GPU access for AI inference or compute"},
	{"Does your app need Bluetooth peripheral access?", appconfig.EntitlementBluetooth, "Bluetooth Low Energy peripherals"},
	{"Does your app need USB peripheral access?", appconfig.EntitlementUSB, "USB device access"},
	{"Does your app need GPIO pin access?", appconfig.EntitlementGPIO, "General-purpose I/O pins"},
	{"Does your app need I2C bus access?", appconfig.EntitlementI2C, "I2C bus devices"},
	{"Does your app need audio input/output?", appconfig.EntitlementAudio, "Microphone and speaker access"},
	{"Does your app need camera access?", appconfig.EntitlementCamera, "Camera device access"},
	{"Does your app need HID input device access (barcode scanners, keyboards)?", appconfig.EntitlementInput, "HID input devices"},
	{"Does your app need persistent storage?", appconfig.EntitlementPersist, "Data persisted across restarts"},
}

func newInitCmd() *cobra.Command {
	var opts initOptions

	cmd := &cobra.Command{
		Use:   "init [app-id]",
		Short: "Initialize a new Wendy project",
		Long:  "Interactively create a new Wendy project with scaffolding, entitlements, and optional AI assistant setup.",
		Example: `  # Interactive wizard
  wendy init

  # Fully non-interactive WendyOS Python app with persist storage
  wendy init \
    --app-id demo-app \
    --target wendyos \
    --language python \
    --entitlement gpu,usb,persist \
    --persist-name demo-data \
    --persist-path /data \
    --assistant skip

  # Fully non-interactive WendyOS app with GPIO and I2C entitlements
  wendy init \
    --app-id edge-sensors \
    --target wendyos \
    --language swift \
    --entitlement gpio,i2c \
    --gpio-pins 17,27,22 \
    --i2c-device /dev/i2c-1 \
    --assistant skip

  # Wendy Lite defaults to Swift; use this to avoid entitlement prompts
  wendy init \
    --app-id lite-app \
    --target wendy-lite \
    --no-extra-entitlements \
    --assistant skip

  # Start Claude after init and install Wendy skills automatically
  wendy init \
    --app-id ai-app \
    --target wendyos \
    --language python \
    --entitlement gpu,audio \
    --assistant claude \
    --install-claude-skills`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.appIDSet = cmd.Flags().Changed("app-id")
			opts.targetSet = cmd.Flags().Changed("target")
			opts.languageSet = cmd.Flags().Changed("language")
			opts.entitlementsSet = cmd.Flags().Changed("entitlement")
			opts.gpioPinsSet = cmd.Flags().Changed("gpio-pins")
			opts.i2cDeviceSet = cmd.Flags().Changed("i2c-device")
			opts.persistNameSet = cmd.Flags().Changed("persist-name")
			opts.persistPathSet = cmd.Flags().Changed("persist-path")
			opts.assistantSet = cmd.Flags().Changed("assistant")

			return runInitWizard(args, opts)
		},
	}

	cmd.Flags().StringVar(&opts.appID, "app-id", "", "Application ID to write into wendy.json")
	cmd.Flags().StringVar(&opts.target, "target", "", "Target platform: wendyos or wendy-lite")
	cmd.Flags().StringVar(&opts.language, "language", "", "Project language: swift or python")
	cmd.Flags().StringSliceVar(&opts.entitlements, "entitlement", nil, "App entitlement to enable (repeatable or comma-separated)")
	cmd.Flags().BoolVar(&opts.noExtraEntitlements, "no-extra-entitlements", false, "Skip entitlement prompts and use only the default network entitlement")
	cmd.Flags().StringVar(&opts.gpioPins, "gpio-pins", "", "GPIO pins for the gpio entitlement (comma-separated, e.g. 17,27,22)")
	cmd.Flags().StringVar(&opts.i2cDevice, "i2c-device", "", "I2C device path for the i2c entitlement (e.g. /dev/i2c-1)")
	cmd.Flags().StringVar(&opts.persistName, "persist-name", "", "Container ID for the persist entitlement")
	cmd.Flags().StringVar(&opts.persistPath, "persist-path", "", "Mount path for the persist entitlement (e.g. /data)")
	cmd.Flags().StringVar(&opts.assistant, "assistant", "", "AI assistant to launch after init: claude, codex, or skip")
	cmd.Flags().BoolVar(&opts.installClaudeSkills, "install-claude-skills", false, "Install Wendy Claude skills before launching Claude")

	return cmd
}

func runInitWizard(args []string, opts initOptions) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	// Determine app ID.
	appID, err := resolveInitAppID(cwd, args, opts)
	if err != nil {
		return err
	}

	cfgPath := filepath.Join(cwd, "wendy.json")
	if _, err := os.Stat(cfgPath); err == nil {
		return fmt.Errorf("wendy.json already exists in %s", cwd)
	}

	if err := validateInitAssistantOptions(opts); err != nil {
		return err
	}

	reader := bufio.NewReader(os.Stdin)

	// Step 1: Pick target device.
	target, err := resolveInitTarget(opts)
	if err != nil {
		return err
	}

	// Step 2: Pick language (constrained by target).
	language, err := resolveInitLanguage(target, opts)
	if err != nil {
		return err
	}

	// Step 3: Interactive entitlement questions.
	entitlements, err := resolveInitEntitlements(reader, target, language, opts)
	if err != nil {
		return err
	}

	// Step 4: Generate wendy.json.
	platform := appconfig.PlatformWendyOS
	if target == targetWendyLite {
		platform = appconfig.PlatformWendyLite
	}

	cfg := appconfig.AppConfig{
		AppID:        appID,
		Version:      "0.1.0",
		Platform:     platform,
		Language:     language,
		Entitlements: entitlements,
	}

	if language == langPython {
		cfg.Python = &appconfig.PythonConfig{}
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	data = append(data, '\n')

	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		return fmt.Errorf("writing wendy.json: %w", err)
	}

	fmt.Printf("\nCreated wendy.json for %s\n", appID)

	// Step 5: Scaffold project files.
	if err := scaffoldProject(cwd, appID, target, language); err != nil {
		return err
	}

	// Step 6: Offer AI assistant session.
	if err := resolveInitAssistant(reader, appID, target, language, entitlements, opts); err != nil {
		return err
	}

	return nil
}

func resolveInitAppID(cwd string, args []string, opts initOptions) (string, error) {
	rawAppID := filepath.Base(cwd)
	if len(args) > 0 {
		rawAppID = args[0]
	}

	if opts.appIDSet {
		flagAppID := strings.TrimSpace(opts.appID)
		if flagAppID == "" {
			return "", fmt.Errorf("app ID cannot be empty or whitespace")
		}

		if len(args) > 0 {
			positionalAppID := strings.TrimSpace(args[0])
			if positionalAppID == "" {
				return "", fmt.Errorf("app ID positional argument cannot be empty or whitespace")
			}
			if positionalAppID != flagAppID {
				return "", fmt.Errorf("app ID mismatch: positional argument %q does not match --app-id %q", args[0], opts.appID)
			}
		}

		return flagAppID, nil
	}

	appID := strings.TrimSpace(rawAppID)
	if appID == "" {
		return "", fmt.Errorf("could not infer a valid app ID; please provide a non-empty value via --app-id or as a positional argument")
	}

	return appID, nil
}

func resolveInitTarget(opts initOptions) (string, error) {
	if opts.targetSet {
		target := normalizeInitChoice(opts.target)
		if !isValidInitTarget(target) {
			return "", fmt.Errorf("invalid target %q (valid: %s, %s)", opts.target, targetWendyOS, targetWendyLite)
		}
		return target, nil
	}

	fmt.Println()
	return pickFromItems("What is your target device?", []tui.PickerItem{
		{Name: "WendyOS", Description: "Full Linux-based edge device (Jetson, Raspberry Pi, ...)", Value: targetWendyOS},
		{Name: "Wendy Lite", Description: "Microcontroller running WASM (ESP32)", Value: targetWendyLite},
	})
}

func resolveInitLanguage(target string, opts initOptions) (string, error) {
	if opts.languageSet {
		language := normalizeInitChoice(opts.language)
		if !isValidInitLanguage(language) {
			return "", fmt.Errorf("invalid language %q (valid: %s, %s)", opts.language, langSwift, langPython)
		}
		if err := validateInitLanguage(target, language); err != nil {
			return "", err
		}
		return language, nil
	}

	fmt.Println()
	return pickInitLanguage(target)
}

func resolveInitEntitlements(reader *bufio.Reader, target, language string, opts initOptions) ([]appconfig.Entitlement, error) {
	if initEntitlementsProvided(opts) {
		return buildInitEntitlementsFromFlags(target, opts)
	}

	fmt.Println()
	return askEntitlementQuestions(reader, target, language)
}

func resolveInitAssistant(reader *bufio.Reader, appID, target, language string, entitlements []appconfig.Entitlement, opts initOptions) error {
	if opts.assistantSet {
		choice := normalizeInitChoice(opts.assistant)
		return runAIAssistantChoice(choice, appID, target, language, entitlements, opts.installClaudeSkills, nil)
	}

	fmt.Println()
	return offerAIAssistant(reader, appID, target, language, entitlements)
}

func pickInitLanguage(target string) (string, error) {
	switch target {
	case targetWendyLite:
		// Only WASM-capable languages (currently just Swift).
		fmt.Println("Wendy Lite requires a WASM-compatible language.")
		return langSwift, nil

	default:
		var items []tui.PickerItem
		for _, l := range wendyOSLanguages {
			items = append(items, tui.PickerItem{
				Name:        l.name,
				Description: l.description,
				Value:       l.key,
			})
		}
		return pickFromItems("What language will you use?", items)
	}
}

func askEntitlementQuestions(reader *bufio.Reader, target, language string) ([]appconfig.Entitlement, error) {
	// Always include network.
	entitlements := []appconfig.Entitlement{
		{Type: appconfig.EntitlementNetwork},
	}

	if target == targetWendyLite {
		// Wendy Lite has limited entitlements; skip interactive questions.
		fmt.Println("Wendy Lite apps have network access by default.")
		return entitlements, nil
	}

	fmt.Println("Let's figure out what your app needs access to.")
	fmt.Println("Answer y/n for each capability:")
	fmt.Println()

	for _, q := range wendyOSEntitlementQuestions {
		answer, err := promptYesNo(reader, q.question)
		if err != nil {
			return nil, err
		}

		if !answer {
			continue
		}

		ent := appconfig.Entitlement{Type: q.entitlement}

		// Prompt for required fields on certain entitlement types.
		if err := promptEntitlementFields(&ent); err != nil {
			return nil, err
		}

		entitlements = append(entitlements, ent)
	}

	return entitlements, nil
}

func initEntitlementsProvided(opts initOptions) bool {
	return opts.entitlementsSet || opts.noExtraEntitlements || opts.gpioPinsSet || opts.i2cDeviceSet || opts.persistNameSet || opts.persistPathSet
}

func buildInitEntitlementsFromFlags(target string, opts initOptions) ([]appconfig.Entitlement, error) {
	entitlements := []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork}}
	seen := map[string]bool{appconfig.EntitlementNetwork: true}

	if opts.noExtraEntitlements {
		if opts.entitlementsSet || opts.gpioPinsSet || opts.i2cDeviceSet || opts.persistNameSet || opts.persistPathSet {
			return nil, fmt.Errorf("--no-extra-entitlements cannot be combined with entitlement-specific flags")
		}
		return entitlements, nil
	}

	rawTypes := make([]string, 0, len(opts.entitlements)+3)
	parsedEntitlementFlag := false
	for _, rawType := range opts.entitlements {
		entType := normalizeInitChoice(rawType)
		if entType == "" {
			continue
		}
		parsedEntitlementFlag = true
		rawTypes = append(rawTypes, entType)
	}
	if opts.entitlementsSet && !parsedEntitlementFlag {
		return nil, fmt.Errorf("--entitlement requires at least one valid entitlement type")
	}

	if opts.gpioPinsSet {
		rawTypes = append(rawTypes, appconfig.EntitlementGPIO)
	}
	if opts.i2cDeviceSet {
		rawTypes = append(rawTypes, appconfig.EntitlementI2C)
	}
	if opts.persistNameSet || opts.persistPathSet {
		rawTypes = append(rawTypes, appconfig.EntitlementPersist)
	}

	for _, rawType := range rawTypes {
		entType := normalizeInitChoice(rawType)
		if !slices.Contains(appconfig.ValidEntitlementTypes, entType) {
			return nil, fmt.Errorf("invalid entitlement %q", rawType)
		}
		if target == targetWendyLite && entType != appconfig.EntitlementNetwork {
			return nil, fmt.Errorf("%s does not support the %q entitlement", targetWendyLite, entType)
		}
		if seen[entType] {
			continue
		}

		ent := appconfig.Entitlement{Type: entType}
		switch entType {
		case appconfig.EntitlementPersist:
			if strings.TrimSpace(opts.persistName) == "" || strings.TrimSpace(opts.persistPath) == "" {
				return nil, fmt.Errorf("persist entitlement requires both --persist-name and --persist-path")
			}
			ent.Name = strings.TrimSpace(opts.persistName)
			ent.Path = strings.TrimSpace(opts.persistPath)
		case appconfig.EntitlementI2C:
			if strings.TrimSpace(opts.i2cDevice) == "" {
				return nil, fmt.Errorf("i2c entitlement requires --i2c-device")
			}
			ent.Device = strings.TrimSpace(opts.i2cDevice)
		case appconfig.EntitlementGPIO:
			if strings.TrimSpace(opts.gpioPins) == "" {
				return nil, fmt.Errorf("gpio entitlement requires --gpio-pins")
			}
			pins, err := parsePins(opts.gpioPins)
			if err != nil {
				return nil, err
			}
			ent.Pins = pins
		}

		entitlements = append(entitlements, ent)
		seen[entType] = true
	}

	return entitlements, nil
}

func normalizeInitChoice(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func isValidInitTarget(target string) bool {
	return target == targetWendyOS || target == targetWendyLite
}

func isValidInitLanguage(language string) bool {
	return language == langSwift || language == langPython
}

func validateInitLanguage(target, language string) error {
	if target == targetWendyLite && language != langSwift {
		return fmt.Errorf("%s requires %s", targetWendyLite, langSwift)
	}
	return nil
}

func isValidInitAssistant(choice string) bool {
	return choice == assistantClaude || choice == assistantCodex || choice == assistantSkip
}

func validateInitAssistantOptions(opts initOptions) error {
	if opts.installClaudeSkills && (!opts.assistantSet || normalizeInitChoice(opts.assistant) != assistantClaude) {
		return fmt.Errorf("--install-claude-skills requires --assistant=%s", assistantClaude)
	}
	if !opts.assistantSet {
		return nil
	}

	choice := normalizeInitChoice(opts.assistant)
	if !isValidInitAssistant(choice) {
		return fmt.Errorf("invalid assistant %q (valid: %s, %s, %s)", opts.assistant, assistantClaude, assistantCodex, assistantSkip)
	}
	if choice == assistantSkip {
		return nil
	}
	if !isCommandAvailable(choice) {
		return fmt.Errorf("%s is not installed or not on PATH", choice)
	}
	return nil
}

func promptYesNo(reader *bufio.Reader, question string) (bool, error) {
	fmt.Printf("  %s [y/N] ", question)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, err
	}
	answer := strings.TrimSpace(strings.ToLower(line))
	return answer == "y" || answer == "yes", nil
}

func scaffoldProject(dir, appID, target, language string) error {
	switch {
	case language == langSwift:
		return initSwiftProject(dir, appID, target)
	case language == langPython:
		return initPythonUVProject(dir, appID)
	default:
		return initDockerProject(dir, appID)
	}
}

// pythonPackageName converts an app ID to a valid Python package name
// by replacing hyphens and dots with underscores.
func pythonPackageName(appID string) string {
	r := strings.NewReplacer("-", "_", ".", "_", " ", "_")
	return r.Replace(appID)
}

// initPythonUVProject creates a uv-based Python project.
func initPythonUVProject(dir, appID string) error {
	pkgName := pythonPackageName(appID)

	// Create pyproject.toml for uv.
	pyprojectPath := filepath.Join(dir, "pyproject.toml")
	if _, err := os.Stat(pyprojectPath); os.IsNotExist(err) {
		content := fmt.Sprintf(`[project]
name = "%s"
version = "0.1.0"
requires-python = ">=3.11"
dependencies = []

[project.scripts]
%s = "%s:main"
`, appID, pkgName, pkgName)

		if err := os.WriteFile(pyprojectPath, []byte(content), 0o644); err != nil {
			return fmt.Errorf("creating pyproject.toml: %w", err)
		}
	}

	// Create source package.
	srcDir := filepath.Join(dir, pkgName)
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return fmt.Errorf("creating source directory: %w", err)
	}

	initPath := filepath.Join(srcDir, "__init__.py")
	if _, err := os.Stat(initPath); os.IsNotExist(err) {
		content := fmt.Sprintf(`"""
%s - A Wendy Edge Application
"""

import signal
import sys


def _signal_handler(sig, frame):
    print("Shutting down gracefully...")
    sys.exit(0)


def main():
    signal.signal(signal.SIGINT, _signal_handler)
    signal.signal(signal.SIGTERM, _signal_handler)

    print("Hello from %s!")


if __name__ == "__main__":
    main()
`, appID, appID)

		if err := os.WriteFile(initPath, []byte(content), 0o644); err != nil {
			return fmt.Errorf("creating __init__.py: %w", err)
		}
	}

	// Create Dockerfile using uv.
	dockerPath := filepath.Join(dir, "Dockerfile")
	if _, err := os.Stat(dockerPath); os.IsNotExist(err) {
		content := fmt.Sprintf(`FROM ghcr.io/astral-sh/uv:python3.12-bookworm-slim

WORKDIR /app

# Install dependencies first for better caching
COPY pyproject.toml uv.lock* ./
RUN uv sync --frozen --no-install-project

# Copy application code
COPY . .
RUN uv sync --frozen

CMD ["uv", "run", "%s"]
`, pkgName)

		if err := os.WriteFile(dockerPath, []byte(content), 0o644); err != nil {
			return fmt.Errorf("creating Dockerfile: %w", err)
		}
	}

	fmt.Println("Created pyproject.toml, source package, and Dockerfile (using uv)")
	return nil
}

func offerAIAssistant(reader *bufio.Reader, appID, target, language string, entitlements []appconfig.Entitlement) error {
	// Check which AI assistants are available.
	hasClaude := isCommandAvailable("claude")
	hasCodex := isCommandAvailable("codex")

	if !hasClaude && !hasCodex {
		return nil
	}

	var assistants []tui.PickerItem
	if hasClaude {
		assistants = append(assistants, tui.PickerItem{
			Name:        "Claude Code",
			Description: "Start an interactive Claude session for your project",
			Value:       "claude",
		})
	}
	if hasCodex {
		assistants = append(assistants, tui.PickerItem{
			Name:        "Codex",
			Description: "Start an interactive Codex session for your project",
			Value:       "codex",
		})
	}
	assistants = append(assistants, tui.PickerItem{
		Name:        "Skip",
		Description: "I'll set things up myself",
		Value:       "skip",
	})

	choice, err := pickFromItems("Would you like to start an AI coding assistant?", assistants)
	if err != nil {
		return err
	}

	return runAIAssistantChoice(choice, appID, target, language, entitlements, false, reader)
}

const wendySkillsMarketplace = "wendylabsinc/claude-skills"
const wendySkillsPluginName = "wendy@claude-skills"

// installWendySkills checks if the Wendy skills plugin is installed and offers
// to install it if missing. This gives Claude expert knowledge about Wendy
// development.
func installWendySkills(reader *bufio.Reader, autoInstall bool) {
	// Check if the plugin is already installed by looking at the plugin list output.
	out, err := exec.Command("claude", "plugin", "list").Output()
	if err != nil {
		return
	}

	if strings.Contains(string(out), "wendy@claude-skills") {
		return
	}

	fmt.Println("\nThe Wendy skills plugin gives Claude expert knowledge about")
	fmt.Println("building and deploying apps to WendyOS and Wendy Lite devices.")
	fmt.Println()

	if !autoInstall {
		install, err := promptYesNo(reader, "Install Wendy skills for Claude Code?")
		if err != nil || !install {
			return
		}

		fmt.Println()
	}

	// Add the marketplace if not already present.
	addMarketplace := exec.Command("claude", "plugin", "marketplace", "add", wendySkillsMarketplace)
	addMarketplace.Stdout = os.Stdout
	addMarketplace.Stderr = os.Stderr
	if err := addMarketplace.Run(); err != nil {
		fmt.Printf("  Could not add marketplace: %v\n", err)
		fmt.Println("  You can install manually: claude plugin marketplace add " + wendySkillsMarketplace)
		return
	}

	// Install the plugin.
	installCmd := exec.Command("claude", "plugin", "install", wendySkillsPluginName)
	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stderr
	if err := installCmd.Run(); err != nil {
		fmt.Printf("  Could not install plugin: %v\n", err)
		fmt.Println("  You can install manually: claude plugin install " + wendySkillsPluginName)
		return
	}

	fmt.Println("  Wendy skills installed successfully!")
}

func runAIAssistantChoice(choice, appID, target, language string, entitlements []appconfig.Entitlement, installClaudeSkills bool, reader *bufio.Reader) error {
	if choice == assistantSkip {
		fmt.Println("\nYour project is ready! Run `wendy run` to build and deploy.")
		return nil
	}

	if !isCommandAvailable(choice) {
		return fmt.Errorf("%s is not installed or not on PATH", choice)
	}

	if choice == assistantClaude {
		switch {
		case installClaudeSkills:
			installWendySkills(nil, true)
		case reader != nil:
			installWendySkills(reader, false)
		}
	}

	prompt := buildAssistantPrompt(appID, target, language, entitlements)

	fmt.Printf("\nStarting %s with project context...\n", choice)

	cmd := exec.Command(choice, prompt)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func buildAssistantPrompt(appID, target, language string, entitlements []appconfig.Entitlement) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("I just initialized a Wendy edge computing project called %q.\n", appID))

	if target == targetWendyLite {
		sb.WriteString("It targets Wendy Lite (ESP32 microcontroller running WASM).\n")
	} else {
		sb.WriteString("It targets WendyOS (a Linux-based edge device like NVIDIA Jetson or Raspberry Pi).\n")
	}

	sb.WriteString(fmt.Sprintf("The language is %s.\n", language))

	if len(entitlements) > 0 {
		sb.WriteString("The app has these entitlements: ")
		var types []string
		for _, e := range entitlements {
			types = append(types, e.Type)
		}
		sb.WriteString(strings.Join(types, ", "))
		sb.WriteString(".\n")
	}

	sb.WriteString("\nHelp me build out this project. Start by examining the generated files, then suggest next steps.")

	return sb.String()
}

func isCommandAvailable(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

// defaultEntitlements returns sensible default entitlements based on language and template.
// Used by helpers.go when auto-generating a wendy.json during build.
func defaultEntitlements(language, template string) []appconfig.Entitlement {
	entitlements := []appconfig.Entitlement{
		{Type: appconfig.EntitlementNetwork},
	}

	switch template {
	case "voice-assistant":
		entitlements = append(entitlements,
			appconfig.Entitlement{Type: appconfig.EntitlementAudio},
			appconfig.Entitlement{Type: appconfig.EntitlementGPU},
			appconfig.Entitlement{Type: appconfig.EntitlementBluetooth},
		)
	case "speech-to-text":
		entitlements = append(entitlements,
			appconfig.Entitlement{Type: appconfig.EntitlementAudio},
			appconfig.Entitlement{Type: appconfig.EntitlementGPU},
		)
	default:
		if language == "python" {
			entitlements = append(entitlements,
				appconfig.Entitlement{Type: appconfig.EntitlementGPU},
			)
		}
	}

	return entitlements
}

// --- Legacy scaffolding helpers (kept for non-interactive / Swift / Docker use) ---

func initSwiftProject(dir, appID, target string) error {
	pkgPath := filepath.Join(dir, "Package.swift")
	if _, err := os.Stat(pkgPath); err == nil {
		return nil
	}

	var content string
	if target == "wendy-lite" {
		content = fmt.Sprintf(`// swift-tools-version:6.2
import PackageDescription

let package = Package(
    name: "%s",
    dependencies: [
        .package(url: "https://github.com/wendylabsinc/wendy-lite", branch: "main"),
    ],
    targets: [
        .executableTarget(
            name: "%s",
            dependencies: [
                .product(name: "WendyLite", package: "wendy-lite"),
            ]
        ),
    ]
)
`, appID, appID)
	} else {
		content = fmt.Sprintf(`// swift-tools-version:6.2
import PackageDescription

let package = Package(
    name: "%s",
    targets: [
        .executableTarget(name: "%s"),
    ]
)
`, appID, appID)
	}

	if err := os.WriteFile(pkgPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("creating Package.swift: %w", err)
	}

	srcDir := filepath.Join(dir, "Sources", appID)
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return fmt.Errorf("creating source directory: %w", err)
	}

	mainContent := fmt.Sprintf("print(\"Hello from %s!\")\n", appID)
	if err := os.WriteFile(filepath.Join(srcDir, "main.swift"), []byte(mainContent), 0o644); err != nil {
		return fmt.Errorf("creating main.swift: %w", err)
	}

	fmt.Println("Created Package.swift and source files")
	return nil
}

func initDockerProject(dir, appID string) error {
	dockerPath := filepath.Join(dir, "Dockerfile")
	if _, err := os.Stat(dockerPath); err == nil {
		return nil
	}

	content := fmt.Sprintf(`FROM ubuntu:22.04
WORKDIR /app
# Add your application here
CMD ["echo", "Hello from %s!"]
`, appID)

	if err := os.WriteFile(dockerPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("creating Dockerfile: %w", err)
	}

	fmt.Println("Created Dockerfile")
	return nil
}
