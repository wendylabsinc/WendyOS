package commands

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/cli/providers"
	"github.com/wendylabsinc/wendy/internal/cli/tui"
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
		Long:  "Detects the project type and builds a Docker image for linux/arm64.",
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("getting working directory: %w", err)
			}

			cfgPath := filepath.Join(cwd, "wendy.json")
			appCfg, cfgErr := ensureAppConfig(cfgPath)

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

			// Existing agent-targeted build path.
			var language string
			if cfgErr == nil {
				language = appCfg.Language
			}

			appID := filepath.Base(cwd)
			if cfgErr == nil {
				appID = appCfg.AppID
			}

			projectType := detectProjectTypeWithLanguage(cwd, language)
			return buildProject(cmd.Context(), cwd, projectType, appID)
		},
	}

	return cmd
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

func buildProject(ctx context.Context, dir, projectType, appID string) error {
	imageName := appID + ":latest"

	switch projectType {
	case "docker":
		return buildDockerProject(dir, imageName)
	case "python":
		return buildPythonProject(dir, imageName)
	case "swift":
		return buildSwiftProject(dir, appID)
	default:
		return fmt.Errorf("unknown project type; add a Dockerfile, Package.swift, or requirements.txt")
	}
}

func buildDockerProject(dir, imageName string) error {
	fmt.Printf("Building Docker image %s for linux/arm64...\n", imageName)

	s := tui.NewSpinner("Building Docker image...")
	p := tea.NewProgram(s)

	go func() {
		cmd := exec.Command("docker", "buildx", "build",
			"--platform", "linux/arm64",
			"-t", imageName,
			"--load",
			".")
		cmd.Dir = dir
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

func buildPythonProject(dir, imageName string) error {
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

	err := buildDockerProject(dir, imageName)

	if generatedDockerfile {
		os.Remove(dockerfilePath)
	}

	return err
}

func buildSwiftProject(dir, appID string) error {
	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err == nil {
		return buildDockerProject(dir, appID+":latest")
	}

	fmt.Println("Building Swift project locally...")
	s := tui.NewSpinner("Building Swift project...")
	p := tea.NewProgram(s)

	go func() {
		cmd := exec.Command("swift", "build")
		cmd.Dir = dir
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
