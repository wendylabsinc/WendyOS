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

// androidBuildContext is stored in BuiltApp.Context for ADB builds.
type androidBuildContext struct {
	APKPath    string
	PackageID  string
	ActivityID string
	Serial     string
}

// AndroidProvider builds Swift packages into APKs and deploys via ADB.
type AndroidProvider struct{}

func (p *AndroidProvider) Key() string         { return "android" }
func (p *AndroidProvider) DisplayName() string { return "Android (ADB)" }

func (p *AndroidProvider) IsAvailable(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "adb", "version")
	return cmd.Run() == nil
}

func (p *AndroidProvider) CheckRequirements(ctx context.Context) error {
	if !p.IsAvailable(ctx) {
		return fmt.Errorf("adb is not installed or not in PATH")
	}
	if cmd := exec.CommandContext(ctx, "swiftly", "--version"); cmd.Run() != nil {
		return fmt.Errorf("swiftly is not installed (needed for Swift Android SDK)")
	}
	return nil
}

func (p *AndroidProvider) DiscoverDevices(ctx context.Context) ([]models.ExternalDevice, error) {
	cmd := exec.CommandContext(ctx, "adb", "devices", "-l")
	out, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	var devices []models.ExternalDevice
	scanner := bufio.NewScanner(strings.NewReader(string(out)))
	for scanner.Scan() {
		line := scanner.Text()
		// Skip header and empty lines.
		if strings.HasPrefix(line, "List of") || strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != "device" {
			continue
		}
		serial := fields[0]

		// Extract model from the key=value pairs.
		displayName := serial
		for _, f := range fields[2:] {
			if strings.HasPrefix(f, "model:") {
				displayName = strings.TrimPrefix(f, "model:")
				break
			}
		}

		devices = append(devices, models.ExternalDevice{
			ID:              "adb:" + serial,
			DisplayName:     displayName,
			ProviderKey:     p.Key(),
			ConnectionInfo:  map[string]string{"serial": serial},
			IsWendyDevice:   false,
			OS:              "android",
			CPUArchitecture: "arm64",
		})
	}
	return devices, nil
}

func (p *AndroidProvider) CanBuild(projectPath string) bool {
	_, err := os.Stat(projectPath + "/Package.swift")
	return err == nil
}

func (p *AndroidProvider) Build(ctx context.Context, device models.ExternalDevice, projectPath, product string, debug bool) (*BuiltApp, error) {
	// Use swiftly to invoke the Swift Android SDK bundle-apk command.
	args := []string{"run", "+main-snapshot", "swift", "package", "bundle-apk"}
	cmd := exec.CommandContext(ctx, "swiftly", args...)
	cmd.Dir = projectPath
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("swift bundle-apk: %w", err)
	}

	serial := device.ConnectionInfo["serial"]
	return &BuiltApp{
		ProviderKey: p.Key(),
		Device:      device,
		AppName:     product,
		Context: &androidBuildContext{
			APKPath:    ".build/apk/" + product + ".apk",
			PackageID:  product,
			ActivityID: product + "/.MainActivity",
			Serial:     serial,
		},
	}, nil
}

func (p *AndroidProvider) Run(ctx context.Context, app *BuiltApp, detach bool, output chan<- RunOutput) error {
	defer close(output)

	bc, ok := app.Context.(*androidBuildContext)
	if !ok {
		return fmt.Errorf("android provider: invalid build context")
	}

	serialArgs := []string{}
	if bc.Serial != "" {
		serialArgs = []string{"-s", bc.Serial}
	}

	// Install the APK.
	installArgs := append(serialArgs, "install", "-r", bc.APKPath)
	cmd := exec.CommandContext(ctx, "adb", installArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("adb install: %w", err)
	}

	// Launch the activity.
	startArgs := append(serialArgs, "shell", "am", "start", "-n", bc.ActivityID)
	cmd = exec.CommandContext(ctx, "adb", startArgs...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("adb am start: %w", err)
	}

	output <- RunOutput{Type: RunOutputStarted}

	if detach {
		return nil
	}

	// Stream logcat for this package.
	logcatArgs := append(serialArgs, "logcat", "--pid", fmt.Sprintf("$(adb %s shell pidof %s)", strings.Join(serialArgs, " "), bc.PackageID))
	// Simplified: stream all logcat filtered by tag.
	logcatArgs = append(serialArgs, "logcat", "-v", "brief")
	logCmd := exec.CommandContext(ctx, "adb", logcatArgs...)
	stdoutPipe, err := logCmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("logcat stdout pipe: %w", err)
	}
	if err := logCmd.Start(); err != nil {
		return fmt.Errorf("adb logcat: %w", err)
	}

	scanner := bufio.NewScanner(stdoutPipe)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	for scanner.Scan() {
		output <- RunOutput{Type: RunOutputStdout, Data: append(scanner.Bytes(), '\n')}
	}
	return logCmd.Wait()
}

func (p *AndroidProvider) Stop(ctx context.Context, app *BuiltApp) error {
	bc, ok := app.Context.(*androidBuildContext)
	if !ok {
		return fmt.Errorf("android provider: invalid build context")
	}
	serialArgs := []string{}
	if bc.Serial != "" {
		serialArgs = []string{"-s", bc.Serial}
	}
	stopArgs := append(serialArgs, "shell", "am", "force-stop", bc.PackageID)
	cmd := exec.CommandContext(ctx, "adb", stopArgs...)
	return cmd.Run()
}
