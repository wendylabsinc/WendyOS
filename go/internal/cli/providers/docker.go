package providers

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
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
	return p.BuildWithType(ctx, device, projectPath, product, "", debug)
}

// BuildWithType is the typed-build entry point. When buildType is "compose" or
// empty (auto), it uses the compose file if one is present. When buildType is
// "docker", it builds the Dockerfile directly even if a compose file also
// exists in the project root.
func (p *DockerProvider) BuildWithType(ctx context.Context, device models.ExternalDevice, projectPath, product, buildType string, debug bool) (*BuiltApp, error) {
	useCompose := false
	cf := composeFile(projectPath)
	switch buildType {
	case "compose":
		useCompose = true
	case "docker":
		useCompose = false
	default:
		// Auto: prefer compose when a compose file is present.
		useCompose = cf != ""
	}

	if useCompose {
		if cf == "" {
			return nil, fmt.Errorf("no Compose file found in %s", projectPath)
		}
		args := []string{"compose", "-f", cf, "build"}
		cmd := exec.CommandContext(ctx, "docker", args...)
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

// composeArgs returns docker-compose CLI arguments with -f set to the project's
// compose file when known. Pinning the file makes execution deterministic when
// multiple compose markers exist in the directory.
func composeArgs(cc *dockerComposeBuildContext, sub ...string) []string {
	args := []string{"compose"}
	if cc.ComposeFile != "" {
		args = append(args, "-f", cc.ComposeFile)
	}
	return append(args, sub...)
}

// scanLines pumps a reader through a Scanner and emits one RunOutput per line.
// Each emitted slice is a fresh copy of the scanner's buffer, because Scanner
// reuses its internal buffer between scans and reusing it across the channel
// would corrupt earlier lines.
func scanLines(r io.Reader, output chan<- RunOutput, kind RunOutputType, done chan<- struct{}) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		buf := make([]byte, len(line)+1)
		copy(buf, line)
		buf[len(line)] = '\n'
		output <- RunOutput{Type: kind, Data: buf}
	}
	done <- struct{}{}
}

func (p *DockerProvider) runCompose(ctx context.Context, cc *dockerComposeBuildContext, detach bool, output chan<- RunOutput) error {
	args := composeArgs(cc, "up", "--remove-orphans")
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
	go scanLines(stdoutPipe, output, RunOutputStdout, done)
	go scanLines(stderrPipe, output, RunOutputStderr, done)

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
	inspectCmd := exec.CommandContext(ctx, "docker", "inspect", "-f", `{{index .Config.Labels "wendy.managed"}}`, bc.ContainerName)
	inspectOut, inspectErr := inspectCmd.Output()
	if inspectErr != nil {
		if exitErr, ok := inspectErr.(*exec.ExitError); ok {
			stderr := string(exitErr.Stderr)
			if !strings.Contains(strings.ToLower(stderr), "no such object") {
				return fmt.Errorf("docker inspect: %w: %s", inspectErr, strings.TrimSpace(stderr))
			}
		} else {
			return fmt.Errorf("docker inspect: %w", inspectErr)
		}
	} else if strings.TrimSpace(string(inspectOut)) == "true" {
		rmOut, rmErr := exec.CommandContext(ctx, "docker", "rm", "-f", bc.ContainerName).CombinedOutput()
		if rmErr != nil {
			if !strings.Contains(string(rmOut), "No such container") {
				return fmt.Errorf("docker rm: %w: %s", rmErr, strings.TrimSpace(string(rmOut)))
			}
		}
	}

	args := []string{"run", "--name", bc.ContainerName, "--label", "wendy.managed=true"}
	for k, v := range appconfig.BuildEntitlementAnnotations(app.Entitlements) {
		args = append(args, "--label", k+"="+v)
	}
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
	go scanLines(stdoutPipe, output, RunOutputStdout, done)
	go scanLines(stderrPipe, output, RunOutputStderr, done)

	<-done
	<-done
	return cmd.Wait()
}

func (p *DockerProvider) Stop(ctx context.Context, app *BuiltApp) error {
	if cc, ok := app.Context.(*dockerComposeBuildContext); ok {
		cmd := exec.CommandContext(ctx, "docker", composeArgs(cc, "down")...)
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
