package commands

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// Questions for WendyOS devices.
var wendyOSEntitlementQuestions = []entitlementQuestion{
	{"Will your app run AI or GPU-accelerated workloads?", appconfig.EntitlementGPU, "GPU access for AI inference or compute"},
	{"Does your app need Bluetooth peripheral access?", appconfig.EntitlementBluetooth, "Bluetooth Low Energy peripherals"},
	{"Does your app need USB peripheral access?", appconfig.EntitlementUSB, "USB device access"},
	{"Does your app need GPIO pin access?", appconfig.EntitlementGPIO, "General-purpose I/O pins"},
	{"Does your app need I2C bus access?", appconfig.EntitlementI2C, "I2C bus devices"},
	{"Does your app need audio input/output?", appconfig.EntitlementAudio, "Microphone and speaker access"},
	{"Does your app need camera access?", appconfig.EntitlementCamera, "Camera device access"},
	{"Does your app need persistent storage?", appconfig.EntitlementPersist, "Data persisted across restarts"},
}

func newInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init [app-id]",
		Short: "Initialize a new Wendy project",
		Long:  "Interactively create a new Wendy project with scaffolding, entitlements, and optional AI assistant setup.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInitWizard(args)
		},
	}

	return cmd
}

func runInitWizard(args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	// Determine app ID.
	appID := filepath.Base(cwd)
	if len(args) > 0 {
		appID = args[0]
	}

	cfgPath := filepath.Join(cwd, "wendy.json")
	if _, err := os.Stat(cfgPath); err == nil {
		return fmt.Errorf("wendy.json already exists in %s", cwd)
	}

	reader := bufio.NewReader(os.Stdin)

	// Step 1: Pick target device.
	fmt.Println()
	target, err := pickFromItems("What is your target device?", []tui.PickerItem{
		{Name: "WendyOS", Description: "Full Linux-based edge device (Jetson, Raspberry Pi, ...)", Value: targetWendyOS},
		{Name: "Wendy Lite", Description: "Microcontroller running WASM (ESP32)", Value: targetWendyLite},
	})
	if err != nil {
		return err
	}

	// Step 2: Pick language (constrained by target).
	fmt.Println()
	language, err := pickInitLanguage(target)
	if err != nil {
		return err
	}

	// Step 3: Interactive entitlement questions.
	fmt.Println()
	entitlements, err := askEntitlementQuestions(reader, target, language)
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
	fmt.Println()
	if err := offerAIAssistant(reader, appID, target, language, entitlements); err != nil {
		return err
	}

	return nil
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
		return initSwiftProject(dir, appID, "")
	case language == langPython:
		return initPythonUVProject(dir, appID)
	default:
		return initDockerProject(dir, appID)
	}
}

// initPythonUVProject creates a uv-based Python project.
func initPythonUVProject(dir, appID string) error {
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
`, appID, appID, appID)

		if err := os.WriteFile(pyprojectPath, []byte(content), 0o644); err != nil {
			return fmt.Errorf("creating pyproject.toml: %w", err)
		}
	}

	// Create source package.
	srcDir := filepath.Join(dir, appID)
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
`, appID)

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

	if choice == "skip" {
		fmt.Println("\nYour project is ready! Run `wendy run` to build and deploy.")
		return nil
	}

	// For Claude, offer to install the Wendy skills plugin.
	if choice == "claude" {
		installWendySkills(reader)
	}

	prompt := buildAssistantPrompt(appID, target, language, entitlements)

	fmt.Printf("\nStarting %s with project context...\n", choice)

	cmd := exec.Command(choice, prompt)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

const wendySkillsMarketplace = "wendylabsinc/claude-skills"
const wendySkillsPluginName = "wendy@claude-skills"

// installWendySkills checks if the Wendy skills plugin is installed and offers
// to install it if missing. This gives Claude expert knowledge about Wendy
// development.
func installWendySkills(reader *bufio.Reader) {
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

	install, err := promptYesNo(reader, "Install Wendy skills for Claude Code?")
	if err != nil || !install {
		return
	}

	fmt.Println()

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

func initSwiftProject(dir, appID, template string) error {
	_ = template

	pkgPath := filepath.Join(dir, "Package.swift")
	if _, err := os.Stat(pkgPath); err == nil {
		return nil
	}

	content := fmt.Sprintf(`// swift-tools-version:6.0
import PackageDescription

let package = Package(
    name: "%s",
    targets: [
        .executableTarget(name: "%s"),
    ]
)
`, appID, appID)

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
