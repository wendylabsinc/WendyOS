package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

const (
	targetWendyOS   = "wendyos"
	targetWendyLite = "wendy-lite"
	targetDarwin    = "darwin"

	langSwift  = "swift"
	langPython = "python"
	langRust   = "rust"
	langNode   = "node"
	langCpp    = "cpp"
	langMojo   = "mojo"

	assistantClaude = "claude"
	assistantCodex  = "codex"
	assistantSkip   = "skip"

	// bareTemplatePickSentinel is the value rewriteBareTemplateFlag injects for
	// a bare --template so cobra can parse it; the wizard treats it as "show
	// the template picker".
	bareTemplatePickSentinel = "_pick"
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

var darwinLanguages = []struct {
	key         string
	name        string
	description string
}{
	{langSwift, "Swift", "Native macOS application for Wendy Agent for Mac"},
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
	here                bool
	target              string
	language            string
	template            string
	branch              string
	vars                []string
	gitInit             string
	entitlements        []string
	allEntitlements     bool
	noExtraEntitlements bool
	gpioPins            string
	i2cDevice           string
	persistName         string
	persistPath         string
	assistant           string
	installClaudeSkills bool
	frameworks          []string
	ros2DomainID        int
	ros2RMW             string
	ros2Distro          string
	ros2DiscoveryScope  string

	appIDSet              bool
	targetSet             bool
	languageSet           bool
	templateSet           bool
	gitInitSet            bool
	entitlementsSet       bool
	gpioPinsSet           bool
	i2cDeviceSet          bool
	persistNameSet        bool
	persistPathSet        bool
	assistantSet          bool
	frameworksSet         bool
	ros2DomainIDSet       bool
	ros2RMWSet            bool
	ros2DistroSet         bool
	ros2DiscoveryScopeSet bool
}

// Questions for WendyOS devices.
var wendyOSEntitlementQuestions = []entitlementQuestion{
	{"Will your app run AI or GPU-accelerated workloads?", appconfig.EntitlementGPU, "GPU access for AI inference or compute"},
	{"Will your app run inference on the NPU?", appconfig.EntitlementNPU, "NPU access for on-device inference"},
	{"Does your app need Bluetooth peripheral access?", appconfig.EntitlementBluetooth, "Bluetooth Low Energy peripherals"},
	{"Does your app need USB peripheral access?", appconfig.EntitlementUSB, "USB device access"},
	{"Does your app need GPIO pin access?", appconfig.EntitlementGPIO, "General-purpose I/O pins"},
	{"Does your app need SPI bus access (displays, sensors, flash)?", appconfig.EntitlementSPI, "SPI bus access (may require GPIO access)"},
	{"Does your app need I2C bus access?", appconfig.EntitlementI2C, "I2C bus devices"},
	{"Does your app need audio input/output?", appconfig.EntitlementAudio, "Microphone and speaker access"},
	{"Does your app need camera access?", appconfig.EntitlementCamera, "Camera device access"},
	{"Does your app need Linux input device access (barcode scanners, keyboards, controllers)?", appconfig.EntitlementInput, "Linux input devices"},
	{"Does your app need persistent storage?", appconfig.EntitlementPersist, "Data persisted across restarts"},
}

func newInitCmd() *cobra.Command {
	var opts initOptions

	cmd := &cobra.Command{
		Use:   "init [app-id]",
		Short: "Initialize a new Wendy project",
		Long: "Interactively create a new Wendy project with scaffolding, entitlements, and optional AI assistant setup.\n\n" +
			"An [app-id] argument (or --app-id) creates a new subdirectory of that name by default; pass --here to " +
			"scaffold into the current directory instead (with no app ID, --here infers one from the current " +
			"directory's name).\n\n" +
			"wendy.json also supports a separate, top-level \"frameworks\" key for framework-level config " +
			"(currently ROS 2: domain ID, RMW middleware, discovery scope). Enable it with --framework " +
			"(see the ROS 2 example below), through the interactive prompt on a WendyOS target, or by hand-editing " +
			"\"frameworks\" in wendy.json afterwards.",
		Example: `  # Interactive wizard
  wendy init

  # Scaffold from a template (interactive language picker)
  wendy init --template simple-api

  # Create a project from a template; target and language are inferred
  # from the template's metadata when it supports exactly one of each
  wendy init go2-app --template go2-rc

  # Non-interactive template scaffold with variable overrides
  wendy init --app-id my-api --template simple-api --language rust --var PORT=8080

  # Use a template from a specific branch of the templates repo
  wendy init --template simple-api --branch feature/new-template

  # Scaffold into the current (already-existing, empty) directory
  wendy init --here my-api --template simple-api

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

  # Native macOS app for Wendy Agent for Mac
  wendy init \
    --app-id mac-llm \
    --target darwin \
    --language swift \
    --template mac-llm \
    --assistant skip

  # Enable all entitlements at once
  wendy init \
    --app-id full-app \
    --target wendyos \
    --language python \
    --all-entitlements \
    --gpio-pins 17,27,22 \
    --i2c-device /dev/i2c-1 \
    --persist-name full-data \
    --persist-path /data \
    --assistant skip

  # Start Claude after init and install Wendy skills automatically
  wendy init \
    --app-id ai-app \
    --target wendyos \
    --language python \
    --entitlement gpu,audio \
    --assistant claude \
    --install-claude-skills

  # ROS 2 app: enable the "frameworks" key with the ros2 framework
  wendy init \
    --app-id go2-network-bridge \
    --target wendyos \
    --language swift \
    --framework ros2 \
    --ros2-rmw cyclonedds \
    --ros2-discovery-scope host \
    --assistant skip`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			opts.appIDSet = cmd.Flags().Changed("app-id")
			opts.targetSet = cmd.Flags().Changed("target")
			opts.languageSet = cmd.Flags().Changed("language")
			opts.templateSet = cmd.Flags().Changed("template")
			opts.gitInitSet = cmd.Flags().Changed("git-init")
			opts.entitlementsSet = cmd.Flags().Changed("entitlement")
			opts.gpioPinsSet = cmd.Flags().Changed("gpio-pins")
			opts.i2cDeviceSet = cmd.Flags().Changed("i2c-device")
			opts.persistNameSet = cmd.Flags().Changed("persist-name")
			opts.persistPathSet = cmd.Flags().Changed("persist-path")
			opts.assistantSet = cmd.Flags().Changed("assistant")
			opts.frameworksSet = cmd.Flags().Changed("framework")
			opts.ros2DomainIDSet = cmd.Flags().Changed("ros2-domain-id")
			opts.ros2RMWSet = cmd.Flags().Changed("ros2-rmw")
			opts.ros2DistroSet = cmd.Flags().Changed("ros2-distro")
			opts.ros2DiscoveryScopeSet = cmd.Flags().Changed("ros2-discovery-scope")

			err := runInitWizard(args, opts)
			if errors.Is(err, tui.ErrCancelled) {
				return ErrUserCancelled
			}
			return err
		},
	}

	cmd.Flags().StringVar(&opts.appID, "app-id", "", "Application ID to write into wendy.json")
	cmd.Flags().BoolVar(&opts.here, "here", false, "Scaffold into the current directory instead of creating a subdirectory")
	cmd.Flags().StringVar(&opts.target, "target", "", "Target platform: wendyos (writes \"linux\" to wendy.json), wendy-lite, or darwin")
	cmd.Flags().StringVar(&opts.language, "language", "", "Project language: python, swift, rust, node, cpp, or mojo")
	cmd.Flags().StringVar(&opts.template, "template", "", "Project template (e.g. simple-api, fullstack)")
	cmd.Flags().StringVar(&opts.branch, "branch", "", fmt.Sprintf("Branch of the templates repo to use (default: %s)", templateRepoBranch))
	cmd.Flags().StringSliceVar(&opts.vars, "var", nil, "Template variable override (repeatable, KEY=VALUE)")
	cmd.Flags().StringVar(&opts.gitInit, "git-init", "", "Initialize a git repo in the project directory (yes or no)")
	cmd.Flags().StringSliceVar(&opts.entitlements, "entitlement", nil, "App entitlement to enable (repeatable or comma-separated)")
	cmd.Flags().BoolVar(&opts.allEntitlements, "all-entitlements", false, "Enable all entitlements (requires field flags for gpio, i2c, persist)")
	cmd.Flags().BoolVar(&opts.noExtraEntitlements, "no-extra-entitlements", false, "Skip entitlement prompts and use only the default network entitlement")
	cmd.Flags().StringVar(&opts.gpioPins, "gpio-pins", "", "GPIO pins for the gpio entitlement (comma-separated, e.g. 17,27,22)")
	cmd.Flags().StringVar(&opts.i2cDevice, "i2c-device", "", "I2C device path for the i2c entitlement (e.g. /dev/i2c-1)")
	cmd.Flags().StringVar(&opts.persistName, "persist-name", "", "Container ID for the persist entitlement")
	cmd.Flags().StringVar(&opts.persistPath, "persist-path", "", "Mount path for the persist entitlement (e.g. /data)")
	cmd.Flags().StringVar(&opts.assistant, "assistant", "", "AI assistant to launch after init: claude, codex, or skip")
	cmd.Flags().BoolVar(&opts.installClaudeSkills, "install-claude-skills", false, "Install Wendy Claude skills before launching Claude")
	cmd.Flags().StringSliceVar(&opts.frameworks, "framework", nil, "App framework to enable in wendy.json's top-level \"frameworks\" key (repeatable or comma-separated; currently: ros2)")
	cmd.Flags().IntVar(&opts.ros2DomainID, "ros2-domain-id", -1, fmt.Sprintf("ROS 2 domain ID, %d-%d (default: derived from the app ID)", appconfig.ROS2DomainIDMin, appconfig.ROS2DomainIDMax))
	cmd.Flags().StringVar(&opts.ros2RMW, "ros2-rmw", "", "ROS 2 middleware implementation: cyclonedds (default), fastrtps, connextdds, or gurumdds")
	cmd.Flags().StringVar(&opts.ros2Distro, "ros2-distro", "", fmt.Sprintf("ROS 2 distribution (default: %s)", appconfig.ROS2DefaultDistro))
	cmd.Flags().StringVar(&opts.ros2DiscoveryScope, "ros2-discovery-scope", "", fmt.Sprintf("ROS 2 discovery scope: %s (default, isolated) or %s (shared with the device's host network)", appconfig.ROS2DiscoveryScopeApp, appconfig.ROS2DiscoveryScopeHost))

	// Allow bare `--template` (no value) by rewriting os.Args before cobra
	// parses flags. When --template appears as the last arg or is followed by
	// another flag (--*), inject a sentinel value so cobra doesn't error with
	// "flag needs an argument".
	rewriteBareTemplateFlag()

	return cmd
}

// rewriteBareTemplateFlag patches os.Args in-place so that a bare --template
// (with no value) becomes --template=_pick. This lets cobra parse it as a
// normal string flag while the init wizard treats "_pick" as "show picker".
func rewriteBareTemplateFlag() {
	for i, arg := range os.Args {
		if arg == "--template" {
			next := ""
			if i+1 < len(os.Args) {
				next = os.Args[i+1]
			}
			// If --template is last arg or next arg is another flag, inject sentinel.
			if next == "" || strings.HasPrefix(next, "-") {
				os.Args[i] = "--template=" + bareTemplatePickSentinel
			}
		}
	}
}

func runInitWizard(args []string, opts initOptions) error {
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	if err := validateInitAssistantOptions(opts); err != nil {
		return err
	}

	// Steps 1-2: target and template resolve together — a concrete --template
	// can pin or narrow the target from registry metadata.
	target, tmpl, meta, err := resolveInitTargetAndTemplate(opts)
	if err != nil {
		return err
	}

	if tmpl != "" {
		destDir, appID, err := resolveInitDestAndID(cwd, args, opts)
		if err != nil {
			return err
		}
		return runTemplateFlow(cwd, destDir, appID, tmpl, target, meta, opts)
	}

	// Standard wizard flow (no template) — check wendy.json doesn't already exist.
	appID, err := resolveInitAppID(cwd, args, opts)
	if err != nil {
		return err
	}

	cfgPath := filepath.Join(cwd, "wendy.json")
	if _, err := os.Stat(cfgPath); err == nil {
		return fmt.Errorf("wendy.json already exists in %s", cwd)
	}

	// Step 2: Pick language (constrained by already-resolved target).
	language, err := resolveInitLanguage(target, opts)
	if err != nil {
		return err
	}

	// Step 3: Interactive entitlement questions.
	entitlements, err := resolveInitEntitlements(target, language, opts)
	if err != nil {
		return err
	}

	// Step 3b: Framework support (currently ROS 2), via --framework/--ros2-*
	// flags or, on a WendyOS target with no entitlement flags, an interactive
	// prompt. wendy.json's top-level "frameworks" key otherwise has no
	// discovery path from `wendy init` (WDY frameworks discoverability).
	frameworks, err := resolveInitFrameworks(target, opts)
	if err != nil {
		return err
	}

	// Step 4: Generate wendy.json.
	// WendyOS is Linux, so the WendyOS target writes the plain "linux"
	// platform. wendy-lite and darwin need distinct values.
	platform := appconfig.PlatformLinux
	switch target {
	case targetWendyLite:
		platform = appconfig.PlatformWendyLite
	case targetDarwin:
		platform = appconfig.PlatformDarwin
	}

	cfg := appconfig.AppConfig{
		AppID:        appID,
		Version:      "0.1.0",
		Platform:     platform,
		Language:     language,
		Entitlements: entitlements,
		Frameworks:   frameworks,
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

	cliSuccess("\nCreated wendy.json for %s", appID)
	if frameworks != nil && frameworks.ROS2 != nil {
		cliLogln("  Framework: ros2 (edit the \"frameworks\" key in wendy.json to change domain ID, RMW, or discovery scope)")
	}

	// Step 5: Scaffold project files.
	if err := scaffoldProject(cwd, appID, target, language); err != nil {
		return err
	}

	// Step 6: Offer AI assistant session.
	if err := resolveInitAssistant(appID, target, language, entitlements, opts); err != nil {
		return err
	}

	return nil
}

// resolveInitTargetAndTemplate resolves steps 1 and 2 of the wizard together:
// a concrete --template can pin or narrow the target from registry metadata,
// so the target question is asked only when the answer is genuinely open.
// Meta is fetched at most once on either path.
func resolveInitTargetAndTemplate(opts initOptions) (string, string, *repoMeta, error) {
	if opts.templateSet && !opts.targetSet {
		if name := normalizeInitChoice(opts.template); name != bareTemplatePickSentinel {
			meta, err := fetchRepoMetaWithUI(opts.branch)
			if err != nil {
				return "", "", nil, err
			}
			t, ok := templateByName(meta, name)
			if !ok {
				return "", "", nil, fmt.Errorf("unknown template %q (available: %s)", opts.template, metaTemplateNames(meta))
			}
			target, err := resolveInitTargetForTemplate(*t)
			if err != nil {
				return "", "", nil, err
			}
			return target, name, meta, nil
		}
	}

	target, err := resolveInitTarget(opts)
	if err != nil {
		return "", "", nil, err
	}
	tmpl, meta, err := resolveInitTemplateForTarget(target, opts)
	return target, tmpl, meta, err
}

// resolveInitTemplateForTarget determines which template to use, filtering by target.
// Returns (template name, meta, error). Empty template name means skip templates.
// Fetches meta.json from the templates repo when needed.
func resolveInitTemplateForTarget(target string, opts initOptions) (string, *repoMeta, error) {
	if opts.templateSet {
		tmpl := normalizeInitChoice(opts.template)

		// Fetch meta.json to validate or show picker.
		meta, err := fetchRepoMetaWithUI(opts.branch)
		if err != nil {
			return "", nil, err
		}

		if tmpl == bareTemplatePickSentinel {
			// Bare --template (no value): show interactive picker filtered by target.
			name, err := resolveBareTemplatePick(target, meta)
			return name, meta, err
		}

		// Explicit template name: validate it and ensure it supports the target.
		for _, t := range meta.Templates {
			if t.Name == tmpl {
				if !templateTargetMatch(t, target) {
					return "", nil, fmt.Errorf("template %q is not available for target %q", opts.template, target)
				}
				return tmpl, meta, nil
			}
		}
		return "", nil, fmt.Errorf("unknown template %q (available: %s)", opts.template, metaTemplateNames(meta))
	}

	// --target set without --template means manual flow (user is not using templates).
	if opts.targetSet {
		return "", nil, nil
	}

	// Other manual-flow flags skip the template picker.
	if opts.entitlementsSet || opts.allEntitlements || opts.noExtraEntitlements {
		return "", nil, nil
	}

	// In interactive mode, fetch meta and offer templates for this target.
	meta, err := fetchRepoMetaWithUI(opts.branch)
	if err != nil {
		return "", nil, err
	}
	name, err := pickTemplateOrSkipForTarget(target, meta)
	if err != nil {
		return "", nil, err
	}
	return name, meta, nil
}

// templateTargetMatch returns true if the template supports the given target.
// Templates without a Targets list default to WendyOS only; Wendy Lite and
// native macOS templates must explicitly include their target in the list.
func templateTargetMatch(t repoMetaTemplate, target string) bool {
	if len(t.Targets) == 0 {
		return target == targetWendyOS
	}
	for _, tgt := range t.Targets {
		if tgt == target {
			return true
		}
	}
	return false
}

// templateTargets returns the targets a template supports, restricted to the
// targets `wendy init` knows about. Mirrors templateTargetMatch: an empty
// Targets list means WendyOS only.
func templateTargets(t repoMetaTemplate) []string {
	if len(t.Targets) == 0 {
		return []string{targetWendyOS}
	}
	var targets []string
	for _, tgt := range t.Targets {
		if isValidInitTarget(tgt) {
			targets = append(targets, tgt)
		}
	}
	return targets
}

// templateItemsForTarget builds the picker items for the templates available
// for target, shared by the interactive picker and its non-interactive
// plain-text fallback.
func templateItemsForTarget(target string, meta *repoMeta) []tui.PickerItem {
	var items []tui.PickerItem
	for _, t := range meta.Templates {
		if templateTargetMatch(t, target) {
			items = append(items, tui.PickerItem{
				Name:        t.Name,
				Description: t.Description,
				Value:       t.Name,
			})
		}
	}
	return items
}

// pickTemplateNameForTarget shows a picker with templates available for the given target.
func pickTemplateNameForTarget(target string, meta *repoMeta) (string, error) {
	fmt.Println()
	items := templateItemsForTarget(target, meta)
	if len(items) == 0 {
		return "", fmt.Errorf("no templates available for %s", target)
	}
	return pickFromItems("Choose a template", items)
}

// resolveBareTemplatePick handles a bare `--template` (rewritten to the
// "_pick" sentinel by rewriteBareTemplateFlag): it shows the interactive
// picker filtered by target, or — with no TTY attached — prints the same
// list as plain text instead of failing on the picker's TTY open. Without
// this, a headless caller (script, CI, or an AI agent) hit
// "picker: could not open a new TTY" with no way to discover what templates
// exist at all. When exactly one template exists for the target it is
// auto-selected with a notice, TTY or not.
func resolveBareTemplatePick(target string, meta *repoMeta) (string, error) {
	items := templateItemsForTarget(target, meta)
	if len(items) == 0 {
		return "", fmt.Errorf("no templates available for %s", target)
	}
	if len(items) == 1 {
		name := items[0].Value.(string)
		cliNotice("Template %q is the only template for %s.", name, initTargetDisplayName(target))
		return name, nil
	}
	if !isInteractiveTerminal() {
		printPickerItemsPlainText("Available templates for "+target, items)
		return "", fmt.Errorf("--template requires a value when running non-interactively; pass --template=<name> using one of the templates listed above")
	}
	return pickTemplateNameForTarget(target, meta)
}

// pickTemplateOrSkipForTarget shows templates for the given target plus a "No template" option.
func pickTemplateOrSkipForTarget(target string, meta *repoMeta) (string, error) {
	fmt.Println()
	var items []tui.PickerItem
	for _, t := range meta.Templates {
		if templateTargetMatch(t, target) {
			items = append(items, tui.PickerItem{
				Name:        t.Name,
				Description: t.Description,
				Value:       t.Name,
			})
		}
	}
	items = append(items, tui.PickerItem{
		Name:        "No template",
		Description: "Configure target, language, and entitlements manually",
		Value:       "",
		SortKey:     "~",
	})
	return pickFromItems("Start from a template?", items)
}

// resolveTemplateLanguage picks the language for the template flow.
// Wendy Lite uses Swift. Other targets offer the languages available for
// the selected template, including native Mojo/MAX chat on Darwin.
func resolveTemplateLanguage(target, tmpl string, meta *repoMeta, opts initOptions) (string, error) {
	if target == targetWendyLite {
		if opts.languageSet && normalizeInitChoice(opts.language) != langSwift {
			return "", fmt.Errorf("%s templates require %s", target, langSwift)
		}
		languages, err := templateLanguagesForTemplate(context.Background(), meta, tmpl, opts.branch)
		if err != nil {
			return "", err
		}
		if !templateLanguageAvailable(langSwift, languages) {
			return "", fmt.Errorf("template %q is not available for language %q (available: %s)", tmpl, langSwift, repoMetaLanguageKeys(languages))
		}
		return langSwift, nil
	}

	languages, err := templateLanguagesForTemplate(context.Background(), meta, tmpl, opts.branch)
	if err != nil {
		return "", err
	}
	if len(languages) == 0 {
		return "", fmt.Errorf("template %q is not available for any registered language", tmpl)
	}

	if opts.languageSet {
		lang := normalizeInitChoice(opts.language)
		if !isTemplateLanguage(lang, meta) {
			names := make([]string, len(meta.Languages))
			for i, l := range meta.Languages {
				names[i] = l.Key
			}
			return "", fmt.Errorf("invalid language %q for templates (available: %s)", opts.language, strings.Join(names, ", "))
		}
		if !templateLanguageAvailable(lang, languages) {
			return "", fmt.Errorf("template %q is not available for language %q (available: %s)", tmpl, opts.language, repoMetaLanguageKeys(languages))
		}
		return lang, nil
	}

	if len(languages) == 1 {
		cliNotice("Template %q uses %s.", tmpl, languages[0].Name)
		return languages[0].Key, nil
	}

	items := templateLanguageItems(languages)
	if !isInteractiveTerminal() {
		printPickerItemsPlainText("Available languages for template "+tmpl, items)
		return "", fmt.Errorf("--language is required when running non-interactively (valid for template %q: %s)",
			tmpl, repoMetaLanguageKeys(languages))
	}

	fmt.Println()
	return pickFromItems("What language will you use?", items)
}

func templateLanguageItems(languages []repoMetaLanguage) []tui.PickerItem {
	var items []tui.PickerItem
	for _, l := range languages {
		items = append(items, tui.PickerItem{Name: l.Name, Value: l.Key})
	}
	return items
}

func templateLanguageAvailable(language string, languages []repoMetaLanguage) bool {
	for _, available := range languages {
		if available.Key == language {
			return true
		}
	}
	return false
}

func repoMetaLanguageKeys(languages []repoMetaLanguage) string {
	keys := make([]string, len(languages))
	for i, language := range languages {
		keys[i] = language.Key
	}
	return strings.Join(keys, ", ")
}

func metaTemplateNames(meta *repoMeta) string {
	names := make([]string, len(meta.Templates))
	for i, t := range meta.Templates {
		names[i] = t.Name
	}
	return strings.Join(names, ", ")
}

// fetchRepoMetaWithUI is a variable so tests can substitute a canned registry
// (the real fetch hits the network).
var fetchRepoMetaWithUI = func(branch string) (*repoMeta, error) {
	if !isInteractiveTerminal() {
		cliLogln("Fetching template registry...")
		return fetchRepoMeta(context.Background(), branch)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prog := tui.NewProgressProgram(tui.NewSpinner("Fetching template registry..."))

	var (
		meta     *repoMeta
		fetchErr error
		done     = make(chan struct{})
	)
	go func() {
		defer close(done)
		meta, fetchErr = fetchRepoMeta(ctx, branch)
		prog.Send(tui.SpinnerDoneMsg{Err: fetchErr})
	}()

	finalModel, err := prog.Run()
	if err != nil {
		cancel()
		<-done
		return nil, fmt.Errorf("spinner TUI: %w", err)
	}

	// If the user quit before the fetch completed, cancel the request and
	// wait for the goroutine to finish so we don't leak it.
	if sm, ok := finalModel.(tui.SpinnerModel); ok && !sm.Done() {
		cancel()
		<-done
		return nil, ErrUserCancelled
	}

	<-done
	return meta, fetchErr
}

func downloadTemplateArchiveWithUI(language, tmpl, branch string) (map[string][]byte, *templateManifest, error) {
	title := fmt.Sprintf("Downloading template %q for %s (branch: %s)", tmpl, language, resolveTemplateBranch(branch))

	if !isInteractiveTerminal() {
		cliLogln("\n%s...", title)
		return downloadTemplateArchive(context.Background(), language, tmpl, branch, nil)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prog := tui.NewProgressProgram(tui.NewProgress(title).WithoutErrorView())

	var (
		files    map[string][]byte
		manifest *templateManifest
		dlErr    error
		done     = make(chan struct{})
	)
	go func() {
		defer close(done)
		files, manifest, dlErr = downloadTemplateArchive(ctx, language, tmpl, branch, func(written, total int64) {
			if total > 0 {
				prog.Send(tui.ProgressUpdateMsg{
					Percent: float64(written) / float64(total),
					Written: written,
					Total:   total,
				})
			}
		})
		prog.Send(tui.ProgressDoneMsg{Err: dlErr})
	}()

	finalModel, err := prog.Run()
	if err != nil {
		cancel()
		<-done
		return nil, nil, fmt.Errorf("progress TUI: %w", err)
	}

	// If the user quit via q / ctrl+c, ProgressModel.Err() returns
	// context.Canceled. Cancel the in-flight request and surface
	// ErrUserCancelled so the caller doesn't dereference nil manifest/files.
	if pm, ok := finalModel.(tui.ProgressModel); ok {
		if errors.Is(pm.Err(), context.Canceled) {
			cancel()
			<-done
			return nil, nil, ErrUserCancelled
		}
	}

	<-done
	return files, manifest, dlErr
}

// runTemplateFlow handles init when a template is selected.
// destDir is the resolved project directory (either cwd or a new subdir).
func runTemplateFlow(cwd, destDir, appID, tmpl, target string, meta *repoMeta, opts initOptions) error {
	// Scaffolding into cwd (most commonly via --here, WDY-2439) must not
	// silently clobber an existing project. Checked first, before any
	// network calls or filesystem mutation, mirroring the non-template
	// wizard's own wendy.json-exists guard.
	if filepath.Clean(destDir) == filepath.Clean(cwd) {
		if _, err := os.Stat(filepath.Join(destDir, "wendy.json")); err == nil {
			return fmt.Errorf("wendy.json already exists here; run from an empty directory or remove it first")
		}
	}

	language, err := resolveTemplateLanguage(target, tmpl, meta, opts)
	if err != nil {
		return err
	}

	// Resolve entitlement flags up front so an invalid combination fails
	// before any files are scaffolded.
	requestedEntitlements, err := templateEntitlementsFromFlags(target, opts)
	if err != nil {
		return err
	}

	// Resolve --framework/--ros2-* flags up front for the same reason.
	requestedFrameworks, err := templateFrameworksFromFlags(target, opts)
	if err != nil {
		return err
	}

	// Parse --var overrides.
	varOverrides, err := parseVarFlags(opts.vars)
	if err != nil {
		return err
	}

	files, manifest, err := downloadTemplateArchiveWithUI(language, tmpl, opts.branch)
	if err != nil {
		return err
	}

	// Collect variable values from flags or interactive prompts.
	vals, err := collectTemplateValues(manifest, appID, varOverrides)
	if err != nil {
		return err
	}

	// Pre-populate vals with any --var overrides not consumed by template
	// variables, so they can answer schema questions.
	for k, v := range varOverrides {
		if _, exists := vals[k]; !exists {
			vals[k] = v
		}
	}

	// Collect schema-driven answers (multi-phase conditional questions).
	if manifest.Schema != nil {
		if err := collectSchemaAnswers(manifest.Schema, vals); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("creating project directory: %w", err)
	}

	// Render and write all template files.
	if err := renderAndWriteTemplate(files, destDir, appID, tmpl, vals); err != nil {
		return err
	}

	addedEntitlements, err := mergeTemplateEntitlements(filepath.Join(destDir, "wendy.json"), requestedEntitlements)
	if err != nil {
		return err
	}

	addedFrameworks, err := mergeTemplateFrameworks(filepath.Join(destDir, "wendy.json"), requestedFrameworks)
	if err != nil {
		return err
	}

	cliSuccess("\nScaffolded %s project from template %q", language, tmpl)
	cliLogln("  Directory: %s", tui.Path(destDir+"/"))
	for _, v := range manifest.Variables {
		if val, ok := vals[v.Name]; ok {
			cliLogln("  %s: %v", v.Name, val)
		}
	}
	if addedFrameworks {
		cliLogln("  Frameworks added from flags: ros2")
	}
	if len(addedEntitlements) > 0 {
		cliLogln("  Entitlements added from flags: %s", strings.Join(addedEntitlements, ", "))
	}

	// Offer git init.
	if err := maybeGitInit(destDir, opts); err != nil {
		return err
	}

	return finishTemplateInit(cwd, destDir, appID)
}

func finishTemplateInit(cwd, destDir, appID string) error {
	cliSuccess("\nYour project is ready!")
	cliLogln("Next steps:")
	for _, step := range templateNextSteps(cwd, destDir, appID) {
		cliLogln("  %s", tui.Command(step))
	}
	if filepath.Clean(destDir) != filepath.Clean(cwd) {
		cliLogln("Note: run the cd command in your shell; a CLI process cannot change its parent shell directory.")
	}
	return nil
}

func pathHasPrefix(path, prefix string) bool {
	sep := string(filepath.Separator)
	cleanPath := strings.TrimRight(filepath.Clean(path), sep) + sep
	cleanPrefix := strings.TrimRight(filepath.Clean(prefix), sep) + sep
	if runtime.GOOS == "windows" {
		if len(cleanPath) < len(cleanPrefix) {
			return false
		}
		return strings.EqualFold(cleanPath[:len(cleanPrefix)], cleanPrefix)
	}
	return strings.HasPrefix(cleanPath, cleanPrefix)
}

func canonicalProjectPath(path string) (string, error) {
	abs, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("%q does not resolve to an existing path: %w", path, err)
	}
	return resolved, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func templateRunCommand(cwd, destDir, appID string) string {
	return strings.Join(templateNextSteps(cwd, destDir, appID), " && ")
}

func templateNextSteps(cwd, destDir, appID string) []string {
	if filepath.Clean(destDir) == filepath.Clean(cwd) {
		return []string{"wendy run"}
	}

	return []string{"cd " + shellQuote(appID), "wendy run"}
}

// confirmInitCurrentDir and promptInitProjectName are variables so tests can
// replace the Bubble Tea prompts with canned answers.
var confirmInitCurrentDir = func() (bool, error) {
	return tui.ConfirmDefaultYes("Initialize in the current directory?")
}

var promptInitProjectName = func() (string, error) {
	return tui.PromptText("Project name", "directory name and app identifier", validateNewProjectName)
}

// resolveInitDestAndID determines the destination directory and app ID for
// the template flow. An explicit app ID (positional or --app-id) answers
// both; otherwise the user is asked on an interactive terminal. Flags that
// answer other questions (--target, entitlement flags, ...) never suppress
// these prompts (WDY-1805).
//
// --here scaffolds into cwd instead of creating a subdirectory (WDY-2439):
// this is what fixes `wendy init cctv-demo` run inside an already-existing,
// empty `cctv-demo/` from nesting a redundant `cctv-demo/cctv-demo/`. With an
// explicit app ID it behaves exactly like the case without --here except the
// destination is cwd rather than a new subdirectory; with no app ID it
// infers one from cwd's basename the same way the interactive "use current
// directory?" prompt does, but without needing a TTY.
func resolveInitDestAndID(cwd string, args []string, opts initOptions) (string, string, error) {
	if opts.here {
		if len(args) > 0 || opts.appIDSet {
			appID, err := resolveInitAppID(cwd, args, opts)
			if err != nil {
				return "", "", err
			}
			return cwd, appID, nil
		}

		appID := strings.TrimSpace(filepath.Base(cwd))
		if err := validateNewProjectName(appID); err != nil {
			return "", "", fmt.Errorf("current directory name %q is not a valid app id: %w; pass one explicitly: wendy init --here <name>", appID, err)
		}
		return cwd, appID, nil
	}

	// Explicit app ID provided: always create a new subdirectory.
	if len(args) > 0 || opts.appIDSet {
		appID, err := resolveInitAppID(cwd, args, opts)
		if err != nil {
			return "", "", err
		}
		return filepath.Join(cwd, appID), appID, nil
	}

	if !isInteractiveTerminal() {
		return "", "", fmt.Errorf("an app ID is required when running non-interactively; pass --app-id or an [app-id] argument")
	}

	fmt.Println()
	useCurrentDir, err := confirmInitCurrentDir()
	if err != nil {
		return "", "", err
	}
	if useCurrentDir {
		appID := strings.TrimSpace(filepath.Base(cwd))
		if err := validateNewProjectName(appID); err != nil {
			return "", "", fmt.Errorf("current directory name %q cannot be used as the app ID: %w; rerun with --app-id", appID, err)
		}
		return cwd, appID, nil
	}

	fmt.Println()
	appID, err := promptInitProjectName()
	if err != nil {
		return "", "", err
	}
	appID = strings.TrimSpace(appID)
	return filepath.Join(cwd, appID), appID, nil
}

func validateNewProjectName(value string) error {
	name := strings.TrimSpace(value)
	if name == "" {
		return fmt.Errorf("project name cannot be empty")
	}
	if filepath.IsAbs(name) || filepath.Clean(name) != name || name == "." || name == ".." || strings.ContainsAny(name, `/\:`) {
		return fmt.Errorf("project name must be a single subdirectory name")
	}
	if strings.HasPrefix(name, "-") || strings.HasPrefix(name, ".") {
		return fmt.Errorf("project name must not start with '-' or '.'")
	}
	for _, r := range name {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			continue
		}
		return fmt.Errorf("project name may only contain letters, numbers, '.', '_' or '-'")
	}
	return nil
}

// maybeGitInit optionally runs git init in the project directory.
func maybeGitInit(dir string, opts initOptions) error {
	doInit := true

	if opts.gitInitSet {
		switch normalizeInitChoice(opts.gitInit) {
		case "yes", "y", "true":
			doInit = true
		case "no", "n", "false":
			doInit = false
		default:
			return fmt.Errorf("invalid --git-init value %q (expected yes or no)", opts.gitInit)
		}
	} else {
		// Interactive yes/no prompt.
		fmt.Println()
		var err error
		doInit, err = tui.ConfirmDefaultYes("Initialize a git repository?")
		if err != nil {
			return err
		}
	}

	if !doInit {
		return nil
	}

	cmd := exec.Command("git", "init", "-b", "main", dir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		cliNotice("  Warning: git init failed: %v", err)
	}

	return nil
}

func resolveInitAppID(cwd string, args []string, opts initOptions) (string, error) {
	if len(args) > 0 {
		appID := strings.TrimSpace(args[0])
		if appID == "" {
			return "", fmt.Errorf("app ID cannot be empty or whitespace")
		}
		return appID, nil
	}

	if opts.appIDSet {
		flagAppID := strings.TrimSpace(opts.appID)
		if flagAppID == "" {
			return "", fmt.Errorf("app ID cannot be empty or whitespace")
		}
		return flagAppID, nil
	}

	// Non-template flow can infer from the current directory name.
	if !opts.templateSet {
		appID := strings.TrimSpace(filepath.Base(cwd))
		if appID == "" {
			return "", fmt.Errorf("could not infer a valid app ID; please provide a non-empty value via --app-id or as a positional argument")
		}
		return appID, nil
	}

	// Template flow (both --template and interactive) needs an explicit
	// app ID since it becomes the project directory name. Return empty
	// here — runTemplateFlow will prompt if needed.
	return "", nil
}

// initTargetItems is the shared source of truth for the target picker (TTY)
// and its non-interactive plain-text fallback (see resolveInitTarget), so the
// two can never drift out of sync.
var initTargetItems = []tui.PickerItem{
	{Name: "WendyOS", Description: "Full Linux-based edge device (Jetson, Raspberry Pi, ...)", Value: targetWendyOS, SortKey: "0"},
	{Name: "macOS", Description: "Native macOS app deployed to Wendy Agent for Mac", Value: targetDarwin, SortKey: "1"},
	{Name: "Wendy Lite", Description: "Microcontroller running WASM (ESP32)", Value: targetWendyLite, SortKey: "2"},
}

// initTargetItemsFor filters the shared initTargetItems so a narrowed target
// picker keeps the canonical names, descriptions, and sort order.
func initTargetItemsFor(targets []string) []tui.PickerItem {
	var items []tui.PickerItem
	for _, item := range initTargetItems {
		for _, target := range targets {
			if item.Value.(string) == target {
				items = append(items, item)
				break
			}
		}
	}
	return items
}

// initTargetDisplayName maps a target value ("wendyos") to its picker display
// name ("WendyOS"); unknown values pass through unchanged.
func initTargetDisplayName(target string) string {
	for _, item := range initTargetItems {
		if item.Value.(string) == target {
			return item.Name
		}
	}
	return target
}

func resolveInitTarget(opts initOptions) (string, error) {
	if opts.targetSet {
		target := normalizeInitTarget(opts.target)
		if !isValidInitTarget(target) {
			return "", fmt.Errorf("invalid target %q (valid: %s, %s, %s)", opts.target, targetWendyOS, targetWendyLite, targetDarwin)
		}
		return target, nil
	}

	if !isInteractiveTerminal() {
		printPickerItemsPlainText("Available targets", initTargetItems)
		return "", fmt.Errorf("--target is required when running non-interactively (valid: %s, %s, %s)", targetWendyOS, targetDarwin, targetWendyLite)
	}

	fmt.Println()
	return pickFromItems("What is your target device?", initTargetItems)
}

// resolveInitTargetForTemplate resolves the target when a concrete --template
// was given without --target: a single-target template pins the answer, a
// multi-target template narrows the picker to its targets.
func resolveInitTargetForTemplate(t repoMetaTemplate) (string, error) {
	targets := templateTargets(t)
	switch len(targets) {
	case 0:
		return "", fmt.Errorf("template %q is not available for any supported target (valid: %s, %s, %s)",
			t.Name, targetWendyOS, targetWendyLite, targetDarwin)
	case 1:
		cliNotice("Template %q targets %s.", t.Name, initTargetDisplayName(targets[0]))
		return targets[0], nil
	}

	items := initTargetItemsFor(targets)
	if !isInteractiveTerminal() {
		printPickerItemsPlainText("Available targets for template "+t.Name, items)
		return "", fmt.Errorf("--target is required when running non-interactively (valid for template %q: %s)",
			t.Name, strings.Join(targets, ", "))
	}

	fmt.Println()
	return pickFromItems("What is your target device?", items)
}

// printPickerItemsPlainText renders picker items as a plain-text list. Used
// as the non-interactive fallback for choices that would otherwise require a
// Bubble Tea picker, which fails ungracefully ("could not open a new TTY")
// when stdin/stdout are not real terminals (WDY frameworks/template
// discoverability follow-up).
func printPickerItemsPlainText(title string, items []tui.PickerItem) {
	cliLogln("%s:", title)
	if len(items) == 0 {
		// Defensive: callers are expected to handle "nothing to choose from"
		// with a specific error, but a bare title with no list under it reads
		// as a rendering bug.
		cliLogln("  (none)")
		return
	}
	for _, item := range items {
		if item.Description != "" {
			cliLogln("  %s - %s", item.Name, item.Description)
		} else {
			cliLogln("  %s", item.Name)
		}
	}
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

func resolveInitEntitlements(target, language string, opts initOptions) ([]appconfig.Entitlement, error) {
	if initEntitlementsProvided(opts) {
		return buildInitEntitlementsFromFlags(target, opts)
	}

	fmt.Println()
	return askEntitlementQuestions(target, language)
}

// resolveInitFrameworks determines the "frameworks" config (currently just
// ROS 2) for the non-template flow: explicit --framework/--ros2-* flags win;
// otherwise, on a WendyOS target with no entitlement flags either (i.e. the
// caller is answering questions interactively, not scripting the whole
// setup), ask. wendy-lite and darwin targets don't run the ROS 2 container
// image, so frameworks are WendyOS-only.
func resolveInitFrameworks(target string, opts initOptions) (*appconfig.FrameworksConfig, error) {
	if initFrameworksProvided(opts) {
		return buildInitFrameworksFromFlags(target, opts)
	}

	if initEntitlementsProvided(opts) || target != targetWendyOS {
		return nil, nil
	}

	fmt.Println()
	return askFrameworkQuestions()
}

func resolveInitAssistant(appID, target, language string, entitlements []appconfig.Entitlement, opts initOptions) error {
	if opts.assistantSet {
		choice := normalizeInitChoice(opts.assistant)
		return runAIAssistantChoice(choice, appID, target, language, entitlements, opts.installClaudeSkills, false)
	}

	fmt.Println()
	return offerAIAssistant(appID, target, language, entitlements)
}

func pickInitLanguage(target string) (string, error) {
	switch target {
	case targetWendyLite:
		// Only WASM-capable languages (currently just Swift).
		cliNotice("Wendy Lite requires a WASM-compatible language.")
		return langSwift, nil
	case targetDarwin:
		cliNotice("Wendy Agent for Mac currently supports native Swift apps.")
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

var askEntitlementQuestions = func(target, language string) ([]appconfig.Entitlement, error) {
	if target == targetDarwin {
		cliLogln("Native macOS apps do not use WendyOS container entitlements.")
		return nil, nil
	}

	// Always include network for WendyOS/Wendy Lite containerized targets.
	entitlements := []appconfig.Entitlement{
		{Type: appconfig.EntitlementNetwork},
	}

	if target == targetWendyLite {
		// Wendy Lite has limited entitlements; skip interactive questions.
		cliLogln("Wendy Lite apps have network access by default.")
		return entitlements, nil
	}

	// Build checklist items from the entitlement questions.
	items := make([]tui.ChecklistItem, len(wendyOSEntitlementQuestions))
	for i, q := range wendyOSEntitlementQuestions {
		items[i] = tui.ChecklistItem{
			Label:       q.question,
			Description: q.description,
			Value:       q.entitlement,
		}
	}

	selected, err := tui.RunChecklist("What does your app need access to?", items)
	if err != nil {
		return nil, err
	}

	for _, item := range selected {
		ent := appconfig.Entitlement{Type: item.Value}

		// Prompt for required fields on certain entitlement types.
		if err := promptEntitlementFields(&ent); err != nil {
			return nil, err
		}

		entitlements = append(entitlements, ent)
	}

	return entitlements, nil
}

// ros2RMWPickerItems lists the RMW implementations offered by the interactive
// ROS 2 setup prompt. Values are the short aliases appconfig.ROS2Config
// accepts (see ResolvedRMW) alongside their full identifiers.
var ros2RMWPickerItems = []tui.PickerItem{
	{Name: "CycloneDDS", Description: "Default; lightweight and widely used", Value: "cyclonedds", SortKey: "0"},
	{Name: "Fast DDS", Description: "eProsima's RMW, ROS 2's historical default", Value: "fastrtps", SortKey: "1"},
	{Name: "Connext DDS", Description: "RTI's commercial-grade RMW", Value: "connextdds", SortKey: "2"},
	{Name: "GurumDDS", Description: "Gurum Networks RMW", Value: "gurumdds", SortKey: "3"},
}

// askFrameworkQuestions interactively offers ROS 2 support for a WendyOS
// app (WDY frameworks discoverability: `wendy init` gave no hint that a
// separate top-level "frameworks" key exists in wendy.json). Domain ID and
// discovery scope are surfaced; the distro is left at its default and can be
// changed by hand afterwards.
var askFrameworkQuestions = func() (*appconfig.FrameworksConfig, error) {
	wantROS2, err := tui.Confirm("Does your app use ROS 2 (Robot Operating System)?")
	if err != nil {
		return nil, err
	}
	if !wantROS2 {
		return nil, nil
	}

	rmw, err := pickFromItems("Which ROS 2 middleware (RMW) implementation?", ros2RMWPickerItems)
	if err != nil {
		return nil, err
	}

	scope, err := pickFromItems("ROS 2 discovery scope?", []tui.PickerItem{
		{Name: "App-local (default)", Description: "Isolated to this app's own containers", Value: appconfig.ROS2DiscoveryScopeApp, SortKey: "0"},
		{Name: "Host network", Description: "Discoverable across the device's host network", Value: appconfig.ROS2DiscoveryScopeHost, SortKey: "1"},
	})
	if err != nil {
		return nil, err
	}

	domainIDInput, err := tui.PromptText(
		"ROS 2 domain ID",
		fmt.Sprintf("%d-%d, leave empty to auto-derive from the app ID", appconfig.ROS2DomainIDMin, appconfig.ROS2DomainIDMax),
		func(v string) error {
			v = strings.TrimSpace(v)
			if v == "" {
				return nil
			}
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("must be a number")
			}
			if n < appconfig.ROS2DomainIDMin || n > appconfig.ROS2DomainIDMax {
				return fmt.Errorf("must be between %d and %d", appconfig.ROS2DomainIDMin, appconfig.ROS2DomainIDMax)
			}
			return nil
		},
	)
	if err != nil {
		return nil, err
	}

	ros2 := &appconfig.ROS2Config{RMW: rmw, DiscoveryScope: scope}
	if domainIDInput = strings.TrimSpace(domainIDInput); domainIDInput != "" {
		domainID, err := strconv.Atoi(domainIDInput)
		if err != nil {
			return nil, fmt.Errorf("invalid ROS 2 domain ID %q: %w", domainIDInput, err)
		}
		ros2.DomainID = &domainID
	}

	cliLogln("ROS 2 distro left at the default (%q) — edit \"frameworks.ros2.distro\" in wendy.json to change it.", appconfig.ROS2DefaultDistro)

	return &appconfig.FrameworksConfig{ROS2: ros2}, nil
}

// initFrameworksProvided reports whether the user supplied any
// --framework/--ros2-* flag, mirroring initEntitlementsProvided.
func initFrameworksProvided(opts initOptions) bool {
	return opts.frameworksSet || opts.ros2DomainIDSet || opts.ros2RMWSet || opts.ros2DistroSet || opts.ros2DiscoveryScopeSet
}

// buildInitFrameworksFromFlags resolves --framework/--ros2-* into a
// FrameworksConfig, or nil when no framework was requested. Field values are
// validated with the same rules appconfig.AppConfig.Validate enforces on
// wendy.json (see validateROS2Config), so a typo fails fast here with a
// --flag-specific message instead of surfacing later from `wendy run` or
// `wendy device ros2`.
func buildInitFrameworksFromFlags(target string, opts initOptions) (*appconfig.FrameworksConfig, error) {
	ros2Requested := opts.ros2DomainIDSet || opts.ros2RMWSet || opts.ros2DistroSet || opts.ros2DiscoveryScopeSet

	if opts.frameworksSet {
		var unknown []string
		requestedROS2 := false
		for _, raw := range opts.frameworks {
			name := normalizeInitChoice(raw)
			if name == "" {
				continue
			}
			if name == "ros2" {
				requestedROS2 = true
				continue
			}
			unknown = append(unknown, name)
		}
		if len(unknown) > 0 {
			return nil, fmt.Errorf("invalid framework %q (valid: ros2)", strings.Join(unknown, ", "))
		}
		if !requestedROS2 && !ros2Requested {
			return nil, fmt.Errorf("--framework requires at least one valid framework")
		}
		ros2Requested = ros2Requested || requestedROS2
	}

	if !ros2Requested {
		return nil, nil
	}

	if target == targetWendyLite || target == targetDarwin {
		return nil, fmt.Errorf("%s does not support the ros2 framework", target)
	}

	ros2 := &appconfig.ROS2Config{}

	if opts.ros2DomainIDSet {
		if opts.ros2DomainID < appconfig.ROS2DomainIDMin || opts.ros2DomainID > appconfig.ROS2DomainIDMax {
			return nil, fmt.Errorf("--ros2-domain-id must be between %d and %d, got %d", appconfig.ROS2DomainIDMin, appconfig.ROS2DomainIDMax, opts.ros2DomainID)
		}
		domainID := opts.ros2DomainID
		ros2.DomainID = &domainID
	}

	if opts.ros2RMWSet {
		ros2.RMW = strings.TrimSpace(opts.ros2RMW)
		if ros2.ResolvedRMW() == "" {
			return nil, fmt.Errorf("invalid --ros2-rmw %q (valid: cyclonedds, fastrtps, connextdds, gurumdds)", opts.ros2RMW)
		}
	}

	if opts.ros2DistroSet {
		distro := strings.TrimSpace(opts.ros2Distro)
		if !appconfig.ROS2DistroPattern.MatchString(strings.ToLower(distro)) {
			return nil, fmt.Errorf("invalid --ros2-distro %q (lowercase letters and digits, starting with a letter — e.g. %q)", opts.ros2Distro, appconfig.ROS2DefaultDistro)
		}
		ros2.Distro = distro
	}

	if opts.ros2DiscoveryScopeSet {
		ros2.DiscoveryScope = strings.TrimSpace(opts.ros2DiscoveryScope)
		if ros2.ResolvedDiscoveryScope() == "" {
			return nil, fmt.Errorf("invalid --ros2-discovery-scope %q (valid: %s, %s)", opts.ros2DiscoveryScope, appconfig.ROS2DiscoveryScopeApp, appconfig.ROS2DiscoveryScopeHost)
		}
	}

	return &appconfig.FrameworksConfig{ROS2: ros2}, nil
}

func initEntitlementsProvided(opts initOptions) bool {
	return opts.entitlementsSet || opts.allEntitlements || opts.noExtraEntitlements || opts.gpioPinsSet || opts.i2cDeviceSet || opts.persistNameSet || opts.persistPathSet
}

func buildInitEntitlementsFromFlags(target string, opts initOptions) ([]appconfig.Entitlement, error) {
	if target == targetDarwin {
		if opts.entitlementsSet || opts.allEntitlements || opts.gpioPinsSet || opts.i2cDeviceSet || opts.persistNameSet || opts.persistPathSet {
			return nil, fmt.Errorf("%s apps do not support WendyOS container entitlements", targetDarwin)
		}
		return nil, nil
	}

	entitlements := []appconfig.Entitlement{{Type: appconfig.EntitlementNetwork}}
	seen := map[string]bool{appconfig.EntitlementNetwork: true}

	if opts.noExtraEntitlements {
		if opts.entitlementsSet || opts.allEntitlements || opts.gpioPinsSet || opts.i2cDeviceSet || opts.persistNameSet || opts.persistPathSet {
			return nil, fmt.Errorf("--no-extra-entitlements cannot be combined with entitlement-specific flags")
		}
		return entitlements, nil
	}

	if opts.allEntitlements && opts.entitlementsSet {
		return nil, fmt.Errorf("--all-entitlements cannot be combined with --entitlement")
	}

	rawTypes := make([]string, 0, len(opts.entitlements)+3)

	if opts.allEntitlements {
		for _, q := range wendyOSEntitlementQuestions {
			rawTypes = append(rawTypes, q.entitlement)
		}
	} else {
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

// templateEntitlementsFromFlags resolves entitlement-related init flags for
// the template flow. The template's wendy.json is scaffolded verbatim, so
// requested entitlements must be merged in afterwards — without that step the
// flags were silently ignored (WDY-1810).
func templateEntitlementsFromFlags(target string, opts initOptions) ([]appconfig.Entitlement, error) {
	if !initEntitlementsProvided(opts) {
		return nil, nil
	}
	return buildInitEntitlementsFromFlags(target, opts)
}

// mergeTemplateEntitlements merges requested entitlements into the scaffolded
// wendy.json at cfgPath and reports the entitlement types it added. Only the
// entitlements key is rewritten; template entries are kept verbatim, and a
// requested entitlement already covered by the template is skipped so the
// template's more specific configuration (network mode, ports, ...) wins.
func mergeTemplateEntitlements(cfgPath string, requested []appconfig.Entitlement) ([]string, error) {
	if len(requested) == 0 {
		return nil, nil
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("reading scaffolded wendy.json to merge entitlement flags: %w", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing scaffolded wendy.json: %w", err)
	}

	var existingRaw []json.RawMessage
	if rawEnts, ok := raw["entitlements"]; ok {
		if err := json.Unmarshal(rawEnts, &existingRaw); err != nil {
			return nil, fmt.Errorf("parsing scaffolded wendy.json entitlements: %w", err)
		}
	}
	existing := make([]appconfig.Entitlement, len(existingRaw))
	for i, entRaw := range existingRaw {
		if err := json.Unmarshal(entRaw, &existing[i]); err != nil {
			return nil, fmt.Errorf("parsing scaffolded wendy.json entitlements[%d]: %w", i, err)
		}
	}

	mergedRaw := existingRaw
	var added []string
	for _, req := range requested {
		if templateEntitlementCovers(existing, req) {
			continue
		}
		entRaw, err := json.Marshal(req)
		if err != nil {
			return nil, fmt.Errorf("marshaling %q entitlement: %w", req.Type, err)
		}
		mergedRaw = append(mergedRaw, entRaw)
		existing = append(existing, req)
		added = append(added, req.Type)
	}
	if len(added) == 0 {
		return nil, nil
	}

	rawMerged, err := json.Marshal(mergedRaw)
	if err != nil {
		return nil, fmt.Errorf("marshaling merged entitlements: %w", err)
	}
	raw["entitlements"] = rawMerged

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling scaffolded wendy.json: %w", err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(cfgPath, out, 0o644); err != nil {
		return nil, fmt.Errorf("writing scaffolded wendy.json: %w", err)
	}
	return added, nil
}

// templateFrameworksFromFlags resolves --framework/--ros2-* flags for the
// template flow, mirroring templateEntitlementsFromFlags.
func templateFrameworksFromFlags(target string, opts initOptions) (*appconfig.FrameworksConfig, error) {
	if !initFrameworksProvided(opts) {
		return nil, nil
	}
	return buildInitFrameworksFromFlags(target, opts)
}

// mergeTemplateFrameworks writes requested into the scaffolded wendy.json's
// top-level "frameworks" key, reporting whether it made a change. Unlike
// mergeTemplateEntitlements (a list, deduplicated entry by entry),
// "frameworks" is a single nested object: if the template already sets one,
// it wins outright and requested is dropped, matching the "template's more
// specific config wins" rule mergeTemplateEntitlements documents.
func mergeTemplateFrameworks(cfgPath string, requested *appconfig.FrameworksConfig) (bool, error) {
	if requested == nil {
		return false, nil
	}

	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return false, fmt.Errorf("reading scaffolded wendy.json to merge framework flags: %w", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return false, fmt.Errorf("parsing scaffolded wendy.json: %w", err)
	}

	if existing, ok := raw["frameworks"]; ok && templateFrameworksAreSet(existing) {
		return false, nil
	}

	frameworksRaw, err := json.Marshal(requested)
	if err != nil {
		return false, fmt.Errorf("marshaling frameworks: %w", err)
	}
	raw["frameworks"] = frameworksRaw

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return false, fmt.Errorf("marshaling scaffolded wendy.json: %w", err)
	}
	out = append(out, '\n')
	if err := os.WriteFile(cfgPath, out, 0o644); err != nil {
		return false, fmt.Errorf("writing scaffolded wendy.json: %w", err)
	}
	return true, nil
}

// templateFrameworksAreSet reports whether a template's "frameworks" value
// actually configures a framework. `null`, `{}`, and an object whose every
// member is null (e.g. `{"ros2": null}`) configure nothing, so they must not
// suppress the caller's --framework/--ros2-* flags — "the template's more
// specific config wins" only applies when the template is in fact more
// specific. A non-object (malformed or a scalar) counts as set: it is not
// this function's job to silently overwrite something it cannot interpret.
func templateFrameworksAreSet(existing json.RawMessage) bool {
	trimmed := strings.TrimSpace(string(existing))
	if trimmed == "" || trimmed == "null" {
		return false
	}

	var members map[string]json.RawMessage
	if err := json.Unmarshal(existing, &members); err != nil {
		return true
	}
	for _, member := range members {
		if strings.TrimSpace(string(member)) != "null" {
			return true
		}
	}
	return false
}

// templateEntitlementCovers reports whether the template's entitlements
// already grant what req asks for.
func templateEntitlementCovers(existing []appconfig.Entitlement, req appconfig.Entitlement) bool {
	for _, e := range existing {
		if e.Type != req.Type {
			continue
		}
		switch req.Type {
		case appconfig.EntitlementPersist:
			if e.Name == req.Name {
				return true
			}
		case appconfig.EntitlementI2C, appconfig.EntitlementSerial:
			if e.Device == req.Device {
				return true
			}
		case appconfig.EntitlementGPIO:
			// A gpio entry without pins grants access to all GPIO chips.
			if len(e.Pins) == 0 || pinsSubset(req.Pins, e.Pins) {
				return true
			}
		default:
			return true
		}
	}
	return false
}

func pinsSubset(sub, super []int) bool {
	for _, p := range sub {
		if !slices.Contains(super, p) {
			return false
		}
	}
	return true
}

func normalizeInitChoice(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func normalizeInitTarget(value string) string {
	target := normalizeInitChoice(value)
	switch target {
	case "mac", "macos", "mac-os":
		return targetDarwin
	default:
		return target
	}
}

func isValidInitTarget(target string) bool {
	return target == targetWendyOS || target == targetWendyLite || target == targetDarwin
}

func isValidInitLanguage(language string) bool {
	return language == langSwift || language == langPython
}

func validateInitLanguage(target, language string) error {
	if target == targetWendyLite && language != langSwift {
		return fmt.Errorf("%s requires %s", targetWendyLite, langSwift)
	}
	if target == targetDarwin && language != langSwift && language != langMojo {
		return fmt.Errorf("%s requires swift or mojo", targetDarwin)
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

	cliSuccess("Created pyproject.toml, source package, and Dockerfile (using uv)")
	return nil
}

func offerAIAssistant(appID, target, language string, entitlements []appconfig.Entitlement) error {
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

	return runAIAssistantChoice(choice, appID, target, language, entitlements, false, true)
}

const wendySkillsMarketplace = "wendylabsinc/claude-skills"
const wendySkillsPluginName = "wendy@claude-skills"

// installWendySkills checks if the Wendy skills plugin is installed and offers
// to install it if missing. This gives Claude expert knowledge about Wendy
// development.
func installWendySkills(autoInstall bool) error {
	// Check if the plugin is already installed by looking at the plugin list output.
	out, err := exec.Command("claude", "plugin", "list").Output()
	if err != nil {
		return nil
	}

	if strings.Contains(string(out), "wendy@claude-skills") {
		return nil
	}

	cliLogln("\nThe Wendy skills plugin gives Claude expert knowledge about")
	cliLogln("building and deploying apps to WendyOS and Wendy Lite devices.")
	fmt.Println()

	if !autoInstall {
		if !confirmDefaultNoFn("Install Wendy skills for Claude Code?") {
			return nil
		}

		fmt.Println()
	}

	// Add the marketplace if not already present.
	addMarketplace := exec.Command("claude", "plugin", "marketplace", "add", wendySkillsMarketplace)
	addMarketplace.Stdout = os.Stdout
	addMarketplace.Stderr = os.Stderr
	if err := addMarketplace.Run(); err != nil {
		cliNotice("  Could not add marketplace: %v", err)
		cliNotice("  You can install manually: claude plugin marketplace add " + wendySkillsMarketplace)
		return nil
	}

	// Install the plugin.
	installCmd := exec.Command("claude", "plugin", "install", wendySkillsPluginName)
	installCmd.Stdout = os.Stdout
	installCmd.Stderr = os.Stderr
	if err := installCmd.Run(); err != nil {
		cliNotice("  Could not install plugin: %v", err)
		cliNotice("  You can install manually: claude plugin install " + wendySkillsPluginName)
		return nil
	}

	cliSuccess("  Wendy skills installed successfully!")
	return nil
}

func runAIAssistantChoice(choice, appID, target, language string, entitlements []appconfig.Entitlement, installClaudeSkills bool, interactive bool) error {
	if choice == assistantSkip {
		cliSuccess("\nYour project is ready! Run %s to build and deploy.", tui.Command("wendy run"))
		return nil
	}

	if !isCommandAvailable(choice) {
		return fmt.Errorf("%s is not installed or not on PATH", choice)
	}

	if choice == assistantClaude {
		var skillsErr error
		switch {
		case installClaudeSkills:
			skillsErr = installWendySkills(true)
		case interactive:
			skillsErr = installWendySkills(false)
		}
		if skillsErr != nil {
			return skillsErr
		}
	}

	prompt := buildAssistantPrompt(appID, target, language, entitlements)

	cliLogln("\nStarting %s with project context...", choice)

	return launchAssistantWithPrompt(choice, prompt)
}

func buildAssistantPrompt(appID, target, language string, entitlements []appconfig.Entitlement) string {
	var sb strings.Builder

	sb.WriteString(fmt.Sprintf("I just initialized a Wendy edge computing project called %q.\n", appID))

	if target == targetWendyLite {
		sb.WriteString("It targets Wendy Lite (ESP32 microcontroller running WASM).\n")
	} else if target == targetDarwin {
		sb.WriteString("It targets Wendy Agent for Mac as a native macOS app.\n")
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

	cliSuccess("Created Package.swift and source files")
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

	cliSuccess("Created Dockerfile")
	return nil
}
