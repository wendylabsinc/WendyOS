package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
)

const (
	targetWendyOS   = "wendyos"
	targetWendyLite = "wendy-lite"

	langSwift  = "swift"
	langPython = "python"
	langRust   = "rust"
	langNode   = "node"
	langCpp    = "cpp"

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

	appIDSet        bool
	targetSet       bool
	languageSet     bool
	templateSet     bool
	gitInitSet      bool
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
	{"Does your app need SPI bus access (displays, sensors, flash)?", appconfig.EntitlementSPI, "SPI bus access (may require GPIO access)"},
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

  # Scaffold from a template (interactive language picker)
  wendy init --template simple-api

  # Non-interactive template scaffold with variable overrides
  wendy init --app-id my-api --template simple-api --language rust --var PORT=8080

  # Use a template from a specific branch of the templates repo
  wendy init --template simple-api --branch feature/new-template

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
    --install-claude-skills`,
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

			err := runInitWizard(args, opts)
			if errors.Is(err, tui.ErrCancelled) {
				return ErrUserCancelled
			}
			return err
		},
	}

	cmd.Flags().StringVar(&opts.appID, "app-id", "", "Application ID to write into wendy.json")
	cmd.Flags().StringVar(&opts.target, "target", "", "Target platform: wendyos or wendy-lite")
	cmd.Flags().StringVar(&opts.language, "language", "", "Project language: python, swift, rust, node, or cpp")
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
				os.Args[i] = "--template=_pick"
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

	// Step 1: Pick target device first so template filtering works.
	target, err := resolveInitTarget(opts)
	if err != nil {
		return err
	}

	// Template flow: offer templates filtered by target, or use --template flag.
	tmpl, meta, err := resolveInitTemplateForTarget(target, opts)
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

	cliSuccess("\nCreated wendy.json for %s", appID)

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

		if tmpl == "_pick" {
			// Bare --template (no value): show interactive picker filtered by target.
			name, err := pickTemplateNameForTarget(target, meta)
			return name, meta, err
		}

		// Explicit template name: validate it.
		for _, t := range meta.Templates {
			if t.Name == tmpl {
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
// Templates without a Targets list default to WendyOS only; Wendy Lite templates
// must explicitly include "wendy-lite" in their Targets list.
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

// pickTemplateNameForTarget shows a picker with templates available for the given target.
func pickTemplateNameForTarget(target string, meta *repoMeta) (string, error) {
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
	if len(items) == 0 {
		return "", fmt.Errorf("no templates available for %s", target)
	}
	return pickFromItems("Choose a template", items)
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
// Wendy Lite always uses Swift; WendyOS offers the languages available for the selected template.
func resolveTemplateLanguage(target, tmpl string, meta *repoMeta, opts initOptions) (string, error) {
	if target == targetWendyLite {
		if opts.languageSet && normalizeInitChoice(opts.language) != langSwift {
			return "", fmt.Errorf("%s templates require %s", targetWendyLite, langSwift)
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

	fmt.Println()
	var items []tui.PickerItem
	for _, l := range languages {
		items = append(items, tui.PickerItem{
			Name:  l.Name,
			Value: l.Key,
		})
	}
	return pickFromItems("What language will you use?", items)
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

// fetchRepoMetaWithUI wraps fetchRepoMeta with a bubbletea spinner when
// stdout is a TTY. In non-interactive contexts it falls back to a plain
// printf so logs stay readable. If the user cancels (q / ctrl+c) the
// in-flight HTTP request is aborted and ErrUserCancelled is returned.
func fetchRepoMetaWithUI(branch string) (*repoMeta, error) {
	if !isInteractiveTerminal() {
		cliLogln("Fetching template registry...")
		return fetchRepoMeta(context.Background(), branch)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prog := tea.NewProgram(tui.NewSpinner("Fetching template registry..."))

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

// downloadTemplateArchiveWithUI wraps downloadTemplateArchive with a
// bubbletea progress bar when stdout is a TTY. In non-interactive contexts
// it falls back to plain text. If the user cancels (q / ctrl+c) the
// in-flight HTTP request is aborted and ErrUserCancelled is returned.
func downloadTemplateArchiveWithUI(language, tmpl, branch string) (map[string][]byte, *templateManifest, error) {
	title := fmt.Sprintf("Downloading template %q for %s (branch: %s)", tmpl, language, resolveTemplateBranch(branch))

	if !isInteractiveTerminal() {
		cliLogln("\n%s...", title)
		return downloadTemplateArchive(context.Background(), language, tmpl, branch, nil)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	prog := tea.NewProgram(tui.NewProgress(title).WithoutErrorView())

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
	language, err := resolveTemplateLanguage(target, tmpl, meta, opts)
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

	cliSuccess("\nScaffolded %s project from template %q", language, tmpl)
	cliLogln("  Directory: %s/", destDir)
	for _, v := range manifest.Variables {
		if val, ok := vals[v.Name]; ok {
			cliLogln("  %s: %v", v.Name, val)
		}
	}

	// Offer git init.
	if err := maybeGitInit(destDir, opts); err != nil {
		return err
	}

	cliSuccess("\nYour project is ready! Run `%s` to build and deploy.", templateRunCommand(cwd, destDir, appID))

	return nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func templateRunCommand(cwd, destDir, appID string) string {
	if filepath.Clean(destDir) == filepath.Clean(cwd) {
		return "wendy run"
	}

	return "cd " + shellQuote(appID) + " && wendy run"
}

// resolveInitDestAndID determines the destination directory and app ID for template flow.
// In fully interactive mode it asks whether to initialize in the current directory
// or create a new project subdirectory.
func resolveInitDestAndID(cwd string, args []string, opts initOptions) (string, string, error) {
	// Explicit app ID provided: always create a new subdirectory.
	if len(args) > 0 || opts.appIDSet {
		appID, err := resolveInitAppID(cwd, args, opts)
		if err != nil {
			return "", "", err
		}
		return filepath.Join(cwd, appID), appID, nil
	}

	// Fully interactive (no directive flags): ask where to set up the project.
	if !opts.targetSet && !opts.entitlementsSet && !opts.allEntitlements && !opts.noExtraEntitlements {
		fmt.Println()
		useCurrentDir, err := tui.ConfirmDefaultYes("Initialize in the current directory?")
		if err != nil {
			return "", "", err
		}
		if useCurrentDir {
			return cwd, strings.TrimSpace(filepath.Base(cwd)), nil
		}

		fmt.Println()
		appID, err := tui.PromptText("Project name", "directory name and app identifier", func(v string) error {
			if strings.TrimSpace(v) == "" {
				return fmt.Errorf("project name cannot be empty")
			}
			return nil
		})
		if err != nil {
			return "", "", err
		}
		appID = strings.TrimSpace(appID)
		return filepath.Join(cwd, appID), appID, nil
	}

	// Semi-interactive or non-interactive without explicit app ID: infer from cwd.
	return cwd, strings.TrimSpace(filepath.Base(cwd)), nil
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
		{Name: "WendyOS", Description: "Full Linux-based edge device (Jetson, Raspberry Pi, ...)", Value: targetWendyOS, SortKey: "0"},
		{Name: "Wendy Lite", Description: "Microcontroller running WASM (ESP32)", Value: targetWendyLite, SortKey: "1"},
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

func resolveInitEntitlements(target, language string, opts initOptions) ([]appconfig.Entitlement, error) {
	if initEntitlementsProvided(opts) {
		return buildInitEntitlementsFromFlags(target, opts)
	}

	fmt.Println()
	return askEntitlementQuestions(target, language)
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

// askEntitlementQuestions is a variable so tests can replace it.
var askEntitlementQuestions = func(target, language string) ([]appconfig.Entitlement, error) {
	// Always include network.
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

func initEntitlementsProvided(opts initOptions) bool {
	return opts.entitlementsSet || opts.allEntitlements || opts.noExtraEntitlements || opts.gpioPinsSet || opts.i2cDeviceSet || opts.persistNameSet || opts.persistPathSet
}

func buildInitEntitlementsFromFlags(target string, opts initOptions) ([]appconfig.Entitlement, error) {
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

// promptYesNo displays a styled yes/no prompt. It is a variable so tests can
// replace it with a non-TTY implementation.
var promptYesNo = func(question string) (bool, error) {
	return tui.Confirm(question)
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
		install, err := promptYesNo("Install Wendy skills for Claude Code?")
		if err != nil {
			return err
		}
		if !install {
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
		cliSuccess("\nYour project is ready! Run `wendy run` to build and deploy.")
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
