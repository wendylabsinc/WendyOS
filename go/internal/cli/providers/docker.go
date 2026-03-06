package providers

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/wendylabsinc/wendy/internal/shared/models"
)

// dockerBuildContext is stored in BuiltApp.Context for Docker builds.
type dockerBuildContext struct {
	ImageName     string
	ContainerName string
	cmd           *exec.Cmd
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

func (p *DockerProvider) CanBuild(projectPath string) bool {
	_, err := os.Stat(projectPath + "/Dockerfile")
	return err == nil
}

func (p *DockerProvider) Build(ctx context.Context, device models.ExternalDevice, projectPath, product string, debug bool) (*BuiltApp, error) {
	imageName := strings.ToLower(product) + ":latest"
	args := []string{"build", "-t", imageName, "."}
	cmd := exec.CommandContext(ctx, "docker", args...)
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

func (p *DockerProvider) Run(ctx context.Context, app *BuiltApp, detach bool, output chan<- RunOutput) error {
	defer close(output)

	bc, ok := app.Context.(*dockerBuildContext)
	if !ok {
		return fmt.Errorf("docker provider: invalid build context")
	}

	args := []string{"run", "--rm", "--name", bc.ContainerName}
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
	bc, ok := app.Context.(*dockerBuildContext)
	if !ok {
		return fmt.Errorf("docker provider: invalid build context")
	}
	cmd := exec.CommandContext(ctx, "docker", "stop", bc.ContainerName)
	return cmd.Run()
}
