package providers

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wendylabsinc/wendy/internal/shared/models"
)

// dockerBuildContext is stored in BuiltApp.Context for Docker builds.
type dockerBuildContext struct {
	ImageName     string
	ContainerName string
	cmd           *exec.Cmd
}

// dockerComposeBuildContext is stored in BuiltApp.Context for Compose builds.
type dockerComposeBuildContext struct {
	ProjectDir  string
	ComposeFile string
}

// composeFile returns the first docker-compose filename found in dir, or "".
func composeFile(dir string) string {
	for _, name := range []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			return name
		}
	}
	return ""
}

// DockerProvider builds and runs applications in Docker Desktop containers.
type DockerProvider struct{}

func (p *DockerProvider) Key() string         { return "docker" }
func (p *DockerProvider) DisplayName() string { return "Docker Desktop" }

func (p *DockerProvider) IsAvailable(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "docker", "--version")
	return cmd.Run() == nil
}

func (p *DockerProvider) CheckRequirements(ctx context.Context) error {
	if !p.IsAvailable(ctx) {
		return fmt.Errorf("docker is not installed or not in PATH")
	}
	cmd := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker daemon is not running: %w", err)
	}
	return nil
}

func (p *DockerProvider) DiscoverDevices(ctx context.Context) ([]models.ExternalDevice, error) {
	cmd := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	out, err := cmd.Output()
	if err != nil {
		return nil, nil // docker not running, no devices
	}
	version := strings.TrimSpace(string(out))
	return []models.ExternalDevice{
		{
			ID:            "docker",
			DisplayName:   "Docker Desktop",
			ProviderKey:   p.Key(),
			IsWendyDevice: false,
			AgentVersion:  version,
		},
	}, nil
}

func (p *DockerProvider) SupportedBuildTypes() []string {
	return []string{"docker", "compose"}
}

func (p *DockerProvider) CanBuild(projectPath string) bool {
	if _, err := os.Stat(filepath.Join(projectPath, "Dockerfile")); err == nil {
		return true
	}
	return composeFile(projectPath) != ""
}

func (p *DockerProvider) Build(ctx context.Context, device models.ExternalDevice, projectPath, product string, debug bool) (*BuiltApp, error) {
	if cf := composeFile(projectPath); cf != "" {
		cmd := exec.CommandContext(ctx, "docker", "compose", "build")
		cmd.Dir = projectPath
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("docker compose build: %w", err)
		}
		return &BuiltApp{
			ProviderKey: p.Key(),
			Device:      device,
			AppName:     product,
			Context:     &dockerComposeBuildContext{ProjectDir: projectPath, ComposeFile: cf},
		}, nil
	}

	imageName := strings.ToLower(product) + ":latest"
	cmd := exec.CommandContext(ctx, "docker", "build", "-t", imageName, ".")
	cmd.Dir = projectPath
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker build: %w", err)
	}
	return &BuiltApp{
		ProviderKey: p.Key(),
		Device:      device,
		AppName:     product,
		Context: &dockerBuildContext{
			ImageName:     imageName,
			ContainerName: product,
		},
	}, nil
}

// BuildFromImage creates a BuiltApp handle for a pre-built Docker image.
// This is used when the image was built outside of the provider's Build method
// (e.g. Swift cross-compilation followed by docker build).
func (p *DockerProvider) BuildFromImage(device models.ExternalDevice, product, imageName string) *BuiltApp {
	return &BuiltApp{
		ProviderKey: p.Key(),
		Device:      device,
		AppName:     product,
		Context: &dockerBuildContext{
			ImageName:     imageName,
			ContainerName: product,
		},
	}
}

func (p *DockerProvider) runCompose(ctx context.Context, cc *dockerComposeBuildContext, detach bool, output chan<- RunOutput) error {
	args := []string{"compose", "up", "--remove-orphans"}
	if detach {
		args = append(args, "-d")
	}

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = cc.ProjectDir

	if detach {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("docker compose up: %w", err)
		}
		output <- RunOutput{Type: RunOutputStarted}
		return nil
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("docker compose up: %w", err)
	}

	output <- RunOutput{Type: RunOutputStarted}

	done := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		for scanner.Scan() {
			output <- RunOutput{Type: RunOutputStdout, Data: append(scanner.Bytes(), '\n')}
		}
		done <- struct{}{}
	}()
	go func() {
		scanner := bufio.NewScanner(stderrPipe)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		for scanner.Scan() {
			output <- RunOutput{Type: RunOutputStderr, Data: append(scanner.Bytes(), '\n')}
		}
		done <- struct{}{}
	}()

	<-done
	<-done
	return cmd.Wait()
}

func (p *DockerProvider) Run(ctx context.Context, app *BuiltApp, detach bool, output chan<- RunOutput) error {
	defer close(output)

	if cc, ok := app.Context.(*dockerComposeBuildContext); ok {
		return p.runCompose(ctx, cc, detach, output)
	}

	bc, ok := app.Context.(*dockerBuildContext)
	if !ok {
		return fmt.Errorf("docker provider: invalid build context")
	}

	// Remove any existing Wendy-managed container with the same name to avoid conflicts.
	inspectCmd := exec.CommandContext(ctx, "docker", "inspect", "-f", "{{.Config.Labels.wendy.managed}}", bc.ContainerName)
	inspectOut, err := inspectCmd.Output()
	if err != nil {
		// If the container does not exist, docker inspect typically reports "No such object".
		if exitErr, ok := err.(*exec.ExitError); ok {
			stderr := string(exitErr.Stderr)
			if !strings.Contains(stderr, "No such object") {
				return fmt.Errorf("docker inspect: %w: %s", err, strings.TrimSpace(stderr))
			}
		} else {
			return fmt.Errorf("docker inspect: %w", err)
		}
	} else if strings.TrimSpace(string(inspectOut)) == "true" {
		rmCmd := exec.CommandContext(ctx, "docker", "rm", "-f", bc.ContainerName)
		rmOut, rmErr := rmCmd.CombinedOutput()
		if rmErr != nil {
			rmMsg := string(rmOut)
			if !strings.Contains(rmMsg, "No such container") {
				return fmt.Errorf("docker rm: %w: %s", rmErr, strings.TrimSpace(rmMsg))
			}
		}
	}

	args := []string{"run", "--name", bc.ContainerName, "--label", "wendy.managed=true"}
	if detach {
		args = append(args, "-d")
	}
	args = append(args, bc.ImageName)

	cmd := exec.CommandContext(ctx, "docker", args...)
	bc.cmd = cmd

	if detach {
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("docker run: %w", err)
		}
		output <- RunOutput{Type: RunOutputStarted}
		return nil
	}

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("docker run: %w", err)
	}

	output <- RunOutput{Type: RunOutputStarted}

	done := make(chan struct{})
	go func() {
		scanner := bufio.NewScanner(stdoutPipe)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		for scanner.Scan() {
			output <- RunOutput{Type: RunOutputStdout, Data: append(scanner.Bytes(), '\n')}
		}
		done <- struct{}{}
	}()
	go func() {
		scanner := bufio.NewScanner(stderrPipe)
		scanner.Buffer(make([]byte, 64*1024), 64*1024)
		for scanner.Scan() {
			output <- RunOutput{Type: RunOutputStderr, Data: append(scanner.Bytes(), '\n')}
		}
		done <- struct{}{}
	}()

	<-done
	<-done
	return cmd.Wait()
}

func (p *DockerProvider) Stop(ctx context.Context, app *BuiltApp) error {
	if cc, ok := app.Context.(*dockerComposeBuildContext); ok {
		cmd := exec.CommandContext(ctx, "docker", "compose", "down")
		cmd.Dir = cc.ProjectDir
		return cmd.Run()
	}

	bc, ok := app.Context.(*dockerBuildContext)
	if !ok {
		return fmt.Errorf("docker provider: invalid build context")
	}
	cmd := exec.CommandContext(ctx, "docker", "stop", bc.ContainerName)
	return cmd.Run()
}

// ContainerManager implementation for Docker Desktop.

func (p *DockerProvider) ListContainers(ctx context.Context) ([]ContainerInfo, error) {
	cmd := exec.CommandContext(ctx, "docker", "ps", "-a",
		"--filter", "label=wendy.managed=true",
		"--format", "{{.Names}}\t{{.Image}}\t{{.State}}\t{{.Status}}")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("docker ps: %w", err)
	}

	var containers []ContainerInfo
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 4)
		if len(parts) < 4 {
			continue
		}
		containers = append(containers, ContainerInfo{
			Name:   parts[0],
			Image:  parts[1],
			State:  parts[2],
			Status: parts[3],
		})
	}
	return containers, nil
}

func (p *DockerProvider) StartContainer(ctx context.Context, name string) error {
	cmd := exec.CommandContext(ctx, "docker", "start", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker start: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (p *DockerProvider) StopContainer(ctx context.Context, name string) error {
	cmd := exec.CommandContext(ctx, "docker", "stop", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker stop: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}

func (p *DockerProvider) RemoveContainer(ctx context.Context, name string) error {
	cmd := exec.CommandContext(ctx, "docker", "rm", "-f", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker rm: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
