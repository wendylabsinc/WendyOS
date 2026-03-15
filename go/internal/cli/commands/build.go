package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/providers"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
	"github.com/wendylabsinc/wendy/proto/gen/agentpb"
	"golang.org/x/term"
)

// BuildResult is the output of the build command. Exactly one field is set.
type BuildResult struct {
	// ProviderApp is set when the build used an external provider.
	ProviderApp *providers.BuiltApp
}

func newBuildCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build the application in the current directory",
		Long:  "Detects the project type and builds a Docker image for the target device architecture.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("getting working directory: %w", err)
			}

			cfgPath := filepath.Join(cwd, "wendy.json")
			appCfg, cfgErr := ensureAppConfig(cfgPath, false)

			target, _ := resolveTarget(cmd.Context())

			// If the target is an external provider device, use the provider build path.
			if target != nil && target.External != nil && target.Provider != nil {
				product := filepath.Base(cwd)
				if cfgErr == nil {
					product = appCfg.AppID
				}

				fmt.Printf("Building with %s provider...\n", target.Provider.DisplayName())
				app, err := target.Provider.Build(cmd.Context(), *target.External, cwd, product, false)
				if err != nil {
					return fmt.Errorf("provider build: %w", err)
				}
				fmt.Printf("Build completed successfully (%s).\n", app.ProviderKey)
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

			selected, err := pickBuildOption(options)
			if err != nil {
				return err
			}

			// Query the device architecture when an agent connection is available.
			platform := "linux/arm64"
			if target != nil && target.Agent != nil {
				versionResp, err := target.Agent.AgentService.GetAgentVersion(cmd.Context(), &agentpb.GetAgentVersionRequest{})
				if err == nil {
					if arch := versionResp.GetCpuArchitecture(); arch != "" {
						platform = "linux/" + arch
					}
				}
			}

			appID := filepath.Base(cwd)
			if cfgErr == nil {
				appID = appCfg.AppID
			}

			return buildProject(cmd.Context(), cwd, selected, appID, platform)
		},
	}

	return cmd
}

// pickBuildOption presents an interactive picker when multiple build options
// are detected. If only one option exists, it is returned directly.
func pickBuildOption(options []BuildOption) (*BuildOption, error) {
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

	picker := tui.NewPickerWithTitle("Select a build type")
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
	return detectProjectType(dir)
}

func buildProject(ctx context.Context, dir string, option *BuildOption, appID, platform string) error {
	imageName := strings.ToLower(appID) + ":latest"

	switch option.Type {
	case "docker":
		return buildDockerProject(dir, imageName, platform, option.File)
	case "python":
		return buildPythonProject(dir, imageName, platform)
	case "swift":
		return buildSwiftProject(dir, appID, platform)
	default:
		return fmt.Errorf("unknown project type; add a Dockerfile, Package.swift, or requirements.txt")
	}
}

func buildDockerProject(dir, imageName, platform, dockerfile string) error {
	fmt.Printf("Building Docker image %s for %s...\n", imageName, platform)

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
		fmt.Println("Build completed successfully.")
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

	fmt.Println("Build completed successfully.")
	return nil
}

func buildPythonProject(dir, imageName, platform string) error {
	dockerfilePath := filepath.Join(dir, "Dockerfile")
	generatedDockerfile := false
	if _, err := os.Stat(dockerfilePath); os.IsNotExist(err) {
		fmt.Println("No Dockerfile found. Generating one for Python project...")
		if _, genErr := generatePythonDockerfile(dir); genErr != nil {
			return fmt.Errorf("generating Dockerfile: %w", genErr)
		}
		generatedDockerfile = true
		fmt.Println("Generated Dockerfile.")
	}

	err := buildDockerProject(dir, imageName, platform, "Dockerfile")

	if generatedDockerfile {
		os.Remove(dockerfilePath)
	}

	return err
}

func buildSwiftProject(dir, appID, platform string) error {
	fmt.Println("Building Swift project locally...")

	cmd := exec.Command("swift", "build")
	cmd.Dir = dir

	if !term.IsTerminal(int(os.Stdout.Fd())) {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return err
		}
		fmt.Println("Build completed successfully.")
		return nil
	}

	s := tui.NewSpinner("Building Swift project...")
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

	fmt.Println("Build completed successfully.")
	return nil
}
