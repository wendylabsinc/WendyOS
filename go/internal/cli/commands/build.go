package commands

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/cli/swifttoolchain"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/stagefile"
	"github.com/wendylabsinc/wendy/go/internal/stagefile/gpu"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
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
	builder    string
	// gpuArch names the GPU architecture a cuda: stage is compiled for when
	// there is no device to ask. With a device selected it is redundant —
	// the device reports its own.
	gpuArch string
	// debug builds compiled languages unoptimized (swift -c debug, cargo
	// without --release), matching `wendy run --debug`.
	debug bool
	// buildHost names a WendyOS device that builds the image instead of this
	// machine. Empty means build locally.
	buildHost string
	// service selects one service (and its dependencies) to build, for a
	// multi-service (wendy.json services map) project. Empty builds every
	// service.
	service string
	// maxConcurrency caps how many service images build at once in a
	// multi-service project (0 = default limit of 4).
	maxConcurrency int
}

var appleContainerLocalProviderHintSupported = func() bool {
	return runtime.GOOS == "darwin" && runtime.GOARCH == "arm64"
}

func newBuildCmd() *cobra.Command {
	var opts buildOptions

	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build the application in the current directory",
		Long:  "Detects the project type and builds a Docker image for the target device architecture. For a wendy.json with a services map, builds one local image per service tagged <appid>-<service>:latest; nothing is pushed or deployed.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.dockerfile != "" && opts.buildType != "" && normalizeBuildType(opts.buildType) != "docker" {
				return fmt.Errorf("--dockerfile cannot be used with --build-type=%s", opts.buildType)
			}
			if _, err := normalizeImageBuilder(opts.builder); err != nil {
				return err
			}
			if opts.maxConcurrency < 0 {
				return fmt.Errorf("--max-concurrency must be >= 0 (0 = default limit of 4)")
			}
			if err := validateBuildHostFlags(opts.buildHost, opts.builder); err != nil {
				return err
			}
			// Refused rather than ignored: `wendy build` never reaches
			// runRemoteBuild, so accepting the flag would build locally while the
			// developer believed the Spark was doing it.
			if strings.TrimSpace(opts.buildHost) != "" {
				return errBuildHostOnBuildCmd
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
				if opts.builder != "" {
					return fmt.Errorf("--builder is only used when --device selects a WendyOS device; use --device docker or --device apple-container for local provider builds")
				}
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

				projectType, ptErr := resolveRunProjectType(cwd, opts.buildType)
				if ptErr != nil {
					return ptErr
				}
				if err := ensureProviderSupportsProjectType(target.Provider, projectType, cwd); err != nil {
					return err
				}

				// Swift projects without a container build file: cross-compile on the host and
				// build a Docker image, bypassing the provider's normal Build method.
				if projectType == "swift" {
					if _, ok := target.Provider.(providers.ImageBuilder); ok {
						if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
							return fmt.Errorf("`wendy build` for Swift packages is not supported on %s; provide a Dockerfile or Containerfile", runtime.GOOS)
						}
						if err := swifttoolchain.EnsureSwiftVersion(cmd.Context(), &dimWriter{}, os.Stderr); err != nil {
							return err
						}
						cliLogln("Building Swift project for %s...", target.Provider.DisplayName())
						// runtime.GOARCH is correct here: Docker Desktop loads images into the
						// host daemon, so the image must match the host architecture.
						if _, err := buildSwiftDockerImage(cmd.Context(), cwd, product, runtime.GOARCH, swiftBuildConfig(opts.debug), &dimWriter{}, os.Stderr); err != nil {
							return fmt.Errorf("building Swift Docker image: %w", err)
						}
						cliSuccess("Build completed successfully.")
						return nil
					}
				}

				// For docker-type projects, resolve which build file to use before
				// calling the provider — shows an interactive picker when multiple
				// build files exist and no --dockerfile flag was given.
				if projectType == "docker" && opts.dockerfile == "" {
					resolved, resolveErr := resolveDockerfile(cwd, "", isInteractiveTerminal(),
						resolveGPUArch(cmd.Context(), cwd, opts.gpuArch, agentConn(target)),
						debugStagefileOptions(opts.debug)...)
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
					app, buildErr = target.Provider.Build(cmd.Context(), *target.External, cwd, projectType, product, false)
				}
				if buildErr != nil {
					return fmt.Errorf("provider build: %w", buildErr)
				}
				cliSuccess("Build completed successfully (%s).", tui.Value(app.ProviderKey))
				return nil
			}

			// Close the agent connection if one was opened during target resolution.
			if target != nil && target.Agent != nil {
				defer target.Agent.Close()
			}

			// A validated wendy.json with a non-empty services map routes to the
			// multi-service build: one LOCAL image per selected service, never
			// pushing to a registry or touching create/start container RPCs.
			if cfgErr == nil && len(appCfg.Services) > 0 {
				// cmd.Flags().Changed, not opts.buildType/opts.dockerfile: the
				// --dockerfile defaulting above (opts.buildType = "docker") mutates
				// the struct, not the flag-set state, so Changed still reflects what
				// the user actually typed.
				if cmd.Flags().Changed("dockerfile") || cmd.Flags().Changed("build-type") {
					return fmt.Errorf("--dockerfile and --build-type are not supported for multi-service projects; each service resolves its own build file from its context")
				}
				return runMultiServiceBuild(cmd.Context(), cwd, appCfg, target, opts)
			}
			if opts.service != "" {
				return fmt.Errorf("--service requires a wendy.json services map")
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
			platform := resolveBuildPlatform(cmd.Context(), target, cfgPlatform)

			appID := filepath.Base(cwd)
			if cfgErr == nil {
				appID = appCfg.AppID
			}

			sfOpts := debugStagefileOptions(opts.debug)
			if cfgErr == nil && selected.Type != "compose" {
				sfOpts = append(sfOpts, frameworkStagefileOptions(appCfg.Frameworks)...)
			}
			return buildProject(cmd.Context(), cwd, selected, appID, platform, opts.builder,
				resolveGPUArch(cmd.Context(), cwd, opts.gpuArch, agentConn(target)),
				opts.debug,
				sfOpts...)
		},
	}

	cmd.Flags().StringVar(&opts.buildType, "build-type", "", "Build type to use when multiple project markers are present: docker, swift, or python")
	cmd.Flags().StringVar(&opts.dockerfile, "dockerfile", "", "Build file to build from: a Dockerfile, Containerfile, or Stagefile (e.g. Dockerfile.prod, Containerfile, prod.stagefile.yaml); shows a selection menu when multiple build files exist")
	cmd.Flags().StringVar(&opts.builder, "builder", "", "Image builder to force for Dockerfile/Containerfile builds: docker, apple-container, or buildkit")
	cmd.Flags().StringVar(&opts.gpuArch, "gpu-arch", "", fmt.Sprintf("GPU architecture a Stagefile cuda: stage targets (%s); taken from the device when one is selected", strings.Join(gpu.KnownArches(), ", ")))
	cmd.Flags().BoolVar(&opts.debug, "debug", false, "Build compiled languages unoptimized (swift build -c debug, cargo without --release) instead of the release default")
	cmd.Flags().StringVar(&opts.buildHost, "build-host", "", "WendyOS device to build the image on instead of this machine (e.g. a DGX Spark)")
	cmd.Flags().StringVar(&opts.service, "service", "", "Build only the named service and its dependencies (multi-service projects)")
	cmd.Flags().IntVar(&opts.maxConcurrency, "max-concurrency", 0, "Multi-service: max service images to build at once (0 = default limit of 4)")
	if err := cmd.Flags().MarkHidden("build-host"); err != nil {
		panic(err)
	}

	return cmd
}

// resolveBuildPlatform is the target platform for a `wendy build`: the
// selected device's own OS/arch when an agent is connected (cfgPlatform
// overrides the device-reported OS (and arch, when it names one)), or
// linux/arm64 with no device to ask.
func resolveBuildPlatform(ctx context.Context, target *SelectedDevice, cfgPlatform string) string {
	platform := "linux/arm64"
	if target != nil && target.Agent != nil {
		versionResp, err := target.Agent.AgentService.GetAgentVersion(ctx, &agentpb.GetAgentVersionRequest{})
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
	return platform
}

// runMultiServiceBuild is the `wendy build` flavor of a services-map project:
// one LOCAL image per selected service, built in parallel. It never pushes to
// a registry and never calls create/start container RPCs — those belong to
// `wendy run`'s multi-service path (runMultiServiceWithAgent).
func runMultiServiceBuild(ctx context.Context, cwd string, appCfg *appconfig.AppConfig, target *SelectedDevice, opts buildOptions) error {
	services, err := resolveServiceSubset(appCfg.Services, opts.service)
	if err != nil {
		return err
	}

	platform := resolveBuildPlatform(ctx, target, appCfg.Platform)

	// Ensure the Apple Container system is up once, before the parallel
	// builds, so an explicit --builder apple-container prompts/starts a single
	// time rather than racing across service goroutines.
	if err := ensureAppleContainerSystemForBuilder(ctx, opts.builder, false); err != nil {
		return err
	}

	dirs := make([]string, 0, len(services))
	for _, svc := range services {
		dirs = append(dirs, filepath.Join(cwd, svc.Context))
	}
	gpuArch := resolveGPUArchForDirs(ctx, dirs, opts.gpuArch, agentConn(target))

	// Debug only: this deliberately matches `wendy run`'s multi-service path
	// (no frameworkStagefileOptions) so both commands build identical images.
	sfOpts := debugStagefileOptions(opts.debug)

	failed, err := buildServicesLocal(ctx, cwd, appCfg.AppID, services, platform, opts.builder, gpuArch, opts.maxConcurrency, false, sfOpts...)
	if err != nil {
		return err
	}
	if len(failed) > 0 {
		return joinServiceErrors(failed)
	}

	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	tags := make([]string, 0, len(names))
	for _, name := range names {
		tags = append(tags, strings.ToLower(appCfg.AppID)+"-"+strings.ToLower(name)+":latest")
	}
	cliSuccess("Built %d service image(s): %s", len(tags), strings.Join(tags, ", "))
	return nil
}

func resolveDetectedBuildOption(options []BuildOption, requestedType, requestedDockerfile string) (*BuildOption, error) {
	interactive := term.IsTerminal(int(os.Stdin.Fd()))

	// --dockerfile selects a specific Dockerfile directly, bypassing type detection.
	if strings.TrimSpace(requestedDockerfile) != "" {
		// Normalise "./Dockerfile.prod" → "Dockerfile.prod" so the flag value
		// matches the plain filenames stored in BuildOption.File.
		normalizedDockerfile := filepath.Clean(requestedDockerfile)
		for i := range options {
			if options[i].Type == "docker" && options[i].File == normalizedDockerfile {
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

	// Non-interactive (CI) fallback: when all detected options are container build
	// files, prefer the base "Dockerfile" or "Containerfile" and fall back to the
	// first variant rather than failing with "multiple build types detected".
	// This mirrors the run-command behaviour and lets CI pipelines that omit
	// --dockerfile build predictably.
	if !interactive {
		allDocker := len(options) > 0
		for _, opt := range options {
			if opt.Type != "docker" {
				allDocker = false
				break
			}
		}
		if allDocker {
			if len(options) == 1 {
				return &options[0], nil
			}
			if preferred := preferredContainerBuildFileOption(options); preferred != nil {
				cliNotice("multiple container build files detected; using %q. Use --dockerfile to select explicitly.", preferred.File)
				return preferred, nil
			}
			cliNotice("multiple container build files detected; using %q. Use --dockerfile to select explicitly.", options[0].File)
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
	for i := range options {
		switch {
		case options[i].Type == "swift" || options[i].Type == "python":
			hasLanguageMarker = true
		case options[i].Type == "docker":
			dockerCount++
		}
	}
	buildFile := preferredContainerBuildFileOption(options)
	if !hasLanguageMarker || buildFile == nil {
		return nil
	}
	if dockerCount == 1 || !interactive {
		return buildFile
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
		buildFile := preferredContainerBuildFileOption(matches)
		if buildFile != nil && !interactive {
			return buildFile, nil
		}
		if len(matches) > 1 {
			if interactive {
				return pickBuildOptionWithTitle(matches, "Select a container build file")
			}
			return nil, fmt.Errorf("multiple container build files detected (%s); keep only one build file or omit --build-type to choose interactively", strings.Join(buildOptionLabels(matches), ", "))
		}
		if buildFile != nil {
			return buildFile, nil
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

func ensureProviderSupportsProjectType(provider providers.DeviceProvider, projectType, projectPath string) error {
	if projectType == "unknown" && provider.CanBuild(projectPath) {
		return nil
	}
	if providerSupportsProjectType(provider, projectType) {
		return nil
	}

	providerName := provider.DisplayName()

	if provider.Key() == providers.ProviderKeyLocal && (projectType == "docker" || projectType == "compose") {
		containerTargets := "Docker with --device docker"
		if projectType == "docker" && appleContainerLocalProviderHintSupported() {
			containerTargets += " or Apple Container with --device apple-container"
		}
		return fmt.Errorf("%s runs host-native apps and does not support %s projects; choose %s for local container runs", providerName, projectType, containerTargets)
	}

	return fmt.Errorf("%s provider does not support %s projects; supported build types: %s", providerName, projectType, strings.Join(provider.SupportedBuildTypes(), ", "))
}

func providerSupportsProjectType(provider providers.DeviceProvider, projectType string) bool {
	if projectType == "swift" {
		if _, ok := provider.(providers.ImageBuilder); ok {
			return true
		}
	}
	for _, supported := range provider.SupportedBuildTypes() {
		if supported == projectType {
			return true
		}
	}
	return false
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

func buildProject(ctx context.Context, dir string, option *BuildOption, appID, platform, builder, gpuArch string, debug bool, sfOpts ...stagefile.Option) error {
	imageName := strings.ToLower(appID) + ":latest"
	normalizedBuilder, err := normalizeImageBuilder(builder)
	if err != nil {
		return err
	}

	switch option.Type {
	case "compose":
		if normalizedBuilder == imageBuilderAppleContainer {
			return fmt.Errorf("Apple Container builder does not support Compose builds; use --builder docker")
		}
		return buildComposeProject(dir, gpuArch, sfOpts...)
	case "docker":
		resolvedFile, err := prepareDockerBuildFile(dir, option.File, gpuArch, sfOpts...)
		if err != nil {
			return err
		}
		return buildDockerProjectWithBuilder(ctx, builder, dir, imageName, platform, resolvedFile)
	case "python":
		return buildPythonProject(ctx, builder, dir, imageName, platform)
	case "swift":
		if normalizedBuilder == imageBuilderAppleContainer {
			return fmt.Errorf("Apple Container builder is only supported for Dockerfile/Containerfile builds; provide a build file or omit --builder")
		}
		// Cross-compiling Swift requires a host toolchain; only darwin and linux ship one.
		if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
			return fmt.Errorf("`wendy build` for Swift packages is not supported on %s; provide a Dockerfile or Containerfile", runtime.GOOS)
		}
		return buildSwiftContainerProject(ctx, dir, appID, platform, debug)
	case "xcode":
		if normalizedBuilder == imageBuilderAppleContainer {
			return fmt.Errorf("Apple Container builder is only supported for Dockerfile/Containerfile builds; provide a build file or omit --builder")
		}
		return buildXcodeProject(ctx, dir, option.File)
	default:
		return fmt.Errorf("unknown project type; add a Dockerfile/Containerfile, a Compose file (docker-compose.yml, docker-compose.yaml, compose.yml, or compose.yaml), Package.swift, or requirements.txt")
	}
}

func buildComposeProject(dir, gpuArch string, sfOpts ...stagefile.Option) error {
	cliLogln("Building Compose services...")
	args := []string{"compose"}
	overridePath, cleanup, err := composeStagefileOverride(dir, gpuArch, sfOpts...)
	if err != nil {
		return err
	}
	if overridePath != "" {
		defer cleanup()
		_, cfgName, err := parseComposeFile(dir)
		if err != nil {
			return err
		}
		args = append(args, "-f", cfgName, "-f", overridePath)
	}
	args = append(args, "build")
	cmd := exec.Command("docker", args...)
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
	cliLogln("Building Docker image %s for %s...", tui.Value(imageName), tui.Value(platform))

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
	p := tui.NewProgressProgram(s)

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

func buildDockerProjectWithBuilder(ctx context.Context, builder, dir, imageName, platform, dockerfile string) error {
	normalized, err := normalizeImageBuilder(builder)
	if err != nil {
		return err
	}
	if !imageBuilderWasExplicit(builder) && shouldAutoAttemptAppleContainerBuilder() {
		// The auto-attempt path must not prompt or start services as a side effect:
		// if Apple Container is not already ready, fall back to Docker. Use
		// --builder apple-container to require Apple Container and get the startup
		// prompt.
		if err := checkAppleContainerBuilder(ctx); err == nil {
			if err := runAppleContainerBuildWithProgress(ctx, dir, imageName, platform, dockerfile); err == nil {
				return nil
			} else {
				logAppleContainerFallback(os.Stderr, err)
			}
		} else {
			logAppleContainerFallback(os.Stderr, err)
		}
	}
	switch normalized {
	case imageBuilderDocker:
		return buildDockerProjectWithDocker(dir, imageName, platform, dockerfile)
	case imageBuilderBuildkit:
		return runBuildWithProgress(ctx, "Building image into Wendy runtime...", dumpRawAlways, func(buildCtx context.Context, stream, logw io.Writer) error {
			return buildDockerProjectWithBuildkit(buildCtx, dir, imageName, platform, dockerfile, nil, stream, logw)
		})
	}

	if err := ensureAppleContainerSystem(ctx, false); err != nil {
		return err
	}
	return runAppleContainerBuildWithProgress(ctx, dir, imageName, platform, dockerfile)
}

// buildLocalServiceImage is the per-service local build step for multi-service
// `wendy build`. Package var so tests can substitute a fake builder.
var buildLocalServiceImage = buildServiceImageLocally

// buildServiceImageLocally builds one service's image into the selected local
// image store (Docker --load, Apple Container's implicit store, or the
// containerd store owned by a BuildKit worker) — never a registry. It mirrors
// buildDockerProjectWithBuilder's builder selection but is writer-driven and
// context-aware, as buildServicesParallelCore requires: no os.Stdout and no
// owned spinner.
func buildServiceImageLocally(ctx context.Context, builder, contextDir, imageName, platform, dockerfile string, buildOut, logOut io.Writer) error {
	if dockerfile == "" {
		return fmt.Errorf("no container build file found in %s; add a Dockerfile, Containerfile, or Stagefile (e.g. build.stagefile.yaml)", contextDir)
	}
	normalized, err := normalizeImageBuilder(builder)
	if err != nil {
		return err
	}
	if !imageBuilderWasExplicit(builder) && shouldAutoAttemptAppleContainerBuilder() {
		// Same auto-attempt semantics as the single-image path: never prompt or
		// start services as a side effect, just try and fall back to Docker.
		if err := checkAppleContainerBuilder(ctx); err == nil {
			if err := buildImageWithAppleContainer(ctx, contextDir, imageName, platform, dockerfile, nil, buildOut, logOut); err == nil {
				return nil
			} else {
				logAppleContainerFallback(logOut, err)
			}
		} else {
			logAppleContainerFallback(logOut, err)
		}
	}

	if normalized == imageBuilderAppleContainer {
		// The Apple Container system itself is ensured once by the caller before
		// the parallel builds start, not per service.
		return buildImageWithAppleContainer(ctx, contextDir, imageName, platform, dockerfile, nil, buildOut, logOut)
	}
	if normalized == imageBuilderBuildkit {
		return buildDockerProjectWithBuildkit(ctx, contextDir, imageName, platform, dockerfile, nil, buildOut, logOut)
	}

	// No OCI-layout export and no per-service cache-dir isolation: a plain
	// --load build uses the daemon-side BuildKit cache, which is
	// concurrency-safe on its own.
	args := []string{"buildx", "build", "--platform", platform, "--progress", "plain", "-f", dockerfile, "-t", imageName, "--load", "."}
	fmt.Fprintf(logOut, "[buildx] starting build: docker %s\n", strings.Join(args, " "))
	cmd := imageBuilderCommandContext(ctx, "docker", args...)
	cmd.Dir = contextDir
	// --progress plain emits BuildKit's step output on stderr; both streams go
	// to buildOut so tui.BuildParser (wired in by the core's per-service
	// writers) sees it.
	cmd.Stdout = buildOut
	cmd.Stderr = buildOut
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("building image %s: %w", imageName, err)
	}
	return nil
}

func buildPythonProject(ctx context.Context, builder, dir, imageName, platform string) error {
	dockerfilePath := filepath.Join(dir, "Dockerfile")
	generatedDockerfile := false
	if _, err := os.Stat(dockerfilePath); os.IsNotExist(err) {
		cliLogln("No Dockerfile found. Generating one for Python project...")
		if _, genErr := generatePythonDockerfile(dir, false); genErr != nil {
			return fmt.Errorf("generating Dockerfile: %w", genErr)
		}
		generatedDockerfile = true
		cliSuccess("Generated Dockerfile.")
	}

	err := buildDockerProjectWithBuilder(ctx, builder, dir, imageName, platform, "Dockerfile")

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

	cliLogln("Building Xcode project %s (scheme: %s)...", tui.Value(xcodeproj), tui.Value(scheme))
	// SECURITY: Xcode project support exists for native Mac packages that cannot be
	// built correctly with SwiftPM alone today, for example packages that require
	// Xcode-only resource or shader build steps (see
	// docs/clients/wendy-cli/commands/build.md).
	// Xcode's macro/plugin prompts are an interactive consent layer on top of
	// SwiftPM's build-time code/sandbox model; headless Wendy CLI builds cannot
	// answer those prompts, so we deliberately make xcodebuild behave like CLI
	// build tools and rely on a trusted, pinned Package.resolved.
	if err := runXcodebuild(ctx, dir,
		"-project", xcodeproj,
		"-scheme", scheme,
		"-configuration", "Release",
		"-derivedDataPath", ".xcode/",
		"-skipMacroValidation",
		"-skipPackagePluginValidation",
	); err != nil {
		return err
	}
	cliSuccess("Build completed successfully.")
	return nil
}

func buildSwiftContainerProject(ctx context.Context, dir, appID, platform string, debug bool) error {
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

	if _, err := buildSwiftDockerImage(ctx, dir, product, arch, swiftBuildConfig(debug), &dimWriter{}, os.Stderr); err != nil {
		return err
	}
	cliSuccess("Build completed successfully.")
	return nil
}
