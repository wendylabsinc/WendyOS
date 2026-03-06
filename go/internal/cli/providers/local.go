package providers

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/wendylabsinc/wendy/internal/shared/models"
)

// localBuildContext is stored in BuiltApp.Context for local builds.
type localBuildContext struct {
	BinaryPath string
	cmd        *exec.Cmd
}

// LocalProvider builds and runs applications on the local machine.
type LocalProvider struct{}

func (p *LocalProvider) Key() string         { return "local" }
func (p *LocalProvider) DisplayName() string { return "Local (This Device)" }

func (p *LocalProvider) IsAvailable(_ context.Context) bool { return true }

func (p *LocalProvider) CheckRequirements(_ context.Context) error { return nil }

func (p *LocalProvider) DiscoverDevices(_ context.Context) ([]models.ExternalDevice, error) {
	return []models.ExternalDevice{
		{
			ID:              "local",
			DisplayName:     "Local Machine",
			ProviderKey:     p.Key(),
			IsWendyDevice:   false,
			OS:              runtime.GOOS,
			CPUArchitecture: runtime.GOARCH,
		},
	}, nil
}

func (p *LocalProvider) CanBuild(projectPath string) bool {
	for _, marker := range []string{"Dockerfile", "Package.swift", "go.mod", "requirements.txt", "pyproject.toml", "setup.py"} {
		if _, err := os.Stat(filepath.Join(projectPath, marker)); err == nil {
			return true
		}
	}
	return false
}

func (p *LocalProvider) Build(ctx context.Context, device models.ExternalDevice, projectPath, product string, debug bool) (*BuiltApp, error) {
	// Determine build strategy based on project markers.
	if _, err := os.Stat(filepath.Join(projectPath, "Dockerfile")); err == nil {
		return p.buildDocker(ctx, projectPath, product, debug)
	}
	if _, err := os.Stat(filepath.Join(projectPath, "Package.swift")); err == nil {
		return p.buildSwift(ctx, device, projectPath, product, debug)
	}
	if _, err := os.Stat(filepath.Join(projectPath, "go.mod")); err == nil {
		return p.buildGo(ctx, device, projectPath, product, debug)
	}
	// Python doesn't need a compile step; just reference the entry point.
	for _, marker := range []string{"requirements.txt", "pyproject.toml", "setup.py"} {
		if _, err := os.Stat(filepath.Join(projectPath, marker)); err == nil {
			return p.buildPython(ctx, device, projectPath, product)
		}
	}
	return nil, fmt.Errorf("local provider: cannot determine build method for %s", projectPath)
}

func (p *LocalProvider) buildDocker(ctx context.Context, projectPath, product string, debug bool) (*BuiltApp, error) {
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
		Device:      models.ExternalDevice{ID: "local", ProviderKey: p.Key()},
		AppName:     product,
		Context:     &localBuildContext{BinaryPath: imageName},
	}, nil
}

func (p *LocalProvider) buildSwift(ctx context.Context, device models.ExternalDevice, projectPath, product string, debug bool) (*BuiltApp, error) {
	args := []string{"build"}
	if !debug {
		args = append(args, "-c", "release")
	}
	cmd := exec.CommandContext(ctx, "swift", args...)
	cmd.Dir = projectPath
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("swift build: %w", err)
	}
	config := "debug"
	if !debug {
		config = "release"
	}
	binaryPath := filepath.Join(projectPath, ".build", config, product)
	return &BuiltApp{
		ProviderKey: p.Key(),
		Device:      device,
		AppName:     product,
		Context:     &localBuildContext{BinaryPath: binaryPath},
	}, nil
}

func (p *LocalProvider) buildGo(ctx context.Context, device models.ExternalDevice, projectPath, product string, debug bool) (*BuiltApp, error) {
	outputPath := filepath.Join(projectPath, product)
	args := []string{"build", "-o", outputPath}
	if !debug {
		args = append(args, "-ldflags", "-s -w")
	}
	args = append(args, ".")
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = projectPath
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("go build: %w", err)
	}
	return &BuiltApp{
		ProviderKey: p.Key(),
		Device:      device,
		AppName:     product,
		Context:     &localBuildContext{BinaryPath: outputPath},
	}, nil
}

func (p *LocalProvider) buildPython(_ context.Context, device models.ExternalDevice, projectPath, product string) (*BuiltApp, error) {
	// No compile step; determine the entry point.
	entry := "app.py"
	for _, candidate := range []string{"app.py", "main.py"} {
		if _, err := os.Stat(filepath.Join(projectPath, candidate)); err == nil {
			entry = candidate
			break
		}
	}
	return &BuiltApp{
		ProviderKey: p.Key(),
		Device:      device,
		AppName:     product,
		Context:     &localBuildContext{BinaryPath: filepath.Join(projectPath, entry)},
	}, nil
}

func (p *LocalProvider) Run(ctx context.Context, app *BuiltApp, detach bool, output chan<- RunOutput) error {
	defer close(output)

	bc, ok := app.Context.(*localBuildContext)
	if !ok {
		return fmt.Errorf("local provider: invalid build context")
	}

	// If built via docker, delegate to docker run.
	if _, err := os.Stat(bc.BinaryPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("local provider: cannot stat binary: %w", err)
	}

	// Determine how to run.
	var cmd *exec.Cmd
	if filepath.Ext(bc.BinaryPath) == ".py" {
		cmd = exec.CommandContext(ctx, "python3", bc.BinaryPath)
	} else {
		cmd = exec.CommandContext(ctx, bc.BinaryPath)
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
		return fmt.Errorf("starting process: %w", err)
	}
	bc.cmd = cmd

	output <- RunOutput{Type: RunOutputStarted}

	if detach {
		return nil
	}

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

func (p *LocalProvider) Stop(_ context.Context, app *BuiltApp) error {
	bc, ok := app.Context.(*localBuildContext)
	if !ok {
		return fmt.Errorf("local provider: invalid build context")
	}
	if bc.cmd != nil && bc.cmd.Process != nil {
		return bc.cmd.Process.Kill()
	}
	return nil
}
