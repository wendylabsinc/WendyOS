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
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/wendylabsinc/wendy/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
)

// runXcodebuild invokes xcodebuild with the given arguments, routing all
// output to .xcode/xcodebuild.log. It prints a single "follow along" line before
// starting so the user can open a second terminal and tail the log. The log
// file is truncated at the start of each build so it always reflects the
// latest run. No spinner is used: Bubble Tea spinners leave residual terminal
// colour state that corrupts subsequent output in long-running commands like
// wendy run; the tail hint already gives the user visibility into progress.
func runXcodebuild(ctx context.Context, dir string, args ...string) error {
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
	fmt.Println()
	fmt.Println(hintStyle.Render("  tail -f .xcode/xcodebuild.log"))
	fmt.Println()

	cmd := execCommandContext(ctx, "xcodebuild", args...)
	cmd.Dir = dir
	cmd.Stdout = logFile
	cmd.Stderr = logFile

	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("xcodebuild is required but not found in PATH; install Xcode from the App Store")
		}
		return err
	}
	return nil
}

// findXcodeProj returns the name of the single .xcodeproj directory found in
// dir (current directory only, not recursive). It returns ("", nil) when none
// are found, and an error when multiple are found so the user gets a clear
// actionable message.
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
	cmd := execCommandContext(ctx, "xcodebuild", "-list", "-json")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", fmt.Errorf("xcodebuild is required but not found in PATH; install Xcode from the App Store")
		}
		return "", fmt.Errorf("xcodebuild -list -json failed: %s: %w", strings.TrimSpace(string(out)), err)
	}
	schemes, err := parseXcodeSchemes(out)
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

	return entries, nil
}

// xcodeEntrypoint derives the container Cmd string from the build product. For
// a plain binary it returns the filename; for a .app bundle it returns the
// bundle root so the agent can resolve CFBundleExecutable on the target.
func xcodeEntrypoint(productPath string, isAppBundle bool) string {
	name := filepath.Base(productPath)
	if isAppBundle {
		return name
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
		return fmt.Errorf("architecture mismatch: device is %s but host is %s", deviceArch, runtime.GOARCH)
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
	derivedDataPath := filepath.Join(cwd, ".xcode")
	cliLogln("Building Xcode project %s (scheme: %s)...", xp, scheme)
	if err := runXcodebuild(ctx, cwd,
		"-project", xp,
		"-scheme", scheme,
		"-configuration", "Release",
		"-derivedDataPath", ".xcode/",
	); err != nil {
		return fmt.Errorf("xcodebuild failed: %w", err)
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
		return fmt.Errorf("syncing files: %w", err)
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
