package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// runXcodebuild invokes xcodebuild with the given arguments, routing all
// output to .xcode/xcodebuild.log. It prints a single "follow along" line before
// starting so the user can open a second terminal and tail the log. The log
// file is truncated at the start of each build so it always reflects the
// latest run. No spinner is used: Bubble Tea spinners leave residual terminal
// colour state that corrupts subsequent output in long-running commands like
// wendy run; the tail hint already gives the user visibility into progress.
func runXcodebuild(ctx context.Context, dir string, args ...string) error {
	return runXcodebuildAttempt(ctx, dir, true, args...)
}

// runXcodebuildAttempt is runXcodebuild's implementation. allowRecovery gates a
// single retry after resolving a Command Line Tools-only xcode-select state
// (see xcodeSelectGuidance); it is false on the retry itself so a persistently
// broken selection cannot loop.
func runXcodebuildAttempt(ctx context.Context, dir string, allowRecovery bool, args ...string) error {
	logDir := filepath.Join(dir, ".xcode")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("creating .xcode directory: %w", err)
	}

	logPath := filepath.Join(logDir, "xcodebuild.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return fmt.Errorf("creating build log: %w", err)
	}
	defer logFile.Close()

	fmt.Fprintf(logFile, "xcodebuild %s\n%s\n\n", strings.Join(args, " "), time.Now().Format(time.RFC3339))

	hintStyle := lipgloss.NewStyle().Foreground(tui.ColorPrimary)
	fmt.Fprintf(runProgressWriter(ctx), "\n%s\n\n", hintStyle.Render("  tail -f .xcode/xcodebuild.log"))

	var stderrBuf strings.Builder
	cmd := execCommandContext(ctx, "xcodebuild", args...)
	cmd.Dir = dir
	cmd.Stdout = logFile
	cmd.Stderr = io.MultiWriter(logFile, &stderrBuf)

	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return commandErrorf(errBuilderUnavailable, "xcodebuild is required but not found in PATH; install Xcode from the App Store")
		}
		if allowRecovery && looksLikeCLTOnlySelected(stderrBuf.String()) {
			if selErr := xcodeSelectGuidanceFn(ctx); selErr != nil {
				return selErr
			}
			return runXcodebuildAttempt(ctx, dir, false, args...)
		}
		return err
	}
	return nil
}

func findXcodeProj(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("reading directory %s: %w", dir, err)
	}
	var found []string
	for _, e := range entries {
		if e.IsDir() && strings.HasSuffix(e.Name(), ".xcodeproj") {
			found = append(found, e.Name())
		}
	}
	switch len(found) {
	case 0:
		return "", nil
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf(
			"multiple .xcodeproj directories found (%s); remove all but one or specify a project with -project",
			strings.Join(found, ", "),
		)
	}
}

// parseXcodeSchemes parses the JSON output of `xcodebuild -list -json` and
// returns the list of schemes. It handles both project and workspace keys.
// This is a pure function, suitable for testing without Xcode installed.
func parseXcodeSchemes(data []byte) ([]string, error) {
	var out struct {
		Project *struct {
			Schemes []string `json:"schemes"`
		} `json:"project"`
		Workspace *struct {
			Schemes []string `json:"schemes"`
		} `json:"workspace"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("parsing xcodebuild -list -json output: %w", err)
	}
	if out.Project != nil {
		return out.Project.Schemes, nil
	}
	if out.Workspace != nil {
		return out.Workspace.Schemes, nil
	}
	return nil, fmt.Errorf("xcodebuild -list -json output contained neither 'project' nor 'workspace' key")
}

// findXcodeScheme shells out to `xcodebuild -list -json` to discover the
// available schemes in dir, then returns the single scheme found or an error.
// Multiple schemes produce an error with a hint to set "xcode.scheme" in wendy.json.
func findXcodeScheme(ctx context.Context, dir string) (string, error) {
	return findXcodeSchemeAttempt(ctx, dir, true)
}

// findXcodeSchemeAttempt is findXcodeScheme's implementation. allowRecovery
// gates a single retry after resolving a Command Line Tools-only xcode-select
// state (see xcodeSelectGuidance); it is false on the retry itself so a
// persistently broken selection cannot loop.
func findXcodeSchemeAttempt(ctx context.Context, dir string, allowRecovery bool) (string, error) {
	cmd := execCommandContext(ctx, "xcodebuild", "-list", "-json")
	cmd.Dir = dir

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", commandErrorf(errBuilderUnavailable, "xcodebuild is required but not found in PATH; install Xcode from the App Store")
		}
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if allowRecovery && looksLikeCLTOnlySelected(msg) {
			if selErr := xcodeSelectGuidanceFn(ctx); selErr != nil {
				return "", selErr
			}
			return findXcodeSchemeAttempt(ctx, dir, false)
		}
		return "", fmt.Errorf("xcodebuild -list -json failed: %s: %w", msg, err)
	}
	schemes, err := parseXcodeSchemes([]byte(stdout.String()))
	if err != nil {
		return "", err
	}
	if len(schemes) == 0 {
		return "", fmt.Errorf("no schemes found in Xcode project; open the project in Xcode to create a scheme")
	}
	if len(schemes) == 1 {
		return schemes[0], nil
	}
	return "", fmt.Errorf(
		"multiple schemes found (%s); set \"xcode.scheme\" in wendy.json to specify which one to build",
		strings.Join(schemes, ", "),
	)
}

// xcodeSelectCLTMarker is the substring xcodebuild prints (via xcode-select)
// when the active developer directory is a Command Line Tools install rather
// than a full Xcode.app. CLT ships none of the project/scheme tooling the
// Xcode build path needs, so every xcodebuild invocation fails the same way
// until a full Xcode is selected.
const xcodeSelectCLTMarker = "requires Xcode, but active developer directory"

// looksLikeCLTOnlySelected reports whether xcodebuild's output indicates
// Command Line Tools (not a full Xcode.app) is the active developer directory.
func looksLikeCLTOnlySelected(output string) bool {
	return strings.Contains(output, xcodeSelectCLTMarker)
}

// applicationsDir is where installedXcodeApps looks for Xcode.app bundles.
// Overridable in tests.
var applicationsDir = "/Applications"

// installedXcodeApps returns the full Xcode.app bundles found under
// applicationsDir, sorted by name. A bundle only qualifies if it actually
// ships xcodebuild (Command Line Tools and stray folders merely named
// "*.app" are excluded), so a picker built from this list never offers a
// dead end.
func installedXcodeApps() []string {
	matches, err := filepath.Glob(filepath.Join(applicationsDir, "*.app"))
	if err != nil {
		return nil
	}
	var apps []string
	for _, m := range matches {
		if _, statErr := os.Stat(filepath.Join(m, "Contents", "Developer", "usr", "bin", "xcodebuild")); statErr == nil {
			apps = append(apps, m)
		}
	}
	sort.Strings(apps)
	return apps
}

// xcodeSelectGuidanceFn is called once xcodebuild's output has matched
// looksLikeCLTOnlySelected. Declared as a var so tests can stub it without
// driving the real interactive picker/confirm/sudo flow.
var xcodeSelectGuidanceFn = xcodeSelectGuidance

// xcodeSelectGuidance is called once xcodebuild's output has matched
// looksLikeCLTOnlySelected. It explains the situation and, where possible,
// resolves it outright:
//
//   - No Xcode installed: explains that Command Line Tools alone cannot build
//     this project and points to the App Store / developer.apple.com for a
//     full install (including betas).
//   - Exactly one Xcode installed: offers to run `sudo xcode-select -s` for it.
//   - Multiple Xcodes installed (e.g. a release plus a beta): shows a picker so
//     the user can choose which one to select, then offers to run the command.
//
// A nil return means the active developer directory was fixed and the caller
// should retry its xcodebuild invocation.
func xcodeSelectGuidance(ctx context.Context) error {
	apps := installedXcodeApps()

	if len(apps) == 0 {
		return fmt.Errorf(
			"xcodebuild requires the full Xcode app, but only Command Line Tools is installed.\n" +
				"Install Xcode from the App Store (search \"Xcode\"), or download a specific version " +
				"(including betas) from https://developer.apple.com/download/all/ (requires a free Apple " +
				"Developer account), then run:\n\n" +
				"    sudo xcode-select -s /Applications/Xcode.app/Contents/Developer\n",
		)
	}

	if jsonOutput || !isInteractiveTerminal() {
		return xcodeSelectNonInteractiveErr(apps)
	}

	chosen := apps[0]
	if len(apps) > 1 {
		var err error
		chosen, err = pickXcodeAppInteractive(apps)
		if err != nil {
			return err
		}
	}

	developerDir := filepath.Join(chosen, "Contents", "Developer")
	run, err := tui.ConfirmDefaultYes(fmt.Sprintf("Select %s as the active Xcode (runs: sudo xcode-select -s)?", chosen))
	if err != nil {
		return err
	}
	if !run {
		return fmt.Errorf("run the following to continue, then re-run this command:\n\n    sudo xcode-select -s %s\n", developerDir)
	}

	selectCmd := execCommandContext(ctx, "sudo", "xcode-select", "-s", developerDir)
	selectCmd.Stdin = os.Stdin
	selectCmd.Stdout = runProgressWriter(ctx)
	selectCmd.Stderr = os.Stderr
	if err := selectCmd.Run(); err != nil {
		return fmt.Errorf("sudo xcode-select -s %s failed: %w", developerDir, err)
	}
	return nil
}

// xcodeSelectNonInteractiveErr builds the guidance error for scripts/--json
// callers, which cannot be shown a picker or a confirm prompt.
func xcodeSelectNonInteractiveErr(apps []string) error {
	if len(apps) == 1 {
		developerDir := filepath.Join(apps[0], "Contents", "Developer")
		return fmt.Errorf(
			"xcodebuild requires the full Xcode app, but the active developer directory is Command Line Tools; select it with:\n\n    sudo xcode-select -s %s\n",
			developerDir,
		)
	}
	return fmt.Errorf(
		"xcodebuild requires the full Xcode app, but the active developer directory is Command Line "+
			"Tools; found %s — select one with:\n\n    sudo xcode-select -s <path>/Contents/Developer\n",
		strings.Join(apps, ", "),
	)
}

// pickXcodeAppInteractive shows a picker over the given Xcode.app bundles
// (used when more than one is installed, e.g. a stable release plus a beta)
// and returns the one the user chose.
func pickXcodeAppInteractive(apps []string) (string, error) {
	items := make([]tui.PickerItem, 0, len(apps))
	for _, a := range apps {
		items = append(items, tui.PickerItem{Name: filepath.Base(a), Value: a})
	}
	picker := tui.NewPickerWithTitleAndColumns("Select an Xcode installation", []tui.PickerColumn{
		{
			Title:    "App",
			MinWidth: 24,
			Required: true,
			Value:    func(item tui.PickerItem) string { return item.Name },
		},
	})

	p := tea.NewProgram(picker)
	go func() {
		p.Send(tui.PickerAddMsg{Items: items})
		p.Send(tui.PickerDoneMsg{})
	}()

	finalModel, err := p.Run()
	if err != nil {
		return "", fmt.Errorf("picker: %w", err)
	}
	pm, ok := finalModel.(tui.PickerModel)
	if !ok {
		return "", fmt.Errorf("picker: unexpected model type %T", finalModel)
	}
	if pm.Cancelled() || pm.Selected() == nil {
		return "", ErrUserCancelled
	}
	appPath, ok := pm.Selected().Value.(string)
	if !ok {
		return "", fmt.Errorf("picker: unexpected selection value %T", pm.Selected().Value)
	}
	return appPath, nil
}

// findXcodeBuildProduct inspects the build products directory produced by
// `xcodebuild -configuration Release -derivedDataPath <derivedDataPath>` and
// returns the absolute path to the product and whether it is a .app bundle.
// Configuration is always Release; the products directory is:
//
//	<derivedDataPath>/Build/Products/Release/
func findXcodeBuildProduct(derivedDataPath, scheme string) (productPath string, isApp bool, err error) {
	releaseDir := filepath.Join(derivedDataPath, "Build", "Products", "Release")

	// Check for a .app bundle first.
	appPath := filepath.Join(releaseDir, scheme+".app")
	if info, statErr := os.Stat(appPath); statErr == nil && info.IsDir() {
		return appPath, true, nil
	}

	// Check for a plain command-line binary.
	binPath := filepath.Join(releaseDir, scheme)
	if _, statErr := os.Stat(binPath); statErr == nil {
		return binPath, false, nil
	}

	return "", false, fmt.Errorf(
		"build product for scheme %q not found in %s (expected %s or %s)",
		scheme, releaseDir, scheme+".app", scheme,
	)
}

// assembleXcodeSyncEntries constructs the fileSyncEntry list for the given
// Xcode build product. The entries include:
//   - For a CLI binary: the binary itself plus any sibling .bundle directories.
//   - For a .app bundle: the entire bundle tree as a single directory entry.
//
// sandbox.sb (if present) and user-declared files from wendy.json are always
// appended.
func assembleXcodeSyncEntries(productPath string, isApp bool, cwd string, appCfg *appconfig.AppConfig) ([]fileSyncEntry, error) {
	var entries []fileSyncEntry

	if isApp {
		// Sync the complete .app bundle as a directory.
		appName := filepath.Base(productPath) // e.g. "HelloXcode.app"
		entries = append(entries, fileSyncEntry{
			localPath:  productPath,
			remotePath: appName,
		})
	} else {
		// Binary.
		name := filepath.Base(productPath)
		entries = append(entries, fileSyncEntry{
			localPath:  productPath,
			remotePath: name,
		})
		// Sibling .bundle directories in the same Release directory.
		releaseDir := filepath.Dir(productPath)
		siblings, err := os.ReadDir(releaseDir)
		if err != nil {
			return nil, fmt.Errorf("reading build products directory %s: %w", releaseDir, err)
		}
		for _, e := range siblings {
			if e.IsDir() && strings.HasSuffix(e.Name(), ".bundle") {
				entries = append(entries, fileSyncEntry{
					localPath:  filepath.Join(releaseDir, e.Name()),
					remotePath: e.Name(),
				})
			}
		}
	}

	// sandbox.sb (optional).
	sandboxPath := filepath.Join(cwd, "sandbox.sb")
	if _, err := os.Stat(sandboxPath); err == nil {
		entries = append(entries, fileSyncEntry{
			localPath:  sandboxPath,
			remotePath: "sandbox.sb",
		})
	}

	// User-declared files from wendy.json.
	for _, f := range appCfg.Files {
		localAbs := filepath.Join(cwd, f.Path)
		entries = append(entries, fileSyncEntry{
			localPath:  localAbs,
			remotePath: effectiveRemotePath(f.Path, f.To),
		})
	}

	return appendNativeBrewfileSyncEntry(entries, cwd, appCfg)
}

// xcodeEntrypoint derives the container Cmd string from the build product. For
// a plain binary it returns the filename; for a .app bundle it returns the
// macOS launcher path: <Name>.app/Contents/MacOS/<Name>.
func xcodeEntrypoint(productPath string, isAppBundle bool) string {
	name := filepath.Base(productPath)
	if isAppBundle {
		stem := strings.TrimSuffix(name, ".app")
		return name + "/Contents/MacOS/" + stem
	}
	return name
}

// runMacOSXcodeWithAgent builds an Xcode project locally with xcodebuild,
// syncs the resulting binary (or .app bundle) plus any sibling .bundle
// resources to the device via SyncFiles gRPC, then creates and starts the
// container. Architecture and code-signing settings are taken from the project's
// build settings; this function does not override them.
func runMacOSXcodeWithAgent(ctx context.Context, conn *grpcclient.AgentConnection, cwd string, appCfg *appconfig.AppConfig, opts runOptions) error {
	// Verify CPU architecture matches.
	versionResp, err := conn.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		return fmt.Errorf("querying device version: %w", err)
	}
	deviceArch := versionResp.GetCpuArchitecture()
	if deviceArch == "" {
		deviceArch = "arm64"
	}
	if deviceArch != runtime.GOARCH {
		return commandErrorf(errProjectTargetMismatch, "architecture mismatch: device is %s but host is %s", deviceArch, runtime.GOARCH)
	}

	// Find the .xcodeproj directory.
	xp, err := findXcodeProj(cwd)
	if err != nil {
		return err
	}
	if xp == "" {
		return fmt.Errorf("no .xcodeproj directory found in %s", cwd)
	}

	// Determine the scheme (wendy.json xcode.scheme override → auto-detect).
	scheme := ""
	if appCfg.Xcode != nil {
		scheme = appCfg.Xcode.Scheme
	}
	if scheme == "" {
		scheme, err = findXcodeScheme(ctx, cwd)
		if err != nil {
			return err
		}
	}

	// Build with xcodebuild -configuration Release.
	// SECURITY: Xcode project support exists for native Mac packages that cannot be
	// built correctly with SwiftPM alone today, for example packages that require
	// Xcode-only resource or shader build steps (see
	// docs/clients/wendy-cli/commands/build.md).
	// Xcode's macro/plugin prompts are an interactive consent layer on top of
	// SwiftPM's build-time code/sandbox model; headless Wendy CLI deploys cannot
	// answer those prompts, so we deliberately make xcodebuild behave like CLI
	// build tools and rely on a trusted, pinned Package.resolved.
	derivedDataPath := filepath.Join(cwd, ".xcode")
	cliLogln("Building Xcode project %s (scheme: %s)...", xp, scheme)
	if err := runXcodebuild(ctx, cwd,
		"-project", xp,
		"-scheme", scheme,
		"-configuration", "Release",
		"-derivedDataPath", ".xcode/",
		"-skipMacroValidation",
		"-skipPackagePluginValidation",
	); err != nil {
		return commandErrorf(errBuildFailed, "xcodebuild failed: %w", err)
	}
	cliLogln("Build completed.")

	// Locate the build product.
	productPath, isApp, err := findXcodeBuildProduct(derivedDataPath, scheme)
	if err != nil {
		return err
	}

	// Assemble file sync entries.
	syncEntries, err := assembleXcodeSyncEntries(productPath, isApp, cwd, appCfg)
	if err != nil {
		return err
	}

	// Sync files to the device.
	if err := syncFiles(ctx, conn, appCfg.AppID, syncEntries); err != nil {
		return commandErrorf(errTransferFailed, "syncing files: %w", err)
	}

	// Create and start the container.
	var runArgs []string
	if appCfg.Run != nil {
		runArgs = appCfg.Run.Args
	}
	createReq := &agentpb.CreateContainerRequest{
		AppName:  appCfg.AppID,
		Cmd:      xcodeEntrypoint(productPath, isApp),
		UserArgs: runArgs,
	}
	return runMacOSNativeContainer(ctx, conn, appCfg, createReq, opts)
}
