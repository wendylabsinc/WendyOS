package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/providers"
	"github.com/wendylabsinc/wendy/internal/cli/swifttoolchain"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"golang.org/x/term"
)

// BuildResult is the output of the build command. Exactly one field is set.
type BuildResult struct {
	// ProviderApp is set when the build used an external provider.
	ProviderApp *providers.BuiltApp
}

type buildOptions struct {
	buildType  string
	dockerfile string
}

func newBuildCmd() *cobra.Command {
	var opts buildOptions

	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build the application in the current directory",
		Long:  "Detects the project type and builds a Docker image for the target device architecture.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.dockerfile != "" && opts.buildType != "" && normalizeBuildType(opts.buildType) != "docker" {
				return fmt.Errorf("--dockerfile cannot be used with --build-type=%s", opts.buildType)
			}
			// --dockerfile implies a Docker build; prevent the provider from
			// auto-selecting a Compose file when both markers are present.
			if opts.dockerfile != "" && opts.buildType == "" {
				opts.buildType = "docker"
			}

			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("getting working directory: %w", err)
			}

			if opts.dockerfile != "" {
				if err := validateDockerfileName(opts.dockerfile); err != nil {
					return fmt.Errorf("--dockerfile: %w", err)
				}
				if _, err := confinedDockerfilePath(cwd, opts.dockerfile); err != nil {
					return fmt.Errorf("--dockerfile: %w", err)
				}
			}

			cfgPath := filepath.Join(cwd, "wendy.json")
			appCfg, cfgErr := ensureAppConfig(cfgPath, false)
			if cfgErr == nil {
				if err := appCfg.Validate(); err != nil {
					return fmt.Errorf("invalid wendy.json: %w", err)
				}
				if err := warnAppConfigFile(cfgPath); err != nil {
					return fmt.Errorf("reading wendy.json warnings: %w", err)
				}
			}

			target, _ := resolveTarget(cmd.Context())

			// If the target is an external provider device, use the provider build path.
			if target != nil && target.External != nil && target.Provider != nil {
				product := filepath.Base(cwd)
				if cfgErr == nil {
					product = appCfg.AppID
				}
				// For Swift projects, resolve the actual product name from Package.swift
				// rather than using the directory name (which may differ in casing).
				if _, err := os.Stat(filepath.Join(cwd, "Package.swift")); err == nil {
					if swiftProduct, err := swifttoolchain.FindSwiftProduct(cwd); err == nil {
						product = swiftProduct
					}
				}

				// Swift projects without a Dockerfile: cross-compile on the host and
				// build a Docker image, bypassing the provider's normal Build method.
				projectType, ptErr := detectProjectType(cwd)
				if ptErr != nil {
					cliLogln("Warning: could not detect project type: %v", ptErr)
				}
				if projectType == "swift" {
					if _, ok := target.Provider.(providers.ImageBuilder); ok {
						if err := swifttoolchain.EnsureSwiftVersion(cmd.Context(), &dimWriter{}, os.Stderr); err != nil {
							return err
						}
						cliLogln("Building Swift project for %s...", target.Provider.DisplayName())
						// runtime.GOARCH is correct here: Docker Desktop loads images into the
						// host daemon, so the image must match the host architecture.
						if _, err := buildSwiftDockerImage(cmd.Context(), cwd, product, runtime.GOARCH, &dimWriter{}, os.Stderr); err != nil {
							return fmt.Errorf("building Swift Docker image: %w", err)
						}
						cliSuccess("Build completed successfully.")
						return nil
					}
				}

				// For docker-type projects, resolve which Dockerfile to use before
				// calling the provider — shows an interactive picker when multiple
				// Dockerfiles exist and no --dockerfile flag was given.
				if projectType == "docker" && opts.dockerfile == "" {
					resolved, resolveErr := resolveDockerfile(cwd, "", isInteractiveTerminal())
					if resolveErr != nil {
						return resolveErr
					}
					opts.dockerfile = resolved
					if resolved != "" && opts.buildType == "" {
						opts.buildType = "docker"
					}
				}

				cliLogln("Building with %s provider...", target.Provider.DisplayName())
				var (
					app      *providers.BuiltApp
					buildErr error
				)
				if db, ok := target.Provider.(providers.DockerfileBuilder); ok && opts.dockerfile != "" {
					app, buildErr = db.BuildWithDockerfile(cmd.Context(), *target.External, cwd, product, opts.buildType, opts.dockerfile, false)
				} else if tb, ok := target.Provider.(providers.TypedBuilder); ok {
					app, buildErr = tb.BuildWithType(cmd.Context(), *target.External, cwd, product, opts.buildType, false)
				} else {
					app, buildErr = target.Provider.Build(cmd.Context(), *target.External, cwd, product, false)
				}
				if buildErr != nil {
					return fmt.Errorf("provider build: %w", buildErr)
				}
				cliSuccess("Build completed successfully (%s).", app.ProviderKey)
				return nil
			}

			// Close the agent connection if one was opened during target resolution.
			if target != nil && target.Agent != nil {
				defer target.Agent.Close()
			}

			// Detect all build options and filter by target capabilities.
			options := detectBuildOptions(cwd)
			if target != nil && target.Provider != nil {
				options = filterBuildOptions(options, target.Provider)
			}
			if len(options) == 0 {
				return fmt.Errorf("no supported build type found for this target; check that the project contains the right files")
			}

			selected, err := resolveDetectedBuildOption(options, opts.buildType, opts.dockerfile)
			if err != nil {
				return err
			}

			// Query the device OS and architecture when an agent connection is
			// available and determine the target platform.
			var cfgPlatform string
			if cfgErr == nil {
				cfgPlatform = appCfg.Platform
			}
			platform := "linux/arm64"
			if target != nil && target.Agent != nil {
				versionResp, err := target.Agent.AgentService.GetAgentVersion(cmd.Context(), &agentpb.GetAgentVersionRequest{})
				if err == nil {
					agentOS := versionResp.GetOs()
					if agentOS == "" {
						agentOS = "linux"
					}
					arch := versionResp.GetCpuArchitecture()
					if arch == "" {
						arch = "arm64"
					}
					platform = resolveAgentPlatform(cfgPlatform, agentOS, arch)
				}
			}

			appID := filepath.Base(cwd)
			if cfgErr == nil {
				appID = appCfg.AppID
			}

			return buildProject(cmd.Context(), cwd, selected, appID, platform)
		},
	}

	cmd.Flags().StringVar(&opts.buildType, "build-type", "", "Build type to use when multiple project markers are present: docker, swift, or python")
	cmd.Flags().StringVar(&opts.dockerfile, "dockerfile", "", "Dockerfile to build from (e.g. Dockerfile.prod); shows a selection menu when multiple Dockerfiles exist")

	return cmd
}

func resolveDetectedBuildOption(options []BuildOption, requestedType, requestedDockerfile string) (*BuildOption, error) {
	interactive := term.IsTerminal(int(os.Stdin.Fd()))

	// --dockerfile selects a specific Dockerfile directly, bypassing type detection.
	if strings.TrimSpace(requestedDockerfile) != "" {
		for i := range options {
			if options[i].Type == "docker" && options[i].File == requestedDockerfile {
				return &options[i], nil
			}
		}
		return nil, fmt.Errorf("dockerfile %q not found; detected %s", requestedDockerfile, strings.Join(buildOptionLabels(options), ", "))
	}

	if strings.TrimSpace(requestedType) != "" {
		return buildOptionForType(options, requestedType, interactive)
	}

	if preferred := preferredBuildOption(options, interactive); preferred != nil {
		return preferred, nil
	}

	// Non-interactive (CI) fallback: when all detected options are Dockerfiles,
	// prefer the base "Dockerfile" and fall back to the first variant rather than
	// failing with "multiple build types detected". This mirrors the run-command
	// behaviour and lets CI pipelines that omit --dockerfile build predictably.
	if !interactive {
		allDocker := len(options) > 0
		for _, opt := range options {
			if opt.Type != "docker" {
				allDocker = false
				break
			}
		}
		if allDocker {
			for i := range options {
				if options[i].File == "Dockerfile" {
					cliNotice("multiple Dockerfiles detected; using %q. Use --dockerfile to select explicitly.", options[i].File)
					return &options[i], nil
				}
			}
			cliNotice("multiple Dockerfiles detected; using %q. Use --dockerfile to select explicitly.", options[0].File)
			return &options[0], nil
		}
	}

	return pickBuildOption(options)
}

// pickBuildOption presents an interactive picker when multiple build options
// are detected. If only one option exists, it is returned directly.
func pickBuildOption(options []BuildOption) (*BuildOption, error) {
	return pickBuildOptionWithTitle(options, "Select a build type")
}

func pickBuildOptionWithTitle(options []BuildOption, title string) (*BuildOption, error) {
	if len(options) == 1 {
		return &options[0], nil
	}

	if !term.IsTerminal(int(os.Stdin.Fd())) {
		var names []string
		for _, o := range options {
			names = append(names, o.Label)
		}
		return nil, fmt.Errorf("multiple build types detected (%s); run in an interactive terminal or remove extra build markers so that only one remains", strings.Join(names, ", "))
	}

	picker := tui.NewPickerWithTitle(title)
	p := tea.NewProgram(picker)

	go func() {
		var items []tui.PickerItem
		for i := range options {
			items = append(items, tui.PickerItem{
				Name:  options[i].Label,
				Value: &options[i],
			})
		}
		p.Send(tui.PickerAddMsg{Items: items})
		p.Send(tui.PickerDoneMsg{})
	}()

	finalModel, err := p.Run()
	if err != nil {
		return nil, fmt.Errorf("build type picker: %w", err)
	}

	pm := finalModel.(tui.PickerModel)
	if pm.Cancelled() {
		return nil, ErrUserCancelled
	}
	sel := pm.Selected()
	if sel == nil {
		return nil, fmt.Errorf("no build type selected")
	}

	opt, ok := sel.Value.(*BuildOption)
	if !ok {
		return nil, fmt.Errorf("invalid selection")
	}
	return opt, nil
}

func preferredBuildOption(options []BuildOption, interactive bool) *BuildOption {
	hasLanguageMarker := false
	dockerCount := 0
	dockerfile := (*BuildOption)(nil)
	for i := range options {
		switch {
		case options[i].Type == "swift" || options[i].Type == "python":
			hasLanguageMarker = true
		case options[i].Type == "docker":
			dockerCount++
			if options[i].File == "Dockerfile" {
				dockerfile = &options[i]
			}
		}
	}
	if !hasLanguageMarker || dockerfile == nil {
		return nil
	}
	if dockerCount == 1 || !interactive {
		return dockerfile
	}
	return nil
}

func buildOptionForType(options []BuildOption, requestedType string, interactive bool) (*BuildOption, error) {
	buildType := normalizeBuildType(requestedType)
	if buildType == "" {
		return nil, fmt.Errorf("build type must be one of docker, swift, or python")
	}

	var matches []BuildOption
	for _, option := range options {
		if option.Type == buildType {
			matches = append(matches, option)
		}
	}
	if len(matches) == 0 {
		return nil, fmt.Errorf("build type %q is not available; detected %s", requestedType, strings.Join(buildOptionLabels(options), ", "))
	}

	if buildType == "docker" {
		var dockerfile *BuildOption
		for i := range matches {
			if matches[i].File == "Dockerfile" {
				dockerfile = &matches[i]
				if !interactive {
					return dockerfile, nil
				}
			}
		}
		if len(matches) > 1 {
			if interactive {
				return pickBuildOptionWithTitle(matches, "Select a Dockerfile")
			}
			return nil, fmt.Errorf("multiple Dockerfiles detected (%s); keep only one Dockerfile or omit --build-type to choose interactively", strings.Join(buildOptionLabels(matches), ", "))
		}
		if dockerfile != nil {
			return dockerfile, nil
		}
	}

	return &matches[0], nil
}

func buildOptionLabels(options []BuildOption) []string {
	labels := make([]string, 0, len(options))
	for _, option := range options {
		labels = append(labels, option.Label)
	}
	return labels
}

func normalizeBuildType(buildType string) string {
	switch strings.ToLower(strings.TrimSpace(buildType)) {
	case "docker", "swift", "python", "compose":
		return strings.ToLower(strings.TrimSpace(buildType))
	default:
		return ""
	}
}

// filterBuildOptions removes options whose Type is not in the provider's
// SupportedBuildTypes list.
func filterBuildOptions(options []BuildOption, provider providers.DeviceProvider) []BuildOption {
	supported := make(map[string]bool)
	for _, t := range provider.SupportedBuildTypes() {
		supported[t] = true
	}
	var filtered []BuildOption
	for _, o := range options {
		if supported[o.Type] {
			filtered = append(filtered, o)
		}
	}
	return filtered
}

// detectProjectTypeWithLanguage determines the project type using the wendy.json
// language field as a hint, falling back to filesystem detection.
func detectProjectTypeWithLanguage(dir, language string) string {
	switch language {
	case "python":
		return "python"
	case "swift":
		return "swift"
	}
	t, _ := detectProjectType(dir) // ignore multiple-xcodeproj error for picker pre-filtering
	return t
}

func buildProject(ctx context.Context, dir string, option *BuildOption, appID, platform string) error {
	imageName := strings.ToLower(appID) + ":latest"

	switch option.Type {
	case "compose":
		return buildComposeProject(dir)
	case "docker":
		return buildDockerProject(dir, imageName, platform, option.File)
	case "python":
		return buildPythonProject(dir, imageName, platform)
	case "swift":
		// Cross-compiling Swift requires a host toolchain; only darwin and linux ship one.
		if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
			return fmt.Errorf("`wendy build` for Swift packages is not supported on %s; provide a Dockerfile", runtime.GOOS)
		}
		return buildSwiftContainerProject(ctx, dir, appID, platform)
	case "xcode":
		return buildXcodeProject(ctx, dir, option.File)
	default:
		return fmt.Errorf("unknown project type; add a Dockerfile, a Compose file (docker-compose.yml, docker-compose.yaml, compose.yml, or compose.yaml), Package.swift, or requirements.txt")
	}
}

func buildComposeProject(dir string) error {
	cliLogln("Building Compose services...")
	cmd := exec.Command("docker", "compose", "build")
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker compose build: %w", err)
	}
	cliSuccess("Build completed successfully.")
	return nil
}

func buildDockerProject(dir, imageName, platform, dockerfile string) error {
	cliLogln("Building Docker image %s for %s...", imageName, platform)

	cmd := exec.Command("docker", "buildx", "build",
		"--platform", platform,
		"-f", dockerfile,
		"-t", imageName,
		"--load",
		".")
	cmd.Dir = dir

	if !term.IsTerminal(int(os.Stdout.Fd())) {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		cliSuccess("Build completed successfully.")
		return nil
	}

	s := tui.NewSpinner("Building Docker image...")
	p := tea.NewProgram(s)

	go func() {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		err := cmd.Run()
		p.Send(tui.SpinnerDoneMsg{Err: err})
	}()

	finalModel, err := p.Run()
	if err != nil {
		return fmt.Errorf("TUI error: %w", err)
	}

	model := finalModel.(tui.SpinnerModel)
	_, buildErr := model.Result()
	if buildErr != nil {
		return buildErr
	}

	cliSuccess("Build completed successfully.")
	return nil
}

func buildPythonProject(dir, imageName, platform string) error {
	dockerfilePath := filepath.Join(dir, "Dockerfile")
	generatedDockerfile := false
	if _, err := os.Stat(dockerfilePath); os.IsNotExist(err) {
		cliLogln("No Dockerfile found. Generating one for Python project...")
		if _, genErr := generatePythonDockerfile(dir); genErr != nil {
			return fmt.Errorf("generating Dockerfile: %w", genErr)
		}
		generatedDockerfile = true
		cliSuccess("Generated Dockerfile.")
	}

	err := buildDockerProject(dir, imageName, platform, "Dockerfile")

	if generatedDockerfile {
		os.Remove(dockerfilePath)
	}

	return err
}

func buildXcodeProject(ctx context.Context, dir, xcodeproj string) error {
	// Resolve scheme: honour wendy.json override, then auto-detect.
	scheme := ""
	if cfg, err := appconfig.LoadFromFile(filepath.Join(dir, "wendy.json")); err == nil && cfg.Xcode != nil {
		scheme = cfg.Xcode.Scheme
	}
	if scheme == "" {
		var err error
		scheme, err = findXcodeScheme(ctx, dir)
		if err != nil {
			return err
		}
	}

	cliLogln("Building Xcode project %s (scheme: %s)...", xcodeproj, scheme)
	if err := runXcodebuild(ctx, dir,
		"-project", xcodeproj,
		"-scheme", scheme,
		"-configuration", "Release",
		"-derivedDataPath", ".xcode/",
	); err != nil {
		return err
	}
	cliSuccess("Build completed successfully.")
	return nil
}

func buildSwiftContainerProject(ctx context.Context, dir, appID, platform string) error {
	if err := swifttoolchain.EnsureSwiftVersion(ctx, &dimWriter{}, os.Stderr); err != nil {
		return err
	}

	product, err := swifttoolchain.FindSwiftProduct(dir)
	if err != nil {
		cliLogln("Warning: could not detect Swift product name (%v); using %q", err, appID)
		product = appID
	}

	arch := runtime.GOARCH
	if parts := strings.SplitN(platform, "/", 2); len(parts) == 2 {
		arch = parts[1]
	}

	if _, err := buildSwiftDockerImage(ctx, dir, product, arch, &dimWriter{}, os.Stderr); err != nil {
		return err
	}
	cliSuccess("Build completed successfully.")
	return nil
}
